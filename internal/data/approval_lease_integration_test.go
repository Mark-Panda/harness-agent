//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

func TestExpiredRunLeaseCannotReviveAfterAnotherRunClaimsSession(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	user, err := NewUserRepository(app, auth).CreateUser(ctx, biz.User{
		Username: "lease_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DELETE FROM users WHERE id = ?", user.ID) })
	runs := NewRunRepository(app)
	sessionID, firstID := approvalIntegrationRun(t, ctx, NewSessionRepository(app), runs, user.ID)
	second, err := runs.CreateRun(ctx, biz.Run{
		ID: uuid.New(), SessionID: sessionID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, oldFence, err := runs.ClaimRun(ctx, user.ID, firstID, "old-worker", time.Minute, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, newFence, err := runs.ClaimRun(ctx, user.ID, second.ID, "new-worker", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runs.RenewRunLease(ctx, user.ID, firstID, oldFence, time.Minute, now); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("expired Run renewal = %v", err)
	}
	if err := runs.RenewRunLease(ctx, user.ID, second.ID, newFence, time.Minute, now); err != nil {
		t.Fatalf("active Run renewal = %v", err)
	}
}

func TestApprovalExecutionRequiresActiveRunAndPlan(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	user, err := NewUserRepository(app, auth).CreateUser(ctx, biz.User{
		Username: "approval_lease_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DELETE FROM users WHERE id = ?", user.ID) })
	sessions, runs := NewSessionRepository(app), NewRunRepository(app)
	approvals, plans := NewToolApprovalRepository(app), NewPlanRepository(app)
	runService, err := biz.NewRunService(runs, sessions, biz.WithToolApprovals(approvals))
	if err != nil {
		t.Fatal(err)
	}
	planService, err := biz.NewPlanService(plans, runService)
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: user.ID}
	for _, tc := range []struct {
		name string
		mode biz.AgentMode
		want error
	}{
		{"cancelled run", biz.AgentModeReact, biz.ErrResourceConflict},
		{"expired lease", biz.AgentModeReact, biz.ErrResourceConflict},
		{"taken over lease", biz.AgentModeReact, biz.ErrResourceConflict},
		{"paused plan", biz.AgentModePlan, biz.ErrPlanPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := sessions.CreateSession(ctx, biz.Session{
				ID: uuid.New(), UserID: user.ID, Title: "approval lease", Source: biz.SessionSourceWeb,
				Status: biz.SessionActive, Version: 1,
			}, testOutboxEvent(user.ID, "session.created"))
			if err != nil {
				t.Fatal(err)
			}
			run, err := runs.CreateRun(ctx, biz.Run{
				ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: tc.mode, Status: biz.RunQueued,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			claimAt := now
			if tc.name == "expired lease" || tc.name == "taken over lease" {
				claimAt = now.Add(-2 * time.Minute)
			}
			_, fence, err := runs.ClaimRun(ctx, user.ID, run.ID, "worker", time.Minute, claimAt)
			if err != nil {
				t.Fatal(err)
			}
			callID := "call-1"
			if tc.mode == biz.AgentModePlan {
				planID, stepID := uuid.New(), uuid.New()
				callID = "plan-step-" + stepID.String()
				if _, err := plans.CreatePlan(ctx, user.ID, biz.Plan{
					ID: planID, UserID: user.ID, RunID: run.ID, SessionID: session.ID, Version: 1,
					Status: biz.PlanWaitingApproval, Goal: "approved tool", MaxSteps: 1,
					Steps: []biz.PlanStep{{ID: stepID, StepNo: 1, Title: "delete", ToolName: "delete",
						Status: biz.PlanStepWaitingApproval, MaxAttempts: 1, IdempotencyKey: "test-tool"}},
				}, testOutboxEvent(user.ID, "plan.created")); err != nil {
					t.Fatal(err)
				}
			}
			approval, err := approvals.CreateApproval(ctx, biz.ToolApproval{
				RunID: run.ID, UserID: user.ID, ToolCallID: callID, ToolName: "delete",
				Arguments: json.RawMessage(`{}`), RiskLevel: "high", Status: biz.ApprovalPending, IdempotencyKey: "test-tool",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runService.DecideApproval(ctx, principal, run.ID, approval.ID, "approve"); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "cancelled run":
				if _, err := runService.Cancel(ctx, principal, run.ID); err != nil {
					t.Fatal(err)
				}
			case "paused plan":
				if _, err := planService.Pause(ctx, principal, run.ID); err != nil {
					t.Fatal(err)
				}
			case "taken over lease":
				if _, _, err := runs.ClaimRun(ctx, user.ID, run.ID, "replacement", time.Minute, now); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := approvals.ClaimApprovalExecution(ctx, user.ID, run.ID, approval.ID, fence, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("approval claim with fence %v = %s, %v, want %v", fence, claim.Status, err, tc.want)
			}
			stored, err := approvals.GetApproval(ctx, user.ID, approval.ID)
			if err != nil || stored.Status != biz.ApprovalApproved {
				t.Fatalf("approval after rejected claim = %s, %v", stored.Status, err)
			}
			if tc.mode == biz.AgentModePlan {
				plan, err := plans.GetPlanByRun(ctx, user.ID, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := plans.UpdatePlan(ctx, user.ID, plan.ID, biz.PlanPaused, biz.PlanExecuting, plan.Revision,
					biz.PlanFence{}, biz.PlanUpdate{}); err != nil {
					t.Fatal(err)
				}
				claim, err = approvals.ClaimApprovalExecution(ctx, user.ID, run.ID, approval.ID, fence, now)
				if err != nil || claim.Status != biz.ApprovalExecuting {
					t.Fatalf("approval after plan resume = %s, %v", claim.Status, err)
				}
			}
		})
	}
}
