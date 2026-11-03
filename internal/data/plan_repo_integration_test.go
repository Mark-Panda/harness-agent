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

// TestPlanRepositoryLifecycle 验证计划 CAS、步骤 fencing、用量累加与终态保护。
func TestPlanRepositoryLifecycle(t *testing.T) {
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
		Username: "plan_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "plan", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunRepository(appDB)
	run, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 直接持有一个有效租约，模拟 Worker 已认领 Run。
	if err := adminDB.Exec("UPDATE agent_runs SET status = 'running', lease_owner = 'worker-a', fencing_token = 7 WHERE id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}

	repo := NewPlanRepository(appDB)
	planID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	steps := []biz.PlanStep{
		{ID: uuid.New(), PlanID: planID, UserID: user.ID, StepNo: 1, Title: "收集", Status: biz.PlanStepPending, MaxAttempts: 2, IdempotencyKey: "p1", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), PlanID: planID, UserID: user.ID, StepNo: 2, Title: "总结", Status: biz.PlanStepPending, MaxAttempts: 2, DependsOn: []int{1}, IdempotencyKey: "p2", CreatedAt: now, UpdatedAt: now},
	}
	created, err := repo.CreatePlan(ctx, user.ID, biz.Plan{
		ID: planID, UserID: user.ID, RunID: run.ID, SessionID: session.ID, Version: 1,
		Status: biz.PlanExecuting, Goal: "生成报告", ConfigSnapshot: map[string]any{"mode": "plan"},
		MaxSteps: 50, Revision: 0, CreatedAt: now, UpdatedAt: now, Steps: steps,
	}, testOutboxEvent(user.ID, "plan.created"))
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != planID || len(created.Steps) != 2 || len(created.Steps[1].DependsOn) != 1 {
		t.Fatalf("created plan = %#v", created)
	}
	loaded, err := repo.GetPlanByRun(ctx, user.ID, run.ID)
	if err != nil || loaded.Goal != "生成报告" || loaded.Steps[0].Status != biz.PlanStepPending {
		t.Fatalf("GetPlanByRun() = %#v, %v", loaded, err)
	}

	fence := biz.PlanFence{Owner: "worker-a", Token: 7}
	updated, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanWaitingApproval, 0, fence, biz.PlanUpdate{})
	if err != nil || updated.Status != biz.PlanWaitingApproval || updated.Revision != 1 {
		t.Fatalf("UpdatePlan() = %#v, %v", updated, err)
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanWaitingApproval, biz.PlanExecuting, 0, fence, biz.PlanUpdate{}); !errors.Is(err, biz.ErrPlanRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanWaitingApproval, biz.PlanExecuting, 1, biz.PlanFence{Owner: "worker-b", Token: 7}, biz.PlanUpdate{}); !errors.Is(err, biz.ErrResourceConflict) && !errors.Is(err, biz.ErrPlanRevisionConflict) {
		t.Fatalf("stale fence error = %v", err)
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanWaitingApproval, biz.PlanExecuting, 1, fence, biz.PlanUpdate{}); err != nil {
		t.Fatal(err)
	}

	claimed, err := repo.ClaimPlanStep(ctx, user.ID, planID, steps[0].ID, nil, fence, time.Now().UTC())
	if err != nil || claimed.Status != biz.PlanStepRunning || claimed.Attempts != 1 {
		t.Fatalf("ClaimPlanStep() = %#v, %v", claimed, err)
	}
	// 同一 running 步骤不能被重复领取，阻止多 Worker 重复推进。
	if _, err := repo.ClaimPlanStep(ctx, user.ID, planID, steps[0].ID, nil, fence, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("duplicate claim error = %v", err)
	}
	if _, err := repo.UpdatePlanStep(ctx, user.ID, planID, steps[0].ID, biz.PlanFence{Owner: "worker-b", Token: 7}, biz.PlanStepUpdate{
		Status: biz.PlanStepCompleted, Result: map[string]any{"content": "stale"},
	}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale step update error = %v", err)
	}
	finished, err := repo.UpdatePlanStep(ctx, user.ID, planID, steps[0].ID, fence, biz.PlanStepUpdate{
		Status: biz.PlanStepCompleted, Result: map[string]any{"content": "done"},
	})
	if err != nil || finished.Status != biz.PlanStepCompleted || finished.Result["content"] != "done" {
		t.Fatalf("UpdatePlanStep() = %#v, %v", finished, err)
	}
	// 终态步骤不得被迟到的 Worker 再次回写。
	if _, err := repo.UpdatePlanStep(ctx, user.ID, planID, steps[0].ID, fence, biz.PlanStepUpdate{
		Status: biz.PlanStepFailed, ErrorCode: "late_event",
	}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("late step update error = %v", err)
	}
	if err := repo.AddPlanUsage(ctx, user.ID, planID, 10, 20, 0.5, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repo.GetPlanByRun(ctx, user.ID, run.ID)
	if err != nil || reloaded.TotalInputTokens != 10 || reloaded.TotalOutputTokens != 20 || reloaded.EstimatedCost != 0.5 {
		t.Fatalf("usage totals = %#v, %v", reloaded, err)
	}

	// 终态计划不得被迟到事件回写。
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanCancelled, reloaded.Revision, fence, biz.PlanUpdate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanCancelled, biz.PlanExecuting, reloaded.Revision+1, fence, biz.PlanUpdate{}); !errors.Is(err, biz.ErrPlanTerminal) {
		t.Fatalf("cancelled plan was rewritten: %v", err)
	}
}
