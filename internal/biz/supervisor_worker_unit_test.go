package biz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// memorySupervisorRepo 是内存子任务仓储，覆盖委派、领取、完成与取消。
type memorySupervisorRepo struct {
	mu    sync.Mutex
	tasks map[uuid.UUID]SupervisorTask
}

// newMemorySupervisorRepo 构造内存仓储。
func newMemorySupervisorRepo() *memorySupervisorRepo {
	return &memorySupervisorRepo{tasks: map[uuid.UUID]SupervisorTask{}}
}
func (r *memorySupervisorRepo) CreateTasks(_ context.Context, userID, parentRunID uuid.UUID, tasks []SupervisorTask, _ OutboxEvent) ([]SupervisorTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	for index := range tasks {
		if tasks[index].ID == uuid.Nil {
			tasks[index].ID = uuid.New()
		}
		tasks[index].UserID = userID
		tasks[index].ParentRunID = parentRunID
		tasks[index].CreatedAt = now
		tasks[index].UpdatedAt = now
		r.tasks[tasks[index].ID] = tasks[index]
	}
	return tasks, nil
}
func (r *memorySupervisorRepo) ListTasks(_ context.Context, _ uuid.UUID, parentRunID uuid.UUID) ([]SupervisorTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]SupervisorTask, 0)
	for _, task := range r.tasks {
		if task.ParentRunID == parentRunID {
			result = append(result, task)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkerIndex < result[j].WorkerIndex })
	return result, nil
}
func (r *memorySupervisorRepo) StartTask(_ context.Context, userID, taskID uuid.UUID, now time.Time) (SupervisorTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok || task.UserID != userID || task.Status != SupervisorTaskPending || task.Attempts >= task.MaxAttempts {
		return SupervisorTask{}, ErrResourceConflict
	}
	task.Status = SupervisorTaskRunning
	task.Attempts++
	task.StartedAt = &now
	r.tasks[taskID] = task
	return task, nil
}
func (r *memorySupervisorRepo) FinishTask(_ context.Context, userID, taskID uuid.UUID, status SupervisorTaskStatus, result map[string]any, code, message string, now time.Time) (SupervisorTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok || task.UserID != userID || task.Status != SupervisorTaskRunning {
		return SupervisorTask{}, ErrResourceConflict
	}
	task.Status = status
	task.Result = result
	task.ErrorCode = code
	task.ErrorMessage = message
	task.FinishedAt = &now
	r.tasks[taskID] = task
	return task, nil
}
func (r *memorySupervisorRepo) CancelPendingTasks(_ context.Context, userID, parentRunID uuid.UUID, reason string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, task := range r.tasks {
		if task.UserID == userID && task.ParentRunID == parentRunID && task.Status == SupervisorTaskPending {
			task.Status = SupervisorTaskCancelled
			task.ErrorMessage = reason
			task.FinishedAt = &now
			r.tasks[id] = task
		}
	}
	return nil
}

// memorySupervisorSessions 记录子会话与消息，验证独立上下文与原始用户归属。
type memorySupervisorSessions struct {
	SessionRepository
	mu       sync.Mutex
	sessions []Session
	messages []Message
}

func (s *memorySupervisorSessions) CreateSession(_ context.Context, session Session, _ OutboxEvent) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.ID == uuid.Nil {
		session.ID = uuid.New()
	}
	s.sessions = append(s.sessions, session)
	return session, nil
}
func (s *memorySupervisorSessions) AppendMessage(_ context.Context, message Message, _ OutboxEvent) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
	return message, nil
}
func (s *memorySupervisorSessions) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.messages {
		if existing.ID == message.ID {
			return existing, nil
		}
	}
	s.messages = append(s.messages, message)
	return message, nil
}

// supervisorFixture 构造 Supervisor Worker 与模型服务器。
func supervisorFixture(t *testing.T, handler func(body string, w http.ResponseWriter)) (*ExecutionWorker, *memorySupervisorRepo, *memorySupervisorSessions, []model.Candidate, *httptest.Server) {
	t.Helper()
	repo := newMemorySupervisorRepo()
	sessions := &memorySupervisorSessions{}
	worker, err := NewExecutionWorker(ExecutionWorkerDependencies{
		Jobs: stubJobQueue{}, Runs: stubRunRepository{}, Sessions: sessions,
		Providers: stubProviders{}, Opener: stubOpener{}, Attempts: noopAttemptRepository{},
		SupervisorTasks: repo,
	}, WorkerConfig{
		WorkerID: "supervisor-worker", Lease: time.Minute, MCPTimeout: time.Second, SupervisorMaxWorkers: 4,
		Routing: model.RouterConfig{MinContextTokens: 1, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 6, MaxSwitchesPerRun: 1, MaxOutputTokens: 64, RequestTimeout: 5 * time.Second, BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		handler(string(raw), w)
	}))
	t.Cleanup(server.Close)
	candidates := []model.Candidate{{
		Priority: 1, BaseURL: server.URL, APIKey: "test-key",
		Ref: model.ModelRef{ProviderID: uuid.NewString(), ModelID: uuid.NewString(), RemoteModelName: "test", ContextWindowTokens: 1_000_000, MaxOutputTokens: 64},
	}}
	return worker, repo, sessions, candidates, server
}

func supervisorReply(t *testing.T, w http.ResponseWriter, content string, status int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, _ := json.Marshal(map[string]any{
		"id": "req", "choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3},
	})
	_, _ = w.Write(encoded)
}

func TestExecuteSupervisorSuccess(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务A","tools":[]},{"name":"B","task":"任务B","tools":[]}]}`
	worker, repo, sessions, candidates, _ := supervisorFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			supervisorReply(t, w, delegation, http.StatusOK)
		case strings.Contains(body, "请汇总以下 Worker 结果"):
			supervisorReply(t, w, "汇总结果：A 与 B 均完成", http.StatusOK)
		default:
			supervisorReply(t, w, "Worker 结果", http.StatusOK)
		}
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	messages := []react.Message{{Role: react.RoleUser, Content: "请分别处理两个任务"}}
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil, messages, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "汇总结果") {
		t.Fatalf("result content = %q", result.Content)
	}
	tasks, _ := repo.ListTasks(context.Background(), run.UserID, run.ID)
	if len(tasks) != 2 || tasks[0].Status != SupervisorTaskCompleted || tasks[1].Status != SupervisorTaskCompleted {
		t.Fatalf("tasks = %#v", tasks)
	}
	if len(sessions.sessions) != 2 {
		t.Fatalf("child sessions = %d", len(sessions.sessions))
	}
	for _, session := range sessions.sessions {
		if session.UserID != run.UserID || session.ParentRunID == nil || *session.ParentRunID != run.ID {
			t.Fatalf("child session binding = %#v", session)
		}
	}
	if len(sessions.messages) == 0 {
		t.Fatal("child messages were not persisted")
	}
}

func TestExecuteSupervisorPartialFailure(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务A","tools":[]},{"name":"B","task":"FAIL 任务","tools":[]}]}`
	worker, repo, _, candidates, _ := supervisorFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			supervisorReply(t, w, delegation, http.StatusOK)
		case strings.Contains(body, "请汇总以下 Worker 结果"):
			supervisorReply(t, w, "部分结果：A 完成，B 失败", http.StatusOK)
		case strings.Contains(body, "FAIL"):
			supervisorReply(t, w, "worker error", http.StatusInternalServerError)
		default:
			supervisorReply(t, w, "Worker 结果", http.StatusOK)
		}
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		[]react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "部分结果") {
		t.Fatalf("partial content = %q", result.Content)
	}
	tasks, _ := repo.ListTasks(context.Background(), run.UserID, run.ID)
	completed, failed := 0, 0
	for _, task := range tasks {
		if task.Status == SupervisorTaskCompleted {
			completed++
		}
		if task.Status == SupervisorTaskFailed {
			failed++
		}
	}
	if completed != 1 || failed != 1 {
		t.Fatalf("partial statuses = %#v", tasks)
	}
}

func TestExecuteSupervisorAllFailed(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"FAIL","tools":[]}]}`
	worker, _, _, candidates, _ := supervisorFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "请输出 Worker 委派 JSON") {
			supervisorReply(t, w, delegation, http.StatusOK)
			return
		}
		supervisorReply(t, w, "error", http.StatusInternalServerError)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	if _, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		[]react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{}); !errors.Is(err, ErrSupervisorAllFailed) {
		t.Fatalf("all failed error = %v", err)
	}
}

func TestExecuteSupervisorDeniesElevatedTool(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务","tools":["high_risk_tool"]}]}`
	worker, _, _, candidates, _ := supervisorFixture(t, func(_ string, w http.ResponseWriter) {
		supervisorReply(t, w, delegation, http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	tools := fakePlanRegistry{tool: fakePlanTool{risk: "high", output: "{}"}}
	if _, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		[]react.Message{{Role: react.RoleUser, Content: "处理"}}, tools); !errors.Is(err, ErrSupervisorInvalid) {
		t.Fatalf("elevated tool error = %v", err)
	}
}

func TestSupervisorBudgetExceeded(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务A","tools":[]}]}`
	worker, _, _, candidates, _ := supervisorFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			supervisorReply(t, w, delegation, http.StatusOK)
		default:
			supervisorReply(t, w, "结果", http.StatusOK)
		}
	})
	// 规划消耗 8 Token，Worker 再消耗 8 Token；上限允许规划完成，但在 Worker 后触发部分结果降级。
	worker.config.SupervisorMaxTokens = 9
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1},
		candidates, nil, []react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{})
	// 预算耗尽但已有 Worker 产出时降级为部分结果，而不是整体失败。
	if err != nil {
		t.Fatalf("budget degradation error = %v", err)
	}
	if !strings.Contains(result.Content, "结果") {
		t.Fatalf("partial result content = %q", result.Content)
	}
}

func TestSupervisorFinalResultRecoveryKeepsStoredSummary(t *testing.T) {
	calls := 0
	worker, _, _, candidates, _ := supervisorFixture(t, func(_ string, w http.ResponseWriter) {
		calls++
		if calls == 1 {
			supervisorReply(t, w, "第一次汇总", http.StatusOK)
			return
		}
		supervisorReply(t, w, "第二次汇总", http.StatusOK)
	})
	sessions := &statefulSessionRepository{}
	worker.sessions = sessions
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	tasks := []SupervisorTask{{
		ID: uuid.New(), UserID: run.UserID, ParentRunID: run.ID, WorkerName: "A", WorkerIndex: 1,
		Status: SupervisorTaskCompleted, Result: map[string]any{"content": "Worker 结果"},
	}}
	messages := []react.Message{{Role: react.RoleUser, Content: "任务"}}
	first, err := worker.aggregateSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		&atomic.Int64{}, messages, supervisorUsage{}, tasks)
	if err != nil || first.Content != "第一次汇总" {
		t.Fatalf("first aggregate = %#v, %v", first, err)
	}
	recovered, err := worker.aggregateSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		&atomic.Int64{}, first.Messages, supervisorUsage{}, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Content != "第一次汇总" || len(recovered.Messages) != len(first.Messages) || len(sessions.messages) != 1 {
		t.Fatalf("recovery duplicated or replaced summary: %#v, stored=%d", recovered, len(sessions.messages))
	}
	if calls != 1 {
		t.Fatalf("recovery called aggregation model %d times, want 1 total call", calls)
	}
}

func TestSupervisorUsageFromEvents(t *testing.T) {
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
	worker := &ExecutionWorker{runs: eventRunRepository{events: []RunEvent{
		{Seq: 1, EventType: "model.attempt.completed", Payload: map[string]any{"input_tokens": 5, "output_tokens": 3, "estimated_cost": 0.1}},
		{Seq: 2, EventType: "model.attempt.completed", Payload: map[string]any{"input_tokens": 7, "output_tokens": 2, "estimated_cost": 0.2}},
	}}}
	usage, has, err := worker.supervisorUsageFromEvents(context.Background(), run)
	if err != nil || !has || usage.input != 12 || usage.output != 5 || usage.cost != 0.30000000000000004 {
		t.Fatalf("usage = %#v, has=%v, err=%v", usage, has, err)
	}
}

func TestSupervisorRecoveryAndResultHelpers(t *testing.T) {
	worker, repo, _, candidates, _ := supervisorFixture(t, func(body string, w http.ResponseWriter) {
		supervisorReply(t, w, `{"workers":[{"name":"A","task":"t","tools":[]}]}`, http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	repo.tasks[uuid.New()] = SupervisorTask{
		ID: uuid.New(), UserID: run.UserID, ParentRunID: run.ID, ChildSessionID: uuid.New(),
		WorkerName: "stale", WorkerIndex: 1, Task: "t", Status: SupervisorTaskRunning, MaxAttempts: 1, Depth: 1,
	}
	if _, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		[]react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{}); !errors.Is(err, ErrSupervisorAllFailed) {
		t.Fatalf("recovery all failed error = %v", err)
	}
	if text := supervisorResultText(SupervisorTask{Result: map[string]any{"tool": "ok"}}); text != `{"tool":"ok"}` {
		t.Fatalf("result fallback = %q", text)
	}
	if text := supervisorResultText(SupervisorTask{}); text != "" {
		t.Fatalf("empty result = %q", text)
	}
	registry := scopedToolRegistry{inner: fakePlanRegistry{tool: fakePlanTool{risk: "low"}}, allowed: []string{"tool"}}
	if _, ok := registry.Lookup("tool"); !ok {
		t.Fatal("allowed tool not found")
	}
	if _, ok := registry.Lookup("blocked"); ok {
		t.Fatal("blocked tool was visible")
	}
	if specs := registry.List(); len(specs) != 1 || specs[0].Name != "tool" {
		t.Fatalf("scoped specs = %#v", specs)
	}
}

func TestSupervisorParentCancellation(t *testing.T) {
	worker, repo, _, candidates, _ := supervisorFixture(t, func(_ string, w http.ResponseWriter) {
		supervisorReply(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	for index := 1; index <= 2; index++ {
		task := SupervisorTask{
			ID: uuid.New(), UserID: run.UserID, ParentRunID: run.ID, ChildSessionID: uuid.New(),
			WorkerName: "W" + string(rune('0'+index)), WorkerIndex: index, Task: "任务", Status: SupervisorTaskPending,
			MaxAttempts: 1, Depth: 1,
		}
		repo.tasks[task.ID] = task
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.executeSupervisor(ctx, run, RunFence{Owner: "supervisor-worker", Token: 1}, candidates, nil,
		[]react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled supervisor error = %v", err)
	}
	tasks, _ := repo.ListTasks(context.Background(), run.UserID, run.ID)
	for _, task := range tasks {
		if task.Status != SupervisorTaskCancelled {
			t.Fatalf("cancelled task status = %s", task.Status)
		}
	}
}

// plannerTestRegistry 是规划校验用的最小工具注册表。
type plannerTestRegistry struct {
	tools map[string]plannerTestTool
	specs []react.ToolSpec
}

// plannerTestTool 是带风险等级的假工具。
type plannerTestTool struct{ risk string }

// Spec 返回测试工具定义。
func (t plannerTestTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: "test", Description: "test tool"}
}

// Invoke 模拟工具执行。
func (t plannerTestTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

// RiskLevel 返回测试风险等级。
func (t plannerTestTool) RiskLevel() string { return t.risk }

// Lookup 按名称查找测试工具。
func (r plannerTestRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

// List 返回测试工具定义。
func (r plannerTestRegistry) List() []react.ToolSpec { return r.specs }

// TestPlanSupervisorDraftRepairsInvalidToolOnce 验证非法工具名会把校验错误回传模型并修正一次。
func TestPlanSupervisorDraftRepairsInvalidToolOnce(t *testing.T) {
	validName := "mcp_contextforge_mcp-codewiki-search-code-graph"
	registry := plannerTestRegistry{
		tools: map[string]plannerTestTool{validName: {risk: "low"}},
		specs: []react.ToolSpec{{Name: validName}},
	}
	first := `{"workers":[{"name":"代码检索员","task":"检索代码","tools":["search_codebase"]}]}`
	second := `{"workers":[{"name":"代码检索员","task":"检索代码","tools":["` + validName + `"]}]}`
	calls := 0
	repairPrompt := ""
	complete := func(messages []react.Message) (react.Completion, error) {
		calls++
		if calls == 1 {
			return react.Completion{Content: first}, nil
		}
		for _, message := range messages {
			if message.Role == react.RoleUser && strings.Contains(message.Content, "委派校验失败") {
				repairPrompt = message.Content
			}
		}
		return react.Completion{Content: second}, nil
	}
	draft, _, err := planSupervisorDraft([]react.Message{{Role: react.RoleUser, Content: "请处理"}}, complete, 4, registry)
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2", calls)
	}
	if !strings.Contains(repairPrompt, "unavailable tool") || !strings.Contains(repairPrompt, "search_codebase") {
		t.Fatalf("repair prompt missing validation detail: %s", repairPrompt)
	}
	if len(draft.Workers) != 1 || len(draft.Workers[0].Tools) != 1 || draft.Workers[0].Tools[0] != validName {
		t.Fatalf("repaired draft = %#v", draft)
	}
}

// TestPlanSupervisorDraftRejectsApprovalRequiredTool 验证中高风险工具不能被委派，且修复失败会被拒绝。
func TestPlanSupervisorDraftRejectsApprovalRequiredTool(t *testing.T) {
	name := "mcp_contextforge_dangerous"
	registry := plannerTestRegistry{
		tools: map[string]plannerTestTool{name: {risk: "medium"}},
		specs: []react.ToolSpec{{Name: name}},
	}
	invalid := `{"workers":[{"name":"执行员","task":"执行","tools":["` + name + `"]}]}`
	complete := func([]react.Message) (react.Completion, error) {
		return react.Completion{Content: invalid}, nil
	}
	if _, _, err := planSupervisorDraft(nil, complete, 4, registry); !errors.Is(err, ErrSupervisorInvalid) {
		t.Fatalf("approval-required tool error = %v", err)
	}
}

// TestSupervisorPlannerSystemListsExactTools 验证规划提示携带真实工具全名，避免模型猜测简写名称。
func TestSupervisorPlannerSystemListsExactTools(t *testing.T) {
	system := supervisorPlannerSystem([]react.ToolSpec{
		{Name: "mcp_contextforge_mcp-codewiki-search-code-graph", Description: "检索代码关系图"},
		{Name: "mcp_contextforge_mcp-archery-archery-query", Description: ""},
	})
	for _, name := range []string{
		"mcp_contextforge_mcp-codewiki-search-code-graph",
		"mcp_contextforge_mcp-archery-archery-query",
	} {
		if !strings.Contains(system, name) {
			t.Fatalf("planner system missing tool %q: %s", name, system)
		}
	}
	if !strings.Contains(system, "禁止简写") {
		t.Fatalf("planner system missing exact-name constraint: %s", system)
	}
	if empty := supervisorPlannerSystem(nil); !strings.Contains(empty, "tools 必须为空数组") {
		t.Fatalf("empty registry instruction = %s", empty)
	}
}
