package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PolicyStatus 表示模型路由策略的生命周期状态。
type PolicyStatus string

const (
	// PolicyDraft 草稿态，可继续编辑候选与规则。
	PolicyDraft PolicyStatus = "draft"
	// PolicyPublished 已发布，可被 Agent 引用，内容不可再修改。
	PolicyPublished PolicyStatus = "published"
	// PolicyDisabled 已停用，不再用于新 Run。
	PolicyDisabled PolicyStatus = "disabled"
)

// PolicyCandidate 是路由策略中的单个候选模型及其优先级与约束。
type PolicyCandidate struct {
	// ModelID 候选模型 ID。
	ModelID uuid.UUID `json:"model_id"`
	// Priority 显式优先级，数值越小越优先，同一策略内唯一。
	Priority int `json:"priority"`
	// Constraints 单候选约束，如数据区域、成本上限等。
	Constraints map[string]any `json:"constraints,omitempty"`
}

// ModelPolicy 是版本化的模型路由策略，Agent 发布时按版本固化引用。
type ModelPolicy struct {
	// ID 策略唯一标识，每个版本一条记录。
	ID uuid.UUID
	// Name 策略名称，与版本共同唯一。
	Name string
	// Version 策略版本号，从 1 递增。
	Version int
	// MinContextTokens 该策略要求的最小上下文窗口，硬准入时使用。
	MinContextTokens int64
	// Rules 路由规则，如健康优先级、成本与区域限制。
	Rules map[string]any
	// Status 策略生命周期状态。
	Status PolicyStatus
	// Candidates 有序候选列表，发布后不可变。
	Candidates []PolicyCandidate
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// PolicyRepository 定义策略持久化契约，写操作与审计事件同事务提交。
type PolicyRepository interface {
	// CreatePolicy 创建策略及候选并写入审计事件。
	CreatePolicy(ctx context.Context, actorID uuid.UUID, policy ModelPolicy, event OutboxEvent) (ModelPolicy, error)
	// UpdatePolicy 更新草稿策略及候选。
	UpdatePolicy(ctx context.Context, actorID uuid.UUID, policy ModelPolicy, event OutboxEvent) (ModelPolicy, error)
	// DeletePolicy 删除策略并写入审计事件。
	DeletePolicy(ctx context.Context, actorID, policyID uuid.UUID, event OutboxEvent) error
	// GetPolicy 按 ID 读取策略及候选。
	GetPolicy(ctx context.Context, policyID uuid.UUID) (ModelPolicy, error)
	// ListPolicies 列出全部策略。
	ListPolicies(ctx context.Context) ([]ModelPolicy, error)
	// ListModelsByIDs 批量读取候选模型，用于发布前准入校验。
	ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]Model, error)
	// NextPolicyVersion 返回同名策略的下一个版本号。
	NextPolicyVersion(ctx context.Context, name string) (int, error)
}

// PolicyService 实现版本化策略管理，发布时强制校验候选可路由性。
type PolicyService struct {
	repo PolicyRepository
}

// NewPolicyService 构造策略服务。
func NewPolicyService(repo PolicyRepository) (*PolicyService, error) {
	if repo == nil {
		return nil, invalid("policy repository is required")
	}
	return &PolicyService{repo: repo}, nil
}

// Create 创建草稿策略，版本固定为 1。
func (s *PolicyService) Create(ctx context.Context, principal Principal, policy ModelPolicy) (ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return ModelPolicy{}, err
	}
	policy.ID = uuid.New()
	policy.Version = 1
	policy.Status = PolicyDraft
	if err := s.validate(ctx, &policy, false); err != nil {
		return ModelPolicy{}, err
	}
	return s.repo.CreatePolicy(ctx, principal.UserID, policy, mutationEvent(principal.UserID, "model_policy.created", "model_policy", policy.ID))
}

// CreateVersion 基于已有策略创建新草稿版本，已发布版本保持不变。
func (s *PolicyService) CreateVersion(ctx context.Context, principal Principal, sourcePolicyID uuid.UUID) (ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return ModelPolicy{}, err
	}
	source, err := s.repo.GetPolicy(ctx, sourcePolicyID)
	if err != nil {
		return ModelPolicy{}, err
	}
	version, err := s.repo.NextPolicyVersion(ctx, source.Name)
	if err != nil {
		return ModelPolicy{}, err
	}
	source.ID = uuid.New()
	source.Version = version
	source.Status = PolicyDraft
	return s.repo.CreatePolicy(ctx, principal.UserID, source, mutationEvent(principal.UserID, "model_policy.version_created", "model_policy", source.ID))
}

// Update 仅允许修改草稿策略，已发布策略必须通过新版本变更。
func (s *PolicyService) Update(ctx context.Context, principal Principal, policy ModelPolicy) (ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return ModelPolicy{}, err
	}
	current, err := s.repo.GetPolicy(ctx, policy.ID)
	if err != nil {
		return ModelPolicy{}, err
	}
	if current.Status != PolicyDraft {
		return ModelPolicy{}, fmt.Errorf("%w: only draft policies can be updated", ErrResourceConflict)
	}
	policy.Name = current.Name
	policy.Version = current.Version
	policy.Status = PolicyDraft
	if err := s.validate(ctx, &policy, false); err != nil {
		return ModelPolicy{}, err
	}
	return s.repo.UpdatePolicy(ctx, principal.UserID, policy, mutationEvent(principal.UserID, "model_policy.updated", "model_policy", policy.ID))
}

// Publish 发布草稿：要求至少一个候选，且每个候选均为已核验、启用且满足 1M+ 的生成模型。
func (s *PolicyService) Publish(ctx context.Context, principal Principal, policyID uuid.UUID) (ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return ModelPolicy{}, err
	}
	policy, err := s.repo.GetPolicy(ctx, policyID)
	if err != nil {
		return ModelPolicy{}, err
	}
	if policy.Status != PolicyDraft {
		return ModelPolicy{}, fmt.Errorf("%w: only draft policies can be published", ErrResourceConflict)
	}
	policy.Status = PolicyPublished
	if err := s.validate(ctx, &policy, true); err != nil {
		return ModelPolicy{}, err
	}
	return s.repo.UpdatePolicy(ctx, principal.UserID, policy, mutationEvent(principal.UserID, "model_policy.published", "model_policy", policy.ID))
}

// Delete 删除策略并写入审计事件。
func (s *PolicyService) Delete(ctx context.Context, principal Principal, policyID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	return s.repo.DeletePolicy(ctx, principal.UserID, policyID, mutationEvent(principal.UserID, "model_policy.deleted", "model_policy", policyID))
}

// Get 读取策略，管理员专用。
func (s *PolicyService) Get(ctx context.Context, principal Principal, policyID uuid.UUID) (ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return ModelPolicy{}, err
	}
	return s.repo.GetPolicy(ctx, policyID)
}

// List 列出策略，管理员专用。
func (s *PolicyService) List(ctx context.Context, principal Principal) ([]ModelPolicy, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListPolicies(ctx)
}

// validate 校验策略结构；publishing 为真时额外执行候选模型的硬准入校验。
func (s *PolicyService) validate(ctx context.Context, policy *ModelPolicy, publishing bool) error {
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.ID == uuid.Nil || policy.Name == "" || len(policy.Name) > 128 || policy.Version <= 0 {
		return invalid("policy id, name and positive version are required")
	}
	if policy.MinContextTokens == 0 {
		policy.MinContextTokens = MinGenerationContextTokens
	}
	// 策略自身的最小上下文不得低于 1M，防止通过策略绕过准入。
	if policy.MinContextTokens < MinGenerationContextTokens {
		return invalid("policy minimum context must be at least %d", MinGenerationContextTokens)
	}
	if len(policy.Candidates) > 16 {
		return invalid("a policy supports at most 16 candidates")
	}
	if publishing && len(policy.Candidates) == 0 {
		return invalid("a published policy requires at least one candidate")
	}
	if err := validateJSONSize(policy.Rules, 64*1024); err != nil {
		return err
	}
	// 候选模型与优先级必须唯一，避免路由顺序不确定。
	ids := make([]uuid.UUID, 0, len(policy.Candidates))
	seenModels := make(map[uuid.UUID]struct{}, len(policy.Candidates))
	seenPriorities := make(map[int]struct{}, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		if candidate.ModelID == uuid.Nil || candidate.Priority <= 0 {
			return invalid("candidate model and positive priority are required")
		}
		if _, exists := seenModels[candidate.ModelID]; exists {
			return invalid("candidate models must be unique")
		}
		if _, exists := seenPriorities[candidate.Priority]; exists {
			return invalid("candidate priorities must be unique")
		}
		if err := validateJSONSize(candidate.Constraints, 16*1024); err != nil {
			return err
		}
		seenModels[candidate.ModelID] = struct{}{}
		seenPriorities[candidate.Priority] = struct{}{}
		ids = append(ids, candidate.ModelID)
	}
	// 仅草稿态的候选存在性延迟到发布时校验，避免草稿反复依赖目录变更。
	if len(ids) == 0 || !publishing {
		return nil
	}
	models, err := s.repo.ListModelsByIDs(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, candidate := range policy.Candidates {
		model, exists := byID[candidate.ModelID]
		if !exists {
			return fmt.Errorf("%w: candidate model %s", ErrResourceNotFound, candidate.ModelID)
		}
		if publishing {
			// 发布候选必须是已启用、已核验的生成模型，且满足策略上下文下限。
			if model.Kind != ModelKindChat || model.Status != ModelEnabled || model.VerifiedAt == nil || strings.TrimSpace(model.VerificationSource) == "" {
				return invalid("published candidates must be enabled and verified generation models")
			}
			if model.ContextWindowTokens < policy.MinContextTokens {
				return invalid("candidate %s does not satisfy the policy context minimum", model.ID)
			}
			// 数据区域约束必须在发布时与模型元数据匹配，防止合规约束只停留在配置层。
			if region, regionErr := candidateDataRegion(candidate.Constraints); regionErr != nil {
				return regionErr
			} else if region != "" && model.DataRegion != region {
				return invalid("candidate %s data_region %q does not satisfy constraint %q", model.ID, model.DataRegion, region)
			}
		}
	}
	return nil
}

// candidateDataRegion 读取候选的数据区域约束；缺省表示不限制，非字符串视为非法配置。
func candidateDataRegion(constraints map[string]any) (string, error) {
	if constraints == nil {
		return "", nil
	}
	raw, exists := constraints["data_region"]
	if !exists || raw == nil {
		return "", nil
	}
	region, ok := raw.(string)
	if !ok {
		return "", invalid("candidate data_region constraint must be a string")
	}
	return strings.TrimSpace(region), nil
}

// validateJSONSize 校验 JSON 配置可序列化且不超过上限，避免无界存储。
func validateJSONSize(value any, maximum int) error {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximum {
		return invalid("JSON configuration is invalid or exceeds %d bytes", maximum)
	}
	return nil
}
