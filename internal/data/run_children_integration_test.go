//go:build integration

package data

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestRunCancelTerminatesPlanAndSupervisorChildren 验证 Run 取消时同事务终止存活的 Plan 与 Supervisor 子状态。
func TestRunCancelTerminatesPlanAndSupervisorChildren(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "run_children_" + strings.ReplaceAll(uuid.NewString(), "-", ""), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })

	sessions, runs := NewSessionRepository(appDB), NewRunRepository(appDB)
	sessionID, runID := approvalIntegrationRun(t, ctx, sessions, runs, user.ID)
	now := time.Now().UTC()
	_, fence, err := runs.ClaimRun(ctx, user.ID, runID, "worker", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}

	plans := NewPlanRepository(appDB)
	planID, stepID := uuid.New(), uuid.New()
	if _, err := plans.CreatePlan(ctx, user.ID, biz.Plan{
		ID: planID, UserID: user.ID, RunID: runID, SessionID: sessionID, Version: 1,
		Status: biz.PlanExecuting, Goal: "cancel children", MaxSteps: 1,
		Steps: []biz.PlanStep{{
			ID: stepID, StepNo: 1, Title: "running step", ToolName: "lookup",
			Status: biz.PlanStepRunning, MaxAttempts: 1, IdempotencyKey: "cancel-step",
		}},
	}, testOutboxEvent(user.ID, "plan.created")); err != nil {
		t.Fatal(err)
	}

	childSession, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, ParentSessionID: &sessionID, ParentRunID: &runID,
		Title: "child", Source: biz.SessionSourceAPI, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := NewSupervisorTaskRepository(appDB)
	taskID := uuid.New()
	if _, err := tasks.CreateTasks(ctx, user.ID, runID, []biz.SupervisorTask{{
		ID: taskID, ChildSessionID: childSession.ID, WorkerName: "worker", WorkerIndex: 1,
		Task: "pending task", Status: biz.SupervisorTaskPending, MaxAttempts: 2, Depth: 1,
	}}, testOutboxEvent(user.ID, "supervisor.tasks.created"), fence); err != nil {
		t.Fatal(err)
	}

	if _, err := runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, now); err != nil {
		t.Fatal(err)
	}

	plan, err := plans.GetPlan(ctx, user.ID, planID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != biz.PlanCancelled {
		t.Fatalf("plan status = %s, want cancelled", plan.Status)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Status != biz.PlanStepCancelled {
		t.Fatalf("plan steps = %#v", plan.Steps)
	}
	listed, err := tasks.ListTasks(ctx, user.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Status != biz.SupervisorTaskCancelled {
		t.Fatalf("supervisor tasks = %#v", listed)
	}
}

// TestRunCancelPreservesPausedPlan 验证取消旧 Run 时不能连带取消暂停计划，
// 否则“继续”触发的计划重绑会因状态 CAS 失败而丢失全部已完成进度。
func TestRunCancelPreservesPausedPlan(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "run_paused_" + strings.ReplaceAll(uuid.NewString(), "-", ""), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })

	sessions, runs := NewSessionRepository(appDB), NewRunRepository(appDB)
	sessionID, runID := approvalIntegrationRun(t, ctx, sessions, runs, user.ID)
	now := time.Now().UTC()
	if _, _, err := runs.ClaimRun(ctx, user.ID, runID, "worker", time.Minute, now); err != nil {
		t.Fatal(err)
	}

	plans := NewPlanRepository(appDB)
	planID := uuid.New()
	if _, err := plans.CreatePlan(ctx, user.ID, biz.Plan{
		ID: planID, UserID: user.ID, RunID: runID, SessionID: sessionID, Version: 1,
		Status: biz.PlanPaused, Goal: "resume me", MaxSteps: 2,
		Steps: []biz.PlanStep{
			{ID: uuid.New(), StepNo: 1, Title: "done", Status: biz.PlanStepCompleted, MaxAttempts: 2, IdempotencyKey: "paused-1"},
			{ID: uuid.New(), StepNo: 2, Title: "pending", Status: biz.PlanStepPending, MaxAttempts: 2, IdempotencyKey: "paused-2"},
		},
	}, testOutboxEvent(user.ID, "plan.created")); err != nil {
		t.Fatal(err)
	}

	if _, err := runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, now); err != nil {
		t.Fatal(err)
	}

	plan, err := plans.GetPlan(ctx, user.ID, planID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != biz.PlanPaused {
		t.Fatalf("paused plan status after run cancel = %s, want paused", plan.Status)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Status != biz.PlanStepCompleted || plan.Steps[1].Status != biz.PlanStepPending {
		t.Fatalf("paused plan steps after run cancel = %#v", plan.Steps)
	}
	_, newRunID := approvalIntegrationRun(t, ctx, sessions, runs, user.ID)
	if _, err := plans.RebindPlanRun(ctx, user.ID, planID, newRunID, plan.Revision, nil, now); err != nil {
		t.Fatalf("RebindPlanRun() after cancel = %v", err)
	}
}
