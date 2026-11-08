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

// TestJobRepositoryEnqueueDedupeAndRequeue 验证各类任务入队幂等、去重键隔离与失败任务重放。
func TestJobRepositoryEnqueueDedupeAndRequeue(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "job_user_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	userID := user.ID
	jobs := NewJobRepository(appDB)
	var dedupeKeys []string
	defer func() {
		for _, key := range dedupeKeys {
			_ = adminDB.Exec("DELETE FROM jobs WHERE dedupe_key = ?", key).Error
		}
	}()

	// 参数校验：缺少身份或来源时拒绝入队。
	if err := jobs.EnqueueSummary(ctx, biz.SummaryTask{UserID: userID}, time.Time{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("enqueue invalid summary error = %v", err)
	}
	if err := jobs.EnqueueFacts(ctx, biz.FactTask{UserID: userID}, time.Time{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("enqueue invalid fact error = %v", err)
	}
	if err := jobs.EnqueueFeishuEvent(ctx, "", time.Time{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("enqueue invalid feishu event error = %v", err)
	}
	if err := jobs.RequeueRun(ctx, uuid.Nil, time.Time{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("requeue nil run error = %v", err)
	}

	// 摘要任务：同一来源指纹重复入队幂等。
	sessionID, messageID := uuid.New(), uuid.New()
	summaryTask := biz.SummaryTask{
		UserID: userID, SessionID: sessionID, BaseVersion: 0, SourceMessageIDs: []uuid.UUID{messageID},
		Fingerprint: "fp-" + suffix, EndMessageID: messageID, EndAt: time.Now().UTC(), CoveredMessageCount: 1,
	}
	dedupeKeys = append(dedupeKeys, "summary:"+sessionID.String()+":fp-"+suffix)
	if err := jobs.EnqueueSummary(ctx, summaryTask, time.Time{}); err != nil {
		t.Fatalf("EnqueueSummary() = %v", err)
	}
	if err := jobs.EnqueueSummary(ctx, summaryTask, time.Time{}); err != nil {
		t.Fatalf("duplicate EnqueueSummary() = %v", err)
	}
	var summaryJobs int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM jobs WHERE dedupe_key = ?", dedupeKeys[0]).Scan(&summaryJobs).Error; err != nil || summaryJobs != 1 {
		t.Fatalf("summary dedupe rows = %d, %v", summaryJobs, err)
	}

	// 事实任务：去重键含事实归属用户，同会话不同发言人不互相吞并。
	speakerOwnerID := uuid.New()
	factTask := biz.FactTask{
		UserID: userID, SessionID: sessionID, MessageIDs: []uuid.UUID{messageID}, Fingerprint: "factfp-" + suffix + "-long",
	}
	dedupeKeys = append(dedupeKeys,
		"fact:"+sessionID.String()+":"+messageID.String()+":"+uuid.Nil.String()+":"+factTask.Fingerprint[:16],
		"fact:"+sessionID.String()+":"+messageID.String()+":"+speakerOwnerID.String()+":"+factTask.Fingerprint[:16],
	)
	if err := jobs.EnqueueFacts(ctx, factTask, time.Time{}); err != nil {
		t.Fatalf("EnqueueFacts() = %v", err)
	}
	speakerTask := factTask
	speakerTask.FactOwnerID = speakerOwnerID
	speakerTask.SpeakerOpenID = "ou_speaker"
	if err := jobs.EnqueueFacts(ctx, speakerTask, time.Time{}); err != nil {
		t.Fatalf("EnqueueFacts() speaker = %v", err)
	}
	var factJobs int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM jobs WHERE job_type = ? AND payload->>'session_id' = ?", biz.FactExtractJobType, sessionID.String()).Scan(&factJobs).Error; err != nil {
		t.Fatal(err)
	}
	if factJobs != 2 {
		t.Fatalf("fact jobs for distinct owners = %d, want 2", factJobs)
	}
	var speakerPayload string
	if err := adminDB.Raw("SELECT payload::text FROM jobs WHERE job_type = ? AND payload->>'fact_owner_id' IS NOT NULL", biz.FactExtractJobType).Scan(&speakerPayload).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(speakerPayload, "ou_speaker") {
		t.Fatalf("speaker filter not persisted in payload: %s", speakerPayload)
	}
	defer func() {
		_ = adminDB.Exec("DELETE FROM jobs WHERE job_type = ? AND payload->>'session_id' = ?", biz.FactExtractJobType, sessionID.String()).Error
	}()

	// 飞书事件任务：event_id 去重，重复投递不产生新行。
	eventID := "evt_" + suffix
	dedupeKeys = append(dedupeKeys, "feishu.event:"+eventID)
	if err := jobs.EnqueueFeishuEvent(ctx, eventID, time.Time{}); err != nil {
		t.Fatalf("EnqueueFeishuEvent() = %v", err)
	}
	if err := jobs.EnqueueFeishuEvent(ctx, eventID, time.Time{}); err != nil {
		t.Fatalf("duplicate EnqueueFeishuEvent() = %v", err)
	}
	var feishuJobs int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM jobs WHERE dedupe_key = ?", "feishu.event:"+eventID).Scan(&feishuJobs).Error; err != nil || feishuJobs != 1 {
		t.Fatalf("feishu event dedupe rows = %d, %v", feishuJobs, err)
	}

	// RequeueRun：仅 failed/dead 可重放；重放后恢复 pending 且清零 attempts。
	runID := uuid.New()
	dedupeKeys = append(dedupeKeys, "run:"+runID.String())
	if err := jobs.RequeueRun(ctx, runID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("requeue without failed job error = %v", err)
	}
	if err := jobs.EnqueueRun(ctx, userID, runID, time.Time{}); err != nil {
		t.Fatalf("EnqueueRun() = %v", err)
	}
	if err := jobs.RequeueRun(ctx, runID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("requeue pending job error = %v", err)
	}
	if err := adminDB.Exec("UPDATE jobs SET status = 'dead', attempts = 8, last_error = 'boom' WHERE dedupe_key = ?", "run:"+runID.String()).Error; err != nil {
		t.Fatal(err)
	}
	if err := jobs.RequeueRun(ctx, runID, time.Now().UTC()); err != nil {
		t.Fatalf("RequeueRun() = %v", err)
	}
	var row struct {
		Status   string
		Attempts int
	}
	if err := adminDB.Raw("SELECT status, attempts FROM jobs WHERE dedupe_key = ?", "run:"+runID.String()).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "pending" || row.Attempts != 0 {
		t.Fatalf("requeued job = %#v", row)
	}
	// 重放后可被认领，证明队列闭环。
	claimed, found, err := jobs.Claim(ctx, "worker-requeue", "agent.run", time.Minute, time.Now().UTC())
	if err != nil || !found || claimed.Type != "agent.run" || claimed.Payload["run_id"] != runID.String() {
		t.Fatalf("claim requeued job = %#v found=%v err=%v", claimed, found, err)
	}
}
