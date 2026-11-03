//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestJobAndRunLeaseFencing 验证任务 SKIP LOCKED 认领、租约恢复与 fencing 提交语义。
func TestJobAndRunLeaseFencing(t *testing.T) {
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
		Username: "lease_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "lease", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs := NewJobRepository(appDB)
	if err := jobs.EnqueueRun(ctx, user.ID, run.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 使用独立任务类型，避免本地运行的真实 Worker 抢走测试任务。
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.test' WHERE dedupe_key = ?", "run:"+run.ID.String()).Error; err != nil {
		t.Fatal(err)
	}
	// 重复入队必须被幂等键吸收，不产生第二个任务。
	if err := jobs.EnqueueRun(ctx, user.ID, run.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 使用独立任务类型，避免本地运行的真实 Worker 抢走测试任务。
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.test' WHERE dedupe_key = ?", "run:"+run.ID.String()).Error; err != nil {
		t.Fatal(err)
	}
	job, claimed, err := jobs.Claim(ctx, "worker-a", "agent.run.test", 20*time.Millisecond, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("Claim() = %#v, %t, %v", job, claimed, err)
	}
	if job.Attempts != 1 || job.FencingToken != 1 {
		t.Fatalf("unexpected job lease state: %#v", job)
	}
	// 租约有效期内其他 Worker 不得重复领取。
	if _, claimed, err := jobs.Claim(ctx, "worker-b", "agent.run.test", time.Minute, time.Now().UTC()); err != nil || claimed {
		t.Fatalf("second claim should be empty: %t, %v", claimed, err)
	}
	time.Sleep(150 * time.Millisecond)
	reclaimed, claimed, err := jobs.Claim(ctx, "worker-b", "agent.run.test", time.Minute, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("expired lease was not reclaimed: %t, %v", claimed, err)
	}
	if reclaimed.ID != job.ID || reclaimed.FencingToken != 2 || reclaimed.Attempts != 2 {
		t.Fatalf("unexpected reclaimed job: %#v", reclaimed)
	}
	// 过期 Worker 不得续约或完成任务。
	if err := jobs.Renew(ctx, job.ID, "worker-a", 1, time.Minute, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale renew error = %v", err)
	}
	if err := jobs.Complete(ctx, job.ID, "worker-a", 1); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale complete error = %v", err)
	}
	if err := jobs.Renew(ctx, reclaimed.ID, "worker-b", 2, time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Complete(ctx, reclaimed.ID, "worker-b", 2); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	claimedRun, fence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker-a", 20*time.Millisecond, now)
	if err != nil || fence.Token != 1 || claimedRun.Status != biz.RunRunning {
		t.Fatalf("ClaimRun() = %#v, %#v, %v", claimedRun, fence, err)
	}
	time.Sleep(150 * time.Millisecond)
	recovered, fenceB, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker-b", time.Minute, time.Now().UTC())
	if err != nil || fenceB.Token != 2 || recovered.Status != biz.RunRunning {
		t.Fatalf("recover ClaimRun() = %#v, %#v, %v", recovered, fenceB, err)
	}
	// 过期 Worker 的事件与终态提交必须被 fencing 拒绝。
	if _, err := runs.AppendEventFenced(ctx, biz.RunEvent{RunID: run.ID, UserID: user.ID, EventType: "stale"}, fence); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale event error = %v", err)
	}
	if _, err := runs.FinishRun(ctx, user.ID, run.ID, biz.RunRunning, biz.RunCompleted, nil, fence, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale finish error = %v", err)
	}
	event, err := runs.AppendEventFenced(ctx, biz.RunEvent{RunID: run.ID, UserID: user.ID, EventType: "step.completed"}, fenceB)
	if err != nil || event.Seq != 1 {
		t.Fatalf("fenced event = %#v, %v", event, err)
	}
	// 事件不可序列化时终态必须回滚，避免出现只有状态或只有事件的半成品。
	if _, err := runs.FinishRunWithEvent(ctx, user.ID, run.ID, biz.RunRunning, biz.RunCompleted, nil,
		biz.RunEvent{RunID: run.ID, UserID: user.ID, EventType: "run.completed", Payload: map[string]any{"bad": make(chan int)}},
		fenceB, time.Now().UTC()); err == nil {
		t.Fatal("unsupported event payload was accepted")
	}
	current, err := runs.GetRun(ctx, user.ID, run.ID)
	if err != nil || current.Status != biz.RunRunning {
		t.Fatalf("finish rollback failed: %#v, %v", current, err)
	}
	completed, err := runs.FinishRunWithEvent(ctx, user.ID, run.ID, biz.RunRunning, biz.RunCompleted, nil,
		biz.RunEvent{RunID: run.ID, UserID: user.ID, EventType: "run.completed", Payload: map[string]any{"status": "ok"}},
		fenceB, time.Now().UTC())
	if err != nil || completed.Status != biz.RunCompleted {
		t.Fatalf("atomic finish failed: %#v, %v", completed, err)
	}
	events, err := runs.ListEvents(ctx, user.ID, run.ID, 0, 200)
	if err != nil || len(events) != 2 || events[1].EventType != "run.completed" {
		t.Fatalf("atomic event timeline = %#v, %v", events, err)
	}
}

// TestJobReleaseDoesNotConsumeAttempts 验证舱壁排队退回任务不会消耗失败预算。
func TestJobReleaseDoesNotConsumeAttempts(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	users := NewUserRepository(appDB, authDB)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := users.CreateUser(ctx, biz.User{
		Username: "release_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "release", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued})
	if err != nil {
		t.Fatal(err)
	}
	jobs := NewJobRepository(appDB)
	if err := jobs.EnqueueRun(ctx, user.ID, run.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := adminDB.Exec("UPDATE jobs SET job_type = 'agent.run.test' WHERE dedupe_key = ?", "run:"+run.ID.String()).Error; err != nil {
		t.Fatal(err)
	}
	job, claimed, err := jobs.Claim(ctx, "worker-a", "agent.run.test", time.Minute, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatalf("Claim() = %t, %v", claimed, err)
	}
	if err := jobs.Release(ctx, job.ID, "worker-a", job.FencingToken, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Status   string
		Attempts int
	}
	if err := adminDB.Raw("SELECT status, attempts FROM jobs WHERE id = ?", job.ID).Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Status != "pending" || state.Attempts != 0 {
		t.Fatalf("released job = %#v", state)
	}
	reclaimed, claimed, err := jobs.Claim(ctx, "worker-a", "agent.run.test", time.Minute, time.Now().UTC())
	if err != nil || !claimed || reclaimed.Attempts != 1 {
		t.Fatalf("reclaim after release = %#v, %t, %v", reclaimed, claimed, err)
	}
}

// TestAppendMessageIdempotent 验证相同消息 ID 重试只落一条消息与一条审计事件。
func TestAppendMessageIdempotent(t *testing.T) {
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
		Username: "idem_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "idempotent", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	message := biz.Message{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Role: biz.MessageAssistant,
		Content: "同一条回答", ToolCallID: "call-1", ToolName: "search",
	}
	event := biz.OutboxEvent{
		UserID: user.ID, EventType: "message.created.agent", AggregateType: "message",
		AggregateID: message.ID.String(), DedupeKey: "message.created.agent:" + message.ID.String(),
		Payload: map[string]any{"run_id": uuid.NewString()},
	}
	for i := 0; i < 2; i++ {
		if _, err := sessions.AppendMessageIdempotent(ctx, message, event); err != nil {
			t.Fatalf("append attempt %d: %v", i, err)
		}
	}
	var count int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM messages WHERE id = ?", message.ID).Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("message count = %d, %v", count, err)
	}
	if err := adminDB.Raw("SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = ?", message.ID.String()).Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("outbox count = %d, %v", count, err)
	}
	stored, err := sessions.ListMessages(ctx, user.ID, session.ID, 10, "")
	if err != nil || len(stored.Items) != 1 || stored.Items[0].ToolCallID != "call-1" || stored.Items[0].ToolName != "search" {
		t.Fatalf("stored message = %#v, %v", stored.Items, err)
	}
}

// TestModelCallRepositoryLifecycle 验证 Attempt 唯一性与终态只能结算一次。
func TestModelCallRepositoryLifecycle(t *testing.T) {
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
		Username: "call_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "calls", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued})
	if err != nil {
		t.Fatal(err)
	}
	calls := NewModelCallRepository(appDB)
	call, err := calls.CreateModelCall(ctx, biz.ModelCall{
		ID: uuid.New(), RunID: run.ID, UserID: user.ID, AttemptNo: 1, Status: biz.ModelCallRunning,
		RequestHash: []byte(strings.Repeat("h", 32)), RequestManifest: map[string]any{"model": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calls.CreateModelCall(ctx, biz.ModelCall{
		ID: uuid.New(), RunID: run.ID, UserID: user.ID, AttemptNo: 1, Status: biz.ModelCallRunning,
	}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("duplicate attempt error = %v", err)
	}
	duration := int64(120)
	finished, err := calls.FinishModelCall(ctx, user.ID, call.ID, biz.ModelCallUpdate{
		Status: biz.ModelCallSucceeded, InputTokens: 10, OutputTokens: 5, DurationMS: &duration,
		FinishReason: "stop", CompletedAt: time.Now().UTC(),
	})
	if err != nil || finished.Status != biz.ModelCallSucceeded {
		t.Fatalf("FinishModelCall() = %#v, %v", finished, err)
	}
	if _, err := calls.FinishModelCall(ctx, user.ID, call.ID, biz.ModelCallUpdate{Status: biz.ModelCallFailed}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("double finish error = %v", err)
	}
}
