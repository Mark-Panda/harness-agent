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
	if err := adminDB.Exec("UPDATE agent_runs SET status = 'running', lease_owner = 'worker-a', fencing_token = 7, lease_expires_at = clock_timestamp() + interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
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
	t.Run("step claims require executing plan and live lease", func(t *testing.T) {
		planFence := biz.PlanFence{Owner: "worker-a", Token: 7}
		for _, tc := range []struct {
			status string
			expiry string
			want   error
		}{
			{"paused", "1 minute", biz.ErrPlanPaused},
			{"executing", "-1 minute", biz.ErrResourceConflict},
		} {
			if err := adminDB.Exec("UPDATE plans SET status = ? WHERE id = ?", tc.status, planID).Error; err != nil {
				t.Fatal(err)
			}
			if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() + ?::interval WHERE id = ?", tc.expiry, run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := repo.ClaimPlanStep(ctx, user.ID, planID, steps[0].ID, nil, planFence, time.Now().UTC()); !errors.Is(err, tc.want) {
				t.Errorf("claim with status %s and lease %s = %v, want %v", tc.status, tc.expiry, err, tc.want)
			}
		}
		if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() + interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
			t.Fatal(err)
		}
	})

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

	execution := NewExecutionStepRepository(appDB)
	parent, err := execution.StartStep(ctx, biz.ExecutionStep{UserID: user.ID, RunID: run.ID, Kind: biz.StepKindPlan})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.ClaimPlanStep(ctx, user.ID, planID, steps[0].ID, &parent.ID, fence, time.Now().UTC())
	if err != nil || claimed.Status != biz.PlanStepRunning || claimed.Attempts != 1 {
		t.Fatalf("ClaimPlanStep() = %#v, %v", claimed, err)
	}
	t.Run("expired Run cannot settle Plan or step", func(t *testing.T) {
		if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() - interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() + interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
				t.Error(err)
			}
		})
		if _, err := repo.UpdatePlanStep(ctx, user.ID, planID, claimed.ID, fence, biz.PlanStepUpdate{Status: biz.PlanStepCompleted}); !errors.Is(err, biz.ErrResourceConflict) {
			t.Errorf("expired worker settled a plan step: %v", err)
		}
		current, err := repo.GetPlan(ctx, user.ID, planID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanCompleted, current.Revision, fence, biz.PlanUpdate{}); !errors.Is(err, biz.ErrPlanRevisionConflict) && !errors.Is(err, biz.ErrResourceConflict) {
			t.Errorf("expired worker completed a plan: %v", err)
		}
	})
	tool := biz.ExecutionStep{UserID: user.ID, RunID: run.ID, Kind: biz.StepKindTool, ParentStepID: &parent.ID}
	if err := adminDB.Exec("UPDATE plans SET status = 'paused' WHERE id = ?", planID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := execution.StartPlanToolStep(ctx, tool, fence); !errors.Is(err, biz.ErrPlanPaused) {
		t.Fatalf("start tool in paused model step = %v", err)
	}
	if err := adminDB.Exec("UPDATE plans SET status = 'executing' WHERE id = ?", planID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := execution.StartPlanToolStep(ctx, tool, fence); err != nil {
		t.Fatalf("start tool in claimed model step = %v", err)
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
	t.Run("replacement rolls back unless parent and successor commit together", func(t *testing.T) {
		next := biz.Plan{ID: uuid.New(), UserID: user.ID, RunID: run.ID, SessionID: session.ID,
			Version: 2, ParentPlanID: &planID, Status: biz.PlanExecuting, Goal: "replacement", MaxSteps: 50}
		event := testOutboxEvent(user.ID, "plan.replanned")
		if _, err := repo.ReplacePlan(ctx, user.ID, next, reloaded.Revision-1, fence, event); !errors.Is(err, biz.ErrPlanRevisionConflict) {
			t.Fatalf("replacement with stale revision = %v", err)
		}
		if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() - interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ReplacePlan(ctx, user.ID, next, reloaded.Revision, fence, event); !errors.Is(err, biz.ErrResourceConflict) {
			t.Fatalf("replacement with expired lease = %v", err)
		}
		if err := adminDB.Exec("UPDATE agent_runs SET lease_expires_at = clock_timestamp() + interval '1 minute' WHERE id = ?", run.ID).Error; err != nil {
			t.Fatal(err)
		}
		event.Payload = map[string]any{"invalid_json": make(chan int)}
		if _, err := repo.ReplacePlan(ctx, user.ID, next, reloaded.Revision, fence, event); err == nil {
			t.Fatal("replacement with unencodable outbox event succeeded")
		}
		versions, err := repo.ListPlanVersions(ctx, user.ID, run.ID)
		if err != nil || len(versions) != 1 || versions[0].ID != planID || versions[0].Status != biz.PlanExecuting || versions[0].Revision != reloaded.Revision {
			t.Fatalf("versions after rejected replacements = %#v, %v", versions, err)
		}
	})

	// 终态计划不得被迟到事件回写。
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanCancelled, reloaded.Revision, fence, biz.PlanUpdate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanCancelled, biz.PlanExecuting, reloaded.Revision+1, fence, biz.PlanUpdate{}); !errors.Is(err, biz.ErrPlanTerminal) {
		t.Fatalf("cancelled plan was rewritten: %v", err)
	}
}

// TestPlanRepositoryCompletePlanAtomic 验证最终消息、Outbox、会话版本与完成 CAS 同生共死，
// 并用 Run 行锁确定性地复现“Worker 等待完成锁时人工暂停先提交”的交错。
func TestPlanRepositoryCompletePlanAtomic(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	// 完成与暂停必须使用不同连接；单连接池会让暂停排在等待 Run 行锁的完成事务之后。
	appSQLDB, err := appDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	appSQLDB.SetMaxOpenConns(2)
	appSQLDB.SetMaxIdleConns(2)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "plan_complete_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	session, err := NewSessionRepository(appDB).CreateSession(ctx, biz.Session{
		ID: uuid.New(), UserID: user.ID, Title: "plan completion", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(user.ID, "session.created"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewRunRepository(appDB).CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := biz.PlanFence{Owner: "worker-a", Token: 9}
	if err := adminDB.Exec("UPDATE agent_runs SET status = 'running', lease_owner = ?, fencing_token = ?, lease_expires_at = clock_timestamp() + interval '1 minute' WHERE id = ?", fence.Owner, fence.Token, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewPlanRepository(appDB)
	planID := uuid.New()
	plan, err := repo.CreatePlan(ctx, user.ID, biz.Plan{
		ID: planID, UserID: user.ID, RunID: run.ID, SessionID: session.ID, Version: 1,
		Status: biz.PlanExecuting, Goal: "atomic completion", MaxSteps: 50,
	}, testOutboxEvent(user.ID, "plan.created"))
	if err != nil {
		t.Fatal(err)
	}
	messageID := uuid.New()
	completion := biz.PlanCompletion{
		ExpectedRevision: plan.Revision, Fence: fence, FinishedAt: time.Now().UTC(),
		Message: biz.Message{ID: messageID, SessionID: session.ID, UserID: user.ID, RunID: &run.ID, Role: biz.MessageAssistant, Content: "最终答案"},
		Event: biz.OutboxEvent{UserID: user.ID, EventType: "message.created.agent", AggregateType: "message", AggregateID: messageID.String(),
			DedupeKey: "message.created.agent:" + messageID.String(), Payload: map[string]any{"run_id": run.ID.String()}},
	}
	countRows := func(table string) int64 {
		t.Helper()
		var count int64
		query := adminDB.Table(table).Where("user_id = ?", user.ID)
		if table == "messages" {
			query = query.Where("id = ?", messageID)
		}
		if table == "outbox_events" {
			query = query.Where("dedupe_key = ?", completion.Event.DedupeKey)
		}
		if err := query.Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	sessionVersion := func() int64 {
		t.Helper()
		var version int64
		if err := adminDB.Table("sessions").Select("version").Where("id = ?", session.ID).Scan(&version).Error; err != nil {
			t.Fatal(err)
		}
		return version
	}

	invalid := completion
	invalid.Event.Payload = map[string]any{"invalid_json": make(chan int)}
	if _, _, err := repo.CompletePlan(ctx, user.ID, planID, invalid); err == nil {
		t.Fatal("completion with invalid outbox payload succeeded")
	}
	afterFailure, err := repo.GetPlan(ctx, user.ID, planID)
	if err != nil || afterFailure.Status != biz.PlanExecuting || afterFailure.Revision != plan.Revision ||
		countRows("messages") != 0 || countRows("outbox_events") != 0 || sessionVersion() != 1 {
		t.Fatalf("failed completion was not rolled back: plan=%#v messages=%d outbox=%d session_version=%d err=%v",
			afterFailure, countRows("messages"), countRows("outbox_events"), sessionVersion(), err)
	}

	locked := adminDB.Begin()
	if locked.Error != nil {
		t.Fatal(locked.Error)
	}
	if err := locked.Exec("SELECT id FROM agent_runs WHERE id = ? FOR UPDATE", run.ID).Error; err != nil {
		_ = locked.Rollback().Error
		t.Fatal(err)
	}
	completionResult := make(chan error, 1)
	go func() {
		_, _, err := repo.CompletePlan(ctx, user.ID, planID, completion)
		completionResult <- err
	}()
	paused, pauseErr := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanPaused, plan.Revision, biz.PlanFence{}, biz.PlanUpdate{ErrorCode: "plan_paused"})
	if rollbackErr := locked.Rollback().Error; rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if pauseErr != nil {
		<-completionResult
		t.Fatal(pauseErr)
	}
	if err := <-completionResult; !errors.Is(err, biz.ErrPlanPaused) {
		t.Fatalf("completion after pause = %v", err)
	}
	if countRows("messages") != 0 || countRows("outbox_events") != 0 || sessionVersion() != 1 {
		t.Fatal("paused completion published a message or outbox event")
	}

	resumed, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanPaused, biz.PlanExecuting, paused.Revision, biz.PlanFence{}, biz.PlanUpdate{})
	if err != nil {
		t.Fatal(err)
	}
	completion.ExpectedRevision = resumed.Revision
	completion.FinishedAt = time.Now().UTC()
	completed, stored, err := repo.CompletePlan(ctx, user.ID, planID, completion)
	if err != nil || completed.Status != biz.PlanCompleted || stored.ID != messageID || stored.Content != "最终答案" {
		t.Fatalf("CompletePlan() = plan=%#v message=%#v err=%v", completed, stored, err)
	}
	if countRows("messages") != 1 || countRows("outbox_events") != 1 || sessionVersion() != 2 {
		t.Fatalf("successful completion did not commit exactly once: messages=%d outbox=%d session_version=%d",
			countRows("messages"), countRows("outbox_events"), sessionVersion())
	}
	if _, err := repo.UpdatePlan(ctx, user.ID, planID, biz.PlanExecuting, biz.PlanPaused, completed.Revision, biz.PlanFence{}, biz.PlanUpdate{}); !errors.Is(err, biz.ErrPlanRevisionConflict) {
		t.Fatalf("completed plan was paused by a stale worker view: %v", err)
	}
}

func TestRevisePlanToolEffects(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	user, err := NewUserRepository(app, auth).CreateUser(ctx, biz.User{
		Username: "plan_revision_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DELETE FROM users WHERE id = ?", user.ID) })
	sessions, runs, jobs := NewSessionRepository(app), NewRunRepository(app), NewJobRepository(app)
	plans, execution, approvals := NewPlanRepository(app), NewExecutionStepRepository(app), NewToolApprovalRepository(app)
	runService, err := biz.NewRunService(runs, sessions, biz.WithRunEnqueuer(jobs))
	if err != nil {
		t.Fatal(err)
	}
	planService, err := biz.NewPlanService(plans, runService)
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: user.ID}
	for _, scenario := range []string{"direct tool running", "model tool dispatched", "approval executing", "approval executed awaiting result", "completed tool"} {
		t.Run(scenario, func(t *testing.T) {
			session, err := sessions.CreateSession(ctx, biz.Session{
				ID: uuid.New(), UserID: user.ID, Title: "revision", Source: biz.SessionSourceWeb, Status: biz.SessionActive, Version: 1,
			}, testOutboxEvent(user.ID, "session.created"))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			deadline := now.Add(time.Minute)
			run, err := runs.CreateRun(ctx, biz.Run{
				ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModePlan, Status: biz.RunQueued, DeadlineAt: &deadline,
			})
			if err != nil {
				t.Fatal(err)
			}
			draft := biz.PlanDraft{Goal: "revise", Steps: []biz.PlanDraftStep{{StepNo: 1, Title: "send", ToolName: "send"}}}
			step := biz.PlanStep{ID: uuid.New(), StepNo: 1, Title: "send", ToolName: "send", Status: biz.PlanStepRunning,
				Attempts: 1, MaxAttempts: 1, IdempotencyKey: "original"}
			switch scenario {
			case "model tool dispatched":
				modelStep, err := execution.StartStep(ctx, biz.ExecutionStep{UserID: user.ID, RunID: run.ID, Kind: biz.StepKindPlan})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := execution.StartStep(ctx, biz.ExecutionStep{UserID: user.ID, RunID: run.ID, ParentStepID: &modelStep.ID,
					Kind: biz.StepKindTool, ToolName: "send", Status: biz.StepCompleted}); err != nil {
					t.Fatal(err)
				}
				step.ToolName, step.ExecutionStepID, draft.Steps[0].ToolName = "", &modelStep.ID, ""
			case "approval executing", "approval executed awaiting result":
				step.Status = biz.PlanStepWaitingApproval
				status := biz.ApprovalExecuting
				if scenario == "approval executed awaiting result" {
					status = biz.ApprovalExecuted
				}
				if _, err := approvals.CreateApproval(ctx, biz.ToolApproval{RunID: run.ID, UserID: user.ID, ToolName: step.ToolName,
					ToolCallID: "plan-step-" + step.ID.String(), Status: status, Arguments: []byte(`{}`), IdempotencyKey: step.IdempotencyKey}); err != nil {
					t.Fatal(err)
				}
			case "completed tool":
				step.Status, step.Result = biz.PlanStepCompleted, map[string]any{"content": "sent"}
				draft.Steps[0].Title = "renamed send"
			}
			plan, err := plans.CreatePlan(ctx, user.ID, biz.Plan{ID: uuid.New(), UserID: user.ID, RunID: run.ID, SessionID: session.ID,
				Version: 1, Status: biz.PlanPaused, Goal: "send", MaxSteps: 50, DeadlineAt: &deadline, UpdatedAt: now.Add(-time.Second),
				Steps: []biz.PlanStep{step}}, testOutboxEvent(user.ID, "plan.created"))
			if err != nil {
				t.Fatal(err)
			}
			revised, err := planService.Revise(ctx, principal, run.ID, draft)
			if scenario == "completed tool" {
				if err != nil || len(revised.Steps) != 1 || revised.Steps[0].Status != biz.PlanStepCompleted || revised.Steps[0].Result["content"] != "sent" {
					t.Fatalf("Revise() completed tool = %#v, %v", revised, err)
				}
				versions, err := plans.ListPlanVersions(ctx, user.ID, run.ID)
				if err != nil || len(versions) != 2 || versions[0].ID != revised.ID || versions[1].ID != plan.ID || versions[1].Status != biz.PlanCancelled {
					t.Fatalf("versions after completed tool revision = %#v, %v", versions, err)
				}
				if _, err := planService.Pause(ctx, principal, run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := planService.Revise(ctx, principal, run.ID, biz.PlanDraft{Goal: "omit tool", Steps: []biz.PlanDraftStep{{StepNo: 1, Title: "model step"}}}); err != nil {
					t.Fatal(err)
				}
				if _, err := planService.Pause(ctx, principal, run.ID); err != nil {
					t.Fatal(err)
				}
				restored, err := planService.Revise(ctx, principal, run.ID, draft)
				if err != nil || restored.Version != 4 || len(restored.Steps) != 1 || restored.Steps[0].Status != biz.PlanStepCompleted || restored.Steps[0].Result["content"] != "sent" {
					t.Fatalf("older completed operation was lost after reintroduction: %#v, %v", restored, err)
				}
				return
			}
			if !errors.Is(err, biz.ErrResourceConflict) {
				t.Fatalf("Revise() with %s = %v, want resource conflict", scenario, err)
			}
			versions, err := plans.ListPlanVersions(ctx, user.ID, run.ID)
			if err != nil || len(versions) != 1 || versions[0].ID != plan.ID || versions[0].Status != biz.PlanPaused ||
				versions[0].DeadlineAt == nil || !versions[0].DeadlineAt.Equal(deadline) {
				t.Fatalf("plan versions after rejected revision = %#v, %v", versions, err)
			}
			persisted, err := runs.GetRun(ctx, user.ID, run.ID)
			if err != nil || persisted.DeadlineAt == nil || !persisted.DeadlineAt.Equal(deadline) {
				t.Fatalf("run deadline after rejected revision = %v, %v", persisted.DeadlineAt, err)
			}
			if exists, err := jobs.HasRunJob(ctx, user.ID, run.ID); err != nil || exists {
				t.Fatalf("job after rejected revision = %t, %v", exists, err)
			}
		})
	}
}
