//go:build integration

package audit

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
)

func TestAuditChainAndAppendOnlyProtection(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	admin, err := data.OpenPostgres("postgres://harness:harness@localhost:5432/harness?sslmode=disable", 1)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := data.OpenPostgres("postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable", 1)
	if err != nil {
		t.Fatal(err)
	}
	const partition = "2099-01-01"
	if err := admin.Exec("SELECT set_config('audit.retention', 'on', false)").Error; err != nil {
		t.Fatal(err)
	}
	_ = admin.Exec("DELETE FROM audit_logs WHERE partition_id = ?", partition).Error
	_ = admin.Exec("DELETE FROM audit_checkpoints WHERE partition_id = ?", partition).Error
	fixed := clock.NewFixed(time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC))
	recorder, err := NewRecorder(auth, []byte("01234567890123456789012345678901"), fixed)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"auth.login", "auth.refresh"} {
		if err := recorder.Record(context.Background(), Event{
			ActorType: "user", ActorID: "user-1", Action: action, ResourceType: "session", ResourceID: "session-1",
			Outcome: "success", Source: "integration-test", Metadata: map[string]any{"result": "ok", "password": "drop"},
		}); err != nil {
			t.Fatalf("record %s: %v", action, err)
		}
	}
	if err := recorder.VerifyPartition(context.Background(), partition); err != nil {
		t.Fatalf("verify chain: %v", err)
	}
	// Checkpoint 必须锚定链头并可独立验证；签名或哈希被篡改时必须拒绝。
	checkpoint, err := recorder.CreateCheckpoint(context.Background(), partition)
	if err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}
	if checkpoint.Seq != 2 {
		t.Fatalf("checkpoint seq = %d", checkpoint.Seq)
	}
	if err := recorder.VerifyCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("verify checkpoint: %v", err)
	}
	forged := checkpoint
	forged.Signature = bytes.Repeat([]byte{0xff}, 32)
	if err := recorder.VerifyCheckpoint(context.Background(), forged); err == nil {
		t.Fatal("forged checkpoint signature was accepted")
	}
	misplaced := checkpoint
	misplaced.EventHash = bytes.Repeat([]byte{0x01}, 32)
	if err := recorder.VerifyCheckpoint(context.Background(), misplaced); err == nil {
		t.Fatal("mismatched checkpoint hash was accepted")
	}
	var checkpointCount int64
	if err := admin.Table("audit_checkpoints").Where("partition_id = ?", partition).Count(&checkpointCount).Error; err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoint count = %d", checkpointCount)
	}
	// 检查点表同样只允许插入；非保留期角色不得删除。
	if err := auth.Exec("DELETE FROM audit_checkpoints WHERE partition_id = ?", partition).Error; err == nil {
		t.Fatal("checkpoint delete was allowed without retention scope")
	}
	if err := auth.Exec("UPDATE audit_logs SET action = 'tampered' WHERE partition_id = ?", partition).Error; err == nil {
		t.Fatal("audit update was allowed")
	}
	if err := auth.Exec("DELETE FROM audit_logs WHERE partition_id = ?", partition).Error; err == nil {
		t.Fatal("audit delete was allowed without retention scope")
	}
	if err := admin.Exec("DELETE FROM audit_logs WHERE partition_id = ?", partition).Error; err != nil {
		t.Fatalf("retention cleanup failed: %v", err)
	}
}
