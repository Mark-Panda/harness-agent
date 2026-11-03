//go:build integration

package data

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestUserCredentialUpdatesAndPasswordResetTokens 验证凭据更新与一次性重置令牌的原子消费。
func TestUserCredentialUpdatesAndPasswordResetTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "password_" + suffix, PasswordHash: "old-hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error })

	updated, err := users.UpdateUser(ctx, biz.User{
		ID: user.ID, PasswordHash: "new-hash", Status: biz.UserStatusDisabled, CredentialVersion: 2,
	}, testOutboxEvent(user.ID, "auth.password.changed"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != biz.UserStatusDisabled || updated.CredentialVersion != 2 {
		t.Fatalf("updated user = %#v", updated)
	}
	stored, err := users.GetUserByID(ctx, user.ID)
	if err != nil || stored.PasswordHash != "new-hash" || stored.Status != biz.UserStatusDisabled || stored.CredentialVersion != 2 {
		t.Fatalf("stored user = %#v err=%v", stored, err)
	}
	if _, err := users.UpdateUser(ctx, biz.User{ID: uuid.Nil}, testOutboxEvent(uuid.Nil, "invalid")); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("missing user id error = %v", err)
	}

	resets := NewPasswordResetRepository(authDB)
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("reset-" + suffix))
	if err := resets.CreatePasswordResetToken(ctx, biz.PasswordResetToken{
		ID: uuid.New(), UserID: user.ID, TokenHash: hash[:], ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now,
	}, testOutboxEvent(user.ID, "auth.password.reset.issued")); err != nil {
		t.Fatal(err)
	}
	consumed, err := resets.ConsumePasswordResetToken(ctx, hash[:], now)
	if err != nil || consumed != user.ID {
		t.Fatalf("consume token = %s err=%v", consumed, err)
	}
	if _, err := resets.ConsumePasswordResetToken(ctx, hash[:], now); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("second consume error = %v", err)
	}
	if _, err := resets.ConsumePasswordResetToken(ctx, hash[:], now.Add(16*time.Minute)); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("expired consume error = %v", err)
	}
	expiredHash := sha256.Sum256([]byte("expired-" + suffix))
	if err := resets.CreatePasswordResetToken(ctx, biz.PasswordResetToken{
		ID: uuid.New(), UserID: user.ID, TokenHash: expiredHash[:], ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}, testOutboxEvent(user.ID, "auth.password.reset.issued")); err != nil {
		t.Fatal(err)
	}
	if _, err := resets.ConsumePasswordResetToken(ctx, expiredHash[:], now.Add(2*time.Minute)); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("expired token error = %v", err)
	}
	if err := resets.CreatePasswordResetToken(ctx, biz.PasswordResetToken{UserID: user.ID}, testOutboxEvent(user.ID, "invalid")); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid token error = %v", err)
	}
}
