//go:build integration

package data

import (
	"context"
	"encoding/json"
	"io"
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

// TestExecutionWorkerSupervisorMode 验证真实数据库下的 Worker 委派、独立子会话、汇总与部分失败。
func TestExecutionWorkerSupervisorMode(t *testing.T) {
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
		Username: "supervisor_worker_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		content := "Worker 结果"
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			content = `{"workers":[{"name":"A","task":"任务A","tools":[]},{"name":"B","task":"任务B","tools":[]}]}`
		case strings.Contains(body, "请汇总以下 Worker 结果"):
			content = "汇总：A 与 B 完成"
		}
		response := map[string]any{
			"id":      "req-" + suffix,
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		}
		encoded, _ := json.Marshal(response)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer server.Close()

	catalog := NewCatalogRepository(appDB)
	providerUUID, modelUUID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := catalog.CreateProvider(ctx, user.ID, biz.Provider{
		ID: providerUUID, Name: "supervisor_provider_" + suffix, ProviderType: "openai_compatible",
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
	supRepo := NewSupervisorTaskRepository(appDB)
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: integrationCredentials{}, Opener: integrationOpener{},
		Attempts: calls, Steps: steps, Approvals: approvals, SupervisorTasks: supRepo,
	}, biz.WorkerConfig{
		WorkerID: "supervisor-worker", JobType: "agent.run.test", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 30 * time.Second, Heartbeat: 10 * time.Second, CancelPoll: 20 * time.Millisecond,
		SupervisorMaxWorkers: 4,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6,
			MaxSwitchesPerRun: 1, RequestTimeout: 10 * time.Second,
			BackoffBase: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxRetryAfter: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, runID := uuid.New(), uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "supervisor", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Role: biz.MessageUser, Content: "分别处理两个任务"}, testOutboxEvent(user.ID, "message.created")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute).UTC()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: user.ID, Mode: biz.AgentModeSupervisor, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "主管助手"}},
		ModelPolicySnapshot: workerSnapshot(server.URL, providerUUID.String(), modelUUID.String()), DeadlineAt: &deadline,
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
	if err != nil || completed.Status != biz.RunCompleted {
		tasks, _ := supRepo.ListTasks(ctx, user.ID, runID)
		events, _ := runs.ListEvents(ctx, user.ID, runID, 0, 200)
		t.Fatalf("run = %#v, %v tasks=%#v events=%#v", completed, err, tasks, events)
	}
	tasks, err := supRepo.ListTasks(ctx, user.ID, runID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks = %#v, %v", tasks, err)
	}
	for _, task := range tasks {
		if task.Status != biz.SupervisorTaskCompleted || task.ChildSessionID == uuid.Nil {
			t.Fatalf("task = %#v", task)
		}
		child, err := sessions.GetSession(ctx, user.ID, task.ChildSessionID)
		if err != nil || child.ParentRunID == nil || *child.ParentRunID != runID {
			t.Fatalf("child session = %#v, %v", child, err)
		}
	}
	messages, err := sessions.ListMessages(ctx, user.ID, sessionID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	assistantFound := false
	for _, message := range messages.Items {
		if message.Role == biz.MessageAssistant && strings.Contains(message.Content, "汇总") {
			assistantFound = true
		}
	}
	if !assistantFound {
		t.Fatalf("supervisor final message not persisted: %#v", messages.Items)
	}
	events, err := runs.ListEvents(ctx, user.ID, runID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.EventType] = true
	}
	for _, want := range []string{"supervisor.tasks.created", "supervisor.task.completed", "supervisor.completed", "run.completed"} {
		if !seen[want] {
			t.Fatalf("missing supervisor event %q in %#v", want, seen)
		}
	}
}
