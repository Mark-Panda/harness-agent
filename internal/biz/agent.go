package biz

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AgentMode Agent 编排模式。
type AgentMode string

// AgentStatus Agent 版本生命周期状态。
type AgentStatus string

const (
	// AgentModeReact ReAct 模式，多轮工具调用。
	AgentModeReact AgentMode = "react"
	// AgentDraft 草稿版本，可编辑。
	AgentDraft AgentStatus = "draft"
	// AgentPublished 已发布版本，内容不可变，可被会话引用。
	AgentPublished AgentStatus = "published"
	// AgentDisabled 已停用版本。
	AgentDisabled AgentStatus = "disabled"
	// AgentArchived 已归档版本，默认不出现在列表中，但保留历史 Run 引用。
	AgentArchived AgentStatus = "archived"
)

// Agent 是用户私有 Agent 聚合，草稿与发布版本分离，Run 只引用不可变版本。
type Agent struct {
	// ID Agent 版本唯一标识。
	ID uuid.UUID
	// UserID 所属用户，所有查询强制按此隔离。
	UserID uuid.UUID
	// Name Agent 名称，同一用户下与版本号共同唯一。
	Name string
	// Description Agent 描述。
	Description string
	// Mode 编排模式。
	Mode AgentMode
	// ModelPolicyID 引用的模型策略 ID，草稿可为空。
	ModelPolicyID *uuid.UUID
	// ModelPolicyVersion 引用的策略版本，与 ID 同时提供。
	ModelPolicyVersion *int
	// Version Agent 版本号，从 1 递增。
	Version int
	// ConfigSnapshot 发布时固化的不可变配置快照（含策略、模型、Skill、MCP）。
	ConfigSnapshot map[string]any
	// SkillIDs 绑定的 Skill 列表。
	SkillIDs []uuid.UUID
	// UserSkillIDs 绑定的用户级技能列表（仅当前用户的 published 技能）。
	UserSkillIDs []uuid.UUID
	// UserSkillOptOuts 草稿更新时显式移除的用户技能；仅作为写入口令，不参与读取与响应。
	UserSkillOptOuts []uuid.UUID
	// MCPIDs 绑定的 MCP 列表。
	MCPIDs []uuid.UUID
	// Status 版本生命周期状态。
	Status AgentStatus
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// Skill 是平台级 Skill 的只读摘要，用于发布时校验绑定有效性。
// 发布时会把描述与提示词模板一并冻结进 Agent 快照，运行时按快照注入系统提示。
type Skill struct {
	// ID Skill 唯一标识。
	ID uuid.UUID `json:"id"`
	// Name Skill 名称。
	Name string `json:"name"`
	// Description Skill 用途说明，用于运行时提示模型何时使用该 Skill。
	Description string `json:"description,omitempty"`
	// PromptTemplate Skill 提示词模板，发布后固化，运行中不随平台配置变化。
	PromptTemplate string `json:"prompt_template,omitempty"`
	// Parameters Skill 参数定义 JSON Schema，发布时一并冻结并注入运行时提示。
	Parameters map[string]any `json:"parameters,omitempty"`
	// Status Skill 状态，发布 Agent 只能绑定 published。
	Status string `json:"status"`
}

// MCP 是平台级 MCP 的只读摘要与配置视图，用于发布校验与管理页展示。
type MCP struct {
	// ID MCP 唯一标识。
	ID uuid.UUID `json:"id"`
	// Name MCP 名称。
	Name string `json:"name"`
	// Endpoint 远端地址，执行层按允许列表与 SSRF 规则校验。
	Endpoint string `json:"endpoint"`
	// AuthType 鉴权类型：none/bearer/oauth/header。
	AuthType string `json:"auth_type"`
	// KeyVersion 凭证密文对应的密钥版本。
	KeyVersion string `json:"key_version"`
	// RiskLevel 风险等级，高风险工具需要审批。
	RiskLevel string `json:"risk_level"`
	// Status MCP 状态，发布 Agent 只能绑定 enabled。
	Status string `json:"status"`
	// ToolTimeouts 按原始工具名或前缀覆盖单次超时，单位秒；未配置时使用默认超时。
	ToolTimeouts map[string]int `json:"tool_timeouts"`
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 更新时间（UTC）。
	UpdatedAt time.Time `json:"updated_at"`
}

// AdminAgentFilter 是管理员全局助手列表的筛选条件。
type AdminAgentFilter struct {
	// UserID 只查看指定用户的助手；为空表示全部用户。
	UserID *uuid.UUID
	// Status 按生命周期状态过滤；为空表示全部状态。
	Status AgentStatus
}

// AdminAgent 是跨用户助手条目，附带所有者用户名，仅供管理员只读审计。
type AdminAgent struct {
	// Agent 助手聚合本体。
	Agent Agent
	// Username 所有者用户名。
	Username string
}

// AgentSnapshotRevision 是助手发布快照原地更新前的历史快照。
type AgentSnapshotRevision struct {
	// ID 历史记录唯一标识。
	ID uuid.UUID `json:"id"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `json:"user_id"`
	// AgentID 所属助手版本。
	AgentID uuid.UUID `json:"agent_id"`
	// AgentVersion 助手版本号。
	AgentVersion int `json:"agent_version"`
	// ConfigSnapshot 更新前的发布快照。
	ConfigSnapshot map[string]any `json:"config_snapshot"`
	// Reason 写入历史的原因。
	Reason string `json:"reason"`
	// CreatedAt 写入时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
}

// AgentRepository 定义用户私有 Agent 的持久化契约，所有方法强制携带 userID。
type AgentRepository interface {
	// CreateAgent 创建 Agent 版本及绑定并写入审计事件。
	CreateAgent(ctx context.Context, agent Agent, event OutboxEvent) (Agent, error)
	// CreateAgentVersion 创建新版本，并在同一事务内复制源版本的显式移除标记。
	CreateAgentVersion(ctx context.Context, agent Agent, sourceAgentID uuid.UUID, event OutboxEvent) (Agent, error)
	// UpdateAgent 更新草稿 Agent 及绑定。
	UpdateAgent(ctx context.Context, agent Agent, event OutboxEvent) (Agent, error)
	// DeleteAgent 删除草稿 Agent。
	DeleteAgent(ctx context.Context, userID, agentID uuid.UUID, event OutboxEvent) error
	// UpdateAgentStatus 按前置状态条件更新版本生命周期，防止并发下出现非法转换。
	UpdateAgentStatus(ctx context.Context, userID, agentID uuid.UUID, from, to AgentStatus, clearSnapshot bool, event OutboxEvent) error
	// GetAgent 按用户与 ID 读取 Agent。
	GetAgent(ctx context.Context, userID, agentID uuid.UUID) (Agent, error)
	// AttachUserSkillToAgent 为指定 Agent 版本追加用户技能绑定，并在已发布快照中同步技能内容。
	// 返回 false 表示该版本已显式移除该技能，按语义跳过挂载。
	AttachUserSkillToAgent(ctx context.Context, userID, agentID uuid.UUID, skill UserSkill, event OutboxEvent) (bool, error)
	// ListPublishedAgentIDsByName 返回同名助手的全部已发布版本 ID。
	ListPublishedAgentIDsByName(ctx context.Context, userID uuid.UUID, name string) ([]uuid.UUID, error)
	// RefreshUserSkillOnBoundAgents 刷新所有绑定该用户技能的已发布 Agent 快照。
	RefreshUserSkillOnBoundAgents(ctx context.Context, userID uuid.UUID, skill UserSkill, event OutboxEvent) error
	// ListUserSkillOptOuts 返回助手版本显式移除的用户技能 ID。
	ListUserSkillOptOuts(ctx context.Context, userID, agentID uuid.UUID) ([]uuid.UUID, error)
	// ListAgentSnapshotRevisions 按时间倒序读取助手快照历史。
	ListAgentSnapshotRevisions(ctx context.Context, userID, agentID uuid.UUID, limit int) ([]AgentSnapshotRevision, error)
	// GetAgentSnapshotRevision 读取单条助手快照历史。
	GetAgentSnapshotRevision(ctx context.Context, userID, agentID, revisionID uuid.UUID) (AgentSnapshotRevision, error)
	// RestoreAgentSnapshot 用历史快照覆盖当前助手快照，并保留当前快照作为新历史。
	RestoreAgentSnapshot(ctx context.Context, userID, agentID uuid.UUID, snapshot map[string]any, event OutboxEvent) error
	// ListAgents 按用户游标分页列出 Agent；includeArchived 为 false 时排除已归档版本。
	ListAgents(ctx context.Context, userID uuid.UUID, includeArchived bool, limit int, cursor string) (Page[Agent], error)
	// ListAllAgents 跨用户游标分页列出助手，仅供管理员只读审计。
	ListAllAgents(ctx context.Context, filter AdminAgentFilter, limit int, cursor string) (Page[AdminAgent], error)
	// GetAgentAdmin 以 system scope 读取任意用户的助手，仅供管理员审计与删除。
	GetAgentAdmin(ctx context.Context, agentID uuid.UUID) (Agent, error)
	// DeleteAgentAdmin 以 system scope 删除任意用户的助手版本，历史记录由外键保留。
	DeleteAgentAdmin(ctx context.Context, actorID, agentID uuid.UUID, event OutboxEvent) error
	// NextAgentVersion 返回同名 Agent 的下一个版本号。
	NextAgentVersion(ctx context.Context, userID uuid.UUID, name string) (int, error)
	// GetPolicy 读取引用的模型策略。
	GetPolicy(ctx context.Context, policyID uuid.UUID) (ModelPolicy, error)
	// ListModelsByIDs 批量读取策略候选模型。
	ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]Model, error)
	// ListSkillsByIDs 批量读取 Skill 摘要。
	ListSkillsByIDs(ctx context.Context, skillIDs []uuid.UUID) ([]Skill, error)
	// ListUserSkillsByIDs 批量读取当前用户的技能摘要，用于 Agent 发布校验。
	ListUserSkillsByIDs(ctx context.Context, userID uuid.UUID, skillIDs []uuid.UUID) ([]UserSkill, error)
	// ListMCPsByIDs 批量读取 MCP 摘要。
	ListMCPsByIDs(ctx context.Context, mcpIDs []uuid.UUID) ([]MCP, error)
	// CountAgents 统计当前用户 Agent 版本总数，用于配额校验。
	CountAgents(ctx context.Context, userID uuid.UUID) (int64, error)
}

// AgentService 实现 Agent 草稿、版本与发布用例。
type AgentService struct {
	repo   AgentRepository
	limits QuotaLimits
}

// NewAgentService 构造 Agent 服务，并应用用户级配额。
func NewAgentService(repo AgentRepository, limits QuotaLimits) (*AgentService, error) {
	if repo == nil {
		return nil, invalid("agent repository is required")
	}
	return &AgentService{repo: repo, limits: limits.Normalize()}, nil
}

// Create 创建 Agent 草稿，用户身份只取自 Principal。
func (s *AgentService) Create(ctx context.Context, principal Principal, agent Agent) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	if err := s.checkQuota(ctx, principal.UserID); err != nil {
		return Agent{}, err
	}
	agent.ID = uuid.New()
	agent.UserID = principal.UserID
	agent.Version = 1
	agent.Status = AgentDraft
	agent.ConfigSnapshot = nil
	if err := s.validate(ctx, &agent, false); err != nil {
		return Agent{}, err
	}
	return s.repo.CreateAgent(ctx, agent, mutationEvent(principal.UserID, "agent.created", "agent", agent.ID))
}

// CreateVersion 基于已有版本创建新草稿，已发布版本保持不可变。
func (s *AgentService) CreateVersion(ctx context.Context, principal Principal, sourceAgentID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	source, err := s.repo.GetAgent(ctx, principal.UserID, sourceAgentID)
	if err != nil {
		return Agent{}, err
	}
	if err := s.checkQuota(ctx, principal.UserID); err != nil {
		return Agent{}, err
	}
	version, err := s.repo.NextAgentVersion(ctx, principal.UserID, source.Name)
	if err != nil {
		return Agent{}, err
	}
	source.ID = uuid.New()
	source.Version = version
	source.Status = AgentDraft
	source.ConfigSnapshot = nil
	// 新版本沿用源版本的显式移除标记，避免用户已移除的自动技能在下次提炼时被重新挂回。
	return s.repo.CreateAgentVersion(ctx, source, sourceAgentID, mutationEvent(principal.UserID, "agent.version_created", "agent", source.ID))
}

// Update 仅允许修改草稿；名称与版本号由服务端保持，防止绕过唯一约束。
func (s *AgentService) Update(ctx context.Context, principal Principal, agent Agent) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	current, err := s.repo.GetAgent(ctx, principal.UserID, agent.ID)
	if err != nil {
		return Agent{}, err
	}
	// 乐观并发：客户端带回读取时的 updated_at，若期间被自动挂载等操作修改则拒绝，避免覆盖新绑定。
	if !agent.UpdatedAt.IsZero() && !agent.UpdatedAt.Equal(current.UpdatedAt) {
		return Agent{}, fmt.Errorf("%w: agent was modified after it was loaded; reload and retry", ErrResourceConflict)
	}
	if current.Status != AgentDraft {
		return Agent{}, fmt.Errorf("%w: only draft agents can be updated", ErrResourceConflict)
	}
	if err := uniqueUUIDs(agent.UserSkillOptOuts); err != nil {
		return Agent{}, err
	}
	for _, id := range agent.UserSkillOptOuts {
		if containsUUID(agent.UserSkillIDs, id) {
			return Agent{}, invalid("a user skill cannot be both bound and explicitly removed")
		}
	}
	agent.UserID = principal.UserID
	agent.Name = current.Name
	agent.Version = current.Version
	agent.Status = AgentDraft
	agent.ConfigSnapshot = nil
	if err := s.validate(ctx, &agent, false); err != nil {
		return Agent{}, err
	}
	return s.repo.UpdateAgent(ctx, agent, mutationEvent(principal.UserID, "agent.updated", "agent", agent.ID))
}

// Publish 发布草稿并固化配置快照；发布后 Run 只引用该不可变版本。
func (s *AgentService) Publish(ctx context.Context, principal Principal, agentID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return Agent{}, err
	}
	if agent.Status != AgentDraft {
		return Agent{}, fmt.Errorf("%w: only draft agents can be published", ErrResourceConflict)
	}
	// 发布前继承同家族已发布版本的自动技能，避免草稿创建后发生的自动挂载丢失。
	if err := s.inheritFamilyAutoUserSkills(ctx, &agent); err != nil {
		return Agent{}, err
	}
	if err := s.validate(ctx, &agent, true); err != nil {
		return Agent{}, err
	}
	snapshot, err := s.buildSnapshot(ctx, agent)
	if err != nil {
		return Agent{}, err
	}
	agent.Status = AgentPublished
	agent.ConfigSnapshot = snapshot
	return s.repo.UpdateAgent(ctx, agent, mutationEvent(principal.UserID, "agent.published", "agent", agent.ID))
}

// Unpublish 取消发布：已发布版本回到草稿，允许继续编辑；历史 Run 与快照不受影响。
func (s *AgentService) Unpublish(ctx context.Context, principal Principal, agentID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return Agent{}, err
	}
	if agent.Status != AgentPublished {
		return Agent{}, fmt.Errorf("%w: only published agents can be unpublished", ErrResourceConflict)
	}
	// 回到草稿后快照已失效，清空可避免展示过期的发布配置。
	if err := s.repo.UpdateAgentStatus(ctx, principal.UserID, agentID, AgentPublished, AgentDraft, true,
		mutationEvent(principal.UserID, "agent.unpublished", "agent", agent.ID)); err != nil {
		return Agent{}, err
	}
	return s.repo.GetAgent(ctx, principal.UserID, agentID)
}

// Archive 归档已发布或已停用版本：从默认列表隐藏，历史对话与 Run 仍可追溯。
func (s *AgentService) Archive(ctx context.Context, principal Principal, agentID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return Agent{}, err
	}
	if agent.Status != AgentPublished && agent.Status != AgentDisabled {
		return Agent{}, fmt.Errorf("%w: only published or disabled agents can be archived", ErrResourceConflict)
	}
	if err := s.repo.UpdateAgentStatus(ctx, principal.UserID, agentID, agent.Status, AgentArchived, false,
		mutationEvent(principal.UserID, "agent.archived", "agent", agent.ID)); err != nil {
		return Agent{}, err
	}
	return s.repo.GetAgent(ctx, principal.UserID, agentID)
}

// Delete 仅允许删除草稿，已发布版本用于历史 Run 复现。
func (s *AgentService) Delete(ctx context.Context, principal Principal, agentID uuid.UUID) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return err
	}
	if agent.Status != AgentDraft {
		return fmt.Errorf("%w: only draft agents can be deleted", ErrResourceConflict)
	}
	return s.repo.DeleteAgent(ctx, principal.UserID, agentID, mutationEvent(principal.UserID, "agent.deleted", "agent", agent.ID))
}

// Get 读取当前用户的 Agent。
func (s *AgentService) Get(ctx context.Context, principal Principal, agentID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	return s.repo.GetAgent(ctx, principal.UserID, agentID)
}

// AttachUserSkillToAgentFamily 把技能绑定到同名助手的全部已发布版本；没有已发布版本时退回来源版本。
// 返回实际未被显式移除的版本数量，不创建新版本，覆盖旧会话、新会话与飞书机器人可能使用的各个已发布版本。
func (s *AgentService) AttachUserSkillToAgentFamily(ctx context.Context, principal Principal, sourceAgentID uuid.UUID, skill UserSkill) (int, error) {
	if principal.UserID == uuid.Nil {
		return 0, ErrForbidden
	}
	source, err := s.repo.GetAgent(ctx, principal.UserID, sourceAgentID)
	if err != nil {
		return 0, err
	}
	agentIDs, err := s.repo.ListPublishedAgentIDsByName(ctx, principal.UserID, source.Name)
	if err != nil {
		return 0, err
	}
	if len(agentIDs) == 0 {
		agentIDs = []uuid.UUID{sourceAgentID}
	}
	sort.Slice(agentIDs, func(i, j int) bool { return agentIDs[i].String() < agentIDs[j].String() })
	attached := 0
	for _, agentID := range agentIDs {
		ok, err := s.AttachUserSkill(ctx, principal, agentID, skill)
		if err != nil {
			return attached, err
		}
		if ok {
			attached++
		}
	}
	return attached, nil
}

// InheritableUserSkills 返回草稿发布时将继承的同家族自动技能，供前端确认或取消。
func (s *AgentService) InheritableUserSkills(ctx context.Context, principal Principal, agentID uuid.UUID) ([]UserSkill, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return nil, err
	}
	if agent.Status != AgentDraft {
		return []UserSkill{}, nil
	}
	return s.familyInheritableAutoUserSkills(ctx, agent)
}

// inheritFamilyAutoUserSkills 在发布时继承同名已发布版本的自动技能。
// 草稿创建后发生的自动挂载由此补齐；用户在当前版本显式移除过的技能保持移除，
// 手动技能与已停用技能不继承，避免覆盖用户的版本级选择。
func (s *AgentService) inheritFamilyAutoUserSkills(ctx context.Context, agent *Agent) error {
	skills, err := s.familyInheritableAutoUserSkills(ctx, *agent)
	if err != nil {
		return err
	}
	for _, skill := range skills {
		agent.UserSkillIDs = append(agent.UserSkillIDs, skill.ID)
	}
	return nil
}

// familyInheritableAutoUserSkills 计算发布会继承的自动技能：同家族已发布版本绑定、
// 当前版本未绑定且未被显式移除，并仍然处于已发布状态的自动技能。
func (s *AgentService) familyInheritableAutoUserSkills(ctx context.Context, agent Agent) ([]UserSkill, error) {
	siblings, err := s.repo.ListPublishedAgentIDsByName(ctx, agent.UserID, agent.Name)
	if err != nil || len(siblings) == 0 {
		return nil, err
	}
	bound := make(map[uuid.UUID]struct{}, len(agent.UserSkillIDs))
	for _, id := range agent.UserSkillIDs {
		bound[id] = struct{}{}
	}
	candidates := make(map[uuid.UUID]struct{})
	for _, siblingID := range siblings {
		sibling, err := s.repo.GetAgent(ctx, agent.UserID, siblingID)
		if err != nil {
			return nil, err
		}
		for _, id := range sibling.UserSkillIDs {
			if _, exists := bound[id]; !exists {
				candidates[id] = struct{}{}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	optOuts, err := s.repo.ListUserSkillOptOuts(ctx, agent.UserID, agent.ID)
	if err != nil {
		return nil, err
	}
	for _, id := range optOuts {
		delete(candidates, id)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	skills, err := s.repo.ListUserSkillsByIDs(ctx, agent.UserID, ids)
	if err != nil {
		return nil, err
	}
	inheritable := make([]UserSkill, 0, len(skills))
	for _, skill := range skills {
		if skill.Status != UserSkillPublished || skill.Origin != UserSkillOriginAuto {
			continue
		}
		inheritable = append(inheritable, skill)
	}
	sort.Slice(inheritable, func(i, j int) bool { return inheritable[i].ID.String() < inheritable[j].ID.String() })
	return inheritable, nil
}

// AttachUserSkill 把已发布的用户技能直接绑定到指定助手版本，并在已发布快照中同步技能内容。
// 返回 false 表示该版本显式移除过该技能，挂载被按语义跳过。
func (s *AgentService) AttachUserSkill(ctx context.Context, principal Principal, agentID uuid.UUID, skill UserSkill) (bool, error) {
	if principal.UserID == uuid.Nil {
		return false, ErrForbidden
	}
	if agentID == uuid.Nil || skill.ID == uuid.Nil || skill.UserID != principal.UserID {
		return false, invalid("agent and user skill must belong to the current user")
	}
	if skill.Status != UserSkillPublished {
		return false, invalid("only published user skills can be attached to an agent")
	}
	return s.repo.AttachUserSkillToAgent(ctx, principal.UserID, agentID, skill,
		mutationEvent(principal.UserID, "agent.user_skill_attached", "agent", agentID))
}

// RefreshBoundAgentsForUserSkill 刷新所有绑定该技能的已发布助手快照，让技能更新只改内容、不升版本。
func (s *AgentService) RefreshBoundAgentsForUserSkill(ctx context.Context, principal Principal, skill UserSkill) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	if skill.ID == uuid.Nil || skill.UserID != principal.UserID {
		return invalid("user skill must belong to the current user")
	}
	if skill.Status != UserSkillPublished {
		return invalid("only published user skills can refresh agent snapshots")
	}
	return s.repo.RefreshUserSkillOnBoundAgents(ctx, principal.UserID, skill,
		mutationEvent(principal.UserID, "user_skill.agent_snapshots_refreshed", "user_skill", skill.ID))
}

// ListSnapshotRevisions 列出助手发布快照的历史版本。
func (s *AgentService) ListSnapshotRevisions(ctx context.Context, principal Principal, agentID uuid.UUID, limit int) ([]AgentSnapshotRevision, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.repo.GetAgent(ctx, principal.UserID, agentID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	return s.repo.ListAgentSnapshotRevisions(ctx, principal.UserID, agentID, limit)
}

// RestoreSnapshotRevision 把助手发布快照恢复到指定历史版本；当前快照会作为新历史保留。
func (s *AgentService) RestoreSnapshotRevision(ctx context.Context, principal Principal, agentID, revisionID uuid.UUID) (Agent, error) {
	if principal.UserID == uuid.Nil {
		return Agent{}, ErrForbidden
	}
	agent, err := s.repo.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return Agent{}, err
	}
	if agent.Status != AgentPublished {
		return Agent{}, fmt.Errorf("%w: only published agents have restorable snapshots", ErrResourceConflict)
	}
	revision, err := s.repo.GetAgentSnapshotRevision(ctx, principal.UserID, agentID, revisionID)
	if err != nil {
		return Agent{}, err
	}
	if err := s.repo.RestoreAgentSnapshot(ctx, principal.UserID, agentID, revision.ConfigSnapshot,
		mutationEvent(principal.UserID, "agent.snapshot_restored", "agent", agentID)); err != nil {
		return Agent{}, err
	}
	return s.repo.GetAgent(ctx, principal.UserID, agentID)
}

// List 按游标分页列出当前用户的 Agent；默认隐藏已归档版本。
func (s *AgentService) List(ctx context.Context, principal Principal, includeArchived bool, limit int, cursor string) (Page[Agent], error) {
	if principal.UserID == uuid.Nil {
		return Page[Agent]{}, ErrForbidden
	}
	return s.repo.ListAgents(ctx, principal.UserID, includeArchived, limit, cursor)
}

// ListAll 跨用户列出助手，仅管理员可用；返回只读摘要，不包含绑定明细。
func (s *AgentService) ListAll(ctx context.Context, principal Principal, filter AdminAgentFilter, limit int, cursor string) (Page[AdminAgent], error) {
	if err := RequireAdmin(principal); err != nil {
		return Page[AdminAgent]{}, err
	}
	switch filter.Status {
	case "", AgentDraft, AgentPublished, AgentDisabled, AgentArchived:
	default:
		return Page[AdminAgent]{}, invalid("unsupported agent status filter")
	}
	return s.repo.ListAllAgents(ctx, filter, limit, cursor)
}

// AdminDelete 允许管理员删除任意用户的助手版本；历史会话与运行记录保留，仅解除助手归属。
func (s *AgentService) AdminDelete(ctx context.Context, principal Principal, agentID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	agent, err := s.repo.GetAgentAdmin(ctx, agentID)
	if err != nil {
		return err
	}
	event := mutationEvent(principal.UserID, "agent.admin_deleted", "agent", agent.ID)
	event.Payload["target_user_id"] = agent.UserID.String()
	event.Payload["agent_name"] = agent.Name
	event.Payload["agent_status"] = string(agent.Status)
	return s.repo.DeleteAgentAdmin(ctx, principal.UserID, agent.ID, event)
}

// checkQuota 校验用户 Agent 版本总数是否达到上限。
func (s *AgentService) checkQuota(ctx context.Context, userID uuid.UUID) error {
	count, err := s.repo.CountAgents(ctx, userID)
	if err != nil {
		return err
	}
	if count >= int64(s.limits.MaxAgentsPerUser) {
		return ErrQuotaExceeded
	}
	return nil
}

// validate 校验 Agent 身份、模式、策略版本一致性与绑定数量。
func (s *AgentService) validate(ctx context.Context, agent *Agent, publishing bool) error {
	agent.Name = strings.TrimSpace(agent.Name)
	if agent.ID == uuid.Nil || agent.UserID == uuid.Nil || agent.Name == "" || len(agent.Name) > 128 || len(agent.Description) > 4096 || agent.Version <= 0 {
		return invalid("agent identity, name, description and version are invalid")
	}
	if agent.Mode != AgentModeReact {
		return invalid("unsupported agent mode")
	}
	// 策略 ID 与版本必须成对出现，避免引用不存在的版本。
	if (agent.ModelPolicyID == nil) != (agent.ModelPolicyVersion == nil) {
		return invalid("model policy id and version must be provided together")
	}
	if len(agent.SkillIDs)+len(agent.UserSkillIDs) > 64 || len(agent.MCPIDs) > 64 {
		return invalid("an agent supports at most 64 skill and MCP bindings")
	}
	if err := uniqueUUIDs(agent.SkillIDs); err != nil {
		return err
	}
	if err := uniqueUUIDs(agent.UserSkillIDs); err != nil {
		return err
	}
	if err := uniqueUUIDs(agent.MCPIDs); err != nil {
		return err
	}
	if agent.ModelPolicyID == nil {
		if publishing {
			return invalid("a published agent requires a model policy")
		}
		return nil
	}
	policy, err := s.repo.GetPolicy(ctx, *agent.ModelPolicyID)
	if err != nil {
		return err
	}
	if agent.ModelPolicyVersion == nil || policy.Version != *agent.ModelPolicyVersion {
		return invalid("agent policy version does not match the catalog")
	}
	if publishing && policy.Status != PolicyPublished {
		return invalid("a published agent requires a published model policy")
	}
	return nil
}

// buildSnapshot 固化发布快照：策略、候选模型、Skill 与 MCP 全部只保存非密元数据。
func (s *AgentService) buildSnapshot(ctx context.Context, agent Agent) (map[string]any, error) {
	policy, err := s.repo.GetPolicy(ctx, *agent.ModelPolicyID)
	if err != nil {
		return nil, err
	}
	modelIDs := make([]uuid.UUID, 0, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		modelIDs = append(modelIDs, candidate.ModelID)
	}
	models, err := s.repo.ListModelsByIDs(ctx, modelIDs)
	if err != nil {
		return nil, err
	}
	if len(models) != len(modelIDs) {
		return nil, invalid("policy snapshot contains missing models")
	}
	skills, err := s.repo.ListSkillsByIDs(ctx, agent.SkillIDs)
	if err != nil {
		return nil, err
	}
	if len(skills) != len(agent.SkillIDs) {
		return nil, invalid("agent snapshot contains missing skills")
	}
	for _, skill := range skills {
		if skill.Status != "published" {
			return nil, invalid("published agents can only bind published skills")
		}
	}
	var userSkills []UserSkill
	if len(agent.UserSkillIDs) > 0 {
		userSkills, err = s.repo.ListUserSkillsByIDs(ctx, agent.UserID, agent.UserSkillIDs)
		if err != nil {
			return nil, err
		}
		if len(userSkills) != len(agent.UserSkillIDs) {
			return nil, invalid("agent snapshot contains missing user skills")
		}
		for _, userSkill := range userSkills {
			if userSkill.Status != UserSkillPublished {
				return nil, invalid("published agents can only bind published user skills")
			}
		}
	}
	mcps, err := s.repo.ListMCPsByIDs(ctx, agent.MCPIDs)
	if err != nil {
		return nil, err
	}
	if len(mcps) != len(agent.MCPIDs) {
		return nil, invalid("agent snapshot contains missing MCPs")
	}
	for _, mcp := range mcps {
		if mcp.Status != "enabled" {
			return nil, invalid("published agents can only bind enabled MCPs")
		}
	}
	return map[string]any{
		"agent":        map[string]any{"name": agent.Name, "description": agent.Description, "mode": agent.Mode, "version": agent.Version},
		"model_policy": policy, "models": models, "skills": skills, "user_skills": userSkills, "mcps": mcps,
	}, nil
}

// uniqueUUIDs 校验绑定 ID 非空且不重复。
func uniqueUUIDs(values []uuid.UUID) error {
	seen := make(map[uuid.UUID]struct{}, len(values))
	for _, value := range values {
		if value == uuid.Nil {
			return invalid("binding ids must be non-nil")
		}
		if _, exists := seen[value]; exists {
			return invalid("binding ids must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}
