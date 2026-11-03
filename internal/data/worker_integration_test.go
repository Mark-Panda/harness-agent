//go:build integration

package data

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// integrationCredentials 返回可解密的测试凭证，避免依赖真实供应商。
type integrationCredentials struct{}

// GetProviderCredential 实现凭证读取端口。
func (integrationCredentials) GetProviderCredential(_ context.Context, providerID uuid.UUID) (biz.Provider, biz.ProviderCredential, error) {
	return biz.Provider{ID: providerID, Status: biz.ProviderEnabled, BaseURL: "https://models.example.com"},
		biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("ciphertext")}, nil
}

// integrationOpener 返回固定明文密钥。
type integrationOpener struct{}

// Open 实现凭证解密端口。
func (integrationOpener) Open(_ string, _ []byte) ([]byte, error) { return []byte("test-key"), nil }

// workerSnapshot 构造执行层集成测试使用的策略快照。
func workerSnapshot(baseURL, providerID, modelID string) map[string]any {
	return map[string]any{
		"policy_id": uuid.NewString(), "policy_version": 1, "min_context_tokens": int64(1_000_000),
		"candidates": []any{map[string]any{
			"priority": 1, "model_id": modelID, "provider_id": providerID,
			"provider_base_url": baseURL, "remote_model_name": "generation",
			"context_window_tokens": int64(1_000_000), "max_output_tokens": 4096,
			"capabilities":            map[string]any{"tools": true},
			"input_price_per_million": 1.0, "output_price_per_million": 1.0,
		}},
	}
}

// TestExecutionWorkerIntegrationSuccessAndCancel 验证真实数据库下的成功执行链与取消中止。
func TestExecutionWorkerIntegrationSuccessAndCancel(t *testing.T) {
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
		Username: "worker_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-1","choices":[{"message":{"content":"真实链路回答"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`))
	}))
	defer server.Close()

	catalog := NewCatalogRepository(appDB)
	providerUUID, modelUUID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := catalog.CreateProvider(ctx, user.ID, biz.Provider{
		ID: providerUUID, Name: "worker_provider_" + suffix, ProviderType: "openai_compatible",
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
	providerID, modelID := providerUUID.String(), modelUUID.String()
	principal := biz.Principal{UserID: user.ID, Role: biz.RoleUser}
	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	jobs := NewJobRepository(appDB)
	calls := NewModelCallRepository(appDB)
	runService, err := biz.NewRunService(runs, sessions)
	if err != nil {
		t.Fatal(err)
	}
	// 通用 Worker 构造函数参数保持与真实仓储一致，避免测试使用不同实现。
	steps := NewExecutionStepRepository(appDB)
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: integrationCredentials{}, Opener: integrationOpener{},
		Attempts: calls, Steps: steps,
	}, biz.WorkerConfig{
		WorkerID: "integration-worker", JobType: "agent.run.test", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 30 * time.Second, Heartbeat: 10 * time.Second, CancelPoll: 20 * time.Millisecond,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 4,
			MaxSwitchesPerRun: 1, RequestTimeout: 10 * time.Second,
			BackoffBase: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, sessionID := uuid.New(), uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "integration", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Role: biz.MessageUser, Content: "你好"}, testOutboxEvent(user.ID, "message.created")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute).UTC()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "集成助手"}},
		ModelPolicySnapshot: workerSnapshot(server.URL, providerID, modelID), DeadlineAt: &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRun(ctx, user.ID, runID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.test' WHERE dedupe_key = ?", "run:"+runID.String()).Error; err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessOne(ctx); err != nil || !processed {
		t.Fatalf("ProcessOne() = %t, %v", processed, err)
	}
	completed, err := runs.GetRun(ctx, user.ID, runID)
	if err != nil || completed.Status != biz.RunCompleted || completed.FinishedAt == nil {
		t.Fatalf("run did not complete: %#v, %v", completed, err)
	}
	messages, err := sessions.ListMessages(ctx, user.ID, sessionID, 10, "")
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages = %#v, %v", messages.Items, err)
	}
	events, err := runs.ListEvents(ctx, user.ID, runID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var hasCompleted, hasMessageEvent bool
	var completedMessageID string
	for _, event := range events {
		switch event.EventType {
		case "run.completed":
			hasCompleted = true
			completedMessageID, _ = event.Payload["message_id"].(string)
		case "message.created":
			hasMessageEvent = true
		}
	}
	if !hasCompleted || !hasMessageEvent {
		t.Fatalf("completion or message event missing: %#v", events)
	}
	// run.completed 中的 message_id 必须指向已落库的最终回答，证明事件与消息一致。
	if messages.Items[0].Role != biz.MessageAssistant || messages.Items[0].ID.String() != completedMessageID {
		t.Fatalf("completed message linkage = %#v, event id %s", messages.Items[0], completedMessageID)
	}
	var callStatus string
	if err := adminDB.Raw("SELECT status FROM model_calls WHERE run_id = ?", runID).Scan(&callStatus).Error; err != nil || callStatus != "succeeded" {
		t.Fatalf("model call status = %q, %v", callStatus, err)
	}
	// 执行树必须包含模型步骤，且模型调用关联到该步骤。
	var stepCount, payloadCount int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM execution_steps WHERE run_id = ? AND kind = 'model' AND status = 'completed'", runID).Scan(&stepCount).Error; err != nil || stepCount != 1 {
		t.Fatalf("execution step count = %d, %v", stepCount, err)
	}
	if err := adminDB.Raw("SELECT COUNT(*) FROM execution_payloads WHERE run_id = ?", runID).Scan(&payloadCount).Error; err != nil || payloadCount != 2 {
		t.Fatalf("execution payload count = %d, %v", payloadCount, err)
	}
	var responseText string
	if err := adminDB.Raw("SELECT content_text FROM execution_payloads WHERE run_id = ? AND payload_type = 'model_response'", runID).Scan(&responseText).Error; err != nil || responseText != "真实链路回答" {
		t.Fatalf("model response payload = %q, %v", responseText, err)
	}
	var linkedSteps int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM model_calls WHERE run_id = ? AND step_id IS NOT NULL", runID).Scan(&linkedSteps).Error; err != nil || linkedSteps != 1 {
		t.Fatalf("model call step linkage = %d, %v", linkedSteps, err)
	}

	// 取消链路：模型调用阻塞时通过 RunService 取消，Worker 必须停止且不写终态。
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"req-2","choices":[{"message":{"content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer slowServer.Close()
	cancelSession := uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: cancelSession, UserID: user.ID, Title: "cancel", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{ID: uuid.New(), SessionID: cancelSession, UserID: user.ID, Role: biz.MessageUser, Content: "取消测试"}, testOutboxEvent(user.ID, "message.created")); err != nil {
		t.Fatal(err)
	}
	cancelRunID := uuid.New()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: cancelRunID, SessionID: cancelSession, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "集成助手"}},
		ModelPolicySnapshot: workerSnapshot(slowServer.URL, providerID, modelID), DeadlineAt: &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.EnqueueRun(ctx, user.ID, cancelRunID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.test' WHERE dedupe_key = ?", "run:"+cancelRunID.String()).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, processErr := worker.ProcessOne(ctx)
		done <- processErr
	}()
	// 等待模型调用记录出现，确保 Run 已进入 running 且模型请求已发起。
	deadlineWait := time.Now().Add(2 * time.Second)
	for {
		var count int64
		_ = adminDB.Raw("SELECT COUNT(*) FROM model_calls WHERE run_id = ?", cancelRunID).Scan(&count).Error
		if count > 0 {
			break
		}
		if time.Now().After(deadlineWait) {
			t.Fatal("worker did not start the model call")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := runService.Cancel(ctx, principal, cancelRunID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("cancelled ProcessOne() error = %v", err)
	}
	cancelled, err := runs.GetRun(ctx, user.ID, cancelRunID)
	if err != nil || cancelled.Status != biz.RunCancelled {
		t.Fatalf("run was not cancelled: %#v, %v", cancelled, err)
	}
	cancelledMessages, err := sessions.ListMessages(ctx, user.ID, cancelSession, 10, "")
	if err != nil || len(cancelledMessages.Items) != 1 {
		t.Fatalf("cancelled run wrote an assistant message: %#v, %v", cancelledMessages.Items, err)
	}
}
