//go:build integration

package data

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func testOutboxEvent(userID uuid.UUID, action string) biz.OutboxEvent {
	aggregateID := ""
	if userID != uuid.Nil {
		aggregateID = userID.String()
	}
	return biz.OutboxEvent{
		UserID: userID, EventType: action, AggregateType: "test", AggregateID: aggregateID,
		DedupeKey: action + ":" + uuid.NewString(), Payload: map[string]any{"outcome": "success"},
	}
}

func integrationDSN(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func openIntegrationDatabase(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := OpenPostgres(dsn, 1)
	if err == nil {
		var sqlDB, dbErr = db.DB()
		err = dbErr
		if err == nil {
			err = sqlDB.Ping()
		}
	}
	if err != nil {
		if os.Getenv("REQUIRE_DATABASE") == "true" {
			t.Fatalf("database is required: %v", err)
		}
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return db
}

func TestUserRepositoryAndRLSIsolation(t *testing.T) {
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	repository := NewUserRepository(app, auth)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	userA, err := repository.CreateUser(ctx, biz.User{
		Username: "Alice_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	defer admin.Exec("DELETE FROM users WHERE id = ?", userA.ID)
	userB, err := repository.CreateUser(ctx, biz.User{
		Username: "Bob_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	defer admin.Exec("DELETE FROM users WHERE id = ?", userB.ID)

	got, err := repository.GetUserByNormalizedUsername(ctx, "alice_"+suffix)
	if err != nil || got.ID != userA.ID {
		t.Fatalf("username lookup = %#v, %v", got, err)
	}
	if _, err := repository.GetUserByID(ctx, userA.ID); err != nil {
		t.Fatalf("self lookup failed: %v", err)
	}

	var count int64
	if err := WithScope(ctx, app, Scope{UserID: userA.ID}, func(tx *gorm.DB) error {
		return tx.Model(&userModel{}).Where("id = ?", userB.ID).Count(&count).Error
	}); err != nil {
		t.Fatalf("cross-user query: %v", err)
	}
	if count != 0 {
		t.Fatalf("user A read %d rows belonging to user B", count)
	}

	count = -1
	if err := app.Model(&userModel{}).Where("id = ?", userA.ID).Count(&count).Error; err != nil {
		t.Fatalf("query after pooled transaction: %v", err)
	}
	if count != 0 {
		t.Fatalf("tenant scope leaked from pooled connection, count=%d", count)
	}

	unauthorized := userModel{
		ID: uuid.New(), Username: "Mallory_" + suffix, UsernameNormalized: "mallory_" + suffix,
		PasswordHash: "hash", Role: "user", Status: "active", CredentialVersion: 1,
	}
	if err := auth.Create(&unauthorized).Error; err == nil {
		defer admin.Exec("DELETE FROM users WHERE id = ?", unauthorized.ID)
		t.Fatal("auth role inserted a user without registration scope")
	}
	if _, err := repository.CreateUser(ctx, biz.User{
		Username: "Alice_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(userA.ID, "test.register.duplicate")); !errors.Is(err, biz.ErrUserAlreadyExists) {
		t.Fatalf("duplicate username error = %v", err)
	}
}

func TestRefreshTokenAtomicRotationAndRevocation(t *testing.T) {
	admin := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	app := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	auth := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	users := NewUserRepository(app, auth)
	tokens := NewRefreshTokenRepository(auth)
	ctx := context.Background()
	user, err := users.CreateUser(ctx, biz.User{
		Username: "token_" + strings.ReplaceAll(uuid.NewString(), "-", ""), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DELETE FROM users WHERE id = ?", user.ID)
	now := time.Now().UTC()
	oldHash := sha256.Sum256([]byte(uuid.NewString()))
	oldToken := biz.RefreshToken{
		ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: oldHash[:], CredentialVersion: 1,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := tokens.CreateRefreshToken(ctx, oldToken, testOutboxEvent(user.ID, "test.token.created")); err != nil {
		t.Fatalf("create refresh token: %v", err)
	}
	newHash := sha256.Sum256([]byte(uuid.NewString()))
	newToken := biz.RefreshToken{
		ID: uuid.New(), UserID: user.ID, FamilyID: oldToken.FamilyID, TokenHash: newHash[:], ParentTokenID: &oldToken.ID,
		CredentialVersion: 1, ExpiresAt: now.Add(2 * time.Hour), CreatedAt: now.Add(time.Minute),
	}
	if err := tokens.RotateRefreshToken(ctx, user.ID, oldToken.ID, oldHash[:], newToken, testOutboxEvent(user.ID, "test.token.rotated")); err != nil {
		t.Fatalf("rotate refresh token: %v", err)
	}
	rotated, err := tokens.GetRefreshTokenByHash(ctx, oldHash[:])
	if err != nil || rotated.RotatedAt == nil {
		t.Fatalf("old token was not marked rotated: %#v, %v", rotated, err)
	}
	if err := tokens.RotateRefreshToken(ctx, user.ID, oldToken.ID, oldHash[:], newToken, testOutboxEvent(user.ID, "test.token.rotated")); !errors.Is(err, biz.ErrRefreshTokenConflict) {
		t.Fatalf("concurrent rotation error = %v", err)
	}
	if err := tokens.RevokeFamily(ctx, user.ID, oldToken.FamilyID, now.Add(2*time.Minute), true, testOutboxEvent(user.ID, "test.token.revoked")); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	revoked, err := tokens.GetRefreshTokenByHash(ctx, newHash[:])
	if err != nil || revoked.RevokedAt == nil || revoked.ReuseDetectedAt == nil {
		t.Fatalf("family was not revoked: %#v, %v", revoked, err)
	}
	if err := tokens.RevokeAllUserTokens(ctx, user.ID, now.Add(3*time.Minute), testOutboxEvent(user.ID, "test.logout.all")); err != nil {
		t.Fatalf("revoke all tokens: %v", err)
	}
	updated, err := users.GetUserByID(ctx, user.ID)
	if err != nil || updated.CredentialVersion != 2 {
		t.Fatalf("credential version = %d, %v", updated.CredentialVersion, err)
	}
	var outboxCount int64
	if err := WithScope(ctx, auth, Scope{UserID: user.ID}, func(tx *gorm.DB) error {
		return tx.Table("outbox_events").Where("user_id = ?", user.ID).Count(&outboxCount).Error
	}); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 5 {
		t.Fatalf("outbox count = %d, want 5", outboxCount)
	}
	var payload string
	if err := WithScope(ctx, auth, Scope{UserID: user.ID}, func(tx *gorm.DB) error {
		return tx.Table("outbox_events").Select("payload::text").Where("event_type = ?", "test.token.rotated").Order("created_at DESC").Limit(1).Scan(&payload).Error
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, string(oldHash[:])) || strings.Contains(payload, string(newHash[:])) {
		t.Fatalf("outbox payload contains token material: %s", payload)
	}
}
