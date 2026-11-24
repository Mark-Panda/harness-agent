//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
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

func TestApprovalExecutionRequiresActiveRun(t *testing.T) {
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
	approvals := NewToolApprovalRepository(app)
	runService, err := biz.NewRunService(runs, sessions, biz.WithToolApprovals(approvals))
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: user.ID}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"cancelled run", biz.ErrResourceConflict},
		{"expired lease", biz.ErrResourceConflict},
		{"taken over lease", biz.ErrResourceConflict},
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
				ID: uuid.New(), SessionID: session.ID, UserID: user.ID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
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
			approval, err := approvals.CreateApproval(ctx, biz.ToolApproval{
				RunID: run.ID, UserID: user.ID, ToolCallID: "call-1", ToolName: "delete",
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
		})
	}
}

func TestRunCancelApprovalAtomicityAndRaces(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	user, err := NewUserRepository(app, auth).CreateUser(ctx, biz.User{
		Username: "approval_cancel_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DELETE FROM users WHERE id = ?", user.ID) })
	sessions, runs := NewSessionRepository(app), NewRunRepository(app)
	approvals := NewToolApprovalRepository(app)
	newRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		_, runID := approvalIntegrationRun(t, ctx, sessions, runs, user.ID)
		return runID
	}
	newPending := func(t *testing.T, runID uuid.UUID, callID string) biz.ToolApproval {
		t.Helper()
		approval, err := approvals.CreateApproval(ctx, biz.ToolApproval{
			RunID: runID, UserID: user.ID, ToolCallID: callID, ToolName: "delete",
			Arguments: json.RawMessage(`{}`), RiskLevel: "high", IdempotencyKey: callID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return approval
	}

	t.Run("approval update failure rolls back run cancellation", func(t *testing.T) {
		runID := newRun(t)
		pending := newPending(t, runID, "pending")
		nonPending := make(map[biz.ApprovalStatus]biz.ToolApproval, 3)
		for _, status := range []biz.ApprovalStatus{biz.ApprovalExecuting, biz.ApprovalExecuted, biz.ApprovalFailed} {
			approval := newPending(t, runID, string(status))
			if err := admin.Exec("UPDATE tool_approvals SET status = ? WHERE id = ?", status, approval.ID).Error; err != nil {
				t.Fatal(err)
			}
			nonPending[status] = approval
		}
		writeErr := errors.New("injected approval denial failure")
		const callback = "test:cancel-approval-denial"
		if err := app.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "tool_approvals" {
				_ = tx.AddError(writeErr)
			}
		}); err != nil {
			t.Fatal(err)
		}
		_, cancelErr := runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, time.Now().UTC())
		if err := app.Callback().Update().Remove(callback); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(cancelErr, writeErr) {
			t.Fatalf("Cancel error = %v, want %v", cancelErr, writeErr)
		}
		run, err := runs.GetRun(ctx, user.ID, runID)
		if err != nil || run.Status != biz.RunQueued || run.FinishedAt != nil {
			t.Fatalf("Run after rollback = %#v, %v", run, err)
		}
		stored, err := approvals.GetApproval(ctx, user.ID, pending.ID)
		if err != nil || stored.Status != biz.ApprovalPending {
			t.Fatalf("Pending approval after rollback = %#v, %v", stored, err)
		}

		now := time.Now().UTC()
		cancelled, err := runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, now)
		if err != nil || cancelled.Status != biz.RunCancelled || cancelled.FinishedAt == nil {
			t.Fatalf("Cancel retry = %#v, %v", cancelled, err)
		}
		denied, err := approvals.GetApproval(ctx, user.ID, pending.ID)
		if err != nil || denied.Status != biz.ApprovalDenied || denied.DecidedBy == nil || *denied.DecidedBy != user.ID ||
			denied.DecidedAt == nil || !denied.DecidedAt.Equal(now) {
			t.Fatalf("Denied approval = %#v, %v", denied, err)
		}
		for status, approval := range nonPending {
			unchanged, err := approvals.GetApproval(ctx, user.ID, approval.ID)
			if err != nil || unchanged.Status != status {
				t.Fatalf("%s approval changed = %#v, %v", status, unchanged, err)
			}
		}
		if _, err := approvals.CreateApproval(ctx, biz.ToolApproval{
			RunID: runID, UserID: user.ID, ToolCallID: "after-cancel", ToolName: "delete",
			Arguments: json.RawMessage(`{}`), RiskLevel: "high", IdempotencyKey: "after-cancel",
		}); !errors.Is(err, biz.ErrResourceConflict) {
			t.Fatalf("CreateApproval after Cancel error = %v", err)
		}
	})

	t.Run("decide and cancel serialize on the parent run", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			runID := newRun(t)
			approval := newPending(t, runID, "decide-race")
			if _, err := runs.TransitionRun(ctx, user.ID, runID, biz.RunQueued, biz.RunRunning, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := runs.TransitionRun(ctx, user.ID, runID, biz.RunRunning, biz.RunWaitingApproval, nil); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var decideErr, cancelErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, decideErr = approvals.DecideApproval(ctx, user.ID, runID, approval.ID, biz.ApprovalDecisionApprove, user.ID, time.Now().UTC())
			}()
			go func() {
				defer wg.Done()
				<-start
				_, cancelErr = runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, time.Now().UTC())
			}()
			close(start)
			wg.Wait()
			if cancelErr != nil {
				t.Fatalf("iteration %d Cancel error = %v", i, cancelErr)
			}
			if decideErr != nil && !errors.Is(decideErr, biz.ErrResourceConflict) {
				t.Fatalf("iteration %d Decide error = %v", i, decideErr)
			}
			stored, err := approvals.GetApproval(ctx, user.ID, approval.ID)
			if err != nil {
				t.Fatal(err)
			}
			if decideErr == nil && stored.Status != biz.ApprovalApproved {
				t.Fatalf("iteration %d approved-before-cancel status = %s", i, stored.Status)
			}
			if decideErr != nil && stored.Status != biz.ApprovalDenied {
				t.Fatalf("iteration %d cancelled-before-decide status = %s", i, stored.Status)
			}
			if pending, err := approvals.CountPendingApprovals(ctx, user.ID, runID); err != nil || pending != 0 {
				t.Fatalf("iteration %d pending = %d, %v", i, pending, err)
			}
			if _, err := runs.ResumeRun(ctx, user.ID, runID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
				t.Fatalf("iteration %d Resume after Cancel error = %v", i, err)
			}
			if _, err := approvals.ClaimApprovalExecution(ctx, user.ID, runID, approval.ID, biz.RunFence{Owner: "worker", Token: 1}, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
				t.Fatalf("iteration %d ClaimExecution after Cancel error = %v", i, err)
			}
		}
	})

	t.Run("create and cancel cannot leave a new pending approval", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			runID := newRun(t)
			start := make(chan struct{})
			var created biz.ToolApproval
			var createErr, cancelErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				created, createErr = approvals.CreateApproval(ctx, biz.ToolApproval{
					RunID: runID, UserID: user.ID, ToolCallID: "create-race", ToolName: "delete",
					Arguments: json.RawMessage(`{}`), RiskLevel: "high", IdempotencyKey: "create-race",
				})
			}()
			go func() {
				defer wg.Done()
				<-start
				_, cancelErr = runs.CancelRunAndDenyPendingApprovals(ctx, user.ID, runID, time.Now().UTC())
			}()
			close(start)
			wg.Wait()
			if cancelErr != nil {
				t.Fatalf("iteration %d Cancel error = %v", i, cancelErr)
			}
			if createErr != nil && !errors.Is(createErr, biz.ErrResourceConflict) {
				t.Fatalf("iteration %d Create error = %v", i, createErr)
			}
			items, err := approvals.ListApprovals(ctx, user.ID, runID)
			if err != nil {
				t.Fatal(err)
			}
			if createErr == nil {
				if len(items) != 1 || items[0].ID != created.ID || items[0].Status != biz.ApprovalDenied {
					t.Fatalf("iteration %d created-before-cancel approvals = %#v", i, items)
				}
			} else if len(items) != 0 {
				t.Fatalf("iteration %d cancelled-before-create approvals = %#v", i, items)
			}
			if pending, err := approvals.CountPendingApprovals(ctx, user.ID, runID); err != nil || pending != 0 {
				t.Fatalf("iteration %d pending = %d, %v", i, pending, err)
			}
		}
	})
}
