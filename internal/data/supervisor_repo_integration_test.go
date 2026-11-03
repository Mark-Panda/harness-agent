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

// TestSupervisorTaskRepositoryLifecycle 验证子任务创建、领取 CAS、终态回写保护与取消。
func TestSupervisorTaskRepositoryLifecycle(t *testing.T) {
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
		Username: "supervisor_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	parent, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "parent", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: parent.ID, UserID: user.ID, Mode: biz.AgentModeSupervisor, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	childA, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, ParentSessionID: &parent.ID, ParentRunID: &run.ID,
		Title: "child-a", Source: biz.SessionSourceAPI, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	childB, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, ParentSessionID: &parent.ID, ParentRunID: &run.ID,
		Title: "child-b", Source: biz.SessionSourceAPI, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	if childA.ParentRunID == nil || *childA.ParentRunID != run.ID {
		t.Fatalf("child session parent binding = %#v", childA)
	}

	repo := NewSupervisorTaskRepository(appDB)
	tasks := []biz.SupervisorTask{
		{ID: uuid.New(), ChildSessionID: childA.ID, WorkerName: "A", WorkerIndex: 1, Task: "任务A", ToolScope: []string{}, Status: biz.SupervisorTaskPending, MaxAttempts: 1, Depth: 1},
		{ID: uuid.New(), ChildSessionID: childB.ID, WorkerName: "B", WorkerIndex: 2, Task: "任务B", ToolScope: []string{}, Status: biz.SupervisorTaskPending, MaxAttempts: 1, Depth: 1},
	}
	created, err := repo.CreateTasks(ctx, user.ID, run.ID, tasks, testOutboxEvent(user.ID, "supervisor.tasks.created"))
	if err != nil || len(created) != 2 {
		t.Fatalf("CreateTasks() = %#v, %v", created, err)
	}
	listed, err := repo.ListTasks(ctx, user.ID, run.ID)
	if err != nil || len(listed) != 2 || listed[0].WorkerName != "A" {
		t.Fatalf("ListTasks() = %#v, %v", listed, err)
	}
	started, err := repo.StartTask(ctx, user.ID, tasks[0].ID, time.Now().UTC())
	if err != nil || started.Status != biz.SupervisorTaskRunning || started.Attempts != 1 {
		t.Fatalf("StartTask() = %#v, %v", started, err)
	}
	if _, err := repo.StartTask(ctx, user.ID, tasks[0].ID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("duplicate start error = %v", err)
	}
	finished, err := repo.FinishTask(ctx, user.ID, tasks[0].ID, biz.SupervisorTaskCompleted, map[string]any{"content": "done"}, "", "", time.Now().UTC())
	if err != nil || finished.Status != biz.SupervisorTaskCompleted || finished.Result["content"] != "done" {
		t.Fatalf("FinishTask() = %#v, %v", finished, err)
	}
	if _, err := repo.FinishTask(ctx, user.ID, tasks[0].ID, biz.SupervisorTaskCompleted, nil, "", "", time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("late finish error = %v", err)
	}
	if err := repo.CancelPendingTasks(ctx, user.ID, run.ID, "parent cancelled", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	listed, _ = repo.ListTasks(ctx, user.ID, run.ID)
	if listed[1].Status != biz.SupervisorTaskCancelled {
		t.Fatalf("pending task not cancelled: %#v", listed[1])
	}
}
