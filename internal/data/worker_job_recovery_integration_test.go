//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestExecutionWorkerRecoversTerminalWriteAfterLastAttempt(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	for _, tc := range []struct {
		name         string
		runStatus    biz.RunStatus
		jobStatus    string
		finalStatus  biz.RunStatus
		eventType    string
		expectedCode string
		blockJob     bool
	}{
		{name: "retry deadline settlement", runStatus: biz.RunRunning, jobStatus: "pending", finalStatus: biz.RunFailed, eventType: "run.failed", expectedCode: "run_deadline_exceeded"},
		{name: "retry approval expiry", runStatus: biz.RunWaitingApproval, jobStatus: "pending", finalStatus: biz.RunFailed, eventType: "run.failed", expectedCode: "approval_timeout"},
		{name: "recover after database blocks both writes", runStatus: biz.RunRunning, jobStatus: "pending", finalStatus: biz.RunReconciliationRequired, eventType: "run.reconciliation_required", expectedCode: "execution_outcome_unknown", blockJob: true},
		{name: "recover stranded queued run", runStatus: biz.RunQueued, jobStatus: "dead", finalStatus: biz.RunFailed, eventType: "run.failed", expectedCode: "execution_aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
			appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
			authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
			ctx := context.Background()
			user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
				Username: "job_recovery_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
				Status: biz.UserStatusActive, CredentialVersion: 1,
			}, testOutboxEvent(uuid.Nil, "test.register"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error; err != nil {
					t.Error(err)
				}
			})

			sessions, runs, jobs := NewSessionRepository(appDB), NewRunRepository(appDB), NewJobRepository(appDB)
			runID, sessionID := uuid.New(), uuid.New()
			if _, err := sessions.CreateSession(ctx, biz.Session{
				ID: sessionID, UserID: user.ID, Title: "Recovery", Source: biz.SessionSourceWeb,
				Status: biz.SessionActive, Version: 1,
			}, testOutboxEvent(user.ID, "session.created")); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			deadline := now.Add(-time.Minute)
			if _, err := runs.CreateRun(ctx, biz.Run{
				ID: runID, UserID: user.ID, SessionID: sessionID, Status: tc.runStatus,
				Mode: biz.AgentModeReact, DeadlineAt: &deadline,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.runStatus == biz.RunWaitingApproval {
				if err := adminDB.Exec("UPDATE agent_runs SET updated_at = ? WHERE id = ?", now.Add(-48*time.Hour), runID).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := jobs.EnqueueRun(ctx, user.ID, runID, now); err != nil {
				t.Fatal(err)
			}
			jobType := "agent.run.recovery." + uuid.NewString()
			attempts := 0
			if tc.jobStatus == "dead" {
				attempts = 1
			}
			dedupeKey := "run:" + runID.String()
			if err := adminDB.Exec("UPDATE jobs SET job_type = ?, status = ?, attempts = ?, max_attempts = 1 WHERE dedupe_key = ?",
				jobType, tc.jobStatus, attempts, dedupeKey).Error; err != nil {
				t.Fatal(err)
			}
			makeWorker := func(workerID string) *biz.ExecutionWorker {
				t.Helper()
				worker, err := biz.NewExecutionWorker(biz.ExecutionWorkerDependencies{
					Jobs: jobs, Runs: runs, Sessions: sessions, Providers: integrationCredentials{},
					Opener: integrationOpener{}, Attempts: NewModelCallRepository(appDB),
					Clock: func() time.Time { return now },
				}, biz.WorkerConfig{WorkerID: workerID, JobType: jobType, Lease: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				return worker
			}
			process := func(workerID string) {
				t.Helper()
				if processed, err := makeWorker(workerID).ProcessOne(ctx); err != nil || !processed {
					t.Fatalf("ProcessOne(%s) = %t, %v", workerID, processed, err)
				}
			}
			if tc.jobStatus != "dead" {
				const callback = "test:block_run_settlement"
				if err := appDB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
					fields, _ := tx.Statement.Dest.(map[string]any)
					if (tx.Statement.Table == "agent_runs" && fields["status"] == "failed") ||
						(tc.blockJob && tx.Statement.Table == "jobs") {
						_ = tx.AddError(errors.New("database write unavailable"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				process("worker-one")
				if err := appDB.Callback().Update().Remove(callback); err != nil {
					t.Fatal(err)
				}
				now = now.Add(2 * time.Minute)
			}
			process("worker-two")

			finished, err := runs.GetRun(ctx, user.ID, runID)
			if err != nil || finished.Status != tc.finalStatus || finished.ErrorCode != tc.expectedCode {
				t.Fatalf("settled run status=%s code=%q err=%v", finished.Status, finished.ErrorCode, err)
			}
			events, err := runs.ListEventsByTypes(ctx, user.ID, runID, 0, 10, []string{tc.eventType})
			if err != nil || len(events) != 1 || events[0].Payload["code"] != tc.expectedCode {
				t.Fatalf("failure events = %#v, err=%v", events, err)
			}
			var job jobModel
			if err := adminDB.First(&job, "dedupe_key = ?", dedupeKey).Error; err != nil {
				t.Fatal(err)
			}
			if job.Status != "completed" || job.Attempts != 1 {
				t.Fatalf("settled job status=%s attempts=%d", job.Status, job.Attempts)
			}
		})
	}
}
