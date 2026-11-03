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
