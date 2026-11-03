package service

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakePasswordResetRepository 是内存重置令牌存储，模拟单次消费与过期。
type fakePasswordResetRepository struct {
	// token 最近写入的令牌。
	token biz.PasswordResetToken
	// used 是否已消费。
	used bool
	// createErr 写入错误。
	createErr error
}

// CreatePasswordResetToken 保存令牌。
func (f *fakePasswordResetRepository) CreatePasswordResetToken(_ context.Context, token biz.PasswordResetToken, _ biz.OutboxEvent) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.token = token
	f.used = false
	return nil
}

// ConsumePasswordResetToken 校验哈希、过期与单次使用。
func (f *fakePasswordResetRepository) ConsumePasswordResetToken(_ context.Context, hash []byte, now time.Time) (uuid.UUID, error) {
	if f.used || f.token.UserID == uuid.Nil || !bytes.Equal(hash, f.token.TokenHash) || !now.Before(f.token.ExpiresAt) {
		return uuid.Nil, biz.ErrPasswordResetInvalid
	}
	f.used = true
	return f.token.UserID, nil
}

// TestRegistrationPolicyAndAdminInvite 验证关闭开放注册后仅管理员可邀请创建用户。
func TestRegistrationPolicyAndAdminInvite(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithRegistrationEnabled(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), "alice", "correct horse battery"); !errors.Is(err, ErrRegistrationDisabled) {
		t.Fatalf("disabled registration error = %v", err)
	}
	if _, err := service.CreateUser(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, "bob", "correct horse battery"); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin create user error = %v", err)
	}
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	fixture.users.EXPECT().CreateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, created biz.User, _ biz.OutboxEvent) (biz.User, error) {
			if created.Role != biz.RoleUser || created.Status != biz.UserStatusActive || created.PasswordHash == "correct horse battery" {
				t.Fatalf("created user = %#v", created)
			}
			return created, nil
		},
	)
	created, err := service.CreateUser(context.Background(), admin, "Bob", "correct horse battery")
	if err != nil || created.Username != "bob" || created.Role != biz.RoleUser {
		t.Fatalf("admin create user = %#v err=%v", created, err)
	}
}

// TestChangePasswordBumpsVersionAndRevokesAll 验证改密校验当前密码、提升版本并撤销全部令牌。
func TestChangePasswordBumpsVersionAndRevokesAll(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil).AnyTimes()
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.User, _ biz.OutboxEvent) (biz.User, error) {
			if updated.CredentialVersion != user.CredentialVersion+1 {
				t.Fatalf("credential version = %d", updated.CredentialVersion)
			}
			if ok, err := auth.VerifyPassword(updated.PasswordHash, "new password 12345"); err != nil || !ok {
				t.Fatalf("new password was not hashed: %v", err)
			}
			return updated, nil
		},
	)
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), user.ID, gomock.Any(), gomock.Any()).Return(nil)
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "correct horse battery", "new password 12345"); err != nil {
		t.Fatal(err)
	}

	// 错误当前密码与弱密码不得写入。
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "wrong password", "new password 12345"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v", err)
	}
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "correct horse battery", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password error = %v", err)
	}
	if err := fixture.service.ChangePassword(context.Background(), uuid.Nil, "x", "y"); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous change error = %v", err)
	}
}

// TestSetUserStatusRequiresAdminAndDisables 验证管理员禁用用户会提升版本并撤销令牌。
func TestSetUserStatusRequiresAdminAndDisables(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	if err := fixture.service.SetUserStatus(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser},
		user.ID, biz.UserStatusDisabled, "manual"); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.User, event biz.OutboxEvent) (biz.User, error) {
			if updated.Status != biz.UserStatusDisabled || updated.CredentialVersion != user.CredentialVersion+1 {
				t.Fatalf("status/version = %s/%d", updated.Status, updated.CredentialVersion)
			}
			if event.Payload["reason"] != "manual" {
				t.Fatalf("audit reason = %#v", event.Payload)
			}
			return updated, nil
		},
	)
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), user.ID, gomock.Any(), gomock.Any()).Return(nil)
	if err := fixture.service.SetUserStatus(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin},
		user.ID, biz.UserStatusDisabled, "manual"); err != nil {
		t.Fatal(err)
	}

	if err := fixture.service.SetUserStatus(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin},
		user.ID, biz.UserStatusDeleted, "invalid"); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid status error = %v", err)
	}
}

// TestIssueAndResetPassword 验证一次性令牌签发、过期前消费与二次消费拒绝。
func TestIssueAndResetPassword(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	resets := &fakePasswordResetRepository{}
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	if err != nil {
		t.Fatal(err)
	}
	user := activeUser(t)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	if _, _, err := service.IssuePasswordReset(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, user.ID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin issue error = %v", err)
	}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	token, expiresAt, err := service.IssuePasswordReset(context.Background(), admin, user.ID)
	if err != nil || token == "" || !expiresAt.After(fixture.clock.Now()) {
		t.Fatalf("issue reset = %q/%v/%v", token, expiresAt, err)
	}
	if len(resets.token.TokenHash) != 32 || bytes.Equal(resets.token.TokenHash, []byte(token)) {
		t.Fatalf("token hash was not stored safely: %#v", resets.token.TokenHash)
	}

	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.User, _ biz.OutboxEvent) (biz.User, error) {
			if ok, verifyErr := auth.VerifyPassword(updated.PasswordHash, "reset password 123"); verifyErr != nil || !ok {
				t.Fatalf("reset password was not hashed: %v", verifyErr)
			}
			return updated, nil
		},
	)
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), user.ID, gomock.Any(), gomock.Any()).Return(nil)
	if err := service.ResetPassword(context.Background(), token, "reset password 123"); err != nil {
		t.Fatal(err)
	}
	if err := service.ResetPassword(context.Background(), token, "reset password 456"); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("token reuse error = %v", err)
	}

	withoutReset, _ := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock)
	if err := withoutReset.ResetPassword(context.Background(), "token", "password123"); !errors.Is(err, biz.ErrPasswordResetUnavailable) {
		t.Fatalf("unavailable reset error = %v", err)
	}
}
