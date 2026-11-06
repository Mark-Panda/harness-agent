//go:build integration

package data

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

// approvalMCPTestServer 构造高风险工具 MCP 服务，并记录 tools/call 的 _meta。
func approvalMCPTestServer(t *testing.T, calls *[]map[string]any) *httptest.Server {
	t.Helper()
	var mutex sync.Mutex
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
				"name": "delete", "description": "delete records", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			mutex.Lock()
			*calls = append(*calls, body.Params)
			mutex.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "deleted"}}, "isError": false}
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

// TestExecutionWorkerApprovalRoundTrip 验证高风险工具在真实数据库中完成审批→恢复→执行的闭环。
func TestExecutionWorkerApprovalRoundTrip(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "approval_worker_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	var mutex sync.Mutex
	modelCalls := 0
	var modelBodies []string
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		mutex.Lock()
		modelCalls++
		call := modelCalls
		modelBodies = append(modelBodies, string(encoded))
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			// 首次调用要求执行高风险工具，触发人工审批暂停。
			_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"","tool_calls":[{"id":"call-approve-1","function":{"name":"mcp_admin_delete","arguments":"{\"id\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"req-2","choices":[{"message":{"content":"已按批准执行"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":6}}`))
	}))
	defer modelServer.Close()
	var mcpCalls []map[string]any
	mcpServer := approvalMCPTestServer(t, &mcpCalls)

	catalog := NewCatalogRepository(appDB)
	providerUUID, modelUUID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := catalog.CreateProvider(ctx, user.ID, biz.Provider{
		ID: providerUUID, Name: "approval_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("ciphertext"), KeyVersion: "v1"}, catalogEvent(user.ID, "provider.created", "llm_provider", providerUUID)); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.CreateModel(ctx, user.ID, biz.Model{
		ID: modelUUID, ProviderID: providerUUID, RemoteModelName: "generation", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 8192,
		Status: biz.ModelEnabled, VerificationSource: "integration-test", VerifiedAt: &now,
		Capabilities: biz.ModelCapabilities{Tools: true},
	}, catalogEvent(user.ID, "model.created", "llm_model", modelUUID)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = adminDB.Exec("DELETE FROM llm_models WHERE id = ?", modelUUID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerUUID).Error
	}()

	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	jobs := NewJobRepository(appDB)
	calls := NewModelCallRepository(appDB)
	steps := NewExecutionStepRepository(appDB)
	approvals := NewToolApprovalRepository(appDB)
	runService, err := biz.NewRunService(runs, sessions,
		biz.WithToolApprovals(approvals), biz.WithRunEnqueuer(jobs))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: integrationCredentials{}, Opener: integrationOpener{},
		Attempts: calls, Steps: steps, Approvals: approvals,
		MCPs: fakeApprovalMCPCredentials{endpoint: mcpServer.URL},
		MCPClientFactory: func(endpoint string, authHeaders http.Header, timeout time.Duration, _ bool) (*mcp.Client, error) {
			return mcp.NewClientWithHeaders(endpoint, authHeaders, timeout, mcp.WithHTTPClient(mcpServer.Client()))
		},
	}, biz.WorkerConfig{
		WorkerID: "approval-worker", JobType: "agent.run.approval", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 30 * time.Second, Heartbeat: 10 * time.Second, CancelPoll: 20 * time.Millisecond,
		ApprovalPoll: 20 * time.Millisecond, MCPTimeout: 5 * time.Second,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 4,
			MaxSwitchesPerRun: 1, RequestTimeout: 10 * time.Second,
			BackoffBase: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionID, runID := uuid.New(), uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "approval", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{
		ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Role: biz.MessageUser, Content: "删除记录",
	}, testOutboxEvent(user.ID, "message.created")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute).UTC()
	mcpID := uuid.NewString()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot: map[string]any{
			"agent": map[string]any{"name": "审批助手"},
			"published_snapshot": map[string]any{"mcps": []any{map[string]any{
				"id": mcpID, "name": "admin", "endpoint": mcpServer.URL,
				"auth_type": "none", "risk_level": "high", "status": "enabled",
			}}},
		},
		ModelPolicySnapshot: workerSnapshot(modelServer.URL, providerUUID.String(), modelUUID.String()),
		DeadlineAt:          &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRun(ctx, user.ID, runID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.approval' WHERE dedupe_key = ?", "run:"+runID.String()).Error; err != nil {
		t.Fatal(err)
	}

	// 第一次执行：模型请求高风险工具，Worker 必须暂停 Run 并释放任务。
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("first ProcessOne() = %t, %v", processed, err)
	}
	waiting, err := runs.GetRun(ctx, user.ID, runID)
	if err != nil || waiting.Status != biz.RunWaitingApproval {
		debugEvents, _ := runs.ListEvents(ctx, user.ID, runID, 0, 200)
		debugSteps, _ := steps.ListSteps(ctx, user.ID, runID, 500)
		t.Fatalf("run status = %s, %v; events=%v; steps=%v; mcpCalls=%v", waiting.Status, err, debugEvents, debugSteps, mcpCalls)
	}
	pending, err := approvals.ListApprovals(ctx, user.ID, runID)
	if err != nil || len(pending) != 1 || pending[0].Status != biz.ApprovalPending {
		t.Fatalf("approvals = %#v, %v", pending, err)
	}
	if len(mcpCalls) != 0 {
		t.Fatalf("high risk tool ran before approval: %#v", mcpCalls)
	}

	// 人工批准：Run 恢复为 running，任务重新可领取。
	decided, err := runService.DecideApproval(ctx, biz.Principal{UserID: user.ID, Role: biz.RoleUser}, runID, pending[0].ID, "approve")
	if err != nil || decided.Status != biz.ApprovalApproved {
		t.Fatalf("DecideApproval() = %#v, %v", decided, err)
	}
	resumed, err := runs.GetRun(ctx, user.ID, runID)
	if err != nil || resumed.Status != biz.RunRunning {
		t.Fatalf("resumed run = %#v, %v", resumed, err)
	}
	if err := adminDB.Exec("UPDATE jobs SET available_at = now() WHERE dedupe_key = ?", "run:"+runID.String()).Error; err != nil {
		t.Fatal(err)
	}

	// 第二次执行：工具以幂等键执行一次，结果回填后 Run 完成。
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("second ProcessOne() = %t, %v", processed, err)
	}
	completed, err := runs.GetRun(ctx, user.ID, runID)
	if err != nil || completed.Status != biz.RunCompleted {
		t.Fatalf("run did not complete: %#v, %v", completed, err)
	}
	finalApprovals, err := approvals.ListApprovals(ctx, user.ID, runID)
	if err != nil || len(finalApprovals) != 1 || finalApprovals[0].Status != biz.ApprovalExecuted || finalApprovals[0].DeliveredAt == nil {
		t.Fatalf("final approvals = %#v, %v", finalApprovals, err)
	}
	mutex.Lock()
	if len(mcpCalls) != 1 {
		mutex.Unlock()
		t.Fatalf("MCP tool calls = %d, want 1", len(mcpCalls))
	}
	meta, _ := mcpCalls[0]["_meta"].(map[string]any)
	if meta == nil || meta["idempotency_key"] != finalApprovals[0].IdempotencyKey {
		mutex.Unlock()
		t.Fatalf("idempotency key was not propagated: %#v", mcpCalls[0])
	}
	callCount := modelCalls
	if callCount != 2 {
		mutex.Unlock()
		t.Fatalf("model calls = %d, want 2", callCount)
	}
	// 数据外泄对抗：模型请求体、Run 事件与执行载荷都不得包含供应商明文凭证。
	for _, body := range modelBodies {
		if strings.Contains(body, "test-key") || strings.Contains(body, "ciphertext") {
			mutex.Unlock()
			t.Fatalf("credential leaked into model request: %s", body)
		}
	}
	mutex.Unlock()
	var leakedPayloads, leakedEvents int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM execution_payloads WHERE run_id = ? AND (content_text LIKE '%test-key%' OR content_json::text LIKE '%test-key%')", runID).Scan(&leakedPayloads).Error; err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Raw("SELECT COUNT(*) FROM run_events WHERE run_id = ? AND payload::text LIKE '%test-key%'", runID).Scan(&leakedEvents).Error; err != nil {
		t.Fatal(err)
	}
	if leakedPayloads != 0 || leakedEvents != 0 {
		t.Fatalf("credential leaked into records: payloads=%d events=%d", leakedPayloads, leakedEvents)
	}

	messages, err := sessions.ListMessages(ctx, user.ID, sessionID, 10, "")
	if err != nil || len(messages.Items) != 4 {
		t.Fatalf("messages = %#v, %v", messages.Items, err)
	}
	// 正序应为 user、assistant(tool_call)、tool(result)、assistant(final)。
	ordered := make([]biz.Message, len(messages.Items))
	for index, message := range messages.Items {
		ordered[len(ordered)-1-index] = message
	}
	if ordered[0].Role != biz.MessageUser || ordered[1].Role != biz.MessageAssistant ||
		ordered[2].Role != biz.MessageTool || ordered[2].ToolCallID != "call-approve-1" ||
		ordered[3].Role != biz.MessageAssistant {
		t.Fatalf("unexpected message order: %#v", ordered)
	}
	events, err := runs.ListEvents(ctx, user.ID, runID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, event := range events {
		types[event.EventType]++
	}
	if types["tool.approval.required"] != 1 || types["run.paused"] != 1 || types["tool.approval.decided"] != 1 ||
		types["run.resumed"] != 1 || types["tool.completed"] != 1 || types["run.completed"] != 1 {
		t.Fatalf("unexpected event types: %#v", types)
	}
	var toolStepStatus string
	if err := adminDB.Raw("SELECT status FROM execution_steps WHERE run_id = ? AND kind = 'tool'", runID).Scan(&toolStepStatus).Error; err != nil || toolStepStatus != "completed" {
		t.Fatalf("tool step status = %q, %v", toolStepStatus, err)
	}
}

// fakeApprovalMCPCredentials 返回高风险 MCP 元数据，凭证为空。
type fakeApprovalMCPCredentials struct {
	// endpoint MCP 服务地址。
	endpoint string
}

// GetMCPCredential 实现 MCP 凭证读取端口。
func (f fakeApprovalMCPCredentials) GetMCPCredential(_ context.Context, mcpID uuid.UUID) (biz.MCP, biz.ProviderCredential, error) {
	return biz.MCP{
		ID: mcpID, Name: "admin", Endpoint: f.endpoint, AuthType: "none",
		RiskLevel: "high", Status: "enabled",
	}, biz.ProviderCredential{}, nil
}
