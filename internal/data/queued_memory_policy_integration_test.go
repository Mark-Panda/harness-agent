//go:build integration

package data

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type queuedSummaryGenerator func() biz.SummaryGeneration

func (f queuedSummaryGenerator) Summarize(context.Context, uuid.UUID, uuid.UUID, string, []biz.Message) (biz.SummaryGeneration, error) {
	return f(), nil
}

type queuedFactExtractor func() []biz.FactCandidate

func (f queuedFactExtractor) Extract(context.Context, uuid.UUID, uuid.UUID, []biz.Message) ([]biz.FactCandidate, error) {
	return f(), nil
}

// queuedMemoryJobQueue 保留历史领取时钟对共享队列的隔离，同时在领取本测试任务后
// 将租约对齐数据库真实时钟，避免策略测试被人为制造的过期租约中断。
type queuedMemoryJobQueue struct {
	*JobRepository
	db            *gorm.DB
	expectedJobID string
}

func (q *queuedMemoryJobQueue) Claim(ctx context.Context, workerID, jobType string, lease time.Duration, now time.Time) (biz.Job, bool, error) {
	job, claimed, err := q.JobRepository.Claim(ctx, workerID, jobType, lease, now)
	if err != nil || !claimed {
		return job, claimed, err
	}
	if job.ID.String() != q.expectedJobID {
		return biz.Job{}, false, fmt.Errorf("claimed unrelated job %s, want %s", job.ID, q.expectedJobID)
	}
	result := q.db.WithContext(ctx).Exec(`UPDATE jobs
		SET lease_expires_at = clock_timestamp() + (? * INTERVAL '1 second')
		WHERE id = ? AND status = 'running' AND lease_owner = ? AND fencing_token = ?`,
		lease.Seconds(), job.ID, workerID, job.FencingToken)
	if result.Error != nil {
		return biz.Job{}, false, result.Error
	}
	if result.RowsAffected != 1 {
		return biz.Job{}, false, fmt.Errorf("align claimed job lease affected %d rows", result.RowsAffected)
	}
	return job, true, nil
}

func (q *queuedMemoryJobQueue) Renew(ctx context.Context, jobID uuid.UUID, workerID string, fencingToken int64, lease time.Duration, _ time.Time) error {
	return q.JobRepository.Renew(ctx, jobID, workerID, fencingToken, lease, time.Now().UTC())
}

func TestQueuedMemoryHonorsSessionPolicy(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	sessions := NewSessionRepository(appDB)
	users := NewUserRepository(appDB, authDB)
	jobs := NewJobRepository(appDB)
	summaries := NewSummaryRepository(appDB)
	facts := NewFactRepository(appDB)
	factService, err := biz.NewFactService(facts, biz.FactPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Use an older clock so the workers claim only the jobs created by this test.
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	for _, jobType := range []string{biz.SummaryJobType, biz.FactExtractJobType} {
		for _, tc := range []struct {
			name       string
			before     biz.SessionMemoryPolicy
			during     biz.SessionMemoryPolicy
			projection bool
			wantCalls  int
			wantRows   int
		}{
			{name: "private", wantCalls: 1, wantRows: 1},
			{name: "disabled before execution", before: biz.SessionMemoryDisabled},
			{name: "disabled during generation", during: biz.SessionMemoryDisabled, wantCalls: 1},
			{name: "group projection", before: biz.SessionMemoryGroup, projection: true},
		} {
			t.Run(jobType+"/"+tc.name, func(t *testing.T) {
				userID, sessionID, messages := summaryIntegrationSetup(t, ctx, sessions, users)
				t.Cleanup(func() {
					if err := adminDB.Exec("DELETE FROM users WHERE id = ?", userID).Error; err != nil {
						t.Error(err)
					}
				})
				setPolicy := func(policy biz.SessionMemoryPolicy) {
					t.Helper()
					session, err := sessions.GetSession(ctx, userID, sessionID)
					if err != nil {
						t.Fatal(err)
					}
					session.MemoryPolicy = policy
					session.Version++
					if _, err := sessions.UpdateSessionMemoryPolicy(ctx, session, testOutboxEvent(userID, "session.memory_policy_updated")); err != nil {
						t.Fatal(err)
					}
				}
				ids := []uuid.UUID{messages[0].ID}
				fingerprint := biz.SummarySourceFingerprint(messages[:1])
				if jobType == biz.SummaryJobType {
					err = jobs.EnqueueSummary(ctx, biz.SummaryTask{
						UserID: userID, SessionID: sessionID, SourceMessageIDs: ids, Fingerprint: fingerprint,
						EndMessageID: messages[0].ID, EndAt: messages[0].CreatedAt, CoveredMessageCount: 1,
					}, now)
				} else {
					err = jobs.EnqueueFacts(ctx, biz.FactTask{
						UserID: userID, SessionID: sessionID, MessageIDs: ids, Fingerprint: fingerprint,
					}, now)
				}
				if err != nil {
					t.Fatal(err)
				}
				var jobID string
				if err := adminDB.Raw("SELECT id::text FROM jobs WHERE user_id = ? AND job_type = ?", userID, jobType).Scan(&jobID).Error; err != nil || jobID == "" {
					t.Fatalf("load queued job id = %q, err=%v", jobID, err)
				}
				queue := &queuedMemoryJobQueue{JobRepository: jobs, db: adminDB, expectedJobID: jobID}
				if tc.before != "" {
					setPolicy(tc.before)
				}
				if tc.projection {
					if err := adminDB.Exec("UPDATE sessions SET feishu_group_projection = true, source = 'feishu' WHERE user_id = ? AND id = ?", userID, sessionID).Error; err != nil {
						t.Fatal(err)
					}
				}

				calls := 0
				onCall := func() {
					calls++
					if tc.during != "" {
						setPolicy(tc.during)
					}
				}
				var processOne func(context.Context) (bool, error)
				if jobType == biz.SummaryJobType {
					worker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
						Jobs: queue, Sessions: sessions, Messages: sessions, Summaries: summaries, Clock: clock,
						Generator: queuedSummaryGenerator(func() biz.SummaryGeneration {
							onCall()
							return biz.SummaryGeneration{Text: "Generated summary"}
						}),
					}, biz.SummaryWorkerConfig{WorkerID: "queued-memory-policy-test"})
					if err != nil {
						t.Fatal(err)
					}
					processOne = worker.ProcessOne
				} else {
					worker, err := biz.NewFactExtractionWorker(biz.FactExtractionWorkerDependencies{
						Jobs: queue, Messages: sessions, Facts: factService, Clock: clock,
						Extractor: queuedFactExtractor(func() []biz.FactCandidate {
							onCall()
							return []biz.FactCandidate{{
								Key: "preference.language", Value: json.RawMessage(`"Chinese"`),
								Confidence: 0.9, SourceMessageID: &messages[0].ID,
							}}
						}),
					}, biz.FactExtractionWorkerConfig{WorkerID: "queued-memory-policy-test"})
					if err != nil {
						t.Fatal(err)
					}
					processOne = worker.ProcessOne
				}
				if claimed, err := processOne(ctx); err != nil || !claimed {
					t.Fatalf("claimed=%v err=%v", claimed, err)
				}
				if calls != tc.wantCalls {
					t.Errorf("provider calls = %d, want %d", calls, tc.wantCalls)
				}
				if jobType == biz.SummaryJobType {
					history, err := summaries.ListSummaries(ctx, userID, sessionID, 10)
					if err != nil || len(history) != tc.wantRows {
						t.Errorf("summary versions = %d, want %d, err=%v", len(history), tc.wantRows, err)
					}
				} else {
					history, err := facts.ListFacts(ctx, userID, 10)
					if err != nil || len(history) != tc.wantRows {
						t.Errorf("fact versions = %d, want %d, err=%v", len(history), tc.wantRows, err)
					}
				}
				var status string
				if err := adminDB.Raw("SELECT status FROM jobs WHERE user_id = ? AND job_type = ?", userID, jobType).Scan(&status).Error; err != nil || status != "completed" {
					t.Errorf("job status = %q, want completed, err=%v", status, err)
				}
			})
		}
	}
}
