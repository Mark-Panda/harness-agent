//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// testFutureExpiry 返回未来的幂等记录过期时间。
func testFutureExpiry() time.Time { return time.Now().UTC().Add(time.Hour) }

// TestIdempotencyRepositoryReplayAndConflict 验证真实数据库中的幂等占位、回放与指纹冲突。
func TestIdempotencyRepositoryReplayAndConflict(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "idem_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID)
	repository := NewIdempotencyRepository(appDB)
	hash := make([]byte, 32)
	hash[0] = 0x01
	key := "idem-key-" + suffix

	outcome, err := repository.Begin(ctx, user.ID, key, hash, "session", testFutureExpiry())
	if err != nil || outcome.Replay {
		t.Fatalf("first Begin() = %#v, %v", outcome, err)
	}
	otherHash := make([]byte, 32)
	otherHash[0] = 0x02
	if _, err := repository.Begin(ctx, user.ID, key, otherHash, "session", testFutureExpiry()); !errors.Is(err, biz.ErrIdempotencyConflict) {
		t.Fatalf("fingerprint mismatch error = %v", err)
	}
	if _, err := repository.Begin(ctx, user.ID, key, hash, "session", testFutureExpiry()); !errors.Is(err, biz.ErrIdempotencyConflict) {
		t.Fatalf("in-progress replay error = %v", err)
	}

	envelope := []byte(`{"status":201,"body":{"id":"` + user.ID.String() + `"}}`)
	if err := repository.Complete(ctx, user.ID, key, envelope, user.ID.String()); err != nil {
		t.Fatal(err)
	}
	outcome, err = repository.Begin(ctx, user.ID, key, hash, "session", testFutureExpiry())
	if err != nil || !outcome.Replay || outcome.Status != 201 {
		t.Fatalf("replay Begin() = %#v, %v", outcome, err)
	}
	var replayed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(outcome.Body, &replayed); err != nil || replayed.ID != user.ID.String() {
		t.Fatalf("replayed body = %s, %v", outcome.Body, err)
	}

	// 另一用户使用相同键不得看到该记录。
	other, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "idem_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DELETE FROM users WHERE id = ?", other.ID)
	outcome, err = repository.Begin(ctx, other.ID, key, hash, "session", testFutureExpiry())
	if err != nil || outcome.Replay {
		t.Fatalf("cross-user idempotency reuse = %#v, %v", outcome, err)
	}
}
