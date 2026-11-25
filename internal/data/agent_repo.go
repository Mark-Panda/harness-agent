package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// agentModel 对应 agents 表：用户私有 Agent 版本。
type agentModel struct {
	// ID Agent 版本主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Name Agent 名称。
	Name string
	// Description Agent 描述。
	Description string
	// Mode 编排模式：目前仅支持 react。
	Mode string
	// ModelPolicyID 引用的模型策略 ID。
	ModelPolicyID *uuid.UUID `gorm:"type:uuid"`
	// ModelPolicyVersion 引用的策略版本，与 ID 一起构成外键。
	ModelPolicyVersion *int
	// Version Agent 版本号。
	Version int
	// ConfigSnapshot 发布时固化的配置快照 JSON。
	ConfigSnapshot datatypes.JSON
	// Status 生命周期：draft/published/disabled。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定 Agent 表名。
func (agentModel) TableName() string { return "agents" }

// agentSkillModel 对应 agent_skills 表：Agent 与 Skill 的绑定。
type agentSkillModel struct {
	// AgentID Agent 版本 ID。
	AgentID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// SkillID 绑定的 Skill。
	SkillID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Config 绑定级配置 JSON。
	Config datatypes.JSON
}

// TableName 指定 Agent Skill 绑定表名。
func (agentSkillModel) TableName() string { return "agent_skills" }

// agentMCPModel 对应 agent_mcps 表：Agent 与 MCP 的绑定。
type agentMCPModel struct {
	// AgentID Agent 版本 ID。
	AgentID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// MCPID 绑定的 MCP。
	MCPID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// Config 绑定级配置 JSON。
	Config datatypes.JSON
}

// TableName 指定 Agent MCP 绑定表名。
func (agentMCPModel) TableName() string { return "agent_mcps" }

// agentUserSkillModel 对应 agent_user_skills 表：Agent 与用户级 Skill 的绑定。
type agentUserSkillModel struct {
	// AgentID Agent 版本 ID。
	AgentID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserSkillID 绑定的用户级 Skill。
	UserSkillID uuid.UUID `gorm:"column:user_skill_id;type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid;not null"`
	// CreatedAt 绑定时间。
	CreatedAt time.Time `gorm:"not null"`
}

// TableName 指定 Agent 用户技能绑定表名。
func (agentUserSkillModel) TableName() string { return "agent_user_skills" }

// agentUserSkillOptOutModel 对应 agent_user_skill_optouts 表：助手版本显式移除的用户技能。
// 自动挂载与发布继承会跳过这里的技能，从而尊重“按版本移除”的用户意图。
type agentUserSkillOptOutModel struct {
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	// AgentID 执行移除操作的助手版本 ID。
	AgentID uuid.UUID `gorm:"column:agent_id;type:uuid;primaryKey"`
	// UserSkillID 被显式移除的用户技能 ID。
	UserSkillID uuid.UUID `gorm:"column:user_skill_id;type:uuid;primaryKey"`
	// CreatedAt 移除时间。
	CreatedAt time.Time `gorm:"not null"`
}

// TableName 指定助手用户技能移除标记表名。
func (agentUserSkillOptOutModel) TableName() string {
	return "agent_user_skill_optouts"
}

// agentSnapshotRevisionModel 对应 agent_snapshot_revisions 表：发布快照原地更新前的历史。
type agentSnapshotRevisionModel struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey"`
	UserID         uuid.UUID      `gorm:"type:uuid;not null"`
	AgentID        uuid.UUID      `gorm:"column:agent_id;type:uuid;not null"`
	AgentVersion   int            `gorm:"column:agent_version;not null"`
	ConfigSnapshot datatypes.JSON `gorm:"column:config_snapshot;not null"`
	Reason         string         `gorm:"not null;default:''"`
	CreatedAt      time.Time      `gorm:"not null"`
}

// TableName 指定助手快照历史表名。
func (agentSnapshotRevisionModel) TableName() string { return "agent_snapshot_revisions" }

// skillModel 是 skills 表的只读摘要，用于发布快照校验。
type skillModel struct {
	// ID Skill 主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name Skill 名称。
	Name string
	// Description Skill 用途说明。
	Description string
	// PromptTemplate Skill 提示词模板。
	PromptTemplate string
	// ParametersJSON Skill 参数定义 JSON Schema，发布快照需要冻结。
	ParametersJSON datatypes.JSON
	// Status Skill 状态，发布 Agent 只能绑定 published。
	Status string
}

// TableName 指定 Skill 表名。
func (skillModel) TableName() string { return "skills" }

// mcpModel 是 mcps 表的只读摘要，用于发布快照校验。
type mcpModel struct {
	// ID MCP 主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name MCP 名称。
	Name string
	// Endpoint 远端地址，执行层另行做 SSRF 校验。
	Endpoint string
	// AuthType 鉴权类型。
	AuthType string
	// RiskLevel 风险等级。
	RiskLevel string
	// Status MCP 状态，发布 Agent 只能绑定 enabled。
	Status string
}

// TableName 指定 MCP 表名。
func (mcpModel) TableName() string { return "mcps" }

// AgentRepository 是用户私有 Agent 的 GORM 实现，所有查询强制携带 user_id。
type AgentRepository struct {
	db      *gorm.DB
	catalog *CatalogRepository
}

// NewAgentRepository 构造 Agent 仓库，并共享模型目录读取能力。
func NewAgentRepository(db *gorm.DB) *AgentRepository {
	return &AgentRepository{db: db, catalog: NewCatalogRepository(db)}
}

// CreateAgent 在用户 scope 事务内创建 Agent 版本、绑定与审计事件。
func (r *AgentRepository) CreateAgent(ctx context.Context, agent biz.Agent, event biz.OutboxEvent) (biz.Agent, error) {
	return r.createAgent(ctx, agent, uuid.Nil, event)
}

// CreateAgentVersion 在同一事务内创建新版本并复制源版本的显式移除标记。
func (r *AgentRepository) CreateAgentVersion(ctx context.Context, agent biz.Agent, sourceAgentID uuid.UUID, event biz.OutboxEvent) (biz.Agent, error) {
	return r.createAgent(ctx, agent, sourceAgentID, event)
}

// createAgent 是创建版本的共享实现；inheritFrom 非空时复制该版本的显式移除标记。
func (r *AgentRepository) createAgent(ctx context.Context, agent biz.Agent, inheritFrom uuid.UUID, event biz.OutboxEvent) (biz.Agent, error) {
	record, skills, userSkills, mcps, err := fromBizAgent(agent)
	if err != nil {
		return biz.Agent{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: agent.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := createAgentBindings(tx, skills, userSkills, mcps); err != nil {
			return err
		}
		if inheritFrom != uuid.Nil {
			if err := copyUserSkillOptOuts(tx, agent.UserID, inheritFrom, agent.ID); err != nil {
				return err
			}
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Agent{}, catalogError("create agent", err)
	}
	return r.GetAgent(ctx, agent.UserID, agent.ID)
}

// UpdateAgent 仅更新草稿版本：按状态条件更新主记录后整体替换 Skill/MCP 绑定。
func (r *AgentRepository) UpdateAgent(ctx context.Context, agent biz.Agent, event biz.OutboxEvent) (biz.Agent, error) {
	record, skills, userSkills, mcps, err := fromBizAgent(agent)
	if err != nil {
		return biz.Agent{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: agent.UserID}, func(tx *gorm.DB) error {
		query := tx.Model(&agentModel{}).Where("id = ? AND user_id = ? AND status = ?", agent.ID, agent.UserID, biz.AgentDraft)
		// 乐观并发：把期望的 updated_at 放进 UPDATE 条件，避免校验与写入之间被自动挂载插入而丢绑定。
		if !agent.UpdatedAt.IsZero() {
			query = query.Where("updated_at = ?", agent.UpdatedAt)
		}
		result := query.Updates(map[string]any{
			"description": record.Description, "mode": record.Mode, "model_policy_id": record.ModelPolicyID,
			"model_policy_version": record.ModelPolicyVersion, "config_snapshot": record.ConfigSnapshot,
			"status": record.Status, "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: agent is missing, not a draft, or was modified concurrently", biz.ErrResourceConflict)
		}
		// 记录用户显式移除的用户技能，自动挂载与发布继承时会跳过这些技能。
		var previousUserSkillIDs []uuid.UUID
		if err := tx.Model(&agentUserSkillModel{}).Where("agent_id = ? AND user_id = ?", agent.ID, agent.UserID).
			Pluck("user_skill_id", &previousUserSkillIDs).Error; err != nil {
			return err
		}
		if err := tx.Delete(&agentSkillModel{}, "agent_id = ? AND user_id = ?", agent.ID, agent.UserID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&agentMCPModel{}, "agent_id = ? AND user_id = ?", agent.ID, agent.UserID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&agentUserSkillModel{}, "agent_id = ? AND user_id = ?", agent.ID, agent.UserID).Error; err != nil {
			return err
		}
		if err := createAgentBindings(tx, skills, userSkills, mcps); err != nil {
			return err
		}
		nextUserSkillIDs := make([]uuid.UUID, 0, len(userSkills))
		for _, binding := range userSkills {
			nextUserSkillIDs = append(nextUserSkillIDs, binding.UserSkillID)
		}
		if err := syncUserSkillOptOuts(tx, agent.UserID, agent.ID, previousUserSkillIDs, nextUserSkillIDs); err != nil {
			return err
		}
		// 显式取消继承的技能即使从未绑定也写入移除标记，发布时不再被同家族继承。
		if err := recordUserSkillOptOuts(tx, agent.UserID, agent.ID, agent.UserSkillOptOuts); err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Agent{}, catalogError("update agent", err)
	}
	return r.GetAgent(ctx, agent.UserID, agent.ID)
}

// DeleteAgent 仅删除当前用户的草稿版本。
func (r *AgentRepository) DeleteAgent(ctx context.Context, userID, agentID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Delete(&agentModel{}, "id = ? AND user_id = ? AND status = ?", agentID, userID, biz.AgentDraft)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete agent", err)
}

// GetAgentAdmin 以 system scope 读取任意用户的助手摘要，仅供管理员审计与删除。
func (r *AgentRepository) GetAgentAdmin(ctx context.Context, agentID uuid.UUID) (biz.Agent, error) {
	var record agentModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", agentID).Error
	})
	if err != nil {
		return biz.Agent{}, catalogError("get agent", err)
	}
	return toBizAgent(record, nil, nil, nil)
}

// DeleteAgentAdmin 以 system scope 删除任意用户的助手版本；会话与运行记录由外键 SET NULL 保留。
func (r *AgentRepository) DeleteAgentAdmin(ctx context.Context, actorID, agentID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&agentModel{}, "id = ?", agentID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete agent", err)
}

// UpdateAgentStatus 按前置状态条件更新生命周期；条件不匹配说明状态已被并发修改。
func (r *AgentRepository) UpdateAgentStatus(ctx context.Context, userID, agentID uuid.UUID, from, to biz.AgentStatus, clearSnapshot bool, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		updates := map[string]any{"status": string(to), "updated_at": time.Now().UTC()}
		if clearSnapshot {
			updates["config_snapshot"] = datatypes.JSON([]byte("{}"))
		}
		result := tx.Model(&agentModel{}).Where("id = ? AND user_id = ? AND status = ?", agentID, userID, string(from)).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: agent is missing or status transition is not allowed", biz.ErrResourceConflict)
		}
		// 取消发布或归档后机器人不再指向该助手，同一事务解除绑定避免机器人被长期占用。
		if err := tx.Delete(&agentFeishuBotModel{}, "agent_id = ? AND user_id = ?", agentID, userID).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	return catalogError("update agent status", err)
}

// GetAgent 读取 Agent 主记录及绑定；跨用户访问由 RLS 与本查询条件双重拦截。
func (r *AgentRepository) GetAgent(ctx context.Context, userID, agentID uuid.UUID) (biz.Agent, error) {
	var record agentModel
	var skillIDs []uuid.UUID
	var userSkillIDs []uuid.UUID
	var mcpIDs []uuid.UUID
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", agentID, userID).Error; err != nil {
			return err
		}
		if err := tx.Model(&agentSkillModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).Order("skill_id").Pluck("skill_id", &skillIDs).Error; err != nil {
			return err
		}
		if err := tx.Model(&agentUserSkillModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).Order("user_skill_id").Pluck("user_skill_id", &userSkillIDs).Error; err != nil {
			return err
		}
		return tx.Model(&agentMCPModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).Order("mcp_id").Pluck("mcp_id", &mcpIDs).Error
	})
	if err != nil {
		return biz.Agent{}, catalogError("get agent", err)
	}
	return toBizAgent(record, skillIDs, userSkillIDs, mcpIDs)
}

// AttachUserSkillToAgent 为指定 Agent 追加用户技能绑定，并在已发布快照中同步技能内容。
// 返回 false 表示该版本已显式移除该技能，按语义跳过挂载；不创建新版本。
func (r *AgentRepository) AttachUserSkillToAgent(ctx context.Context, userID, agentID uuid.UUID, skill biz.UserSkill, event biz.OutboxEvent) (bool, error) {
	attached := false
	changed := false
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var record agentModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&record, "id = ? AND user_id = ?", agentID, userID).Error; err != nil {
			return err
		}
		// 用户在当前版本显式移除过该技能时不再自动挂回，尊重“按版本移除”语义。
		var optOutCount int64
		if err := tx.Model(&agentUserSkillOptOutModel{}).
			Where("user_id = ? AND agent_id = ? AND user_skill_id = ?", userID, agentID, skill.ID).
			Count(&optOutCount).Error; err != nil {
			return err
		}
		if optOutCount > 0 {
			return nil
		}
		attached = true
		binding := agentUserSkillModel{
			AgentID: agentID, UserSkillID: skill.ID, UserID: userID, CreatedAt: time.Now().UTC(),
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			changed = true
		}
		if record.Status != string(biz.AgentPublished) {
			if !changed {
				return nil
			}
			// 草稿绑定变化也要推进 updated_at，让并发的草稿编辑能通过乐观并发发现冲突。
			if err := tx.Model(&agentModel{}).Where("id = ? AND user_id = ?", agentID, userID).
				Update("updated_at", time.Now().UTC()).Error; err != nil {
				return err
			}
			return appendOutbox(tx, event)
		}
		updated, snapshotChanged, err := refreshAgentUserSkillSnapshot(record.ConfigSnapshot, skill)
		if err != nil {
			return err
		}
		if snapshotChanged {
			if err := insertAgentSnapshotRevision(tx, userID, agentID, record.Version, record.ConfigSnapshot, "user_skill_attached"); err != nil {
				return err
			}
			if err := tx.Model(&agentModel{}).Where("id = ? AND user_id = ?", agentID, userID).
				Updates(map[string]any{
					"config_snapshot": datatypes.JSON(updated), "updated_at": time.Now().UTC(),
				}).Error; err != nil {
				return err
			}
			changed = true
		}
		if !changed {
			return nil
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return false, catalogError("attach user skill to agent", err)
	}
	return attached, nil
}

// ListPublishedAgentIDsByName 返回同名助手的全部已发布版本 ID，按版本升序。
func (r *AgentRepository) ListPublishedAgentIDsByName(ctx context.Context, userID uuid.UUID, name string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&agentModel{}).
			Where("user_id = ? AND name = ? AND status = ?", userID, name, string(biz.AgentPublished)).
			Order("version").Pluck("id", &ids).Error
	})
	if err != nil {
		return nil, catalogError("list published agent ids by name", err)
	}
	return ids, nil
}

// ListUserSkillOptOuts 返回助手版本显式移除的用户技能 ID，按 ID 升序。
func (r *AgentRepository) ListUserSkillOptOuts(ctx context.Context, userID, agentID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&agentUserSkillOptOutModel{}).
			Where("user_id = ? AND agent_id = ?", userID, agentID).
			Order("user_skill_id").Pluck("user_skill_id", &ids).Error
	})
	if err != nil {
		return nil, catalogError("list agent user skill opt-outs", err)
	}
	return ids, nil
}

// RefreshUserSkillOnBoundAgents 刷新所有绑定该技能的已发布 Agent 快照；单个助手失败不阻断其他助手。
func (r *AgentRepository) RefreshUserSkillOnBoundAgents(ctx context.Context, userID uuid.UUID, skill biz.UserSkill, event biz.OutboxEvent) error {
	var agentIDs []uuid.UUID
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&agentUserSkillModel{}).
			Where("user_id = ? AND user_skill_id = ?", userID, skill.ID).
			Order("agent_id").Pluck("agent_id", &agentIDs).Error
	})
	if err != nil {
		return catalogError("list agents bound to user skill", err)
	}
	var firstErr error
	for _, agentID := range agentIDs {
		if _, err := r.AttachUserSkillToAgent(ctx, userID, agentID, skill, event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// maxAgentSnapshotRevisions 是单个助手保留的发布快照历史上限。
const maxAgentSnapshotRevisions = 20

// ListAgentSnapshotRevisions 按时间倒序读取助手发布快照历史。
func (r *AgentRepository) ListAgentSnapshotRevisions(ctx context.Context, userID, agentID uuid.UUID, limit int) ([]biz.AgentSnapshotRevision, error) {
	if limit <= 0 {
		limit = 20
	}
	var records []agentSnapshotRevisionModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND agent_id = ?", userID, agentID).
			Order("created_at DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list agent snapshot revisions", err)
	}
	revisions := make([]biz.AgentSnapshotRevision, 0, len(records))
	for _, record := range records {
		revision, err := toBizAgentSnapshotRevision(record)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, nil
}

// GetAgentSnapshotRevision 读取单条助手发布快照历史。
func (r *AgentRepository) GetAgentSnapshotRevision(ctx context.Context, userID, agentID, revisionID uuid.UUID) (biz.AgentSnapshotRevision, error) {
	var record agentSnapshotRevisionModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ? AND agent_id = ?", revisionID, userID, agentID).First(&record).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.AgentSnapshotRevision{}, fmt.Errorf("%w: agent snapshot revision not found", biz.ErrResourceNotFound)
		}
		return biz.AgentSnapshotRevision{}, catalogError("get agent snapshot revision", err)
	}
	return toBizAgentSnapshotRevision(record)
}

// RestoreAgentSnapshot 用历史快照覆盖当前发布快照，并把当前快照保留为新的历史。
func (r *AgentRepository) RestoreAgentSnapshot(ctx context.Context, userID, agentID uuid.UUID, snapshot map[string]any, event biz.OutboxEvent) error {
	encoded, err := json.Marshal(nonNilMap(snapshot))
	if err != nil {
		return err
	}
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var record agentModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&record, "id = ? AND user_id = ?", agentID, userID).Error; err != nil {
			return err
		}
		if record.Status != string(biz.AgentPublished) {
			return fmt.Errorf("%w: only published agents have restorable snapshots", biz.ErrResourceConflict)
		}
		if err := insertAgentSnapshotRevision(tx, userID, agentID, record.Version, record.ConfigSnapshot, "snapshot_restored"); err != nil {
			return err
		}
		if err := tx.Model(&agentModel{}).Where("id = ? AND user_id = ?", agentID, userID).
			Updates(map[string]any{
				"config_snapshot": datatypes.JSON(encoded), "updated_at": time.Now().UTC(),
			}).Error; err != nil {
			return err
		}
		// 快照恢复同时还原技能绑定与移除标记，避免运行时注入与绑定表不一致。
		if err := restoreUserSkillBindings(tx, userID, agentID, snapshot); err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("restore agent snapshot", err)
	}
	return nil
}

// insertAgentSnapshotRevision 记录更新前的发布快照，并裁剪超出上限的历史。
func insertAgentSnapshotRevision(tx *gorm.DB, userID, agentID uuid.UUID, version int, snapshot datatypes.JSON, reason string) error {
	if len(snapshot) == 0 || string(snapshot) == "{}" {
		return nil
	}
	record := agentSnapshotRevisionModel{
		ID: uuid.New(), UserID: userID, AgentID: agentID, AgentVersion: version,
		ConfigSnapshot: snapshot, Reason: reason, CreatedAt: time.Now().UTC(),
	}
	if err := tx.Create(&record).Error; err != nil {
		return err
	}
	return tx.Exec(`
DELETE FROM agent_snapshot_revisions
WHERE id IN (
  SELECT id FROM agent_snapshot_revisions
  WHERE user_id = ? AND agent_id = ?
  ORDER BY created_at DESC
  OFFSET ?
)`, userID, agentID, maxAgentSnapshotRevisions).Error
}

// toBizAgentSnapshotRevision 把快照历史行转换为领域结构。
func toBizAgentSnapshotRevision(record agentSnapshotRevisionModel) (biz.AgentSnapshotRevision, error) {
	snapshot := map[string]any{}
	if len(record.ConfigSnapshot) > 0 {
		if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
			return biz.AgentSnapshotRevision{}, err
		}
	}
	return biz.AgentSnapshotRevision{
		ID: record.ID, UserID: record.UserID, AgentID: record.AgentID, AgentVersion: record.AgentVersion,
		ConfigSnapshot: snapshot, Reason: record.Reason, CreatedAt: record.CreatedAt,
	}, nil
}

// refreshAgentUserSkillSnapshot 把用户技能内容写入 Agent 发布快照；内容未变化时返回 false。
func refreshAgentUserSkillSnapshot(raw datatypes.JSON, skill biz.UserSkill) ([]byte, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	snapshot := map[string]any{}
	// 使用 UseNumber 保留快照中的原始数值，避免价格等字段被 float64 重新序列化后失真。
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, false, err
	}
	if len(snapshot) == 0 {
		return nil, false, nil
	}
	entries := make([]map[string]any, 0)
	switch existing := snapshot["user_skills"].(type) {
	case []any:
		for _, item := range existing {
			if entry, ok := item.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	}
	changed := false
	found := false
	for index, entry := range entries {
		entryID := fmt.Sprint(entry["id"])
		if entryID == "<nil>" || entryID == "" {
			// 兼容早期快照使用 Go 字段名 ID 的形态。
			entryID = fmt.Sprint(entry["ID"])
		}
		if entryID != skill.ID.String() {
			continue
		}
		found = true
		if fmt.Sprint(entry["prompt_template"]) != skill.PromptTemplate ||
			fmt.Sprint(entry["description"]) != skill.Description ||
			fmt.Sprint(entry["status"]) != string(skill.Status) ||
			!jsonValuesEqual(entry["parameters"], skill.Parameters) ||
			userSkillSnapshotBundleDigest(entry) != skill.Bundle.Digest {
			entries[index] = userSkillSnapshotEntry(entry, skill)
			changed = true
		}
		break
	}
	if !found {
		entries = append(entries, userSkillSnapshotEntry(nil, skill))
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	snapshot["user_skills"] = entries
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

// userSkillSnapshotEntry 生成或更新快照中的用户技能条目，保留历史条目中的额外字段。
func userSkillSnapshotEntry(existing map[string]any, skill biz.UserSkill) map[string]any {
	entry := existing
	if entry == nil {
		entry = map[string]any{}
	}
	entry["id"] = skill.ID.String()
	entry["name"] = skill.Name
	entry["version"] = skill.Version
	entry["description"] = skill.Description
	entry["prompt_template"] = skill.PromptTemplate
	entry["parameters"] = skill.Parameters
	entry["status"] = string(skill.Status)
	entry["origin"] = string(skill.Origin)
	entry["source_session_ids"] = skill.SourceSessionIDs
	entry["confidence"] = skill.Confidence
	entry["bundle"] = skill.Bundle
	entry["updated_at"] = skill.UpdatedAt
	return entry
}

// jsonValuesEqual 以 JSON 表示比较两个值，兼容 json.Number 与 float64 的差异。
func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return bytes.Equal(leftJSON, rightJSON)
}

// userSkillSnapshotBundleDigest 读取快照条目中的技能包摘要。
func userSkillSnapshotBundleDigest(entry map[string]any) string {
	bundle, ok := entry["bundle"].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprint(bundle["digest"])
}

// ListAgents 按创建时间与 ID 降序游标分页，多取一条用于判断是否还有下一页；默认排除已归档版本。
func (r *AgentRepository) ListAgents(ctx context.Context, userID uuid.UUID, includeArchived bool, limit int, cursor string) (biz.Page[biz.Agent], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.Agent]{}, fmt.Errorf("%w: invalid agent cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []agentModel
	err = WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ?", userID)
		if !includeArchived {
			query = query.Where("status <> ?", string(biz.AgentArchived))
		}
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error
	})
	if err != nil {
		return biz.Page[biz.Agent]{}, catalogError("list agents", err)
	}
	page := biz.Page[biz.Agent]{Items: make([]biz.Agent, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.Agent]{}, err
		}
	}
	for _, record := range records {
		agent, err := r.GetAgent(ctx, userID, record.ID)
		if err != nil {
			return biz.Page[biz.Agent]{}, err
		}
		page.Items = append(page.Items, agent)
	}
	return page, nil
}

// ListAllAgents 跨用户游标分页列出助手并附带所有者用户名，仅供管理员只读审计。
func (r *AgentRepository) ListAllAgents(ctx context.Context, filter biz.AdminAgentFilter, limit int, cursor string) (biz.Page[biz.AdminAgent], error) {
	decoded, err := DecodeCursor(cursor)
	if err != nil {
		return biz.Page[biz.AdminAgent]{}, fmt.Errorf("%w: invalid agent cursor", biz.ErrInvalidConfig)
	}
	limit = NormalizeLimit(limit)
	var records []agentModel
	usernames := map[uuid.UUID]string{}
	err = WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		query := tx.Model(&agentModel{})
		if filter.UserID != nil {
			query = query.Where("user_id = ?", *filter.UserID)
		}
		if filter.Status != "" {
			query = query.Where("status = ?", string(filter.Status))
		}
		if decoded.ID != uuid.Nil {
			query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", decoded.CreatedAt, decoded.CreatedAt, decoded.ID)
		}
		if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		userIDs := make([]uuid.UUID, 0, len(records))
		seen := map[uuid.UUID]bool{}
		for _, record := range records {
			if !seen[record.UserID] {
				seen[record.UserID] = true
				userIDs = append(userIDs, record.UserID)
			}
		}
		var users []userModel
		if err := tx.Model(&userModel{}).Select("id", "username").Where("id IN ?", userIDs).Find(&users).Error; err != nil {
			return err
		}
		for _, user := range users {
			usernames[user.ID] = user.Username
		}
		return nil
	})
	if err != nil {
		return biz.Page[biz.AdminAgent]{}, catalogError("list all agents", err)
	}
	page := biz.Page[biz.AdminAgent]{Items: make([]biz.AdminAgent, 0, min(limit, len(records)))}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1]
		page.NextCursor, err = EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return biz.Page[biz.AdminAgent]{}, err
		}
	}
	for _, record := range records {
		agent, err := toBizAgent(record, nil, nil, nil)
		if err != nil {
			return biz.Page[biz.AdminAgent]{}, catalogError("decode agent", err)
		}
		page.Items = append(page.Items, biz.AdminAgent{Agent: agent, Username: usernames[record.UserID]})
	}
	return page, nil
}

// NextAgentVersion 返回同用户同名 Agent 的最大版本号加一。
func (r *AgentRepository) NextAgentVersion(ctx context.Context, userID uuid.UUID, name string) (int, error) {
	var maximum int
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&agentModel{}).Select("COALESCE(MAX(version), 0)").Where("user_id = ? AND name = ?", userID, name).Scan(&maximum).Error
	})
	return maximum + 1, catalogError("get next agent version", err)
}

// GetPolicy 读取 Agent 引用的平台策略（复用模型目录仓库）。
func (r *AgentRepository) GetPolicy(ctx context.Context, policyID uuid.UUID) (biz.ModelPolicy, error) {
	return r.catalog.GetPolicy(ctx, policyID)
}

// ListModelsByIDs 批量读取策略候选模型，用于发布快照。
func (r *AgentRepository) ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]biz.Model, error) {
	return r.catalog.ListModelsByIDs(ctx, modelIDs)
}

// ListSkillsByIDs 批量读取 Skill 摘要（平台级只读数据，使用 system scope）。
func (r *AgentRepository) ListSkillsByIDs(ctx context.Context, skillIDs []uuid.UUID) ([]biz.Skill, error) {
	if len(skillIDs) == 0 {
		return []biz.Skill{}, nil
	}
	var records []skillModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Where("id IN ?", skillIDs).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list agent skills", err)
	}
	result := make([]biz.Skill, 0, len(records))
	for _, record := range records {
		// 参数定义随发布快照冻结，运行时不回查平台配置。
		var parameters map[string]any
		if len(record.ParametersJSON) > 0 {
			_ = json.Unmarshal(record.ParametersJSON, &parameters)
		}
		result = append(result, biz.Skill{
			ID: record.ID, Name: record.Name, Description: record.Description,
			PromptTemplate: record.PromptTemplate, Parameters: parameters, Status: record.Status,
		})
	}
	return result, nil
}

// ListUserSkillsByIDs 批量读取当前用户的技能摘要（用户级隔离数据，使用用户作用域）。
func (r *AgentRepository) ListUserSkillsByIDs(ctx context.Context, userID uuid.UUID, skillIDs []uuid.UUID) ([]biz.UserSkill, error) {
	if len(skillIDs) == 0 {
		return []biz.UserSkill{}, nil
	}
	var records []userSkillModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND id IN ?", userID, skillIDs).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list agent user skills", err)
	}
	result := make([]biz.UserSkill, 0, len(records))
	for _, record := range records {
		skill, err := toUserSkill(record)
		if err != nil {
			return nil, err
		}
		result = append(result, skill)
	}
	return result, nil
}

// ListMCPsByIDs 批量读取 MCP 摘要（平台级只读数据，使用 system scope）。
func (r *AgentRepository) ListMCPsByIDs(ctx context.Context, mcpIDs []uuid.UUID) ([]biz.MCP, error) {
	if len(mcpIDs) == 0 {
		return []biz.MCP{}, nil
	}
	var records []mcpModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Where("id IN ?", mcpIDs).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list agent MCPs", err)
	}
	result := make([]biz.MCP, 0, len(records))
	for _, record := range records {
		result = append(result, biz.MCP{ID: record.ID, Name: record.Name, Endpoint: record.Endpoint, AuthType: record.AuthType, RiskLevel: record.RiskLevel, Status: record.Status})
	}
	return result, nil
}

// CountAgents 统计当前用户 Agent 版本总数（含草稿与已发布）。
func (r *AgentRepository) CountAgents(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&agentModel{}).Where("user_id = ?", userID).Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count agents", err)
	}
	return count, nil
}

// fromBizAgent 将 Agent 聚合拆分为主记录与 Skill/MCP 绑定行。
func fromBizAgent(agent biz.Agent) (agentModel, []agentSkillModel, []agentUserSkillModel, []agentMCPModel, error) {
	snapshot, err := json.Marshal(nonNilMap(agent.ConfigSnapshot))
	if err != nil {
		return agentModel{}, nil, nil, nil, err
	}
	skills := make([]agentSkillModel, 0, len(agent.SkillIDs))
	for _, skillID := range agent.SkillIDs {
		skills = append(skills, agentSkillModel{AgentID: agent.ID, SkillID: skillID, UserID: agent.UserID, Config: datatypes.JSON([]byte("{}"))})
	}
	userSkills := make([]agentUserSkillModel, 0, len(agent.UserSkillIDs))
	now := time.Now().UTC()
	for _, userSkillID := range agent.UserSkillIDs {
		userSkills = append(userSkills, agentUserSkillModel{AgentID: agent.ID, UserSkillID: userSkillID, UserID: agent.UserID, CreatedAt: now})
	}
	mcps := make([]agentMCPModel, 0, len(agent.MCPIDs))
	for _, mcpID := range agent.MCPIDs {
		mcps = append(mcps, agentMCPModel{AgentID: agent.ID, MCPID: mcpID, UserID: agent.UserID, Config: datatypes.JSON([]byte("{}"))})
	}
	return agentModel{
		ID: agent.ID, UserID: agent.UserID, Name: agent.Name, Description: agent.Description, Mode: string(agent.Mode),
		ModelPolicyID: agent.ModelPolicyID, ModelPolicyVersion: agent.ModelPolicyVersion, Version: agent.Version,
		ConfigSnapshot: datatypes.JSON(snapshot), Status: string(agent.Status),
	}, skills, userSkills, mcps, nil
}

// createAgentBindings 在同一事务内批量写入 Skill、用户技能与 MCP 绑定。
func createAgentBindings(tx *gorm.DB, skills []agentSkillModel, userSkills []agentUserSkillModel, mcps []agentMCPModel) error {
	if len(skills) > 0 {
		if err := tx.Create(&skills).Error; err != nil {
			return err
		}
	}
	if len(userSkills) > 0 {
		if err := tx.Create(&userSkills).Error; err != nil {
			return err
		}
	}
	if len(mcps) > 0 {
		if err := tx.Create(&mcps).Error; err != nil {
			return err
		}
	}
	return nil
}

// syncUserSkillOptOuts 根据本次更新的绑定差异维护显式移除标记：
// 被移除的技能写入标记，被重新加回或新绑定的技能清除标记。
func syncUserSkillOptOuts(tx *gorm.DB, userID, agentID uuid.UUID, previous, next []uuid.UUID) error {
	previousSet := make(map[uuid.UUID]struct{}, len(previous))
	for _, id := range previous {
		previousSet[id] = struct{}{}
	}
	nextSet := make(map[uuid.UUID]struct{}, len(next))
	for _, id := range next {
		nextSet[id] = struct{}{}
	}
	now := time.Now().UTC()
	optOuts := make([]agentUserSkillOptOutModel, 0)
	restored := make([]uuid.UUID, 0)
	for _, id := range previous {
		if _, exists := nextSet[id]; !exists {
			optOuts = append(optOuts, agentUserSkillOptOutModel{UserID: userID, AgentID: agentID, UserSkillID: id, CreatedAt: now})
		}
	}
	for _, id := range next {
		if _, exists := previousSet[id]; !exists {
			restored = append(restored, id)
		}
	}
	if len(optOuts) > 0 {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&optOuts).Error; err != nil {
			return err
		}
	}
	if len(restored) > 0 {
		if err := tx.Where("user_id = ? AND agent_id = ? AND user_skill_id IN ?", userID, agentID, restored).
			Delete(&agentUserSkillOptOutModel{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// copyUserSkillOptOuts 在同一事务内把源版本的显式移除标记复制到新版本。
func copyUserSkillOptOuts(tx *gorm.DB, userID, fromAgentID, toAgentID uuid.UUID) error {
	return tx.Exec(`INSERT INTO agent_user_skill_optouts (user_id, agent_id, user_skill_id)
SELECT user_id, ?, user_skill_id FROM agent_user_skill_optouts WHERE user_id = ? AND agent_id = ?
ON CONFLICT DO NOTHING`, toAgentID, userID, fromAgentID).Error
}

// recordUserSkillOptOuts 记录显式取消继承的技能；即使从未绑定也写入移除标记。
func recordUserSkillOptOuts(tx *gorm.DB, userID, agentID uuid.UUID, skillIDs []uuid.UUID) error {
	if len(skillIDs) == 0 {
		return nil
	}
	now := time.Now().UTC()
	optOuts := make([]agentUserSkillOptOutModel, 0, len(skillIDs))
	for _, id := range skillIDs {
		optOuts = append(optOuts, agentUserSkillOptOutModel{UserID: userID, AgentID: agentID, UserSkillID: id, CreatedAt: now})
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&optOuts).Error
}

// restoreUserSkillBindings 用快照中的用户技能列表还原绑定表，并同步移除标记。
// 已删除或不属于当前用户的技能无法恢复绑定，但仍保留在冻结快照中。
func restoreUserSkillBindings(tx *gorm.DB, userID, agentID uuid.UUID, snapshot map[string]any) error {
	desired := snapshotUserSkillIDs(snapshot)
	if len(desired) > 0 {
		var existing []uuid.UUID
		if err := tx.Model(&userSkillModel{}).Where("user_id = ? AND id IN ?", userID, desired).
			Pluck("id", &existing).Error; err != nil {
			return err
		}
		desired = existing
	}
	var previous []uuid.UUID
	if err := tx.Model(&agentUserSkillModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).
		Pluck("user_skill_id", &previous).Error; err != nil {
		return err
	}
	if err := tx.Delete(&agentUserSkillModel{}, "agent_id = ? AND user_id = ?", agentID, userID).Error; err != nil {
		return err
	}
	if len(desired) > 0 {
		now := time.Now().UTC()
		bindings := make([]agentUserSkillModel, 0, len(desired))
		for _, id := range desired {
			bindings = append(bindings, agentUserSkillModel{AgentID: agentID, UserSkillID: id, UserID: userID, CreatedAt: now})
		}
		if err := tx.Create(&bindings).Error; err != nil {
			return err
		}
	}
	return syncUserSkillOptOuts(tx, userID, agentID, previous, desired)
}

// snapshotUserSkillIDs 从发布快照中提取用户技能 ID，忽略非法或重复项。
func snapshotUserSkillIDs(snapshot map[string]any) []uuid.UUID {
	raw, exists := snapshot["user_skills"]
	if !exists {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var entries []struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(encoded, &entries); err != nil {
		return nil
	}
	seen := make(map[uuid.UUID]struct{}, len(entries))
	ids := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		if entry.ID == uuid.Nil {
			continue
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			continue
		}
		seen[entry.ID] = struct{}{}
		ids = append(ids, entry.ID)
	}
	return ids
}

// toBizAgent 将主记录与绑定 ID 组装为领域聚合，并解码配置快照 JSON。
func toBizAgent(record agentModel, skillIDs, userSkillIDs, mcpIDs []uuid.UUID) (biz.Agent, error) {
	var snapshot map[string]any
	if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
		return biz.Agent{}, err
	}
	return biz.Agent{
		ID: record.ID, UserID: record.UserID, Name: record.Name, Description: record.Description, Mode: biz.AgentMode(record.Mode),
		ModelPolicyID: record.ModelPolicyID, ModelPolicyVersion: record.ModelPolicyVersion, Version: record.Version,
		ConfigSnapshot: snapshot, SkillIDs: skillIDs, UserSkillIDs: userSkillIDs, MCPIDs: mcpIDs, Status: biz.AgentStatus(record.Status),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

var _ biz.AgentRepository = (*AgentRepository)(nil)
