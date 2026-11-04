package biz

import (
	"context"
	"fmt"
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
	// AgentModePlan Plan-and-Execute 模式，先规划再执行。
	AgentModePlan AgentMode = "plan"
	// AgentModeSupervisor Supervisor 模式，委派多个子 Agent。
	AgentModeSupervisor AgentMode = "super"
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
type Skill struct {
	// ID Skill 唯一标识。
	ID uuid.UUID `json:"id"`
	// Name Skill 名称。
	Name string `json:"name"`
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
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 更新时间（UTC）。
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentRepository 定义用户私有 Agent 的持久化契约，所有方法强制携带 userID。
type AgentRepository interface {
	// CreateAgent 创建 Agent 版本及绑定并写入审计事件。
	CreateAgent(ctx context.Context, agent Agent, event OutboxEvent) (Agent, error)
	// UpdateAgent 更新草稿 Agent 及绑定。
	UpdateAgent(ctx context.Context, agent Agent, event OutboxEvent) (Agent, error)
	// DeleteAgent 删除草稿 Agent。
	DeleteAgent(ctx context.Context, userID, agentID uuid.UUID, event OutboxEvent) error
	// UpdateAgentStatus 按前置状态条件更新版本生命周期，防止并发下出现非法转换。
	UpdateAgentStatus(ctx context.Context, userID, agentID uuid.UUID, from, to AgentStatus, clearSnapshot bool, event OutboxEvent) error
	// GetAgent 按用户与 ID 读取 Agent。
	GetAgent(ctx context.Context, userID, agentID uuid.UUID) (Agent, error)
	// ListAgents 按用户游标分页列出 Agent；includeArchived 为 false 时排除已归档版本。
	ListAgents(ctx context.Context, userID uuid.UUID, includeArchived bool, limit int, cursor string) (Page[Agent], error)
	// NextAgentVersion 返回同名 Agent 的下一个版本号。
	NextAgentVersion(ctx context.Context, userID uuid.UUID, name string) (int, error)
	// GetPolicy 读取引用的模型策略。
	GetPolicy(ctx context.Context, policyID uuid.UUID) (ModelPolicy, error)
	// ListModelsByIDs 批量读取策略候选模型。
	ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]Model, error)
	// ListSkillsByIDs 批量读取 Skill 摘要。
	ListSkillsByIDs(ctx context.Context, skillIDs []uuid.UUID) ([]Skill, error)
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
	return s.repo.CreateAgent(ctx, source, mutationEvent(principal.UserID, "agent.version_created", "agent", source.ID))
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
	if current.Status != AgentDraft {
		return Agent{}, fmt.Errorf("%w: only draft agents can be updated", ErrResourceConflict)
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

// List 按游标分页列出当前用户的 Agent；默认隐藏已归档版本。
func (s *AgentService) List(ctx context.Context, principal Principal, includeArchived bool, limit int, cursor string) (Page[Agent], error) {
	if principal.UserID == uuid.Nil {
		return Page[Agent]{}, ErrForbidden
	}
	return s.repo.ListAgents(ctx, principal.UserID, includeArchived, limit, cursor)
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
	if agent.Mode != AgentModeReact && agent.Mode != AgentModePlan && agent.Mode != AgentModeSupervisor {
		return invalid("unsupported agent mode")
	}
	// 策略 ID 与版本必须成对出现，避免引用不存在的版本。
	if (agent.ModelPolicyID == nil) != (agent.ModelPolicyVersion == nil) {
		return invalid("model policy id and version must be provided together")
	}
	if len(agent.SkillIDs) > 64 || len(agent.MCPIDs) > 64 {
		return invalid("an agent supports at most 64 skill and MCP bindings")
	}
	if err := uniqueUUIDs(agent.SkillIDs); err != nil {
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
		"model_policy": policy, "models": models, "skills": skills, "mcps": mcps,
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
