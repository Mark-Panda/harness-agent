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

// TestPlanRepositoryResumePaths 验证计划延期、版本列表、暂停查找、Run 迁移与步骤决议。
func TestPlanRepositoryResumePaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "plan_resume_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()

	sessions := NewSessionRepository(appDB)
	session, err := sessions.CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "plan-resume", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
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
	repo := NewPlanRepository(appDB)
	now := time.Now().UTC().Truncate(time.Millisecond)

	// 参数校验。
	if _, err := repo.ExtendPlanDeadline(ctx, user.ID, uuid.Nil, time.Now().UTC(), time.Time{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("extend nil plan error = %v", err)
	}

	planID := uuid.New()
	modelStep := biz.PlanStep{ID: uuid.New(), PlanID: planID, UserID: user.ID, StepNo: 1, Title: "模型步骤", Status: biz.PlanStepRunning, Attempts: 1, MaxAttempts: 3, LeaseOwner: "worker-a", FenceToken: 3, IdempotencyKey: "s1", CreatedAt: now, UpdatedAt: now}
	approvalStep := biz.PlanStep{ID: uuid.New(), PlanID: planID, UserID: user.ID, StepNo: 2, Title: "审批步骤", Status: biz.PlanStepWaitingApproval, MaxAttempts: 3, ToolName: "shell", IdempotencyKey: "s2", CreatedAt: now, UpdatedAt: now}
	completedStep := biz.PlanStep{ID: uuid.New(), PlanID: planID, UserID: user.ID, StepNo: 3, Title: "已完成", Status: biz.PlanStepCompleted, MaxAttempts: 3, IdempotencyKey: "s3", CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
	planSteps := []biz.PlanStep{modelStep, approvalStep, completedStep}
	execution := NewExecutionStepRepository(appDB)
	for _, status := range []biz.StepStatus{biz.StepRunning, biz.StepCompleted, biz.StepReconciliationRequired} {
		parent, err := execution.StartStep(ctx, biz.ExecutionStep{RunID: run.ID, UserID: user.ID, Kind: biz.StepKindPlan})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := execution.StartStep(ctx, biz.ExecutionStep{
			RunID: run.ID, UserID: user.ID, ParentStepID: &parent.ID, Kind: biz.StepKindTool, Status: status,
		}); err != nil {
			t.Fatal(err)
		}
		step := modelStep
		step.ID, step.StepNo, step.ExecutionStepID, step.IdempotencyKey = uuid.New(), len(planSteps)+1, &parent.ID, string(status)
		planSteps = append(planSteps, step)
	}
	approvals := NewToolApprovalRepository(appDB)
	var executedStepNo int
	for _, status := range []biz.ApprovalStatus{biz.ApprovalExecuting, biz.ApprovalExecuted, biz.ApprovalFailed} {
		step := approvalStep
		step.ID, step.StepNo, step.IdempotencyKey = uuid.New(), len(planSteps)+1, string(status)
		approval := biz.ToolApproval{
			RunID: run.ID, UserID: user.ID, ToolCallID: "plan-step-" + step.ID.String(), ToolName: step.ToolName,
			Status: status, Arguments: []byte(`{}`), IdempotencyKey: step.IdempotencyKey,
		}
		if status == biz.ApprovalExecuted {
			executedStepNo = step.StepNo
			approval.ResultJSON = []byte(`{"result":"saved"}`)
		}
		if _, err := approvals.CreateApproval(ctx, approval); err != nil {
			t.Fatal(err)
		}
		planSteps = append(planSteps, step)
	}
	plan, err := repo.CreatePlan(ctx, user.ID, biz.Plan{
		ID: planID, UserID: user.ID, RunID: run.ID, SessionID: session.ID, Version: 1,
		Status: biz.PlanPaused, Goal: "恢复计划", ConfigSnapshot: map[string]any{"mode": "plan"},
		MaxSteps: 50, Revision: 2, CreatedAt: now, UpdatedAt: now,
		Steps: planSteps,
	}, testOutboxEvent(user.ID, "plan.created"))
	if err != nil {
		t.Fatal(err)
	}

	// ExtendPlanDeadline：非终态可延期，且不改 revision。
	deadline := now.Add(2 * time.Hour)
	extended, err := repo.ExtendPlanDeadline(ctx, user.ID, planID, deadline, now)
	if err != nil || extended.DeadlineAt == nil || !extended.DeadlineAt.Equal(deadline) || extended.Revision != plan.Revision {
		t.Fatalf("ExtendPlanDeadline() = %#v, %v", extended, err)
	}
	versions, err := repo.ListPlanVersions(ctx, user.ID, run.ID)
	if err != nil || len(versions) != 1 || versions[0].Version != 1 {
		t.Fatalf("ListPlanVersions() = %#v, %v", versions, err)
	}
	if len(versions[0].Steps) != 0 {
		t.Fatalf("ListPlanVersions must not load steps: %#v", versions[0].Steps)
	}

	// FindPausedPlanBySession 返回最近暂停计划；完成态计划不再返回。
	paused, err := repo.FindPausedPlanBySession(ctx, user.ID, session.ID)
	if err != nil || paused.ID != planID {
		t.Fatalf("FindPausedPlanBySession() = %#v, %v", paused, err)
	}

	// RebindPlanRun：迁移到新 Run，模型/审批步骤回 pending，已完成步骤保留。
	newRun, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	newDeadline := now.Add(3 * time.Hour)
	rebound, err := repo.RebindPlanRun(ctx, user.ID, planID, newRun.ID, paused.Revision, &newDeadline, now)
	if err != nil {
		t.Fatalf("RebindPlanRun() = %v", err)
	}
	if rebound.RunID != newRun.ID || rebound.Status != biz.PlanExecuting || rebound.Revision != paused.Revision+1 {
		t.Fatalf("rebound plan = %#v", rebound)
	}
	// Simulate call records written before Plan counters were updated atomically.
	attempts := NewModelCallRepository(appDB)
	for _, runID := range []uuid.UUID{run.ID, newRun.ID} {
		call, err := attempts.CreateModelCall(ctx, biz.ModelCall{
			RunID: runID, UserID: user.ID, AttemptNo: 1, InputTokens: 12, OutputTokens: 6, EstimatedCost: 0.125,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attempts.FinishModelCall(ctx, user.ID, call.ID, biz.ModelCallUpdate{
			Status: biz.ModelCallSucceeded, InputTokens: 12, OutputTokens: 6, EstimatedCost: 0.125,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runs.AppendEvent(ctx, biz.RunEvent{
		RunID: newRun.ID, UserID: user.ID, EventType: "plan.resumed",
		Payload: map[string]any{"plan_id": planID.String(), "previous_run_id": run.ID.String()},
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		usage, err := repo.RecoverPlanUsage(ctx, user.ID, newRun.ID)
		if err != nil || usage.InputTokens != 24 || usage.OutputTokens != 12 || usage.EstimatedCost != 0.25 {
			t.Fatalf("recovered usage = %+v, %v", usage, err)
		}
	}
	recovered, err := repo.GetPlan(ctx, user.ID, planID)
	if err != nil || recovered.TotalInputTokens != 24 || recovered.Revision != rebound.Revision {
		t.Fatalf("stored Plan usage after recovery = %+v, %v", recovered, err)
	}
	for _, call := range []biz.ModelCall{
		{InputTokens: 1, OutputTokens: 1, PlanMaxTokens: 37},
		{EstimatedCost: 0.02, PlanMaxCost: 0.26},
	} {
		call.RunID, call.UserID, call.AttemptNo = newRun.ID, user.ID, 2
		call.RequestManifest = map[string]any{"plan_usage": true}
		if _, err := attempts.CreateModelCall(ctx, call); !errors.Is(err, biz.ErrPlanBudget) {
			t.Fatalf("reservation above durable usage limit = %v", err)
		}
	}
	if _, err := attempts.CreateModelCall(ctx, biz.ModelCall{
		RunID: newRun.ID, UserID: user.ID, AttemptNo: 2, RequestManifest: map[string]any{"plan_usage": true},
		InputTokens: 1, OutputTokens: 1, PlanMaxTokens: 38,
	}); err != nil {
		t.Fatalf("reservation within durable usage limit = %v", err)
	}
	steps := map[int]biz.PlanStep{}
	for _, step := range rebound.Steps {
		steps[step.StepNo] = step
	}
	if steps[1].Status != biz.PlanStepPending || steps[1].Attempts != 0 || steps[1].LeaseOwner != "" {
		t.Fatalf("model step not reset: %#v", steps[1])
	}
	if steps[2].Status != biz.PlanStepPending || steps[2].Attempts != 0 {
		t.Fatalf("approval step not reset: %#v", steps[2])
	}
	if steps[3].Status != biz.PlanStepCompleted {
		t.Fatalf("completed step was touched: %#v", steps[3])
	}
	for _, original := range planSteps[3:] {
		step := steps[original.StepNo]
		if step.StepNo == executedStepNo {
			content, _ := step.Result["content"].(string)
			if step.Status != biz.PlanStepCompleted || !strings.Contains(content, "saved") {
				t.Errorf("executed approval result not preserved: %+v", step)
			}
			continue
		}
		if step.Status != biz.PlanStepUnknown || step.ErrorCode != "tool_execution_unknown" {
			t.Errorf("step %d with prior dispatch evidence reset to %s (%s)", step.StepNo, step.Status, step.ErrorCode)
		}
	}
	// 旧 revision 重复迁移必须冲突。
	if _, err := repo.RebindPlanRun(ctx, user.ID, planID, newRun.ID, paused.Revision, &newDeadline, now); !errors.Is(err, biz.ErrPlanRevisionConflict) {
		t.Fatalf("stale rebind error = %v", err)
	}
	// 迁移后暂停计划消失。
	if _, err := repo.FindPausedPlanBySession(ctx, user.ID, session.ID); err == nil {
		t.Fatal("paused plan still found after rebind")
	}

	// ResolvePlanStep：pending -> skipped 合法；已终态再决议冲突；非法迁移被拒。
	resolved, err := repo.ResolvePlanStep(ctx, user.ID, planID, steps[1].ID, biz.PlanStepPending, biz.PlanStepUpdate{Status: biz.PlanStepSkipped})
	if err != nil || resolved.Status != biz.PlanStepSkipped {
		t.Fatalf("ResolvePlanStep() = %#v, %v", resolved, err)
	}
	// 终态作为来源状态在校验层直接拒绝。
	if _, err := repo.ResolvePlanStep(ctx, user.ID, planID, steps[1].ID, biz.PlanStepSkipped, biz.PlanStepUpdate{Status: biz.PlanStepSkipped}); !errors.Is(err, biz.ErrPlanStepTerminal) {
		t.Fatalf("terminal resolve error = %v", err)
	}
	// 来源状态与库中当前状态不一致时按冲突处理。
	if _, err := repo.ResolvePlanStep(ctx, user.ID, planID, steps[1].ID, biz.PlanStepPending, biz.PlanStepUpdate{Status: biz.PlanStepSkipped}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("stale status resolve error = %v", err)
	}
	if _, err := repo.ResolvePlanStep(ctx, user.ID, planID, steps[2].ID, biz.PlanStepPending, biz.PlanStepUpdate{Status: biz.PlanStepCompleted}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("illegal transition error = %v", err)
	}
}
