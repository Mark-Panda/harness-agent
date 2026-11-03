package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

type authServiceFixture struct {
	service *AuthService
	users   *mocks.MockUserRepository
	tokens  *mocks.MockRefreshTokenRepository
	jwt     *auth.JWTManager
	clock   *clock.Fixed
	pepper  []byte
}

func newAuthServiceFixture(t *testing.T) *authServiceFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	users := mocks.NewMockUserRepository(ctrl)
	tokens := mocks.NewMockRefreshTokenRepository(ctrl)
	fixed := clock.NewFixed(time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC))
	manager, err := auth.NewJWTManager(auth.SigningKey{ID: "test", Secret: []byte("01234567890123456789012345678901")}, nil, "harness-test", fixed)
	if err != nil {
		t.Fatal(err)
	}
	pepper := []byte("12345678901234567890123456789012")
	service, err := NewAuthService(users, tokens, manager, pepper, fixed)
	if err != nil {
		t.Fatal(err)
	}
	return &authServiceFixture{service: service, users: users, tokens: tokens, jwt: manager, clock: fixed, pepper: pepper}
}

func activeUser(t *testing.T) biz.User {
	t.Helper()
	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return biz.User{ID: uuid.New(), Username: "alice", PasswordHash: hash, Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}
}

func TestRegisterHashesPasswordAndIssuesTokens(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	fixture.users.EXPECT().CreateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, created biz.User, _ biz.OutboxEvent) (biz.User, error) {
		if created.PasswordHash == "correct horse battery" {
			t.Fatal("plaintext password reached repository")
		}
		created.ID = user.ID
		created.CreatedAt = fixture.clock.Now()
		return created, nil
	})
	fixture.tokens.EXPECT().CreateRefreshToken(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, token biz.RefreshToken, _ biz.OutboxEvent) error {
		if len(token.TokenHash) != 32 || token.CredentialVersion != 1 {
			t.Fatalf("invalid refresh token record: %#v", token)
		}
		return nil
	})
	result, err := fixture.service.Register(context.Background(), "Alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" || result.User.ID != user.ID {
		t.Fatalf("unexpected register result: %#v", result)
	}
	claims, err := fixture.jwt.ParseAccess(result.Tokens.AccessToken)
	if err != nil || claims.Subject != user.ID.String() {
		t.Fatalf("access claims = %#v, %v", claims, err)
	}
}

func TestLoginUsesGenericInvalidCredentials(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	fixture.users.EXPECT().GetUserByNormalizedUsername(gomock.Any(), "alice").Return(biz.User{}, biz.ErrUserNotFound)
	_, err := fixture.service.Login(context.Background(), "alice", "wrong password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v", err)
	}
}

func TestLoginSuccess(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	fixture.users.EXPECT().GetUserByNormalizedUsername(gomock.Any(), "alice").Return(user, nil)
	fixture.tokens.EXPECT().CreateRefreshToken(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	result, err := fixture.service.Login(context.Background(), "alice", "correct horse battery")
	if err != nil || result.User.ID != user.ID {
		t.Fatalf("Login() = %#v, %v", result, err)
	}
}

func TestRefreshRotatesTokenFamily(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	const rawToken = "existing-refresh-token"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	oldToken := biz.RefreshToken{
		ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1,
		ExpiresAt: fixture.clock.Now().Add(time.Hour), CreatedAt: fixture.clock.Now().Add(-time.Hour),
	}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RotateRefreshToken(gomock.Any(), user.ID, oldToken.ID, hash, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ uuid.UUID, _ []byte, newToken biz.RefreshToken, _ biz.OutboxEvent) error {
			if newToken.FamilyID != oldToken.FamilyID || newToken.ParentTokenID == nil || *newToken.ParentTokenID != oldToken.ID {
				t.Fatalf("invalid rotation: %#v", newToken)
			}
			return nil
		},
	)
	result, err := fixture.service.Refresh(context.Background(), rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tokens.RefreshToken == rawToken || result.Tokens.AccessToken == "" {
		t.Fatal("refresh token was not rotated")
	}
}

func TestRefreshReuseRevokesFamily(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	userID := uuid.New()
	familyID := uuid.New()
	rotatedAt := fixture.clock.Now().Add(-time.Minute)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: userID, FamilyID: familyID, RotatedAt: &rotatedAt, CredentialVersion: 1}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(oldToken, nil)
	fixture.tokens.EXPECT().RevokeFamily(gomock.Any(), userID, familyID, fixture.clock.Now().UTC(), true, gomock.Any()).Return(nil)
	_, err := fixture.service.Refresh(context.Background(), "reused-token")
	if !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestAuthenticateChecksCurrentUserState(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	token, err := fixture.jwt.IssueAccess(user.ID.String(), string(user.Role), user.CredentialVersion)
	if err != nil {
		t.Fatal(err)
	}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	principal, err := fixture.service.Authenticate(context.Background(), token)
	if err != nil || principal.UserID != user.ID {
		t.Fatalf("Authenticate() = %#v, %v", principal, err)
	}
}

func TestLogoutAllRevokesTokensAndCredentialVersion(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	userID := uuid.New()
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), userID, fixture.clock.Now().UTC(), gomock.Any()).Return(nil)
	if err := fixture.service.LogoutAll(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
}
