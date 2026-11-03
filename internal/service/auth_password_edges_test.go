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

// TestChangePasswordErrorPaths 覆盖读取、状态、写入与撤销失败。
func TestChangePasswordErrorPaths(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	user := activeUser(t)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(biz.User{}, biz.ErrUserNotFound)
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "old", "new password 123"); !errors.Is(err, biz.ErrUserNotFound) {
		t.Fatalf("read error = %v", err)
	}
	disabled := user
	disabled.Status = biz.UserStatusDisabled
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(disabled, nil)
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "correct horse battery", "new password 123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled error = %v", err)
	}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil).AnyTimes()
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.User{}, errors.New("db down"))
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "correct horse battery", "new password 123"); err == nil {
		t.Fatal("update error was ignored")
	}
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(user, nil)
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), user.ID, gomock.Any(), gomock.Any()).Return(errors.New("redis down"))
	if err := fixture.service.ChangePassword(context.Background(), user.ID, "correct horse battery", "new password 123"); err == nil {
		t.Fatal("revoke error was ignored")
	}
}

// TestSetUserStatusErrorPaths 覆盖启用、未找到、写入失败与理由截断。
func TestSetUserStatusErrorPaths(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	user := activeUser(t)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(biz.User{}, biz.ErrUserNotFound)
	if err := fixture.service.SetUserStatus(context.Background(), admin, user.ID, biz.UserStatusDisabled, "x"); !errors.Is(err, biz.ErrUserNotFound) {
		t.Fatalf("missing user error = %v", err)
	}
	deleted := user
	deleted.Status = biz.UserStatusDeleted
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(deleted, nil)
	if err := fixture.service.SetUserStatus(context.Background(), admin, user.ID, biz.UserStatusDisabled, "x"); !errors.Is(err, biz.ErrUserNotFound) {
		t.Fatalf("deleted user error = %v", err)
	}
	disabled := user
	disabled.Status = biz.UserStatusDisabled
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(disabled, nil).AnyTimes()
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.User{}, errors.New("db down"))
	if err := fixture.service.SetUserStatus(context.Background(), admin, user.ID, biz.UserStatusActive, "re-enable"); err == nil {
		t.Fatal("enable update error was ignored")
	}
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.User, event biz.OutboxEvent) (biz.User, error) {
			if len(event.Payload["reason"].(string)) != 512 {
				t.Fatalf("reason length = %d", len(event.Payload["reason"].(string)))
			}
			return updated, nil
		},
	)
	if err := fixture.service.SetUserStatus(context.Background(), admin, user.ID, biz.UserStatusActive, string(make([]byte, 600))); err != nil {
		t.Fatal(err)
	}
}

// TestIssuePasswordResetErrorPaths 覆盖未装配、目标不存在与写入失败。
func TestIssuePasswordResetErrorPaths(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	user := activeUser(t)
	if _, _, err := fixture.service.IssuePasswordReset(context.Background(), admin, user.ID); !errors.Is(err, biz.ErrPasswordResetUnavailable) {
		t.Fatalf("unavailable error = %v", err)
	}
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(&fakePasswordResetRepository{}))
	if err != nil {
		t.Fatal(err)
	}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(biz.User{}, biz.ErrUserNotFound)
	if _, _, err := service.IssuePasswordReset(context.Background(), admin, user.ID); !errors.Is(err, biz.ErrUserNotFound) {
		t.Fatalf("missing user error = %v", err)
	}
	resets := &fakePasswordResetRepository{createErr: errors.New("db down")}
	service, _ = NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	if _, _, err := service.IssuePasswordReset(context.Background(), admin, user.ID); err == nil {
		t.Fatal("create error was ignored")
	}
}

// TestResetPasswordErrorPaths 覆盖空令牌、读取失败、弱密码与写入/撤销失败。
func TestResetPasswordErrorPaths(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	resets := &fakePasswordResetRepository{}
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ResetPassword(context.Background(), "", "new password 123"); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("empty token error = %v", err)
	}
	resetHash, err := auth.HashOpaqueToken("token", fixture.pepper)
	if err != nil {
		t.Fatal(err)
	}
	user := activeUser(t)
	resets.token = biz.PasswordResetToken{UserID: user.ID, TokenHash: resetHash, ExpiresAt: fixture.clock.Now().Add(time.Minute)}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(biz.User{}, biz.ErrUserNotFound)
	if err := service.ResetPassword(context.Background(), "token", "new password 123"); !errors.Is(err, biz.ErrPasswordResetInvalid) {
		t.Fatalf("read error = %v", err)
	}

	resets = &fakePasswordResetRepository{}
	service, _ = NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	resets.token = biz.PasswordResetToken{UserID: user.ID, TokenHash: resetHash, ExpiresAt: fixture.clock.Now().Add(time.Minute)}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil).AnyTimes()
	if err := service.ResetPassword(context.Background(), "token", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password error = %v", err)
	}
	// 重新签发令牌后覆盖写入与撤销失败。
	resets = &fakePasswordResetRepository{}
	service, _ = NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	resets.token = biz.PasswordResetToken{UserID: user.ID, TokenHash: resetHash, ExpiresAt: fixture.clock.Now().Add(time.Minute)}
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.User{}, errors.New("db down"))
	if err := service.ResetPassword(context.Background(), "token", "new password 123"); err == nil {
		t.Fatal("reset update error was ignored")
	}
	resets = &fakePasswordResetRepository{}
	service, _ = NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithPasswordReset(resets))
	resets.token = biz.PasswordResetToken{UserID: user.ID, TokenHash: resetHash, ExpiresAt: fixture.clock.Now().Add(time.Minute)}
	fixture.users.EXPECT().UpdateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(user, nil)
	fixture.tokens.EXPECT().RevokeAllUserTokens(gomock.Any(), user.ID, gomock.Any(), gomock.Any()).Return(errors.New("redis down"))
	if err := service.ResetPassword(context.Background(), "token", "new password 123"); err == nil {
		t.Fatal("reset revoke error was ignored")
	}
}

// TestCreateUserErrorPaths 覆盖用户名、弱密码与仓储失败。
func TestCreateUserErrorPaths(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	if _, err := fixture.service.CreateUser(context.Background(), admin, "x", "correct horse battery"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("invalid username error = %v", err)
	}
	if _, err := fixture.service.CreateUser(context.Background(), admin, "bob", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password error = %v", err)
	}
	fixture.users.EXPECT().CreateUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(biz.User{}, biz.ErrUserAlreadyExists)
	if _, err := fixture.service.CreateUser(context.Background(), admin, "bob", "correct horse battery"); !errors.Is(err, biz.ErrUserAlreadyExists) {
		t.Fatalf("repository error = %v", err)
	}
}
