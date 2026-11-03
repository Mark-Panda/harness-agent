package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

func TestAuthServiceDependencyValidation(t *testing.T) {
	if _, err := NewAuthService(nil, nil, nil, nil, nil); err == nil {
		t.Fatal("nil dependencies were accepted")
	}
	fixture := newAuthServiceFixture(t)
	if _, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, []byte("short"), nil); err == nil {
		t.Fatal("short pepper was accepted")
	}
	if _, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, nil); err != nil {
		t.Fatalf("default clock was rejected: %v", err)
	}
}

func TestRegisterValidationAndDuplicate(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	if _, err := fixture.service.Register(context.Background(), "a", "correct horse battery"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("invalid username error = %v", err)
	}
	if _, err := fixture.service.Register(context.Background(), "alice", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password error = %v", err)
	}
	fixture.users.EXPECT().CreateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.User{}, biz.ErrUserAlreadyExists)
	if _, err := fixture.service.Register(context.Background(), "alice", "correct horse battery"); !errors.Is(err, biz.ErrUserAlreadyExists) {
		t.Fatalf("duplicate registration error = %v", err)
	}
}

func TestLoginRejectsWrongPasswordAndDisabledUser(t *testing.T) {
	for name, disabled := range map[string]bool{"wrong_password": false, "disabled_user": true} {
		t.Run(name, func(t *testing.T) {
			fixture := newAuthServiceFixture(t)
			user := activeUser(t)
			if disabled {
				user.Status = biz.UserStatusDisabled
			}
			password := "correct horse battery"
			if !disabled {
				password = "wrong password"
			}
			fixture.users.EXPECT().GetUserByNormalizedUsername(gomock.Any(), "alice").Return(user, nil)
			if _, err := fixture.service.Login(context.Background(), "alice", password); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("Login() error = %v", err)
			}
		})
	}
}

func TestRefreshExpiredToken(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: uuid.New(), FamilyID: uuid.New(), ExpiresAt: fixture.clock.Now().Add(-time.Second)}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(oldToken, nil)
	fixture.tokens.EXPECT().RevokeRefreshToken(gomock.Any(), oldToken.UserID, oldToken.ID, fixture.clock.Now().UTC(), gomock.Any()).Return(nil)
	if _, err := fixture.service.Refresh(context.Background(), "expired"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestRefreshRevokesFamilyForInvalidUser(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	user.Status = biz.UserStatusDisabled
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), CredentialVersion: 1, ExpiresAt: fixture.clock.Now().Add(time.Hour)}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RevokeFamily(gomock.Any(), user.ID, oldToken.FamilyID, fixture.clock.Now().UTC(), false, gomock.Any()).Return(nil)
	if _, err := fixture.service.Refresh(context.Background(), "disabled"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestRefreshRotationConflictRevokesFamily(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), CredentialVersion: 1, ExpiresAt: fixture.clock.Now().Add(time.Hour)}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RotateRefreshToken(gomock.Any(), user.ID, oldToken.ID, gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.ErrRefreshTokenConflict)
	fixture.tokens.EXPECT().RevokeFamily(gomock.Any(), user.ID, oldToken.FamilyID, fixture.clock.Now().UTC(), true, gomock.Any()).Return(nil)
	if _, err := fixture.service.Refresh(context.Background(), "conflict"); !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	if err := fixture.service.Logout(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(biz.RefreshToken{}, biz.ErrRefreshTokenNotFound)
	if err := fixture.service.Logout(context.Background(), "unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestLogoutRevokesExistingToken(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	token := biz.RefreshToken{ID: uuid.New(), UserID: uuid.New()}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(token, nil)
	fixture.tokens.EXPECT().RevokeRefreshToken(gomock.Any(), token.UserID, token.ID, fixture.clock.Now().UTC(), gomock.Any()).Return(nil)
	if err := fixture.service.Logout(context.Background(), "existing"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateRejectsInvalidTokenAndCredentialVersion(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	if _, err := fixture.service.Authenticate(context.Background(), "invalid"); err == nil {
		t.Fatal("invalid token was accepted")
	}
	user := activeUser(t)
	token, err := fixture.jwt.IssueAccess(user.ID.String(), string(user.Role), 1)
	if err != nil {
		t.Fatal(err)
	}
	user.CredentialVersion = 2
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	if _, err := fixture.service.Authenticate(context.Background(), token); err == nil {
		t.Fatal("stale credential version was accepted")
	}
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	if _, err := fixture.service.Refresh(context.Background(), ""); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("empty refresh token error = %v", err)
	}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(biz.RefreshToken{}, biz.ErrRefreshTokenNotFound)
	if _, err := fixture.service.Refresh(context.Background(), "unknown"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("unknown refresh token error = %v", err)
	}
}

func TestRefreshReturnsFamilyRevocationError(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: uuid.New(), FamilyID: uuid.New(), RevokedAt: new(time.Time)}
	expected := errors.New("revoke failed")
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), gomock.Any()).Return(oldToken, nil)
	fixture.tokens.EXPECT().RevokeFamily(gomock.Any(), oldToken.UserID, oldToken.FamilyID, fixture.clock.Now().UTC(), true, gomock.Any()).Return(expected)
	if _, err := fixture.service.Refresh(context.Background(), "revoked"); !errors.Is(err, expected) {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestAuthenticateRejectsMalformedSubject(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	token, err := fixture.jwt.IssueAccess("not-a-uuid", "user", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Authenticate(context.Background(), token); err == nil {
		t.Fatal("malformed subject was accepted")
	}
}

func TestLoginReturnsTokenPersistenceError(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	fixture.users.EXPECT().GetUserByNormalizedUsername(gomock.Any(), "alice").Return(user, nil)
	expected := errors.New("database unavailable")
	fixture.tokens.EXPECT().CreateRefreshToken(gomock.Any(), gomock.Any(), gomock.Any()).Return(expected)
	if _, err := fixture.service.Login(context.Background(), "alice", "correct horse battery"); !errors.Is(err, expected) {
		t.Fatalf("Login() error = %v", err)
	}
}
