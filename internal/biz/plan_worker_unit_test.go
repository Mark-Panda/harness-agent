package biz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// stubRunRepository 只实现 Plan 执行器使用的事件追加方法。
type stubRunRepository struct {
	RunRepository
}

// AppendEventFenced 返回成功事件。
func (stubRunRepository) AppendEventFenced(_ context.Context, event RunEvent, _ RunFence) (RunEvent, error) {
	return event, nil
}

// SetRunAttempt 更新尝试序号；测试中无需持久化。
func (stubRunRepository) SetRunAttempt(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int, _ RunFence) error {
	return nil
}

// ListEvents 默认没有历史事件；需要事件的场景由 eventRunRepository 覆盖。
func (stubRunRepository) ListEvents(_ context.Context, _, _ uuid.UUID, _ int64, _ int) ([]RunEvent, error) {
	return nil, nil
}

// stubSessionRepository 只实现最终消息幂等落库。
type stubSessionRepository struct {
	SessionRepository
}

// AppendMessageIdempotent 返回输入消息。
func (stubSessionRepository) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent) (Message, error) {
	return message, nil
}

// statefulSessionRepository 模拟数据库按消息 ID 幂等返回原记录，用于完成恢复测试。
type statefulSessionRepository struct {
	stubSessionRepository
	messages map[uuid.UUID]Message
}

// AppendMessageIdempotent 已存在时返回首次保存的内容，模拟 ON CONFLICT DO NOTHING。
func (r *statefulSessionRepository) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent) (Message, error) {
	if r.messages == nil {
		r.messages = map[uuid.UUID]Message{}
	}
	if existing, ok := r.messages[message.ID]; ok {
		return existing, nil
	}
	r.messages[message.ID] = message
	return message, nil
}

// stubJobQueue 仅用于满足构造函数依赖。
type stubJobQueue struct {
	JobQueue
}

// stubProviders 仅用于满足构造函数依赖。
type stubProviders struct {
	ProviderCredentialRepository
}

// stubOpener 仅用于满足构造函数依赖。
type stubOpener struct {
	CredentialOpener
}

// memoryPlanRepo 是内存计划仓储，用于覆盖 Plan 执行器的成功、失败与暂停路径。
type memoryPlanRepo struct {
	mu    sync.Mutex
	plans map[uuid.UUID]Plan
}

// newMemoryPlanRepo 构造内存计划仓储。
func newMemoryPlanRepo() *memoryPlanRepo {
	return &memoryPlanRepo{plans: map[uuid.UUID]Plan{}}
}

// seedPlan 预置计划，模拟暂停或超时等恢复场景。
func (r *memoryPlanRepo) seedPlan(plan Plan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans[plan.ID] = clonePlan(plan)
}

// CreatePlan 保存计划与步骤。
func (r *memoryPlanRepo) CreatePlan(_ context.Context, userID uuid.UUID, plan Plan, _ OutboxEvent) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan.UserID = userID
	r.plans[plan.ID] = clonePlan(plan)
	return clonePlan(plan), nil
}

// GetPlanByRun 返回 Run 的最新版本计划。
func (r *memoryPlanRepo) GetPlanByRun(_ context.Context, _ uuid.UUID, runID uuid.UUID) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest Plan
	found := false
	for _, plan := range r.plans {
		if plan.RunID != runID {
			continue
		}
		if !found || plan.Version > latest.Version {
			latest = plan
			found = true
		}
	}
	if !found {
		return Plan{}, ErrResourceNotFound
	}
	return clonePlan(latest), nil
}

// GetPlan 按 ID 读取计划。
func (r *memoryPlanRepo) GetPlan(_ context.Context, _ uuid.UUID, planID uuid.UUID) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return Plan{}, ErrResourceNotFound
	}
	return clonePlan(plan), nil
}

// ListPlanVersions 返回 Run 的全部计划版本。
func (r *memoryPlanRepo) ListPlanVersions(_ context.Context, _ uuid.UUID, runID uuid.UUID) ([]Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var versions []Plan
	for _, plan := range r.plans {
		if plan.RunID == runID {
			versions = append(versions, clonePlan(plan))
		}
	}
	return versions, nil
}

// UpdatePlan 以 revision 与状态为前置条件推进计划。
func (r *memoryPlanRepo) UpdatePlan(_ context.Context, _ uuid.UUID, planID uuid.UUID, from, to PlanStatus, expectedRevision int64, _ PlanFence, update PlanUpdate) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return Plan{}, ErrResourceNotFound
	}
	if plan.Status != from || plan.Revision != expectedRevision {
		return Plan{}, ErrPlanRevisionConflict
	}
	if err := TransitionPlanStatus(from, to); err != nil {
		return Plan{}, err
	}
	plan.Status = to
	plan.Revision++
	plan.ErrorCode = update.ErrorCode
	plan.ErrorMessage = update.ErrorMessage
	plan.TotalInputTokens += update.InputTokensDelta
	plan.TotalOutputTokens += update.OutputTokensDelta
	plan.EstimatedCost += update.EstimatedCostDelta
	if update.FinishedAt != nil {
		plan.FinishedAt = update.FinishedAt
	}
	r.plans[planID] = plan
	return clonePlan(plan), nil
}

// AddPlanUsage 累加计划用量。
func (r *memoryPlanRepo) AddPlanUsage(_ context.Context, _ uuid.UUID, planID uuid.UUID, inputTokens, outputTokens int64, cost float64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return ErrResourceNotFound
	}
	plan.TotalInputTokens += inputTokens
	plan.TotalOutputTokens += outputTokens
	plan.EstimatedCost += cost
	plan.Revision++
	r.plans[planID] = plan
	return nil
}

// ClaimPlanStep 领取步骤并写入 running 与 attempts。
func (r *memoryPlanRepo) ClaimPlanStep(_ context.Context, _ uuid.UUID, planID, stepID uuid.UUID, executionStepID *uuid.UUID, _ PlanFence, _ time.Time) (PlanStep, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return PlanStep{}, ErrResourceNotFound
	}
	for index := range plan.Steps {
		step := plan.Steps[index]
		if step.ID != stepID {
			continue
		}
		if (step.Status != PlanStepPending && step.Status != PlanStepFailed) || step.Attempts >= step.MaxAttempts {
			return PlanStep{}, ErrResourceConflict
		}
		step.Status = PlanStepRunning
		step.Attempts++
		step.ExecutionStepID = executionStepID
		plan.Steps[index] = step
		r.plans[planID] = plan
		return step, nil
	}
	return PlanStep{}, ErrResourceNotFound
}

// UpdatePlanStep 推进 running 步骤。
func (r *memoryPlanRepo) UpdatePlanStep(_ context.Context, _ uuid.UUID, planID, stepID uuid.UUID, _ PlanFence, update PlanStepUpdate) (PlanStep, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return PlanStep{}, ErrResourceNotFound
	}
	for index := range plan.Steps {
		step := plan.Steps[index]
		if step.ID != stepID || step.Status != PlanStepRunning {
			continue
		}
		step.Status = update.Status
		step.Result = update.Result
		step.ErrorCode = update.ErrorCode
		step.ErrorMessage = update.ErrorMessage
		step.FinishedAt = update.FinishedAt
		plan.Steps[index] = step
		r.plans[planID] = plan
		return step, nil
	}
	return PlanStep{}, ErrResourceConflict
}

// ResolvePlanStep 以 from 状态推进等待审批或待执行步骤。
func (r *memoryPlanRepo) ResolvePlanStep(_ context.Context, _ uuid.UUID, planID, stepID uuid.UUID, from PlanStepStatus, update PlanStepUpdate) (PlanStep, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok {
		return PlanStep{}, ErrResourceNotFound
	}
	for index := range plan.Steps {
		step := plan.Steps[index]
		if step.ID != stepID || step.Status != from {
			continue
		}
		step.Status = update.Status
		step.Result = update.Result
		step.ErrorCode = update.ErrorCode
		step.ErrorMessage = update.ErrorMessage
		step.FinishedAt = update.FinishedAt
		plan.Steps[index] = step
		r.plans[planID] = plan
		return step, nil
	}
	return PlanStep{}, ErrResourceConflict
}

// FindPausedPlanBySession 返回会话中最近一个暂停中的计划。
func (r *memoryPlanRepo) FindPausedPlanBySession(_ context.Context, userID, sessionID uuid.UUID) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest Plan
	found := false
	for _, plan := range r.plans {
		if plan.UserID != userID || plan.SessionID != sessionID || plan.Status != PlanPaused {
			continue
		}
		if !found || plan.CreatedAt.After(latest.CreatedAt) {
			latest = plan
			found = true
		}
	}
	if !found {
		return Plan{}, ErrResourceNotFound
	}
	return clonePlan(latest), nil
}

// RebindPlanRun 把暂停中的计划迁移到新的 Run。
func (r *memoryPlanRepo) RebindPlanRun(_ context.Context, _ uuid.UUID, planID, runID uuid.UUID, expectedRevision int64, deadlineAt *time.Time, _ time.Time) (Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[planID]
	if !ok || plan.Status != PlanPaused || plan.Revision != expectedRevision {
		return Plan{}, ErrPlanRevisionConflict
	}
	plan.RunID = runID
	plan.Status = PlanExecuting
	plan.DeadlineAt = deadlineAt
	plan.Revision++
	r.plans[planID] = plan
	return clonePlan(plan), nil
}

// planRiskTool 是测试用工具，只提供名称与风险等级。
type planRiskTool struct {
	name string
	risk string
}

func (t planRiskTool) Spec() react.ToolSpec { return react.ToolSpec{Name: t.name, Description: t.name} }
func (t planRiskTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (t planRiskTool) RiskLevel() string { return t.risk }

// planRiskRegistry 是测试用工具注册表。
type planRiskRegistry struct {
	tools []planRiskTool
}

func (r planRiskRegistry) Lookup(name string) (react.Tool, bool) {
	for _, tool := range r.tools {
		if tool.name == name {
			return tool, true
		}
	}
	return nil, false
}

func (r planRiskRegistry) List() []react.ToolSpec {
	specs := make([]react.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

// TestValidatePlanDraftTools 验证规划引用的工具必须存在且为低风险。
func TestValidatePlanDraftTools(t *testing.T) {
	registry := planRiskRegistry{tools: []planRiskTool{
		{name: "search_docs", risk: "low"},
		{name: "delete_all", risk: "high"},
	}}
	valid := PlanDraft{Steps: []PlanDraftStep{{StepNo: 1, ToolName: "search_docs"}, {StepNo: 2}}}
	if err := validatePlanDraftTools(valid, registry); err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}
	invalid := PlanDraft{Steps: []PlanDraftStep{
		{StepNo: 1, ToolName: "search"},
		{StepNo: 2, ToolName: "delete_all"},
	}}
	err := validatePlanDraftTools(invalid, registry)
	if err == nil {
		t.Fatal("invalid draft was accepted")
	}
	if !strings.Contains(err.Error(), `"search"`) || !strings.Contains(err.Error(), "delete_all") {
		t.Fatalf("validation error missing details: %v", err)
	}
	if !strings.Contains(err.Error(), "search_docs") {
		t.Fatalf("validation error should suggest the real tool name: %v", err)
	}
}

// clonePlan 深拷贝计划，避免测试断言受后续写入影响。
func clonePlan(plan Plan) Plan {
	steps := make([]PlanStep, len(plan.Steps))
	copy(steps, plan.Steps)
	plan.Steps = steps
	return plan
}

// noopAttemptRepository 是模型调用事实仓储空实现。
type noopAttemptRepository struct{}

// CreateModelCall 返回输入。
func (noopAttemptRepository) CreateModelCall(_ context.Context, call ModelCall) (ModelCall, error) {
	return call, nil
}

// FinishModelCall 返回空结果。
func (noopAttemptRepository) FinishModelCall(_ context.Context, _, _ uuid.UUID, _ ModelCallUpdate) (ModelCall, error) {
	return ModelCall{}, nil
}

// ListModelCalls 返回空列表。
func (noopAttemptRepository) ListModelCalls(_ context.Context, _, _ uuid.UUID, _ int) ([]ModelCall, error) {
	return nil, nil
}

// planWorkerFixture 构造可直接调用 executePlan 的 Worker 与测试服务器。
func planWorkerFixture(t *testing.T, handler func(body string, w http.ResponseWriter)) (*ExecutionWorker, *memoryPlanRepo, *model.Router, *httptest.Server) {
	t.Helper()
	runs := stubRunRepository{}
	sessions := stubSessionRepository{}
	jobs := stubJobQueue{}
	providers := stubProviders{}
	opener := stubOpener{}
	plans := newMemoryPlanRepo()
	worker, err := NewExecutionWorker(ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: providers, Opener: opener,
		Attempts: noopAttemptRepository{}, Plans: plans,
	}, WorkerConfig{
		WorkerID: "plan-worker", Lease: time.Minute, MCPTimeout: time.Second,
		Routing: model.RouterConfig{MinContextTokens: 1, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 100, MaxSwitchesPerRun: 10, MaxOutputTokens: 64, RequestTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		handler(string(raw), w)
	}))
	t.Cleanup(server.Close)
	router, err := model.NewRouter([]model.Candidate{{
		Priority: 1, BaseURL: server.URL, APIKey: "test-key",
		Ref: model.ModelRef{ProviderID: "provider", ModelID: "model", RemoteModelName: "test", ContextWindowTokens: 1_000_000, MaxOutputTokens: 64},
	}}, nil, model.RouterConfig{
		MinContextTokens: 1, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 100, MaxSwitchesPerRun: 10,
		MaxOutputTokens: 64, RequestTimeout: 5 * time.Second, BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return worker, plans, router, server
}

// writeCompletion 返回 OpenAI-compatible 响应。
func writeCompletion(t *testing.T, w http.ResponseWriter, content string, status int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, _ := json.Marshal(map[string]any{
		"id": "req", "choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3},
	})
	_, _ = w.Write(encoded)
}

func TestExecutePlanSuccess(t *testing.T) {
	planner := `{"goal":"整理数据","steps":[{"step_no":1,"title":"收集","description":"收集数据","depends_on":[],"tool_name":"","arguments":{}},{"step_no":2,"title":"总结","description":"输出总结","depends_on":[1],"tool_name":"","arguments":{}}]}`
	worker, plans, router, _ := planWorkerFixture(t, func(body string, w http.ResponseWriter) {
		content := "步骤结果"
		if strings.Contains(body, "生成可执行计划") {
			content = planner
		}
		// 计划完成后会额外调用一次汇总模型，返回面向用户的最终答案。
		if strings.Contains(body, "最终汇总器") {
			content = "最终汇总"
		}
		writeCompletion(t, w, content, http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	messages := []react.Message{{Role: react.RoleSystem, Content: "system"}, {Role: react.RoleUser, Content: "整理数据"}}
	result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, messages, emptyToolRegistry{})
	if err != nil {
		plan, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
		t.Logf("plan after failure: %#v", plan)
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "最终汇总") {
		t.Fatalf("final content = %q", result.Content)
	}
	plan, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if plan.Status != PlanCompleted || len(plan.Steps) != 2 || plan.Steps[1].Status != PlanStepCompleted {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestExecutePlanInvalidPlannerOutput(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "不是 JSON", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("invalid planner error = %v", err)
	}
	if _, err := plans.GetPlanByRun(context.Background(), run.UserID, run.ID); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("invalid plan was persisted: %v", err)
	}
}

func TestExecutePlanPaused(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(body string, w http.ResponseWriter) {
		writeCompletion(t, w, `{"goal":"g","steps":[{"step_no":1,"title":"s"}]}`, http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanPaused, Goal: "g", MaxSteps: 50,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanPaused) {
		t.Fatalf("paused plan error = %v", err)
	}
}

func TestExecutePlanModelStepFailure(t *testing.T) {
	planner := `{"goal":"g","steps":[{"step_no":1,"title":"s","description":"d"}]}`
	worker, _, router, _ := planWorkerFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "生成可执行计划") {
			writeCompletion(t, w, planner, http.StatusOK)
			return
		}
		writeCompletion(t, w, "失败", http.StatusInternalServerError)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	_, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{})
	if !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("step failure error = %v", err)
	}
}

func TestExecutePlanDeadlineExceeded(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, `{}`, http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	deadline := time.Now().Add(-time.Minute)
	plans.seedPlan(Plan{
		ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Revision: 0, DeadlineAt: &deadline,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router,
		[]react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("deadline error = %v", err)
	}
}

// fakePlanTool 是计划步骤测试用工具。
type fakePlanTool struct {
	risk   string
	output string
	err    error
}

// Spec 返回工具定义。
func (t fakePlanTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: "tool", Description: "test", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// Invoke 返回预置结果。
func (t fakePlanTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if t.err != nil {
		return nil, t.err
	}
	return json.RawMessage(t.output), nil
}

// RiskLevel 返回预置风险等级。
func (t fakePlanTool) RiskLevel() string { return t.risk }

// fakePlanRegistry 只包含一个工具。
type fakePlanRegistry struct {
	tool react.Tool
}

// Lookup 按名称返回工具。
func (r fakePlanRegistry) Lookup(name string) (react.Tool, bool) {
	if name == "tool" {
		return r.tool, true
	}
	return nil, false
}

// List 返回工具列表。
func (r fakePlanRegistry) List() []react.ToolSpec {
	if r.tool == nil {
		return nil
	}
	return []react.ToolSpec{r.tool.Spec()}
}

// fakePlanApprovals 是内存审批仓储，覆盖计划审批分支。
type fakePlanApprovals struct {
	ToolApprovalRepository
	mu    sync.Mutex
	items []ToolApproval
}

// CreateApproval 生成审批记录。
func (r *fakePlanApprovals) CreateApproval(_ context.Context, approval ToolApproval) (ToolApproval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	approval.ID = uuid.New()
	r.items = append(r.items, approval)
	return approval, nil
}

// ListApprovals 返回全部审批。
func (r *fakePlanApprovals) ListApprovals(_ context.Context, _, _ uuid.UUID) ([]ToolApproval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]ToolApproval, len(r.items))
	copy(result, r.items)
	return result, nil
}

// planToolFixture 构造工具步骤测试所需的 Worker 与计划。
func planToolFixture(t *testing.T, tool react.Tool, approvals ToolApprovalRepository) (*ExecutionWorker, *memoryPlanRepo, Plan, PlanStep) {
	t.Helper()
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	worker.approvals = approvals
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	step := PlanStep{
		ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "调用工具",
		ToolName: "tool", Arguments: map[string]any{"x": 1}, Status: PlanStepPending,
		MaxAttempts: 1, IdempotencyKey: "plan:test:step:1",
	}
	plan := Plan{
		ID: step.PlanID, UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Steps: []PlanStep{step},
	}
	plans.seedPlan(plan)
	return worker, plans, plan, step
}

func TestExecutePlanToolStepSuccess(t *testing.T) {
	worker, plans, plan, step := planToolFixture(t, fakePlanTool{risk: "low", output: `{"ok":true}`}, nil)
	if err := worker.executePlanToolStep(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1},
		PlanFence{Owner: "plan-worker", Token: 1}, fakePlanRegistry{tool: fakePlanTool{risk: "low", output: `{"ok":true}`}}, plan, step); err != nil {
		t.Fatal(err)
	}
	stored, err := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if err != nil || stored.Steps[0].Status != PlanStepCompleted || stored.Steps[0].Result["content"] != `{"ok":true}` {
		t.Fatalf("tool step = %#v, %v", stored.Steps[0], err)
	}
}

func TestExecutePlanToolStepFailure(t *testing.T) {
	worker, plans, plan, step := planToolFixture(t, fakePlanTool{risk: "low", err: errors.New("tool boom")}, nil)
	registry := fakePlanRegistry{tool: fakePlanTool{risk: "low", err: errors.New("tool boom")}}
	if err := worker.executePlanToolStep(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1},
		PlanFence{Owner: "plan-worker", Token: 1}, registry, plan, step); err != nil {
		t.Fatal(err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Steps[0].Status != PlanStepFailed || stored.Steps[0].ErrorCode == "" {
		t.Fatalf("failed tool step = %#v", stored.Steps[0])
	}
}

func TestExecutePlanToolStepApproval(t *testing.T) {
	approvals := &fakePlanApprovals{}
	worker, plans, plan, step := planToolFixture(t, fakePlanTool{risk: "high", output: "{}"}, approvals)
	err := worker.executePlanToolStep(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1},
		PlanFence{Owner: "plan-worker", Token: 1}, fakePlanRegistry{tool: fakePlanTool{risk: "high"}}, plan, step)
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("approval step error = %v", err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Status != PlanWaitingApproval || stored.Steps[0].Status != PlanStepWaitingApproval {
		t.Fatalf("waiting plan = %#v", stored)
	}
	if len(approvals.items) != 1 || approvals.items[0].Status != ApprovalPending {
		t.Fatalf("approval = %#v", approvals.items)
	}
}

func TestResolvePlanApprovals(t *testing.T) {
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	approvals := &fakePlanApprovals{}
	worker.approvals = approvals
	execStepID := uuid.New()
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	step := PlanStep{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "s",
		Status: PlanStepWaitingApproval, ExecutionStepID: &execStepID, MaxAttempts: 1}
	plan := Plan{ID: step.PlanID, UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanWaitingApproval, Goal: "g", MaxSteps: 50, Steps: []PlanStep{step}}
	plans.seedPlan(plan)

	approvals.items = []ToolApproval{{ID: uuid.New(), RunID: run.ID, StepID: &execStepID, UserID: run.UserID,
		Status: ApprovalPending, ToolName: "tool"}}
	updated, pending, err := worker.resolvePlanApprovals(context.Background(), run, plan, PlanFence{Owner: "plan-worker", Token: 1})
	if err != nil || !pending || updated.Status != PlanWaitingApproval {
		t.Fatalf("pending approvals = %#v pending=%t err=%v", updated, pending, err)
	}

	approvals.items[0].Status = ApprovalExecuted
	approvals.items[0].ResultJSON = json.RawMessage(`{"ok":true}`)
	updated, pending, err = worker.resolvePlanApprovals(context.Background(), run, plan, PlanFence{Owner: "plan-worker", Token: 1})
	if err != nil || pending || updated.Status != PlanExecuting || updated.Steps[0].Status != PlanStepCompleted {
		t.Fatalf("executed approvals = %#v pending=%t err=%v", updated, pending, err)
	}

	approvals.items[0].Status = ApprovalDenied
	plans.seedPlan(plan)
	updated, _, err = worker.resolvePlanApprovals(context.Background(), run, plan, PlanFence{Owner: "plan-worker", Token: 1})
	if err != nil || updated.Steps[0].Status != PlanStepFailed {
		t.Fatalf("denied approval = %#v, %v", updated.Steps[0], err)
	}
}

func TestRecoverPlanSteps(t *testing.T) {
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Revision: 0,
		Steps: []PlanStep{
			{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "tool", ToolName: "tool", Status: PlanStepRunning, MaxAttempts: 1},
			{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 2, Title: "model", Status: PlanStepPending, MaxAttempts: 2},
		}}
	plan.Steps[0].PlanID = plan.ID
	plan.Steps[1].PlanID = plan.ID
	plans.seedPlan(plan)
	if _, err := worker.recoverPlanSteps(context.Background(), plan, RunFence{Owner: "plan-worker", Token: 1}); !errors.Is(err, ErrPlanStepUnknown) {
		t.Fatalf("recover tool step error = %v", err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Status != PlanFailed || stored.Steps[0].Status != PlanStepUnknown {
		t.Fatalf("recovered plan = %#v", stored)
	}
}

func TestFinishPlanAndFinalResult(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "汇总答案", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Revision: 0,
		Steps: []PlanStep{{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "s",
			Status: PlanStepCompleted, Result: map[string]any{"content": "结果"}}}}
	plan.Steps[0].PlanID = plan.ID
	plans.seedPlan(plan)
	messages := []react.Message{{Role: react.RoleUser, Content: "任务"}}
	result, err := worker.finishPlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, plan, messages)
	if err != nil || result.Content != "汇总答案" || len(result.Messages) != 2 {
		t.Fatalf("finishPlan() = %#v, %v", result, err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if stored.Status != PlanCompleted || stored.FinishedAt == nil {
		t.Fatalf("completed plan = %#v", stored)
	}
	final, err := worker.planFinalResult(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, stored, messages)
	if err != nil || !strings.Contains(final.Content, "结果") {
		t.Fatalf("planFinalResult() = %#v, %v", final, err)
	}
}

func TestPlanFinalResultRecoveryKeepsStoredSummary(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "模型汇总", http.StatusOK)
	})
	sessions := &statefulSessionRepository{}
	worker.sessions = sessions
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepCompleted, Result: map[string]any{"content": "步骤结果"}}}}
	plans.seedPlan(plan)
	messages := []react.Message{{Role: react.RoleUser, Content: "任务"}}
	result, err := worker.finishPlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, plan, messages)
	if err != nil || result.Content != "模型汇总" {
		t.Fatalf("finishPlan() = %#v, %v", result, err)
	}
	if result.TotalTokens != 8 {
		t.Fatalf("aggregator usage was not recorded: %d", result.TotalTokens)
	}
	stored, _ := plans.GetPlan(context.Background(), run.UserID, plan.ID)
	recovered, err := worker.planFinalResult(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, stored, result.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Content != "模型汇总" || len(recovered.Messages) != len(result.Messages) || len(sessions.messages) != 1 {
		t.Fatalf("recovery duplicated or replaced final answer: %#v, stored=%d", recovered, len(sessions.messages))
	}
}

func TestFailPlanSkipsPendingSteps(t *testing.T) {
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanExecuting, Goal: "g", MaxSteps: 50, Revision: 0,
		Steps: []PlanStep{
			{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "failed", Status: PlanStepFailed, Attempts: 1, MaxAttempts: 1},
			{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 2, Title: "pending", Status: PlanStepPending, DependsOn: []int{1}, MaxAttempts: 2},
		}}
	plan.Steps[0].PlanID = plan.ID
	plan.Steps[1].PlanID = plan.ID
	plans.seedPlan(plan)
	err := worker.failPlan(context.Background(), plan, PlanFence{Owner: "plan-worker", Token: 1}, "step_failed", "步骤失败")
	if !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("failPlan error = %v", err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), run.UserID, run.ID)
	if stored.Status != PlanFailed || stored.Steps[1].Status != PlanStepSkipped {
		t.Fatalf("failed plan = %#v", stored)
	}
}

func TestResolvePlanApprovalsMissingAndExecuting(t *testing.T) {
	worker, plans, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	approvals := &fakePlanApprovals{}
	worker.approvals = approvals
	execStepID := uuid.New()
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	baseStep := PlanStep{ID: uuid.New(), PlanID: uuid.New(), UserID: run.UserID, StepNo: 1, Title: "s",
		Status: PlanStepWaitingApproval, ExecutionStepID: &execStepID, MaxAttempts: 1}
	plan := Plan{ID: baseStep.PlanID, UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanWaitingApproval, Goal: "g", MaxSteps: 50, Steps: []PlanStep{baseStep}}
	plans.seedPlan(plan)

	updated, _, err := worker.resolvePlanApprovals(context.Background(), run, plan, PlanFence{Owner: "plan-worker", Token: 1})
	if err != nil || updated.Steps[0].Status != PlanStepFailed || updated.Steps[0].ErrorCode != "approval_missing" {
		t.Fatalf("missing approval = %#v, %v", updated.Steps[0], err)
	}

	plans.seedPlan(plan)
	approvals.items = []ToolApproval{{ID: uuid.New(), RunID: run.ID, StepID: &execStepID, UserID: run.UserID, Status: ApprovalExecuting}}
	if _, _, err := worker.resolvePlanApprovals(context.Background(), run, plan, PlanFence{Owner: "plan-worker", Token: 1}); !errors.Is(err, ErrApprovalExecutionUnknown) {
		t.Fatalf("executing approval error = %v", err)
	}
}

func TestPlanTextTruncationKeepsUTF8(t *testing.T) {
	background := planConversationBackground([]react.Message{{Role: react.RoleUser, Content: strings.Repeat("中文", 1000)}})
	if !utf8.ValidString(background) {
		t.Fatal("plan background contains invalid UTF-8")
	}
	payload, _ := truncatePayloadText(strings.Repeat("中文", 1000), 7)
	if !utf8.ValidString(payload) {
		t.Fatal("payload truncation contains invalid UTF-8")
	}
}

func TestPlanStepMessageIDsUseStepNamespace(t *testing.T) {
	firstStep, secondStep := uuid.New(), uuid.New()
	ids := make([]string, 0, 2)
	for _, stepID := range []uuid.UUID{firstStep, secondStep} {
		scopePlanStepMessageEvents(stepID, func(event react.StepEvent) {
			ids = append(ids, event.Payload["message_id"].(string))
		})(react.StepEvent{Type: "message.created", Payload: map[string]any{"index": 4, "role": "assistant", "content": "结果"}})
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("plan step message IDs collide: %#v", ids)
	}
	if ids[0] != deterministicMessageID(firstStep, 4).String() || ids[1] != deterministicMessageID(secondStep, 4).String() {
		t.Fatalf("unexpected scoped IDs: %#v", ids)
	}
}

func TestPlanHelpers(t *testing.T) {
	if approvalErrorCode(&ToolApproval{}) != "tool_execution_failed" {
		t.Fatal("default approval error code mismatch")
	}
	if approvalErrorCode(&ToolApproval{ErrorCode: "x"}) != "x" {
		t.Fatal("approval error code not preserved")
	}
	if arguments := planArguments(nil); len(arguments) != 0 {
		t.Fatalf("nil arguments = %#v", arguments)
	}
}

func TestPlanCompletedAndCancelledEntry(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	messages := []react.Message{{Role: react.RoleUser, Content: "任务"}}
	completedPlan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanCompleted, Goal: "g", MaxSteps: 50, Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepCompleted, Result: map[string]any{"content": "结果"}}}}
	plans.seedPlan(completedPlan)
	if result, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, messages, emptyToolRegistry{}); err != nil || !strings.Contains(result.Content, "结果") {
		t.Fatalf("completed plan result = %#v, %v", result, err)
	}
	plans.seedPlan(Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 2, Status: PlanCancelled, MaxSteps: 50})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, messages, emptyToolRegistry{}); !errors.Is(err, ErrPlanCancelled) {
		t.Fatalf("cancelled plan error = %v", err)
	}
}

func TestFindPlanApproval(t *testing.T) {
	worker, _, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	approvals := &fakePlanApprovals{}
	worker.approvals = approvals
	run := Run{ID: uuid.New(), UserID: uuid.New()}
	approvals.items = []ToolApproval{{ID: uuid.New(), RunID: run.ID, UserID: run.UserID, ToolCallID: "call-1"}}
	found, err := worker.findPlanApproval(context.Background(), run, "call-1")
	if err != nil || found == nil || found.ToolCallID != "call-1" {
		t.Fatalf("find approval = %#v, %v", found, err)
	}
	if found, err := worker.findPlanApproval(context.Background(), run, "missing"); err != nil || found != nil {
		t.Fatalf("missing approval = %#v, %v", found, err)
	}
}

func TestPlanHelperFallbacks(t *testing.T) {
	if payload := trimReasonPayload([]memory.TrimReason{{Code: "budget", Detail: "trim"}}); len(payload) != 1 || payload[0]["code"] != "budget" {
		t.Fatalf("trim reason payload = %#v", payload)
	}
	step := PlanStep{Result: map[string]any{"tool": "ok"}}
	if text := planStepResultText(step); text != `{"tool":"ok"}` {
		t.Fatalf("result fallback = %q", text)
	}
	if text := planStepResultText(PlanStep{}); text != "" {
		t.Fatalf("empty result = %q", text)
	}
}

func TestExecutePlanToolStepApprovalUnavailable(t *testing.T) {
	worker, plans, plan, step := planToolFixture(t, fakePlanTool{risk: "high", output: "{}"}, nil)
	err := worker.executePlanToolStep(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1},
		PlanFence{Owner: "plan-worker", Token: 1}, fakePlanRegistry{tool: fakePlanTool{risk: "high"}}, plan, step)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Steps[0].Status != PlanStepFailed || stored.Steps[0].ErrorCode != "approval_unavailable" {
		t.Fatalf("approval unavailable step = %#v", stored.Steps[0])
	}
}

// failingRunRepository 返回事件写入错误，用于覆盖事件失败降级。
type failingRunRepository struct {
	RunRepository
}

// AppendEventFenced 返回错误。
func (failingRunRepository) AppendEventFenced(_ context.Context, _ RunEvent, _ RunFence) (RunEvent, error) {
	return RunEvent{}, errors.New("event store down")
}

func TestTransitionPlanAndEventFailure(t *testing.T) {
	worker, _, _, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	plan := Plan{ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), SessionID: uuid.New(), Status: PlanExecuting}
	if updated, err := worker.transitionPlan(context.Background(), plan, PlanExecuting, PlanFence{Owner: "plan-worker", Token: 1}, "", ""); err != nil || updated.Status != PlanExecuting {
		t.Fatalf("same-state transition = %#v, %v", updated, err)
	}
	worker.runs = failingRunRepository{}
	worker.emitPlanEvent(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1}, "plan.created", map[string]any{})
	if code, message := failureDetails(ErrPlanFailed); code != "plan_failed" || message == "" {
		t.Fatalf("plan failure details = %q, %q", code, message)
	}
	if code, _ := failureDetails(ErrPlanCancelled); code != "plan_cancelled" {
		t.Fatalf("cancelled failure code = %q", code)
	}
}

func TestExecutePlanToolStepMissingTool(t *testing.T) {
	worker, plans, plan, step := planToolFixture(t, fakePlanTool{risk: "low"}, nil)
	if err := worker.executePlanToolStep(context.Background(), Run{ID: plan.RunID, UserID: plan.UserID}, RunFence{Owner: "plan-worker", Token: 1},
		PlanFence{Owner: "plan-worker", Token: 1}, emptyToolRegistry{}, plan, step); err != nil {
		t.Fatal(err)
	}
	stored, _ := plans.GetPlanByRun(context.Background(), plan.UserID, plan.RunID)
	if stored.Steps[0].Status != PlanStepFailed || stored.Steps[0].ErrorCode != "tool_unavailable" {
		t.Fatalf("missing tool step = %#v", stored.Steps[0])
	}
}

func TestExecutePlanFailedAndRecoverModelStep(t *testing.T) {
	worker, plans, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		writeCompletion(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModePlan}
	messages := []react.Message{{Role: react.RoleUser, Content: "任务"}}
	plans.seedPlan(Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 1,
		Status: PlanFailed, ErrorMessage: "previous failure", MaxSteps: 50})
	if _, err := worker.executePlan(context.Background(), run, RunFence{Owner: "plan-worker", Token: 1}, router, messages, emptyToolRegistry{}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("failed plan error = %v", err)
	}
	modelStep := PlanStep{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepRunning, MaxAttempts: 2}
	plan := Plan{ID: uuid.New(), UserID: run.UserID, RunID: run.ID, SessionID: run.SessionID, Version: 2,
		Status: PlanExecuting, MaxSteps: 50, Steps: []PlanStep{modelStep}}
	modelStep.PlanID = plan.ID
	plan.Steps[0].PlanID = plan.ID
	plans.seedPlan(plan)
	recovered, err := worker.recoverPlanSteps(context.Background(), plan, RunFence{Owner: "plan-worker", Token: 1})
	if err != nil || recovered.Steps[0].Status != PlanStepFailed {
		t.Fatalf("model step recovery = %#v, %v", recovered.Steps[0], err)
	}
}
