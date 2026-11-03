package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Run 是一次 Agent 执行的领域聚合，保存不可变配置快照与状态机。
type Run struct {
	// ID Run 唯一标识。
	ID uuid.UUID
	// SessionID 所属会话。
	SessionID uuid.UUID
	// UserID 所属用户，所有查询强制携带。
	UserID uuid.UUID
	// AgentID 使用的 Agent 版本，可为空。
	AgentID *uuid.UUID
	// AgentVersion Agent 版本号。
	AgentVersion *int
	// Mode 编排模式。
	Mode AgentMode
	// Status 生命周期状态。
	Status RunStatus
	// ConfigSnapshot Run 创建时固化的 Agent 配置快照。
	ConfigSnapshot map[string]any
	// ModelPolicySnapshot Run 创建时固化的模型路由快照。
	ModelPolicySnapshot map[string]any
	// CurrentStepSeq 当前执行步骤序号。
	CurrentStepSeq int64
	// AttemptNo 当前模型尝试序号。
	AttemptNo int
	// IdempotencyKey 创建幂等键，同用户内唯一。
	IdempotencyKey string
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏后的失败原因。
	ErrorMessage string
	// DeadlineAt Run 截止时间。
	DeadlineAt *time.Time
	// StartedAt 开始执行时间。
	StartedAt *time.Time
	// FinishedAt 结束时间。
	FinishedAt *time.Time
	// LeaseOwner 当前持有执行租约的 Worker 标识。
	LeaseOwner string
	// LeaseExpiresAt 执行租约到期时间。
	LeaseExpiresAt *time.Time
	// FencingToken 租约栅栏令牌，每次认领递增。
	FencingToken int64
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 更新时间（UTC）。
	UpdatedAt time.Time
}

// RunEvent 是 Run 的可重放事件，seq 在 Run 内单调递增。
type RunEvent struct {
	// RunID 所属 Run。
	RunID uuid.UUID
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID
	// Seq Run 内单调递增序号。
	Seq int64
	// EventID 事件唯一标识，用于 SSE id 去重。
	EventID uuid.UUID
	// EventType 事件类型，与 SSE 事件名一致。
	EventType string
	// SchemaVersion 事件载荷版本。
	SchemaVersion int
	// Payload 事件载荷，不得包含密钥或隐藏思维链。
	Payload map[string]any
	// OccurredAt 事件发生时间（UTC）。
	OccurredAt time.Time
}

// RunFence 是 Worker 持有的租约栅栏，用于阻止过期 Worker 提交事件或终态。
type RunFence struct {
	// Owner Worker 实例标识。
	Owner string
	// Token 单调递增的 fencing token，每次认领加一。
	Token int64
}

// RunRepository 定义 Run 与事件的持久化契约。
type RunRepository interface {
	// CreateRun 创建 Run；同一用户幂等键冲突时返回已存在的 Run。
	CreateRun(ctx context.Context, run Run) (Run, error)
	// GetRun 按用户与 Run ID 读取。
	GetRun(ctx context.Context, userID, runID uuid.UUID) (Run, error)
	// ListRuns 按会话游标分页列出 Run。
	ListRuns(ctx context.Context, userID, sessionID uuid.UUID, limit int, cursor string) (Page[Run], error)
	// TransitionRun 以状态 CAS 推进 Run 状态。
	TransitionRun(ctx context.Context, userID, runID uuid.UUID, from, to RunStatus, fields map[string]any) (Run, error)
	// AppendEvent 追加事件并分配单调 seq。
	AppendEvent(ctx context.Context, event RunEvent) (RunEvent, error)
	// AppendEventFenced 仅在租约栅栏有效时追加事件，防止过期 Worker 写入幽灵事件。
	AppendEventFenced(ctx context.Context, event RunEvent, fence RunFence) (RunEvent, error)
	// ListEvents 按 seq 升序读取 afterSeq 之后的事件。
	ListEvents(ctx context.Context, userID, runID uuid.UUID, afterSeq int64, limit int) ([]RunEvent, error)
	// ClaimRun 认领排队中或租约已过期的 Run，返回新的 fencing token。
	ClaimRun(ctx context.Context, userID, runID uuid.UUID, owner string, lease time.Duration, now time.Time) (Run, RunFence, error)
	// RenewRunLease 续约执行租约；栅栏失效时返回冲突。
	RenewRunLease(ctx context.Context, userID, runID uuid.UUID, fence RunFence, lease time.Duration, now time.Time) error
	// PauseRun 在栅栏有效时把运行中的 Run 推进到 waiting_approval，并清空租约。
	PauseRun(ctx context.Context, userID, runID uuid.UUID, fence RunFence, now time.Time) (Run, error)
	// ResumeRun 在无待决策审批时把 waiting_approval 推进到 running，并清空租约。
	ResumeRun(ctx context.Context, userID, runID uuid.UUID, now time.Time) (Run, error)
	// MarkRunReconciliationRequired 在栅栏有效时把 Run 推进到人工对账状态。
	MarkRunReconciliationRequired(ctx context.Context, userID, runID uuid.UUID, fence RunFence, code, message string, now time.Time) (Run, error)
	// FinishRun 在栅栏有效时推进终态并写入结束字段。
	FinishRun(ctx context.Context, userID, runID uuid.UUID, from, to RunStatus, fields map[string]any, fence RunFence, now time.Time) (Run, error)
	// FinishRunWithEvent 在同一事务内推进终态并追加事件，保证状态与事件不会分裂。
	FinishRunWithEvent(ctx context.Context, userID, runID uuid.UUID, from, to RunStatus, fields map[string]any, event RunEvent, fence RunFence, now time.Time) (Run, error)
	// SetRunAttempt 在栅栏有效时更新 Run 当前 Attempt 序号。
	SetRunAttempt(ctx context.Context, userID, runID uuid.UUID, attemptNo int, fence RunFence) error
}

// RunSnapshotRepository 提供 Run 创建时固化 Agent、策略与模型快照的读取能力。
type RunSnapshotRepository interface {
	// GetAgent 按用户与 ID 读取已发布 Agent 版本。
	GetAgent(ctx context.Context, userID, agentID uuid.UUID) (Agent, error)
	// GetPolicy 读取 Agent 引用的模型策略版本。
	GetPolicy(ctx context.Context, policyID uuid.UUID) (ModelPolicy, error)
	// ListModelsByIDs 批量读取候选模型元数据。
	ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]Model, error)
	// GetProvider 读取候选模型所属供应商连接。
	GetProvider(ctx context.Context, providerID uuid.UUID) (Provider, error)
}

// RunServiceOption 用于按运行环境装配快照来源与任务入队能力。
type RunServiceOption func(*RunService)

// WithRunSnapshots 注入 Run 创建时的快照来源；生产环境必须提供。
func WithRunSnapshots(repo RunSnapshotRepository) RunServiceOption {
	return func(s *RunService) {
		s.snapshots = repo
	}
}

// WithRunEnqueuer 注入 Run 执行任务入队端口；生产环境必须提供。
func WithRunEnqueuer(queue RunEnqueuer) RunServiceOption {
	return func(s *RunService) {
		s.enqueuer = queue
	}
}

// WithExecutionSteps 注入执行步骤与载荷读取端口，用于执行树查询。
func WithExecutionSteps(steps ExecutionStepRepository) RunServiceOption {
	return func(s *RunService) {
		s.steps = steps
	}
}

// WithToolApprovals 注入工具审批仓储，用于审批查询与人工决策。
func WithToolApprovals(approvals ToolApprovalRepository) RunServiceOption {
	return func(s *RunService) {
		s.approvals = approvals
	}
}

// WithBudgets 注入每日预算校验器，用于 Run 创建前拒绝已超限的用户或 Agent。
func WithBudgets(budgets BudgetChecker) RunServiceOption {
	return func(s *RunService) {
		s.budgets = budgets
	}
}

// WithModelCalls 注入模型尝试记录读取端口，用于展示 Attempt 与切换记录。
func WithModelCalls(calls ModelCallRepository) RunServiceOption {
	return func(s *RunService) {
		s.calls = calls
	}
}

// WithSupervisorTasks 注入 Supervisor 子任务读取端口，用于执行树展示。
func WithSupervisorTasks(tasks SupervisorTaskRepository) RunServiceOption {
	return func(s *RunService) {
		s.supervisorTasks = tasks
	}
}

// RunService 实现 Run 创建、查询、取消与事件读取。
type RunService struct {
	repo            RunRepository
	sessions        SessionRepository
	snapshots       RunSnapshotRepository
	enqueuer        RunEnqueuer
	steps           ExecutionStepRepository
	approvals       ToolApprovalRepository
	budgets         BudgetChecker
	calls           ModelCallRepository
	supervisorTasks SupervisorTaskRepository
	clock           func() time.Time
}

// NewRunService 构造 Run 服务；可选能力通过 Option 注入，便于单测替换依赖。
func NewRunService(repo RunRepository, sessions SessionRepository, options ...RunServiceOption) (*RunService, error) {
	if repo == nil || sessions == nil {
		return nil, invalid("run repository and session repository are required")
	}
	service := &RunService{repo: repo, sessions: sessions, clock: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		option(service)
	}
	return service, nil
}

// CreateRunInput 是创建 Run 的入参。
type CreateRunInput struct {
	// SessionID 目标会话，必须属于当前用户且处于活跃状态。
	SessionID uuid.UUID
	// AgentID 可选的 Agent 版本。
	AgentID *uuid.UUID
	// AgentVersion 可选的 Agent 版本号。
	AgentVersion *int
	// Mode 编排模式。
	Mode AgentMode
	// IdempotencyKey 可选的创建幂等键。
	IdempotencyKey string
}

// Create 创建排队中的 Run；会话归属与模式在此校验。
func (s *RunService) Create(ctx context.Context, principal Principal, input CreateRunInput) (Run, error) {
	// Run 创建 Span 关联入站 request_id 与新生成的 run_id，打通请求到下文的 Trace 链路。
	ctx, span := telemetry.Start(ctx, "run.create",
		attribute.String(telemetry.AttrSessionID, input.SessionID.String()),
		attribute.String(telemetry.AttrAgentMode, string(input.Mode)),
	)
	defer span.End()
	if requestID := telemetry.RequestIDFrom(ctx); requestID != "" {
		telemetry.Annotate(span, attribute.String(telemetry.AttrRequestID, requestID))
	}
	if principal.UserID == uuid.Nil {
		telemetry.Fail(span, "forbidden")
		return Run{}, ErrForbidden
	}
	// 创建前先校验每日预算，避免已超限用户继续排队产生费用；Provider 维度在 Worker 调用前校验。
	if s.budgets != nil {
		if _, err := s.budgets.Check(ctx, principal.UserID, input.AgentID, nil); err != nil {
			return Run{}, err
		}
	}
	session, err := s.sessions.GetSession(ctx, principal.UserID, input.SessionID)
	if err != nil {
		return Run{}, err
	}
	if session.Status != SessionActive {
		return Run{}, fmt.Errorf("%w: runs require an active session", ErrResourceConflict)
	}
	if input.Mode != AgentModeReact && input.Mode != AgentModePlan && input.Mode != AgentModeSupervisor {
		return Run{}, invalid("unsupported run mode")
	}
	if (input.AgentID == nil) != (input.AgentVersion == nil) {
		return Run{}, invalid("agent id and version must be provided together")
	}
	if len(input.IdempotencyKey) > 128 {
		return Run{}, invalid("run idempotency key is too long")
	}
	configSnapshot, policySnapshot, err := s.buildSnapshots(ctx, principal, input)
	if err != nil {
		return Run{}, err
	}
	deadline := s.clock().Add(5 * time.Minute)
	run := Run{
		ID: uuid.New(), SessionID: input.SessionID, UserID: principal.UserID,
		AgentID: input.AgentID, AgentVersion: input.AgentVersion, Mode: input.Mode,
		Status: RunQueued, IdempotencyKey: input.IdempotencyKey, DeadlineAt: &deadline,
		ConfigSnapshot: configSnapshot, ModelPolicySnapshot: policySnapshot,
	}
	created, err := s.repo.CreateRun(ctx, run)
	if err != nil {
		return Run{}, err
	}
	// Run 落库后入队；入队使用 Run ID 去重，客户端重试不会产生重复执行。
	if s.enqueuer != nil && created.Status == RunQueued {
		if err := s.enqueuer.EnqueueRun(ctx, created.UserID, created.ID, s.clock()); err != nil {
			telemetry.Fail(span, "enqueue_failed")
			return Run{}, err
		}
	}
	telemetry.Annotate(span, attribute.String(telemetry.AttrRunID, created.ID.String()))
	metrics.IncRunCreated(string(created.Mode))
	return created, nil
}

// buildSnapshots 固化 Agent 配置、策略候选与模型元数据；未配置快照来源时返回空快照。
// Run 创建后策略与模型的后续变更不得影响在途 Run，因此候选必须完整复制到 model_policy_snapshot。
func (s *RunService) buildSnapshots(ctx context.Context, principal Principal, input CreateRunInput) (map[string]any, map[string]any, error) {
	if s.snapshots == nil {
		return nil, nil, nil
	}
	if input.AgentID == nil || input.AgentVersion == nil {
		return nil, nil, invalid("runs require a published agent version")
	}
	agent, err := s.snapshots.GetAgent(ctx, principal.UserID, *input.AgentID)
	if err != nil {
		return nil, nil, err
	}
	if agent.Status != AgentPublished {
		return nil, nil, fmt.Errorf("%w: runs require a published agent", ErrResourceConflict)
	}
	if agent.Version != *input.AgentVersion {
		return nil, nil, invalid("agent version does not match the published snapshot")
	}
	if agent.Mode != input.Mode {
		return nil, nil, invalid("run mode must match the agent mode")
	}
	if agent.ModelPolicyID == nil || agent.ModelPolicyVersion == nil {
		return nil, nil, invalid("published agent is missing its model policy")
	}
	policy, err := s.snapshots.GetPolicy(ctx, *agent.ModelPolicyID)
	if err != nil {
		return nil, nil, err
	}
	if policy.Version != *agent.ModelPolicyVersion || policy.Status != PolicyPublished {
		return nil, nil, fmt.Errorf("%w: agent policy version is not published", ErrResourceConflict)
	}
	modelIDs := make([]uuid.UUID, 0, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		modelIDs = append(modelIDs, candidate.ModelID)
	}
	models, err := s.snapshots.ListModelsByIDs(ctx, modelIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(models) != len(modelIDs) {
		return nil, nil, fmt.Errorf("%w: policy snapshot contains missing models", ErrResourceConflict)
	}
	byID := make(map[uuid.UUID]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	providerCache := make(map[uuid.UUID]Provider)
	candidates := make([]map[string]any, 0, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		model, exists := byID[candidate.ModelID]
		if !exists {
			return nil, nil, fmt.Errorf("%w: candidate model %s", ErrResourceNotFound, candidate.ModelID)
		}
		// 候选切换只能选择已启用、已核验且满足 1M+ 的生成模型。
		if model.Kind != ModelKindChat || model.Status != ModelEnabled || model.VerifiedAt == nil || model.VerificationSource == "" {
			return nil, nil, invalid("candidate %s is not an enabled and verified generation model", model.ID)
		}
		if model.ContextWindowTokens < policy.MinContextTokens {
			return nil, nil, invalid("candidate %s does not satisfy the policy context minimum", model.ID)
		}
		// 创建 Run 前再次校验数据区域约束，避免历史策略快照绕过合规硬过滤。
		if region, regionErr := candidateDataRegion(candidate.Constraints); regionErr != nil {
			return nil, nil, regionErr
		} else if region != "" && model.DataRegion != region {
			return nil, nil, invalid("candidate %s data_region %q does not satisfy constraint %q", model.ID, model.DataRegion, region)
		}
		provider, exists := providerCache[model.ProviderID]
		if !exists {
			provider, err = s.snapshots.GetProvider(ctx, model.ProviderID)
			if err != nil {
				return nil, nil, err
			}
			providerCache[model.ProviderID] = provider
		}
		if provider.Status != ProviderEnabled {
			return nil, nil, invalid("provider %s is disabled", provider.ID)
		}
		candidates = append(candidates, map[string]any{
			"priority": candidate.Priority, "model_id": model.ID.String(), "provider_id": model.ProviderID.String(),
			"provider_base_url": provider.BaseURL, "provider_data_region": provider.DataRegion,
			"remote_model_name": model.RemoteModelName, "context_window_tokens": model.ContextWindowTokens,
			"max_output_tokens": model.MaxOutputTokens, "capabilities": model.Capabilities,
			"input_price_per_million": model.InputPricePerMillion, "output_price_per_million": model.OutputPricePerMillion,
			"data_region": model.DataRegion, "verification_source": model.VerificationSource,
			"verified_at": model.VerifiedAt, "constraints": candidate.Constraints,
		})
	}
	if len(candidates) == 0 {
		return nil, nil, invalid("published policy has no routable candidates")
	}
	configSnapshot := map[string]any{
		"agent": map[string]any{
			"name": agent.Name, "description": agent.Description, "mode": agent.Mode, "version": agent.Version,
		},
		"published_snapshot": agent.ConfigSnapshot,
	}
	policySnapshot := map[string]any{
		"policy_id": policy.ID.String(), "policy_version": policy.Version,
		"min_context_tokens": policy.MinContextTokens, "rules": policy.Rules, "candidates": candidates,
	}
	return configSnapshot, policySnapshot, nil
}

// Get 读取当前用户的 Run。
func (s *RunService) Get(ctx context.Context, principal Principal, runID uuid.UUID) (Run, error) {
	if principal.UserID == uuid.Nil {
		return Run{}, ErrForbidden
	}
	return s.repo.GetRun(ctx, principal.UserID, runID)
}

// List 按会话游标分页列出 Run。
func (s *RunService) List(ctx context.Context, principal Principal, sessionID uuid.UUID, limit int, cursor string) (Page[Run], error) {
	if principal.UserID == uuid.Nil {
		return Page[Run]{}, ErrForbidden
	}
	if _, err := s.sessions.GetSession(ctx, principal.UserID, sessionID); err != nil {
		return Page[Run]{}, err
	}
	return s.repo.ListRuns(ctx, principal.UserID, sessionID, limit, cursor)
}

// Enqueue 把非终态 Run 重新加入执行队列，用于恢复暂停的 Plan 或人工重放。
func (s *RunService) Enqueue(ctx context.Context, principal Principal, runID uuid.UUID) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	run, err := s.repo.GetRun(ctx, principal.UserID, runID)
	if err != nil {
		return err
	}
	if RunTerminal(run.Status) {
		return fmt.Errorf("%w: run is already terminal", ErrResourceConflict)
	}
	if s.enqueuer == nil {
		return fmt.Errorf("%w: run queue is not configured", ErrInvalidConfig)
	}
	return s.enqueuer.EnqueueRun(ctx, principal.UserID, runID, s.clock())
}

// Cancel 取消排队、执行中或等待审批的 Run；终态 Run 返回冲突。
func (s *RunService) Cancel(ctx context.Context, principal Principal, runID uuid.UUID) (Run, error) {
	run, err := s.Get(ctx, principal, runID)
	if err != nil {
		return Run{}, err
	}
	if RunTerminal(run.Status) {
		return Run{}, fmt.Errorf("%w: run is already terminal", ErrResourceConflict)
	}
	if err := TransitionRunStatus(run.Status, RunCancelled); err != nil {
		return Run{}, err
	}
	now := s.clock()
	cancelled, err := s.repo.TransitionRun(ctx, principal.UserID, runID, run.Status, RunCancelled, map[string]any{"finished_at": now})
	if err != nil {
		return Run{}, err
	}
	metrics.IncRunStatus(string(run.Mode), string(RunCancelled))
	return cancelled, nil
}

// ListSteps 读取 Run 的执行步骤；调用方须先通过 GetRun 校验归属。
func (s *RunService) ListSteps(ctx context.Context, principal Principal, runID uuid.UUID, limit int) ([]ExecutionStep, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if s.steps == nil {
		return []ExecutionStep{}, nil
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	return s.steps.ListSteps(ctx, principal.UserID, runID, limit)
}

// ListModelCalls 读取 Run 的模型 Attempt 记录；未配置仓储时返回空列表。
func (s *RunService) ListModelCalls(ctx context.Context, principal Principal, runID uuid.UUID) ([]ModelCall, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	if s.calls == nil {
		return []ModelCall{}, nil
	}
	return s.calls.ListModelCalls(ctx, principal.UserID, runID, 200)
}

// ListSupervisorTasks 读取 Run 的 Supervisor 子任务；未配置仓储时返回空列表。
func (s *RunService) ListSupervisorTasks(ctx context.Context, principal Principal, runID uuid.UUID) ([]SupervisorTask, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	if s.supervisorTasks == nil {
		return []SupervisorTask{}, nil
	}
	return s.supervisorTasks.ListTasks(ctx, principal.UserID, runID)
}

// RecordPayloadAccess 记录载荷查阅审计事件：理由必填并写入 Run 事件流，便于事后追责。
func (s *RunService) RecordPayloadAccess(ctx context.Context, principal Principal, runID uuid.UUID, stepID *uuid.UUID, reason string) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 4 || len(reason) > 500 {
		return fmt.Errorf("%w: payload access reason must contain 4-500 characters", ErrInvalidConfig)
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return err
	}
	payload := map[string]any{"actor_user_id": principal.UserID.String(), "reason": reason}
	if stepID != nil {
		payload["step_id"] = stepID.String()
	}
	_, err := s.repo.AppendEvent(ctx, RunEvent{
		RunID: runID, UserID: principal.UserID, EventType: "audit.payload.viewed", Payload: payload,
	})
	return err
}

// BreakGlassResult 是一次管理员特权读取的完整结果。
// 结果只包含脱敏执行记录，不包含凭证、Prompt/Completion 原文或隐藏推理。
type BreakGlassResult struct {
	// Run 被读取的 Run。
	Run Run
	// Steps 执行步骤。
	Steps []ExecutionStep
	// Payloads 脱敏执行载荷。
	Payloads []ExecutionPayload
	// ModelCalls 模型尝试事实。
	ModelCalls []ModelCall
}

// BreakGlassRead 允许管理员在提供书面理由后读取指定用户的执行记录，并强制写入审计事件。
// 关键约束：理由必填；审计写入失败时拒绝返回数据，避免出现无审计的特权读取。
func (s *RunService) BreakGlassRead(
	ctx context.Context, principal Principal, targetUserID, runID uuid.UUID, reason string,
) (BreakGlassResult, error) {
	if principal.Role != RoleAdmin || principal.UserID == uuid.Nil || targetUserID == uuid.Nil || runID == uuid.Nil {
		return BreakGlassResult{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 4 || len(reason) > 500 {
		return BreakGlassResult{}, invalid("break-glass reason must contain 4-500 characters")
	}
	// 读取使用目标用户作用域，确保 RLS 只放行被授权查看的该用户数据。
	run, err := s.repo.GetRun(ctx, targetUserID, runID)
	if err != nil {
		return BreakGlassResult{}, err
	}
	steps := []ExecutionStep{}
	if s.steps != nil {
		steps, err = s.steps.ListSteps(ctx, targetUserID, runID, 500)
		if err != nil {
			return BreakGlassResult{}, err
		}
	}
	payloads := []ExecutionPayload{}
	if s.steps != nil {
		payloads, err = s.steps.ListPayloads(ctx, targetUserID, runID, nil, 500)
		if err != nil {
			return BreakGlassResult{}, err
		}
	}
	calls := []ModelCall{}
	if s.calls != nil {
		calls, err = s.calls.ListModelCalls(ctx, targetUserID, runID, 200)
		if err != nil {
			return BreakGlassResult{}, err
		}
	}
	// 审计事件写入目标用户 Run 事件流，记录真实操作者与理由；失败时 fail-closed。
	if _, err := s.repo.AppendEvent(ctx, RunEvent{
		RunID: runID, UserID: targetUserID, EventType: "audit.break_glass.read",
		Payload: map[string]any{
			"actor_user_id": principal.UserID.String(), "target_user_id": targetUserID.String(),
			"reason": reason, "step_count": len(steps), "payload_count": len(payloads),
		},
	}); err != nil {
		return BreakGlassResult{}, err
	}
	return BreakGlassResult{Run: run, Steps: steps, Payloads: payloads, ModelCalls: calls}, nil
}

// ListPayloads 读取 Run 或指定步骤的脱敏执行载荷；默认不返回正文以外的敏感内容。
func (s *RunService) ListPayloads(ctx context.Context, principal Principal, runID uuid.UUID, stepID *uuid.UUID, limit int) ([]ExecutionPayload, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if s.steps == nil {
		return []ExecutionPayload{}, nil
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	return s.steps.ListPayloads(ctx, principal.UserID, runID, stepID, limit)
}

// ListEvents 读取 afterSeq 之后的事件，调用方须先通过 GetRun 校验归属。
func (s *RunService) ListEvents(ctx context.Context, principal Principal, runID uuid.UUID, afterSeq int64, limit int) ([]RunEvent, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		afterSeq = 0
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return s.repo.ListEvents(ctx, principal.UserID, runID, afterSeq, limit)
}

// ListApprovals 读取当前用户 Run 的工具审批记录；未配置审批仓储时返回空列表。
func (s *RunService) ListApprovals(ctx context.Context, principal Principal, runID uuid.UUID) ([]ToolApproval, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	if _, err := s.repo.GetRun(ctx, principal.UserID, runID); err != nil {
		return nil, err
	}
	if s.approvals == nil {
		return []ToolApproval{}, nil
	}
	return s.approvals.ListApprovals(ctx, principal.UserID, runID)
}

// DecideApproval 对高风险工具审批做出批准或拒绝决策。
// 同一决策重复提交幂等返回；全部审批决策完成后恢复 Run 并为 Worker 重新入队。
func (s *RunService) DecideApproval(ctx context.Context, principal Principal, runID, approvalID uuid.UUID, rawDecision string) (ToolApproval, error) {
	if principal.UserID == uuid.Nil {
		return ToolApproval{}, ErrForbidden
	}
	if s.approvals == nil {
		return ToolApproval{}, fmt.Errorf("%w: tool approvals are not configured", ErrInvalidConfig)
	}
	decision, err := ParseApprovalDecision(rawDecision)
	if err != nil {
		return ToolApproval{}, err
	}
	run, err := s.repo.GetRun(ctx, principal.UserID, runID)
	if err != nil {
		return ToolApproval{}, err
	}
	// 只有运行中或等待审批的 Run 可以接受决策；终态与对账态必须拒绝。
	if run.Status != RunRunning && run.Status != RunWaitingApproval {
		return ToolApproval{}, fmt.Errorf("%w: run is not accepting approval decisions", ErrResourceConflict)
	}
	approval, err := s.approvals.GetApproval(ctx, principal.UserID, approvalID)
	if err != nil {
		return ToolApproval{}, err
	}
	if approval.RunID != runID {
		return ToolApproval{}, fmt.Errorf("%w: approval does not belong to the run", ErrResourceConflict)
	}
	updated, err := s.approvals.DecideApproval(ctx, principal.UserID, approvalID, decision, principal.UserID, s.clock())
	if err != nil {
		return ToolApproval{}, err
	}
	// 决策事件与审批状态分开提交：即使事件写入失败，决策结果仍可查询且不会丢失。
	if _, err := s.repo.AppendEvent(ctx, RunEvent{
		RunID: runID, UserID: principal.UserID, EventType: "tool.approval.decided",
		Payload: map[string]any{
			"approval_id": approvalID.String(), "tool": updated.ToolName,
			"tool_call_id": updated.ToolCallID, "decision": string(decision), "status": string(updated.Status),
		},
	}); err != nil {
		return ToolApproval{}, err
	}
	pending, err := s.approvals.CountPendingApprovals(ctx, principal.UserID, runID)
	if err != nil {
		return ToolApproval{}, err
	}
	if pending > 0 {
		return updated, nil
	}
	// 全部决策完成：把等待审批的 Run 推回 running；已由 Worker 恢复的 Run 保持原状态。
	if run.Status == RunWaitingApproval {
		if _, err := s.repo.ResumeRun(ctx, principal.UserID, runID, s.clock()); err != nil && !errors.Is(err, ErrResourceConflict) {
			return ToolApproval{}, err
		}
	}
	// 唤醒任务只是加速恢复；任务已由 Worker 退回队列，入队失败不影响已提交的决策。
	if s.enqueuer != nil {
		_ = s.enqueuer.EnqueueRun(ctx, principal.UserID, runID, s.clock())
	}
	return updated, nil
}
