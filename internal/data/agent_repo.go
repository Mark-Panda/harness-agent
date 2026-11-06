package data

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
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
	// Mode 编排模式：react/plan/super。
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
	record, skills, mcps, err := fromBizAgent(agent)
	if err != nil {
		return biz.Agent{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: agent.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := createAgentBindings(tx, skills, mcps); err != nil {
			return err
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
	record, skills, mcps, err := fromBizAgent(agent)
	if err != nil {
		return biz.Agent{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: agent.UserID}, func(tx *gorm.DB) error {
		result := tx.Model(&agentModel{}).Where("id = ? AND user_id = ? AND status = ?", agent.ID, agent.UserID, biz.AgentDraft).Updates(map[string]any{
			"description": record.Description, "mode": record.Mode, "model_policy_id": record.ModelPolicyID,
			"model_policy_version": record.ModelPolicyVersion, "config_snapshot": record.ConfigSnapshot,
			"status": record.Status, "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("%w: agent is missing or not a draft", biz.ErrResourceConflict)
		}
		if err := tx.Delete(&agentSkillModel{}, "agent_id = ? AND user_id = ?", agent.ID, agent.UserID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&agentMCPModel{}, "agent_id = ? AND user_id = ?", agent.ID, agent.UserID).Error; err != nil {
			return err
		}
		if err := createAgentBindings(tx, skills, mcps); err != nil {
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
	return toBizAgent(record, nil, nil)
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
	var mcpIDs []uuid.UUID
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", agentID, userID).Error; err != nil {
			return err
		}
		if err := tx.Model(&agentSkillModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).Order("skill_id").Pluck("skill_id", &skillIDs).Error; err != nil {
			return err
		}
		return tx.Model(&agentMCPModel{}).Where("agent_id = ? AND user_id = ?", agentID, userID).Order("mcp_id").Pluck("mcp_id", &mcpIDs).Error
	})
	if err != nil {
		return biz.Agent{}, catalogError("get agent", err)
	}
	return toBizAgent(record, skillIDs, mcpIDs)
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
		agent, err := toBizAgent(record, nil, nil)
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
func fromBizAgent(agent biz.Agent) (agentModel, []agentSkillModel, []agentMCPModel, error) {
	snapshot, err := json.Marshal(nonNilMap(agent.ConfigSnapshot))
	if err != nil {
		return agentModel{}, nil, nil, err
	}
	skills := make([]agentSkillModel, 0, len(agent.SkillIDs))
	for _, skillID := range agent.SkillIDs {
		skills = append(skills, agentSkillModel{AgentID: agent.ID, SkillID: skillID, UserID: agent.UserID, Config: datatypes.JSON([]byte("{}"))})
	}
	mcps := make([]agentMCPModel, 0, len(agent.MCPIDs))
	for _, mcpID := range agent.MCPIDs {
		mcps = append(mcps, agentMCPModel{AgentID: agent.ID, MCPID: mcpID, UserID: agent.UserID, Config: datatypes.JSON([]byte("{}"))})
	}
	return agentModel{
		ID: agent.ID, UserID: agent.UserID, Name: agent.Name, Description: agent.Description, Mode: string(agent.Mode),
		ModelPolicyID: agent.ModelPolicyID, ModelPolicyVersion: agent.ModelPolicyVersion, Version: agent.Version,
		ConfigSnapshot: datatypes.JSON(snapshot), Status: string(agent.Status),
	}, skills, mcps, nil
}

// createAgentBindings 在同一事务内批量写入 Skill 与 MCP 绑定。
func createAgentBindings(tx *gorm.DB, skills []agentSkillModel, mcps []agentMCPModel) error {
	if len(skills) > 0 {
		if err := tx.Create(&skills).Error; err != nil {
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

// toBizAgent 将主记录与绑定 ID 组装为领域聚合，并解码配置快照 JSON。
func toBizAgent(record agentModel, skillIDs, mcpIDs []uuid.UUID) (biz.Agent, error) {
	var snapshot map[string]any
	if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
		return biz.Agent{}, err
	}
	return biz.Agent{
		ID: record.ID, UserID: record.UserID, Name: record.Name, Description: record.Description, Mode: biz.AgentMode(record.Mode),
		ModelPolicyID: record.ModelPolicyID, ModelPolicyVersion: record.ModelPolicyVersion, Version: record.Version,
		ConfigSnapshot: snapshot, SkillIDs: skillIDs, MCPIDs: mcpIDs, Status: biz.AgentStatus(record.Status),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

var _ biz.AgentRepository = (*AgentRepository)(nil)
