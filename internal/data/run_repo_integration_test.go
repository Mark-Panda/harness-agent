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

// TestRunRepositoryLifecycleAndEventCursor 验证 Run 创建、事件单调序号、游标与跨用户隔离。
func TestRunRepositoryLifecycleAndEventCursor(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	userA, err := users.CreateUser(ctx, biz.User{Username: "run_a_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := users.CreateUser(ctx, biz.User{Username: "run_b_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", userA.ID, userB.ID)

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: userA.ID, Title: "run-session", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(userA.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	deadline := time.Now().UTC().Add(5 * time.Minute)
	key := "run-key-" + suffix
	created, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: userA.ID, Mode: biz.AgentModeReact,
		Status: biz.RunQueued, IdempotencyKey: key, DeadlineAt: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 相同幂等键必须返回同一条 Run。
	again, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: userA.ID, Mode: biz.AgentModeReact,
		Status: biz.RunQueued, IdempotencyKey: key, DeadlineAt: &deadline,
	})
	if err != nil || again.ID != created.ID {
		t.Fatalf("idempotent create = %#v, %v", again, err)
	}
	if _, err := runs.GetRun(ctx, userB.ID, created.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user run lookup error = %v", err)
	}
	// 轻量状态查询必须同样按 user_id 隔离。
	if status, err := runs.GetRunStatus(ctx, userA.ID, created.ID); err != nil || status != biz.RunQueued {
		t.Fatalf("GetRunStatus() = %q, %v", status, err)
	}
	if _, err := runs.GetRunStatus(ctx, userB.ID, created.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user status lookup error = %v", err)
	}

	for index := 0; index < 3; index++ {
		event, err := runs.AppendEvent(ctx, biz.RunEvent{
			RunID: created.ID, UserID: userA.ID, EventType: "message.delta", Payload: map[string]any{"index": index},
		})
		if err != nil {
			t.Fatal(err)
		}
		if event.Seq != int64(index+1) {
			t.Fatalf("event seq = %d, want %d", event.Seq, index+1)
		}
	}
	events, err := runs.ListEvents(ctx, userA.ID, created.ID, 1, 10)
	if err != nil || len(events) != 2 || events[0].Seq != 2 {
		t.Fatalf("ListEvents(after=1) = %#v, %v", events, err)
	}
	if _, err := runs.ListEvents(ctx, userB.ID, created.ID, 0, 10); err != nil {
		t.Fatal(err)
	}
	page, err := runs.ListRuns(ctx, userA.ID, session.ID, 20, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListRuns() = %#v, %v", page, err)
	}

	running, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunQueued, biz.RunRunning, nil)
	if err != nil || running.Status != biz.RunRunning {
		t.Fatalf("TransitionRun(running) = %#v, %v", running, err)
	}
	completed, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunRunning, biz.RunCompleted, map[string]any{"finished_at": time.Now().UTC()})
	if err != nil || completed.Status != biz.RunCompleted {
		t.Fatalf("TransitionRun(completed) = %#v, %v", completed, err)
	}
	if _, err := runs.TransitionRun(ctx, userA.ID, created.ID, biz.RunQueued, biz.RunRunning, nil); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("illegal transition error = %v", err)
	}
}

// TestRunRepositorySerializesSessionRuns 验证同会话 Run 允许排队但必须串行认领。
func TestRunRepositorySerializesSessionRuns(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{Username: "serial_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID)
	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "serial", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "test.session"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	// 触发消息边界与执行截止时间必须随 Run 一起持久化，供 Worker 组装上下文与超时判定。
	deadline := time.Now().UTC().Add(5 * time.Minute)
	triggerID := uuid.New()
	triggerAt := time.Now().UTC()
	first, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		DeadlineAt: &deadline, TriggerMessageID: &triggerID, TriggerMessageAt: &triggerAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.TriggerMessageID == nil || *first.TriggerMessageID != triggerID || first.TriggerMessageAt == nil {
		t.Fatalf("trigger boundary was not persisted: %#v", first)
	}
	// 同会话第二个 Run 允许排队，不再直接返回 409。
	secondDeadline := time.Now().UTC().Add(5 * time.Minute)
	second, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		DeadlineAt: &secondDeadline,
	})
	if err != nil {
		t.Fatalf("queued creation must not conflict: %v", err)
	}
	// 模拟排队两分钟：截止时间固定于创建时刻，若不补偿，后续 Run 认领后执行窗口会被排队耗尽。
	if err := adminDB.Exec("UPDATE agent_runs SET created_at = ? WHERE id = ?", time.Now().UTC().Add(-2*time.Minute), second.ID).Error; err != nil {
		t.Fatalf("backdate queued run: %v", err)
	}
	now := time.Now().UTC()
	claimed, fence, err := runs.ClaimRun(ctx, user.ID, first.ID, "worker-a", time.Minute, now)
	if err != nil || claimed.Status != biz.RunRunning {
		t.Fatalf("ClaimRun(first) = %#v, %#v, %v", claimed, fence, err)
	}
	// 并发 Attempt 乱序写入时，Run 只能保留最大序号，不能回退。
	if err := runs.SetRunAttempt(ctx, user.ID, first.ID, 7, fence); err != nil {
		t.Fatal(err)
	}
	if err := runs.SetRunAttempt(ctx, user.ID, first.ID, 3, fence); err != nil {
		t.Fatal(err)
	}
	current, err := runs.GetRun(ctx, user.ID, first.ID)
	if err != nil || current.AttemptNo != 7 {
		t.Fatalf("attempt number regressed: %#v, %v", current, err)
	}
	// 前序 Run 运行中时，后续 Run 只能继续排队。
	if _, _, err := runs.ClaimRun(ctx, user.ID, second.ID, "worker-b", time.Minute, time.Now().UTC()); !errors.Is(err, biz.ErrSessionRunActive) {
		t.Fatalf("second ClaimRun() error = %v", err)
	}
	if _, err := runs.FinishRun(ctx, user.ID, first.ID, biz.RunRunning, biz.RunCompleted, nil, fence, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	claimedSecond, _, err := runs.ClaimRun(ctx, user.ID, second.ID, "worker-b", time.Minute, time.Now().UTC())
	if err != nil || claimedSecond.Status != biz.RunRunning {
		t.Fatalf("ClaimRun(second) = %#v, %v", claimedSecond, err)
	}
	// 排队时长要补回截止时间，避免后续 Run 一被认领就超时。
	if claimedSecond.DeadlineAt == nil || claimedSecond.DeadlineAt.Before(secondDeadline.Add(time.Minute)) {
		t.Fatalf("queued wait was not compensated: %#v", claimedSecond.DeadlineAt)
	}
}

// TestRunRepositoryListAllRuns 验证管理员全局运行列表的系统作用域、联表字段与筛选。
func TestRunRepositoryListAllRuns(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	repository := NewRunRepository(appDB)
	ctx := context.Background()
	page, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{}, 5, "")
	if err != nil {
		t.Fatalf("list all runs: %v", err)
	}
	if len(page.Items) == 0 {
		return
	}
	first := page.Items[0]
	if first.Source == "" || first.Username == "" || first.Run.UserID == uuid.Nil {
		t.Fatalf("missing joined fields: %#v", first)
	}
	userPage, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{UserID: &first.Run.UserID}, 5, "")
	if err != nil || len(userPage.Items) == 0 {
		t.Fatalf("filter by user returned %#v, %v", userPage, err)
	}
	for _, item := range userPage.Items {
		if item.Run.UserID != first.Run.UserID {
			t.Fatalf("user filter leaked another user: %#v", item)
		}
	}
	sourcePage, err := repository.ListAllRuns(ctx, biz.AdminRunFilter{Source: first.Source}, 5, "")
	if err != nil || len(sourcePage.Items) == 0 {
		t.Fatalf("filter by source returned %#v, %v", sourcePage, err)
	}
	for _, item := range sourcePage.Items {
		if item.Source != first.Source {
			t.Fatalf("source filter leaked another source: %#v", item)
		}
	}
}
