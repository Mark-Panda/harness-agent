package service

import (
	"context"
	"errors"
	"strings"
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

// TestListUsersRequiresAdminAndMapsDirectory 验证用户目录的管理员校验、筛选透传与时间字段映射。
func TestListUsersRequiresAdminAndMapsDirectory(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	regular := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	if _, err := fixture.service.ListUsers(context.Background(), regular, biz.UserFilter{}, 10, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin list users error = %v", err)
	}

	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	if _, err := fixture.service.ListUsers(context.Background(), admin, biz.UserFilter{Status: "unknown"}, 10, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid status error = %v", err)
	}
	if _, err := fixture.service.ListUsers(context.Background(), admin, biz.UserFilter{Search: strings.Repeat("a", 65)}, 10, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("long search error = %v", err)
	}

	targetID := uuid.New()
	createdAt := fixture.clock.Now().UTC()
	updatedAt := createdAt.Add(time.Minute)
	fixture.users.EXPECT().ListUsers(gomock.Any(), biz.UserFilter{Status: biz.UserStatusActive, Search: "ali"}, 10, "").Return(biz.Page[biz.User]{
		NextCursor: "next",
		Items: []biz.User{{
			ID: targetID, Username: "alice", Role: biz.RoleAdmin, Status: biz.UserStatusActive,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		}},
	}, nil)
	page, err := fixture.service.ListUsers(context.Background(), admin, biz.UserFilter{Status: biz.UserStatusActive, Search: " ali "}, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "next" || len(page.Items) != 1 {
		t.Fatalf("unexpected page: %#v", page)
	}
	item := page.Items[0]
	if item.ID != targetID || item.Username != "alice" || item.Role != biz.RoleAdmin || item.Status != biz.UserStatusActive {
		t.Fatalf("unexpected user: %#v", item)
	}
	if !item.CreatedAt.Equal(createdAt) || !item.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("unexpected timestamps: %#v", item)
	}
}

// TestSetUserRoleGuardsAndAudit 验证角色调整的管理员校验、自降级保护与审计载荷。
func TestSetUserRoleGuardsAndAudit(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	regular := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	if err := fixture.service.SetUserRole(context.Background(), regular, uuid.New(), biz.RoleAdmin, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin role error = %v", err)
	}
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	if err := fixture.service.SetUserRole(context.Background(), admin, admin.UserID, biz.RoleUser, ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("self demotion error = %v", err)
	}
	if err := fixture.service.SetUserRole(context.Background(), admin, uuid.New(), biz.Role("owner"), ""); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid role error = %v", err)
	}

	target := biz.User{ID: uuid.New(), Username: "bob", Role: biz.RoleUser, Status: biz.UserStatusActive}
	fixture.users.EXPECT().GetUserByID(gomock.Any(), target.ID).Return(target, nil)
	fixture.users.EXPECT().UpdateUserRole(gomock.Any(), target.ID, biz.RoleAdmin, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ biz.Role, event biz.OutboxEvent) error {
			if event.Payload["role"] != string(biz.RoleAdmin) || event.Payload["actor_id"] != admin.UserID.String() {
				t.Fatalf("unexpected role audit payload: %#v", event.Payload)
			}
			return nil
		},
	)
	if err := fixture.service.SetUserRole(context.Background(), admin, target.ID, biz.RoleAdmin, "promote"); err != nil {
		t.Fatal(err)
	}
}

// fakeFeishuLoginClient 提供可控的飞书授权码交换结果。
type fakeFeishuLoginClient struct {
	openID    string
	tenantKey string
	err       error
}

func (f fakeFeishuLoginClient) AuthorizeURL(redirectURI, state string) string {
	return "https://example.com/authorize?redirect=" + redirectURI + "&state=" + state
}

func (f fakeFeishuLoginClient) Exchange(context.Context, string, string) (string, string, string, error) {
	return f.openID, f.tenantKey, "Alice", f.err
}

// TestFeishuLoginMapsIdentityAndIssuesSession 验证飞书 open_id 映射后签发内部会话。
func TestFeishuLoginMapsIdentityAndIssuesSession(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	ctrl := gomock.NewController(t)
	identities := mocks.NewMockFeishuIdentityStore(ctrl)
	client := fakeFeishuLoginClient{openID: "ou_alice", tenantKey: "tenant_1"}
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock,
		WithFeishuLogin(client, identities, "https://example.com/callback"))
	if err != nil {
		t.Fatal(err)
	}
	if !service.FeishuLoginEnabled() {
		t.Fatal("feishu login should be enabled")
	}
	if _, ok := service.FeishuAuthorizeURL("state-1"); !ok {
		t.Fatal("authorize URL should be available")
	}

	user := activeUser(t)
	identities.EXPECT().Resolve(gomock.Any(), "tenant_1", "ou_alice").Return(biz.FeishuIdentity{UserID: user.ID}, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().CreateRefreshToken(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	result, err := service.LoginWithFeishu(context.Background(), "code-1")
	if err != nil || result.User.ID != user.ID || result.Tokens.AccessToken == "" {
		t.Fatalf("LoginWithFeishu() = %#v, %v", result, err)
	}

	plain, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.LoginWithFeishu(context.Background(), "code-1"); !errors.Is(err, ErrFeishuLoginUnavailable) {
		t.Fatalf("disabled feishu login error = %v", err)
	}
}
