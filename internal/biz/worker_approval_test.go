package biz_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// approvalMCPToolServer 构造单工具 MCP TLS 服务，并记录 tools/call 请求参数。
func approvalMCPToolServer(t *testing.T, toolName string, onCall func(params map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		if body.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch body.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": toolName, "description": "dangerous", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			if onCall != nil {
				onCall(body.Params)
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "approved-result"}}, "isError": false}
		default:
			t.Errorf("unexpected MCP method %s", body.Method)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	return server
}

// approvalWorkerDependencies 构造带审批仓储的 Worker 依赖。
func approvalWorkerDependencies(t *testing.T, ctrl *gomock.Controller, mcpServer *httptest.Server, riskLevel string, budgetCheckers ...biz.BudgetChecker) (*biz.ExecutionWorker, *mocks.MockJobQueue, *mocks.MockRunRepository, *mocks.MockSessionRepository, *mocks.MockExecutionStepRepository, *mocks.MockToolApprovalRepository) {
	t.Helper()
	jobs := mocks.NewMockJobQueue(ctrl)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	attempts := mocks.NewMockModelCallRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	approvals := mocks.NewMockToolApprovalRepository(ctrl)
	// mcpServer 为空时构造不带 MCP 的 Worker，用于只验证审批状态机的场景。
	providerCredential := biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("ciphertext")}
	var mcpCredentials biz.MCPCredentialRepository
	var clientFactory func(endpoint, authHeader, authValue string, timeout time.Duration) (*mcp.Client, error)
	if mcpServer != nil {
		mcpCredentials = fakeMCPCredentials{record: biz.MCP{
			ID: uuid.New(), Name: "admin", Endpoint: mcpServer.URL, AuthType: "none",
			RiskLevel: riskLevel, Status: "enabled",
		}}
		clientFactory = func(endpoint, header, value string, timeout time.Duration) (*mcp.Client, error) {
			return mcp.NewClient(endpoint, header, value, timeout, mcp.WithHTTPClient(mcpServer.Client()))
		}
	}
	steps.EXPECT().StartStep(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, step biz.ExecutionStep) (biz.ExecutionStep, error) {
			if step.ID == uuid.Nil {
				step.ID = uuid.New()
			}
			step.StepSeq = 1
			return step, nil
		},
	).AnyTimes()
	steps.EXPECT().SavePayload(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, payload biz.ExecutionPayload) (biz.ExecutionPayload, error) {
			return payload, nil
		},
	).AnyTimes()
	steps.EXPECT().FinishStep(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ExecutionStep{}, nil).AnyTimes()
	attempts.EXPECT().CreateModelCall(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, call biz.ModelCall) (biz.ModelCall, error) { return call, nil },
	).AnyTimes()
	attempts.EXPECT().FinishModelCall(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ModelCall{}, nil).AnyTimes()
	var budgetChecker biz.BudgetChecker
	if len(budgetCheckers) > 0 {
		budgetChecker = budgetCheckers[0]
	}
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions,
		Providers: fakeProviderCredentials{
			provider:   biz.Provider{Status: biz.ProviderEnabled},
			credential: providerCredential,
		},
		Opener: fakeOpener{key: "test-key"}, Attempts: attempts, Steps: steps, Approvals: approvals,
		Budgets:          budgetChecker,
		MCPs:             mcpCredentials,
		MCPClientFactory: clientFactory,
	}, biz.WorkerConfig{
		WorkerID: "worker-1", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 60 * time.Second, Heartbeat: 20 * time.Second, CancelPoll: time.Hour,
		ApprovalPoll: 10 * time.Millisecond,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6,
			MaxSwitchesPerRun: 2, RequestTimeout: 5 * time.Second,
			BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, jobs, runs, sessions, steps, approvals
}

// TestWorkerPausesRunForHighRiskToolApproval 验证高风险工具只创建审批并把 Run 暂停到等待审批。
func TestWorkerPausesRunForHighRiskToolApproval(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID, mcpID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var mutex sync.Mutex
	mcpCalled := false
	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		modelCalls++
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","function":{"name":"mcp_admin_delete","arguments":"{\"id\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(modelServer.Close)
	mcpServer := approvalMCPToolServer(t, "delete", func(map[string]any) {
		mutex.Lock()
		mcpCalled = true
		mutex.Unlock()
	})
	worker, jobs, runs, sessions, steps, approvals := approvalWorkerDependencies(t, ctrl, mcpServer, "high")

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(modelServer.URL, providerID, modelID))
	run.Status = biz.RunQueued
	run.ConfigSnapshot = map[string]any{
		"agent": map[string]any{"name": "审批助手"},
		"published_snapshot": map[string]any{"mcps": []any{map[string]any{
			"id": mcpID, "name": "admin", "endpoint": mcpServer.URL,
			"auth_type": "none", "risk_level": "high", "status": "enabled",
		}}},
	}
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "删除数据"}},
	}, nil)
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return(nil, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.Message{}, nil).AnyTimes()
	var approvalID uuid.UUID
	var createdApproval biz.ToolApproval
	approvals.EXPECT().CreateApproval(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, approval biz.ToolApproval) (biz.ToolApproval, error) {
			approval.ID = uuid.New()
			approvalID = approval.ID
			createdApproval = approval
			return approval, nil
		},
	)
	var waitingStepID uuid.UUID
	steps.EXPECT().MarkStepWaitingApproval(gomock.Any(), userID, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, stepID uuid.UUID) (biz.ExecutionStep, error) {
			waitingStepID = stepID
			return biz.ExecutionStep{ID: stepID, Status: biz.StepWaitingApproval}, nil
		},
	)
	runs.EXPECT().PauseRun(gomock.Any(), userID, runID, fence, gomock.Any()).Return(biz.Run{ID: runID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), userID, runID).Return(1, nil)
	jobs.EXPECT().Release(gomock.Any(), job.ID, "worker-1", job.FencingToken, gomock.Any(), gomock.Any()).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if mcpCalled {
		t.Fatal("high risk MCP tool was executed without approval")
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
	if approvalID == uuid.Nil || createdApproval.ToolCallID != "call-1" || createdApproval.RiskLevel != "high" {
		t.Fatalf("unexpected approval: %#v", createdApproval)
	}
	if createdApproval.StepID == nil || *createdApproval.StepID != waitingStepID {
		t.Fatalf("approval step linkage = %#v, waiting step %s", createdApproval.StepID, waitingStepID)
	}
}

// TestWorkerExecutesApprovedToolAndResumes 验证批准后工具幂等执行、结果回填并继续完成 Run。
func TestWorkerExecutesApprovedToolAndResumes(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID, mcpID, approvalID, stepID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.New(), uuid.New()
	var mutex sync.Mutex
	var toolParams map[string]any
	var modelBodies []string
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		mutex.Lock()
		modelBodies = append(modelBodies, string(encoded))
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-2","choices":[{"message":{"content":"审批后完成"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}`))
	}))
	t.Cleanup(modelServer.Close)
	mcpServer := approvalMCPToolServer(t, "delete", func(params map[string]any) {
		mutex.Lock()
		toolParams = params
		mutex.Unlock()
	})
	worker, jobs, runs, sessions, _, approvals := approvalWorkerDependencies(t, ctrl, mcpServer, "high")

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(modelServer.URL, providerID, modelID))
	run.Status = biz.RunRunning
	run.ConfigSnapshot = map[string]any{
		"agent": map[string]any{"name": "审批助手"},
		"published_snapshot": map[string]any{"mcps": []any{map[string]any{
			"id": mcpID, "name": "admin", "endpoint": mcpServer.URL,
			"auth_type": "none", "risk_level": "high", "status": "enabled",
		}}},
	}
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(run, fence, nil)

	approved := biz.ToolApproval{
		ID: approvalID, RunID: runID, UserID: userID, StepID: &stepID,
		ToolCallID: "call-1", ToolName: "mcp_admin_delete", Arguments: json.RawMessage(`{"id":"x"}`),
		RiskLevel: "high", Status: biz.ApprovalApproved, IdempotencyKey: "idem-1",
	}
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return([]biz.ToolApproval{approved}, nil)
	executing := approved
	executing.Status = biz.ApprovalExecuting
	approvals.EXPECT().ClaimApprovalExecution(gomock.Any(), userID, approvalID, gomock.Any()).Return(executing, nil)
	executed := executing
	executed.Status = biz.ApprovalExecuted
	executed.ResultJSON = json.RawMessage(`{"content":[{"type":"text","text":"approved-result"}],"is_error":false}`)
	approvals.EXPECT().FinishApprovalExecution(gomock.Any(), userID, approvalID, biz.ApprovalExecuted, gomock.Any(), "", "", gomock.Any()).Return(executed, nil)
	approvals.EXPECT().MarkApprovalDelivered(gomock.Any(), userID, approvalID, gomock.Any()).Return(executed, nil)

	// 审批结果消息必须先落库，模型才能在上下文中看到工具返回。
	var persisted []biz.Message
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			mutex.Lock()
			persisted = append(persisted, message)
			mutex.Unlock()
			return message, nil
		},
	).AnyTimes()
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{
			{Role: biz.MessageTool, Content: string(executed.ResultJSON), ToolCallID: "call-1", ToolName: "mcp_admin_delete"},
			{Role: biz.MessageAssistant, ToolCalls: []map[string]any{{"id": "call-1", "name": "mcp_admin_delete", "arguments": `{"id":"x"}`}}},
			{Role: biz.MessageUser, Content: "删除数据"},
		},
	}, nil)
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunCompleted, gomock.Any(), gomock.Any(), fence, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCompleted}, nil,
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if toolParams == nil {
		t.Fatal("approved tool was not executed")
	}
	meta, _ := toolParams["_meta"].(map[string]any)
	if meta == nil || meta["idempotency_key"] != "idem-1" {
		t.Fatalf("idempotency key was not propagated: %#v", toolParams)
	}
	found := false
	for _, message := range persisted {
		if message.Role == biz.MessageTool && message.Content == string(executed.ResultJSON) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result was not persisted: %#v", persisted)
	}
	// 审批恢复后的模型请求必须包含工具结果，证明上下文已回填。
	if len(modelBodies) == 0 || !strings.Contains(modelBodies[len(modelBodies)-1], "approved-result") {
		t.Fatalf("approval result was not fed back to the model: %#v", modelBodies)
	}
}

// TestWorkerReconcilesUnknownApprovalExecution 验证执行态审批结果未知时进入人工对账而非重复执行。
func TestWorkerReconcilesUnknownApprovalExecution(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	worker, jobs, runs, _, _, approvals := approvalWorkerDependencies(t, ctrl, nil, "high")

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture("https://models.example.com", providerID, modelID))
	run.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(run, fence, nil)
	stepID := uuid.New()
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return([]biz.ToolApproval{{
		ID: uuid.New(), RunID: runID, UserID: userID, StepID: &stepID,
		ToolCallID: "call-1", ToolName: "mcp_admin_delete", RiskLevel: "high",
		Status: biz.ApprovalExecuting, IdempotencyKey: "idem-1",
	}}, nil)
	runs.EXPECT().MarkRunReconciliationRequired(gomock.Any(), userID, runID, fence, "tool_execution_unknown", gomock.Any(), gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunReconciliationRequired}, nil,
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}

// TestWorkerDeliversDeniedApproval 验证拒绝结果作为错误工具消息回填模型并继续完成 Run。
func TestWorkerDeliversDeniedApproval(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	worker, jobs, runs, sessions, _, approvals := approvalWorkerDependencies(t, ctrl, nil, "high")
	modelServer := completionServer(t, "已取消操作")

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture(modelServer.URL, providerID, modelID))
	run.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(run, fence, nil)
	stepID := uuid.New()
	denied := biz.ToolApproval{
		ID: uuid.New(), RunID: runID, UserID: userID, StepID: &stepID,
		ToolCallID: "call-1", ToolName: "mcp_admin_delete", RiskLevel: "high",
		Status: biz.ApprovalDenied, IdempotencyKey: "idem-1",
	}
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return([]biz.ToolApproval{denied}, nil)
	approvals.EXPECT().MarkApprovalDelivered(gomock.Any(), userID, denied.ID, gomock.Any()).Return(denied, nil)
	var persisted []biz.Message
	sessions.EXPECT().AppendMessageIdempotent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			persisted = append(persisted, message)
			return message, nil
		},
	).AnyTimes()
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "删除数据"}},
	}, nil)
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	runs.EXPECT().SetRunAttempt(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), fence).Return(nil).AnyTimes()
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunCompleted, gomock.Any(), gomock.Any(), fence, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCompleted}, nil,
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	found := false
	for _, message := range persisted {
		if message.Role == biz.MessageTool && message.ToolCallID == "call-1" && message.Content == `{"error":"approval_denied"}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied result was not delivered: %#v", persisted)
	}
}

// TestWorkerStartStopsOnContextCancel 验证 Worker 启动循环在上下文取消后退出。
func TestWorkerStartStopsOnContextCancel(t *testing.T) {
	ctrl := gomock.NewController(t)
	worker, jobs, _, _, _, _ := approvalWorkerDependencies(t, ctrl, nil, "high")
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(biz.Job{}, false, nil).AnyTimes()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Start(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

// TestWorkerFailsRunWhenBudgetExhausted 验证预算超限时模型调用被拒绝且 Run 直接失败。
func TestWorkerFailsRunWhenBudgetExhausted(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, runID, sessionID := uuid.New(), uuid.New(), uuid.New()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	checker := mocks.NewMockBudgetChecker(ctrl)
	worker, jobs, runs, sessions, _, approvals := approvalWorkerDependencies(t, ctrl, nil, "low", checker)

	job := jobFixture(userID, runID)
	jobs.EXPECT().Claim(gomock.Any(), "worker-1", "agent.run", gomock.Any(), gomock.Any()).Return(job, true, nil)
	run := runFixture(userID, runID, sessionID, snapshotFixture("https://models.example.com", providerID, modelID))
	running := run
	running.Status = biz.RunRunning
	fence := biz.RunFence{Owner: "worker-1", Token: 9}
	runs.EXPECT().GetRun(gomock.Any(), userID, runID).Return(run, nil)
	runs.EXPECT().ClaimRun(gomock.Any(), userID, runID, "worker-1", gomock.Any(), gomock.Any()).Return(running, fence, nil)
	approvals.EXPECT().ListApprovals(gomock.Any(), userID, runID).Return(nil, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 50, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{Role: biz.MessageUser, Content: "继续"}},
	}, nil)
	runs.EXPECT().AppendEventFenced(gomock.Any(), gomock.Any(), fence).Return(biz.RunEvent{}, nil).AnyTimes()
	checker.EXPECT().Check(gomock.Any(), userID, gomock.Any(), gomock.Any()).Return(nil, biz.ErrBudgetExceeded)
	runs.EXPECT().FinishRunWithEvent(gomock.Any(), userID, runID, biz.RunRunning, biz.RunFailed, gomock.Any(), gomock.Any(), fence, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.RunStatus, fields map[string]any, _ biz.RunEvent, _ biz.RunFence, _ time.Time) (biz.Run, error) {
			if fields["error_code"] != "budget_exceeded" {
				t.Fatalf("unexpected failure code: %#v", fields)
			}
			return biz.Run{ID: runID, Status: biz.RunFailed}, nil
		},
	)
	jobs.EXPECT().Complete(gomock.Any(), job.ID, "worker-1", job.FencingToken).Return(nil)

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
}
