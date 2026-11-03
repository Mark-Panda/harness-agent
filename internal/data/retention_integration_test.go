//go:build integration

package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/audit"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
)

// TestPurgeExpiredRetention 验证保留期清理只删除过期执行载荷与审计数据，并且 dry-run 不修改数据。
func TestPurgeExpiredRetention(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "retention_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })

	sessionID, runID := uuid.New(), uuid.New()
	if err := adminDB.Exec("INSERT INTO sessions(id, user_id, title) VALUES (?, ?, 'retention')", sessionID, user.ID).Error; err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := adminDB.Exec("INSERT INTO agent_runs(id, session_id, user_id, mode, status) VALUES (?, ?, ?, 'react', 'completed')", runID, sessionID, user.ID).Error; err != nil {
		t.Fatalf("insert run: %v", err)
	}
	oldPayloadID, newPayloadID := uuid.New(), uuid.New()
	oldAt := time.Now().UTC().Add(-120 * 24 * time.Hour)
	newAt := time.Now().UTC()
	for _, item := range []struct {
		id  uuid.UUID
		at  time.Time
		typ string
	}{
		{id: oldPayloadID, at: oldAt, typ: "model_response"},
		{id: newPayloadID, at: newAt, typ: "tool_output"},
	} {
		if err := adminDB.Exec(
			`INSERT INTO execution_payloads(id, run_id, user_id, payload_type, capture_mode, created_at)
			 VALUES (?, ?, ?, ?, 'metadata_only', ?)`,
			item.id, runID, user.ID, item.typ, item.at,
		).Error; err != nil {
			t.Fatalf("insert payload: %v", err)
		}
	}

	// 飞书事件回执保存原始重放载荷，超过保留期必须整行清除。
	oldEventID, newEventID := "retention-old-"+suffix, "retention-new-"+suffix
	_ = adminDB.Exec("DELETE FROM feishu_event_receipts WHERE event_id IN (?, ?)", oldEventID, newEventID).Error
	t.Cleanup(func() {
		_ = adminDB.Exec("DELETE FROM feishu_event_receipts WHERE event_id IN (?, ?)", oldEventID, newEventID).Error
	})
	for _, item := range []struct {
		eventID string
		at      time.Time
	}{
		{eventID: oldEventID, at: oldAt},
		{eventID: newEventID, at: newAt},
	} {
		if err := adminDB.Exec(`INSERT INTO feishu_event_receipts
			(id, event_id, event_type, tenant_key, payload_redacted, status, received_at)
			VALUES (?, ?, 'im.message.receive_v1', 'retention', '{"body":"echo"}', 'processed', ?)`,
			uuid.New(), item.eventID, item.at).Error; err != nil {
			t.Fatalf("insert feishu receipt: %v", err)
		}
	}

	// 审计旧事件与检查点使用历史时钟写入，便于按保留期清理。
	oldPartition := "1999-01-01"
	// 清理历史残留需要保留期特权开关；该设置只在当前管理员连接生效。
	if err := adminDB.Exec("SELECT set_config('audit.retention', 'on', false)").Error; err != nil {
		t.Fatalf("enable retention scope: %v", err)
	}
	_ = adminDB.Exec("DELETE FROM audit_checkpoints WHERE partition_id = ?", oldPartition).Error
	_ = adminDB.Exec("DELETE FROM audit_logs WHERE partition_id = ?", oldPartition).Error
	fixed := clock.NewFixed(time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC))
	recorder, err := audit.NewRecorder(authDB, []byte("01234567890123456789012345678901"), fixed)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	if err := recorder.Record(ctx, audit.Event{
		ActorType: "system", Action: "retention.probe", ResourceType: "run", ResourceID: runID.String(),
		Outcome: "success", Source: "integration-test",
	}); err != nil {
		t.Fatalf("record old audit: %v", err)
	}
	if _, err := recorder.CreateCheckpoint(ctx, oldPartition); err != nil {
		t.Fatalf("create old checkpoint: %v", err)
	}

	executionCutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
	auditCutoff := time.Now().UTC().Add(-365 * 24 * time.Hour)
	feishuCutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	dryRun, err := PurgeExpired(ctx, adminDB, executionCutoff, auditCutoff, feishuCutoff, false)
	if err != nil {
		t.Fatalf("dry-run purge: %v", err)
	}
	if dryRun.ExecutionPayloads != 1 || dryRun.AuditLogs != 1 || dryRun.AuditCheckpoints != 1 || dryRun.FeishuEvents != 1 {
		t.Fatalf("dry-run counts = %+v", dryRun)
	}
	// dry-run 不得删除任何数据。
	var count int64
	if err := adminDB.Table("execution_payloads").Where("id = ?", oldPayloadID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("dry-run deleted payload: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("feishu_event_receipts").Where("event_id = ?", oldEventID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("dry-run deleted feishu receipt: count=%d err=%v", count, err)
	}

	applied, err := PurgeExpired(ctx, adminDB, executionCutoff, auditCutoff, feishuCutoff, true)
	if err != nil {
		t.Fatalf("apply purge: %v", err)
	}
	if applied.ExecutionPayloads != 1 || applied.AuditLogs != 1 || applied.AuditCheckpoints != 1 || applied.FeishuEvents != 1 {
		t.Fatalf("applied counts = %+v", applied)
	}
	if err := adminDB.Table("execution_payloads").Where("id = ?", oldPayloadID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old payload survived purge: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("execution_payloads").Where("id = ?", newPayloadID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("new payload removed: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("feishu_event_receipts").Where("event_id = ?", oldEventID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old feishu receipt survived purge: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("feishu_event_receipts").Where("event_id = ?", newEventID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("new feishu receipt removed: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("audit_logs").Where("partition_id = ?", oldPartition).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old audit survived purge: count=%d err=%v", count, err)
	}
	if err := adminDB.Table("audit_checkpoints").Where("partition_id = ?", oldPartition).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("old checkpoint survived purge: count=%d err=%v", count, err)
	}
}
