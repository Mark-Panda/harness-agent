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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// TestExecutionWorkerPlanMode 验证 Plan 模式生成、步骤依赖执行、终态与最终消息落库。
func TestExecutionWorkerPlanMode(t *testing.T) {
	t.Run("generate and complete", func(t *testing.T) { testExecutionWorkerPlanMode(t, false, false) })
	t.Run("replan and complete", func(t *testing.T) { testExecutionWorkerPlanMode(t, true, false) })
	t.Run("pause during replan", func(t *testing.T) { testExecutionWorkerPlanMode(t, true, true) })
}

func testExecutionWorkerPlanMode(t *testing.T, seedFailedStep, pauseDuringReplan bool) {
	t.Helper()
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
		Username: "plan_worker_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	jobs := NewJobRepository(appDB)
	plans := NewPlanRepository(appDB)
	runService, err := biz.NewRunService(runs, sessions, biz.WithRunEnqueuer(jobs))
	if err != nil {
		t.Fatal(err)
	}
	planService, err := biz.NewPlanService(plans, runService)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, runID := uuid.New(), uuid.New()
	principal := biz.Principal{UserID: user.ID}
	paused := make(chan error, 1)
	planner := `{"goal":"生成季度总结","steps":[{"step_no":1,"title":"收集数据","description":"汇总数据","depends_on":[],"tool_name":"","arguments":{}},{"step_no":2,"title":"生成总结","description":"输出总结","depends_on":[1],"tool_name":"","arguments":{}}]}`
	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNo := providerRequests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		content := "模型步骤结果"
		if strings.Contains(body, "生成可执行计划") {
			content = planner
			if requestNo == 1 && !seedFailedStep {
				content = `{"goal":"生成季度总结","steps":[{"step_no":1,"title":"查询","description":"查询数据","tool_name":"unavailable_tool"}]}`
			}
		}
		if pauseDuringReplan && requestNo == 1 && strings.Contains(body, "修正后的完整可执行计划") {
			_, err := planService.Pause(r.Context(), principal, runID)
			paused <- err
		}
		// 计划汇总器调用需要可区分的最终答案；P1-7 修正预算后该调用会真实成功。
		if strings.Contains(body, "计划目标：") {
			content = "汇总后的最终答案"
		}
		response := map[string]any{
			"id":      "req-" + suffix,
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 6},
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
		ID: providerUUID, Name: "plan_provider_" + suffix, ProviderType: "openai_compatible",
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

	calls := NewModelCallRepository(appDB)
	steps := NewExecutionStepRepository(appDB)
	providerLimiter := &planWorkerProviderLimiter{}
	worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
		Jobs: jobs, Runs: runs, Sessions: sessions, Providers: integrationCredentials{baseURL: server.URL}, Opener: integrationOpener{},
		Attempts: calls, Steps: steps, Plans: plans,
	}, biz.WorkerConfig{
		WorkerID: "plan-worker", JobType: "agent.run.test", Concurrency: 1, PollInterval: time.Millisecond,
		Lease: 30 * time.Second, Heartbeat: 10 * time.Second, CancelPoll: 20 * time.Millisecond,
		Routing: model.RouterConfig{
			MinContextTokens: 1_000_000, MaxAttemptsPerModel: 2, MaxAttemptsPerRun: 6,
			MaxSwitchesPerRun: 1, RequestTimeout: 10 * time.Second,
			BackoffBase: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxRetryAfter: time.Second,
			Limiter: providerLimiter,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: user.ID, Title: "plan", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created")); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Role: biz.MessageUser, Content: "生成本季度总结"}, testOutboxEvent(user.ID, "message.created")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute).UTC()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "计划助手"}},
		ModelPolicySnapshot: workerSnapshot(server.URL, providerUUID.String(), modelUUID.String()), DeadlineAt: &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if seedFailedStep {
		if _, err := plans.CreatePlan(ctx, user.ID, biz.Plan{
			ID: uuid.New(), UserID: user.ID, RunID: runID, SessionID: sessionID, Version: 1, Status: biz.PlanExecuting,
			Goal: "生成季度总结", MaxSteps: 50, DeadlineAt: &deadline,
			Steps: []biz.PlanStep{{ID: uuid.New(), StepNo: 1, Title: "查询", Status: biz.PlanStepFailed, Attempts: 1, MaxAttempts: 1}},
		}, testOutboxEvent(user.ID, "plan.created")); err != nil {
			t.Fatal(err)
		}
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
	versions, err := plans.ListPlanVersions(ctx, user.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if pauseDuringReplan {
		select {
		case err := <-paused:
			if err != nil {
				t.Fatalf("Pause() during replan = %v", err)
			}
		default:
			t.Fatal("provider did not receive a replan request")
		}
		if len(versions) != 1 || versions[0].Status != biz.PlanPaused {
			t.Fatalf("plan versions after pause during replan = %#v", versions)
		}
		run, err := runs.GetRun(ctx, user.ID, runID)
		if err != nil || run.Status != biz.RunQueued || providerRequests.Load() != 1 {
			t.Fatalf("run after replan pause = %s, %v, provider requests = %d", run.Status, err, providerRequests.Load())
		}
		return
	}
	if seedFailedStep && (len(versions) != 2 || versions[1].Status != biz.PlanFailed || versions[1].ErrorCode != "plan_replanned") {
		t.Fatalf("plan versions after replan = %#v", versions)
	}
	completed, err := runs.GetRun(ctx, user.ID, runID)
	if err != nil || completed.Status != biz.RunCompleted || completed.FinishedAt == nil {
		t.Fatalf("run status = %#v, %v", completed, err)
	}
	plan, err := plans.GetPlanByRun(ctx, user.ID, runID)
	if err != nil || plan.Status != biz.PlanCompleted || len(plan.Steps) != 2 {
		t.Fatalf("plan = %#v, %v", plan, err)
	}
	wantRequests := int32(5)
	if seedFailedStep {
		wantRequests = 4
	}
	if plan.TotalInputTokens != int64(wantRequests)*12 || plan.TotalOutputTokens != int64(wantRequests)*6 {
		t.Errorf("plan usage = %d input, %d output for %d requests", plan.TotalInputTokens, plan.TotalOutputTokens, wantRequests)
	}
	if providerRequests.Load() != wantRequests || providerLimiter.acquired.Load() != wantRequests || providerLimiter.released.Load() != wantRequests {
		t.Errorf("provider requests/acquires/releases = %d/%d/%d, want %d each", providerRequests.Load(), providerLimiter.acquired.Load(), providerLimiter.released.Load(), wantRequests)
	}
	for _, step := range plan.Steps {
		if step.Status != biz.PlanStepCompleted || step.Result["content"] == nil {
			t.Fatalf("plan step not completed: %#v", step)
		}
	}
	messages, err := sessions.ListMessages(ctx, user.ID, sessionID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	assistantFound := false
	for _, message := range messages.Items {
		// 汇总器成功时最终消息为模型汇总文本；失败回退时为“计划执行完成”拼接。
		if message.Role == biz.MessageAssistant && (strings.Contains(message.Content, "汇总后的最终答案") || strings.Contains(message.Content, "计划执行完成")) {
			assistantFound = true
		}
	}
	if !assistantFound {
		t.Fatalf("plan final assistant message not persisted: %#v", messages.Items)
	}
	events, err := runs.ListEvents(ctx, user.ID, runID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.EventType] = true
	}
	versionEvent := "plan.created"
	if seedFailedStep {
		versionEvent = "plan.replanned"
	}
	for _, want := range []string{versionEvent, "plan.step.started", "plan.step.completed", "plan.completed", "run.completed"} {
		if !seen[want] {
			t.Fatalf("missing run event %q in %#v", want, seen)
		}
	}
}

type planWorkerProviderLimiter struct {
	acquired atomic.Int32
	released atomic.Int32
}

func (l *planWorkerProviderLimiter) Acquire(_ context.Context, _ string) (func(), error) {
	l.acquired.Add(1)
	return func() { l.released.Add(1) }, nil
}

func (l *planWorkerProviderLimiter) TryAcquire(ctx context.Context, key string) (func(), bool, error) {
	release, err := l.Acquire(ctx, key)
	return release, err == nil, err
}
