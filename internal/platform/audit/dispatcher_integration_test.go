//go:build integration

package audit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestDispatcherPublishesOutboxExactlyOnce(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	admin, err := data.OpenPostgres("postgres://harness:harness@localhost:5432/harness?sslmode=disable", 1)
	if err != nil {
		t.Fatal(err)
	}
	app, err := data.OpenPostgres("postgres://harness_app:harness@localhost:5432/harness?sslmode=disable", 1)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := data.OpenPostgres("postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("SELECT set_config('audit.retention', 'on', false)").Error; err != nil {
		t.Fatal(err)
	}
	_ = admin.Exec("DELETE FROM audit_logs WHERE partition_id = ?", "2099-01-02").Error
	fixed := clock.NewFixed(time.Date(2099, time.January, 2, 0, 0, 0, 0, time.UTC))
	recorder, err := NewRecorder(auth, []byte("01234567890123456789012345678901"), fixed)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(auth, recorder, fixed)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	users := data.NewUserRepository(app, auth)
	user, err := users.CreateUser(context.Background(), biz.User{
		ID: userID, Username: "dispatch_" + userID.String()[:8], PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, biz.OutboxEvent{
		UserID: userID, EventType: "auth.user.registered", AggregateType: "user", AggregateID: userID.String(),
		DedupeKey: "dispatch:" + userID.String(), Payload: map[string]any{"outcome": "success", "provider": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DELETE FROM users WHERE id = ?", user.ID)
	defer admin.Exec("DELETE FROM audit_logs WHERE partition_id = ?", "2099-01-02")
	// 集成测试包可能并行写入其他 Outbox 事件，因此不能断言全局发布数量；
	// 这里轮询直到本事件被发布，再用 attempts 证明它只被结算一次。
	status, attempts := "", 0
	for iteration := 0; iteration < 5; iteration++ {
		if _, err := dispatcher.DispatchOnce(context.Background(), 100); err != nil {
			t.Fatalf("DispatchOnce() error = %v", err)
		}
		status, attempts = "", 0
		if err := data.WithScope(context.Background(), auth, data.Scope{System: true}, func(tx *gorm.DB) error {
			return tx.Table("outbox_events").Select("status, attempts").Where("dedupe_key = ?", "dispatch:"+userID.String()).Row().Scan(&status, &attempts)
		}); err != nil {
			t.Fatal(err)
		}
		if status == "published" {
			break
		}
	}
	if status != "published" {
		t.Fatalf("outbox status = %s", status)
	}
	if attempts != 1 {
		t.Fatalf("outbox attempts = %d, want exactly one publish", attempts)
	}
	if err := recorder.VerifyPartition(context.Background(), "2099-01-02"); err != nil {
		t.Fatalf("verify audit chain: %v", err)
	}
	// 再次调度不得重复结算同一事件。
	if _, err := dispatcher.DispatchOnce(context.Background(), 100); err != nil {
		t.Fatalf("second DispatchOnce() error = %v", err)
	}
	attempts = 0
	if err := data.WithScope(context.Background(), auth, data.Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("outbox_events").Select("attempts").Where("dedupe_key = ?", "dispatch:"+userID.String()).Row().Scan(&attempts)
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("second dispatch changed attempts to %d", attempts)
	}
}
