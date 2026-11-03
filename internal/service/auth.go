package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/Mark-Panda/harness-agent/internal/platform/clock"
	"github.com/google/uuid"
)

const RefreshTokenTTL = 30 * 24 * time.Hour

var (
	ErrInvalidCredentials  = errors.New("invalid username or password")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrRefreshTokenReused  = errors.New("refresh token reuse detected")
	ErrInvalidUsername     = errors.New("username must contain between 3 and 64 characters")
	// ErrRegistrationDisabled 开放注册已关闭，需要管理员邀请。
	ErrRegistrationDisabled = errors.New("open registration is disabled")
)

type PublicUser struct {
	ID       uuid.UUID      `json:"id"`
	Username string         `json:"username"`
	Role     biz.Role       `json:"role"`
	Status   biz.UserStatus `json:"status"`
}

type TokenPair struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type AuthResult struct {
	User   PublicUser `json:"user"`
	Tokens TokenPair  `json:"tokens"`
}

type AuthService struct {
	users  biz.UserRepository
	tokens biz.RefreshTokenRepository
	jwt    *auth.JWTManager
	pepper []byte
	clock  clock.Clock
	// replay 可选的刷新并发重放存储；为空时并发刷新按重用处理并撤销整族。
	replay RefreshReplayStore
	// replayTTL 重放窗口时长，必须远小于刷新令牌有效期。
	replayTTL time.Duration
	// resets 一次性密码重置令牌存储；为空表示未启用。
	resets biz.PasswordResetRepository
	// registrationEnabled 是否允许开放注册，默认开启。
	registrationEnabled bool
}

// PasswordResetTTL 是一次性密码重置令牌有效期。
const PasswordResetTTL = 15 * time.Minute

// RefreshReplay 保存一次成功轮换的短时结果及其后继令牌哈希。
// 重放前必须确认后继令牌仍然有效，避免注销后旧令牌在窗口内复活。
type RefreshReplay struct {
	// Result 是轮换返回的完整令牌对与用户信息。
	Result AuthResult `json:"result"`
	// SuccessorHash 是轮换生成的新令牌哈希，用于校验是否已被撤销或再次轮换。
	SuccessorHash []byte `json:"successor_hash"`
}

// RefreshReplayStore 保存刷新轮换的短时结果，用于同一令牌的并发刷新只成功一次。
// 实现必须保证记录不落日志、按令牌哈希隔离并在 TTL 后自动过期。
type RefreshReplayStore interface {
	// SaveRefreshReplay 保存一次成功轮换的令牌结果。
	SaveRefreshReplay(ctx context.Context, tokenHash []byte, replay RefreshReplay, ttl time.Duration) error
	// LoadRefreshReplay 读取重放结果；不存在时 found 为 false。
	LoadRefreshReplay(ctx context.Context, tokenHash []byte) (replay RefreshReplay, found bool, err error)
}

// DefaultRefreshReplayTTL 是并发刷新重放窗口，覆盖前端多标签页同时刷新的竞态。
const DefaultRefreshReplayTTL = 15 * time.Second

// AuthServiceOption 是鉴权服务的可选能力装配函数。
type AuthServiceOption func(*AuthService)

// WithRefreshReplay 启用刷新并发重放；ttl 为零时使用默认窗口。
func WithRefreshReplay(store RefreshReplayStore, ttl time.Duration) AuthServiceOption {
	return func(service *AuthService) {
		if ttl <= 0 {
			ttl = DefaultRefreshReplayTTL
		}
		service.replay = store
		service.replayTTL = ttl
	}
}

// WithPasswordReset 启用一次性密码重置令牌能力。
func WithPasswordReset(repo biz.PasswordResetRepository) AuthServiceOption {
	return func(service *AuthService) {
		service.resets = repo
	}
}

// WithRegistrationEnabled 设置开放注册开关；关闭后仅管理员可邀请创建用户。
func WithRegistrationEnabled(enabled bool) AuthServiceOption {
	return func(service *AuthService) {
		service.registrationEnabled = enabled
	}
}

func NewAuthService(users biz.UserRepository, tokens biz.RefreshTokenRepository, jwtManager *auth.JWTManager, pepper []byte, timeSource clock.Clock, options ...AuthServiceOption) (*AuthService, error) {
	if users == nil || tokens == nil || jwtManager == nil || len(pepper) < 32 {
		return nil, errors.New("auth service dependencies are required")
	}
	if timeSource == nil {
		timeSource = clock.System{}
	}
	service := &AuthService{users: users, tokens: tokens, jwt: jwtManager, pepper: append([]byte(nil), pepper...), clock: timeSource, replayTTL: DefaultRefreshReplayTTL, registrationEnabled: true}
	for _, option := range options {
		option(service)
	}
	return service, nil
}

func (s *AuthService) Register(ctx context.Context, username, password string) (AuthResult, error) {
	if !s.registrationEnabled {
		return AuthResult{}, ErrRegistrationDisabled
	}
	normalized := biz.NormalizeUsername(username)
	if len(normalized) < 3 || len(normalized) > 64 {
		return AuthResult{}, ErrInvalidUsername
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return AuthResult{}, err
	}
	user := biz.User{
		ID: uuid.New(), Username: normalized, PasswordHash: passwordHash, Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}
	user, err = s.users.CreateUser(ctx, user, authEvent(user.ID, "auth.user.registered", "user", user.ID.String(), "auth.register:"+user.ID.String(), "success"))
	if err != nil {
		return AuthResult{}, err
	}
	return s.issueSession(ctx, user, uuid.New(), nil)
}

// CreateUser 由管理员邀请创建普通用户；不签发令牌，初始密码通过安全渠道交付。
func (s *AuthService) CreateUser(ctx context.Context, actor biz.Principal, username, password string) (PublicUser, error) {
	if actor.UserID == uuid.Nil || actor.Role != biz.RoleAdmin {
		return PublicUser{}, biz.ErrForbidden
	}
	normalized := biz.NormalizeUsername(username)
	if len(normalized) < 3 || len(normalized) > 64 {
		return PublicUser{}, ErrInvalidUsername
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return PublicUser{}, err
	}
	user := biz.User{
		ID: uuid.New(), Username: normalized, PasswordHash: passwordHash, Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}
	user, err = s.users.CreateUser(ctx, user, authEvent(user.ID, "auth.user.created_by_admin", "user",
		user.ID.String(), "auth.user.created_by_admin:"+user.ID.String(), "success"))
	if err != nil {
		return PublicUser{}, err
	}
	return PublicUser{ID: user.ID, Username: user.Username, Role: user.Role, Status: user.Status}, nil
}

// Login 校验用户名密码并签发令牌。
func (s *AuthService) Login(ctx context.Context, username, password string) (AuthResult, error) {
	user, err := s.users.GetUserByNormalizedUsername(ctx, biz.NormalizeUsername(username))
	if errors.Is(err, biz.ErrUserNotFound) {
		return AuthResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return AuthResult{}, err
	}
	valid, err := auth.VerifyPassword(user.PasswordHash, password)
	if err != nil || !valid || user.Status != biz.UserStatusActive {
		return AuthResult{}, ErrInvalidCredentials
	}
	return s.issueSession(ctx, user, uuid.New(), nil)
}

func (s *AuthService) Refresh(ctx context.Context, rawToken string) (AuthResult, error) {
	hash, err := auth.HashOpaqueToken(rawToken, s.pepper)
	if err != nil {
		return AuthResult{}, ErrInvalidRefreshToken
	}
	oldToken, err := s.tokens.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		return AuthResult{}, ErrInvalidRefreshToken
	}
	now := s.clock.Now().UTC()
	if oldToken.ReuseDetectedAt != nil || oldToken.RevokedAt != nil {
		if err := s.tokens.RevokeFamily(ctx, oldToken.UserID, oldToken.FamilyID, now, true, authEvent(oldToken.UserID, "auth.refresh.reuse_detected", "refresh_token_family", oldToken.FamilyID.String(), "auth.refresh.reuse:"+oldToken.ID.String(), "denied")); err != nil {
			return AuthResult{}, fmt.Errorf("revoke reused token family: %w", err)
		}
		return AuthResult{}, ErrRefreshTokenReused
	}
	if oldToken.RotatedAt != nil {
		// 并发刷新：若同一次轮换的结果仍在重放窗口内，返回相同令牌对，避免误判重用。
		if result, ok := s.waitReplay(ctx, oldToken, hash); ok {
			return result, nil
		}
		if err := s.tokens.RevokeFamily(ctx, oldToken.UserID, oldToken.FamilyID, now, true, authEvent(oldToken.UserID, "auth.refresh.reuse_detected", "refresh_token_family", oldToken.FamilyID.String(), "auth.refresh.reuse:"+oldToken.ID.String(), "denied")); err != nil {
			return AuthResult{}, fmt.Errorf("revoke reused token family: %w", err)
		}
		return AuthResult{}, ErrRefreshTokenReused
	}
	if !now.Before(oldToken.ExpiresAt) {
		_ = s.tokens.RevokeRefreshToken(ctx, oldToken.UserID, oldToken.ID, now, authEvent(oldToken.UserID, "auth.refresh.expired", "refresh_token", oldToken.ID.String(), "auth.refresh.expired:"+oldToken.ID.String(), "denied"))
		return AuthResult{}, ErrInvalidRefreshToken
	}
	user, err := s.users.GetUserByID(ctx, oldToken.UserID)
	if err != nil || user.Status != biz.UserStatusActive || user.CredentialVersion != oldToken.CredentialVersion {
		_ = s.tokens.RevokeFamily(ctx, oldToken.UserID, oldToken.FamilyID, now, false, authEvent(oldToken.UserID, "auth.refresh.invalid", "refresh_token_family", oldToken.FamilyID.String(), "auth.refresh.invalid:"+oldToken.ID.String(), "denied"))
		return AuthResult{}, ErrInvalidRefreshToken
	}
	result, newToken, err := s.prepareSession(user, oldToken.FamilyID, &oldToken.ID)
	if err != nil {
		return AuthResult{}, err
	}
	if err := s.tokens.RotateRefreshToken(ctx, user.ID, oldToken.ID, hash, newToken, authEvent(user.ID, "auth.refresh.rotated", "refresh_token_family", oldToken.FamilyID.String(), "auth.refresh.rotated:"+newToken.ID.String(), "success")); err != nil {
		if errors.Is(err, biz.ErrRefreshTokenConflict) {
			// 另一个并发请求先完成了轮换，优先重放它的结果；确认不存在后才按重用撤销整族。
			if replay, ok := s.waitReplay(ctx, oldToken, hash); ok {
				return replay, nil
			}
			_ = s.tokens.RevokeFamily(ctx, user.ID, oldToken.FamilyID, now, true, authEvent(user.ID, "auth.refresh.reuse_detected", "refresh_token_family", oldToken.FamilyID.String(), "auth.refresh.reuse:"+oldToken.ID.String(), "denied"))
			return AuthResult{}, ErrRefreshTokenReused
		}
		return AuthResult{}, fmt.Errorf("rotate refresh token: %w", err)
	}
	// 轮换成功后写入短时重放记录，使同一令牌的并发刷新返回同一次结果。
	if s.replay != nil {
		replay := RefreshReplay{Result: result, SuccessorHash: newToken.TokenHash}
		if err := s.replay.SaveRefreshReplay(ctx, hash, replay, s.replayTTL); err != nil {
			slog.Warn("store refresh replay failed", "error", err)
		}
	}
	return result, nil
}

// waitReplay 在重放窗口内读取并发轮换结果，短暂重试以覆盖“先提交轮换、后写重放”的间隙。
func (s *AuthService) waitReplay(ctx context.Context, oldToken biz.RefreshToken, hash []byte) (AuthResult, bool) {
	if s.replay == nil {
		return AuthResult{}, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return AuthResult{}, false
			case <-time.After(50 * time.Millisecond):
			}
		}
		replay, found, err := s.replay.LoadRefreshReplay(ctx, hash)
		if err != nil || !found {
			continue
		}
		// 重放记录必须属于同一用户且包含完整令牌对，否则视为不可用。
		if replay.Result.User.ID != oldToken.UserID || replay.Result.Tokens.AccessToken == "" || replay.Result.Tokens.RefreshToken == "" {
			continue
		}
		// 后继令牌必须仍然有效；注销或再次轮换后不得通过重放复活旧会话。
		if len(replay.SuccessorHash) == 0 {
			continue
		}
		successor, err := s.tokens.GetRefreshTokenByHash(ctx, replay.SuccessorHash)
		if err != nil || successor.RevokedAt != nil || successor.RotatedAt != nil || successor.ReuseDetectedAt != nil {
			continue
		}
		return replay.Result, true
	}
	return AuthResult{}, false
}

func (s *AuthService) Authenticate(ctx context.Context, accessToken string) (biz.Principal, error) {
	claims, err := s.jwt.ParseAccess(accessToken)
	if err != nil {
		return biz.Principal{}, auth.ErrInvalidAccessToken
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return biz.Principal{}, auth.ErrInvalidAccessToken
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user.Status != biz.UserStatusActive || user.CredentialVersion != claims.CredentialVersion {
		return biz.Principal{}, auth.ErrInvalidAccessToken
	}
	return biz.NewPrincipal(userID, user.Role)
}

func (s *AuthService) Logout(ctx context.Context, rawToken string) error {
	hash, err := auth.HashOpaqueToken(rawToken, s.pepper)
	if err != nil {
		return nil
	}
	token, err := s.tokens.GetRefreshTokenByHash(ctx, hash)
	if errors.Is(err, biz.ErrRefreshTokenNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.tokens.RevokeRefreshToken(ctx, token.UserID, token.ID, s.clock.Now().UTC(), authEvent(token.UserID, "auth.logout", "refresh_token", token.ID.String(), "auth.logout:"+token.ID.String(), "success"))
}

func (s *AuthService) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	return s.tokens.RevokeAllUserTokens(ctx, userID, s.clock.Now().UTC(), authEvent(userID, "auth.logout_all", "user", userID.String(), "auth.logout_all:"+userID.String()+":"+uuid.NewString(), "success"))
}

// ChangePassword 校验当前密码后更新密码，并提升凭据版本使全部令牌失效。
func (s *AuthService) ChangePassword(ctx context.Context, userID uuid.UUID, currentPassword, newPassword string) error {
	if userID == uuid.Nil {
		return biz.ErrForbidden
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.Status != biz.UserStatusActive {
		return ErrInvalidCredentials
	}
	valid, verifyErr := auth.VerifyPassword(user.PasswordHash, currentPassword)
	if verifyErr != nil || !valid {
		return ErrInvalidCredentials
	}
	passwordHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = passwordHash
	user.CredentialVersion++
	if _, err := s.users.UpdateUser(ctx, user, authEvent(user.ID, "auth.password.changed", "user", user.ID.String(),
		"auth.password.changed:"+user.ID.String()+":"+uuid.NewString(), "success")); err != nil {
		return err
	}
	return s.tokens.RevokeAllUserTokens(ctx, user.ID, s.clock.Now().UTC(), authEvent(user.ID, "auth.logout_all",
		"user", user.ID.String(), "auth.password.changed.logout:"+user.ID.String()+":"+uuid.NewString(), "success"))
}

// SetUserStatus 由管理员启用或禁用用户；禁用时提升凭据版本并撤销全部令牌。
func (s *AuthService) SetUserStatus(ctx context.Context, actor biz.Principal, targetID uuid.UUID, status biz.UserStatus, reason string) error {
	if actor.UserID == uuid.Nil || actor.Role != biz.RoleAdmin {
		return biz.ErrForbidden
	}
	if status != biz.UserStatusActive && status != biz.UserStatusDisabled {
		return fmt.Errorf("%w: unsupported user status", biz.ErrInvalidConfig)
	}
	if len(reason) > 512 {
		reason = reason[:512]
	}
	user, err := s.users.GetUserByID(ctx, targetID)
	if err != nil || user.Status == biz.UserStatusDeleted {
		return biz.ErrUserNotFound
	}
	user.Status = status
	if status == biz.UserStatusDisabled {
		user.CredentialVersion++
	}
	event := authEvent(user.ID, "auth.user."+string(status), "user", user.ID.String(),
		"auth.user.status:"+user.ID.String()+":"+uuid.NewString(), "success")
	if reason != "" {
		event.Payload["reason"] = reason
	}
	if _, err := s.users.UpdateUser(ctx, user, event); err != nil {
		return err
	}
	if status == biz.UserStatusDisabled {
		return s.tokens.RevokeAllUserTokens(ctx, user.ID, s.clock.Now().UTC(), authEvent(user.ID, "auth.logout_all",
			"user", user.ID.String(), "auth.user.disabled.logout:"+user.ID.String()+":"+uuid.NewString(), "success"))
	}
	return nil
}

// IssuePasswordReset 由管理员生成一次性短期改密令牌，明文只返回一次。
func (s *AuthService) IssuePasswordReset(ctx context.Context, actor biz.Principal, targetID uuid.UUID) (string, time.Time, error) {
	if s.resets == nil {
		return "", time.Time{}, biz.ErrPasswordResetUnavailable
	}
	if actor.UserID == uuid.Nil || actor.Role != biz.RoleAdmin {
		return "", time.Time{}, biz.ErrForbidden
	}
	user, err := s.users.GetUserByID(ctx, targetID)
	if err != nil || user.Status == biz.UserStatusDeleted {
		return "", time.Time{}, biz.ErrUserNotFound
	}
	token, err := auth.NewOpaqueToken()
	if err != nil {
		return "", time.Time{}, err
	}
	hash, err := auth.HashOpaqueToken(token, s.pepper)
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.clock.Now().UTC()
	expiresAt := now.Add(PasswordResetTTL)
	if err := s.resets.CreatePasswordResetToken(ctx, biz.PasswordResetToken{
		ID: uuid.New(), UserID: targetID, TokenHash: hash, ExpiresAt: expiresAt, CreatedAt: now,
	}, authEvent(targetID, "auth.password.reset.issued", "user", targetID.String(),
		"auth.password.reset.issued:"+uuid.NewString(), "success")); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// ResetPassword 消费一次性令牌并设置新密码，同时提升凭据版本撤销全部令牌。
func (s *AuthService) ResetPassword(ctx context.Context, token, newPassword string) error {
	if s.resets == nil {
		return biz.ErrPasswordResetUnavailable
	}
	hash, err := auth.HashOpaqueToken(token, s.pepper)
	if err != nil {
		return biz.ErrPasswordResetInvalid
	}
	now := s.clock.Now().UTC()
	userID, err := s.resets.ConsumePasswordResetToken(ctx, hash, now)
	if err != nil {
		return biz.ErrPasswordResetInvalid
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user.Status == biz.UserStatusDeleted {
		return biz.ErrPasswordResetInvalid
	}
	passwordHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = passwordHash
	user.CredentialVersion++
	if _, err := s.users.UpdateUser(ctx, user, authEvent(user.ID, "auth.password.reset", "user", user.ID.String(),
		"auth.password.reset:"+user.ID.String()+":"+uuid.NewString(), "success")); err != nil {
		return err
	}
	return s.tokens.RevokeAllUserTokens(ctx, user.ID, now, authEvent(user.ID, "auth.logout_all",
		"user", user.ID.String(), "auth.password.reset.logout:"+user.ID.String()+":"+uuid.NewString(), "success"))
}

func (s *AuthService) issueSession(ctx context.Context, user biz.User, familyID uuid.UUID, parentTokenID *uuid.UUID) (AuthResult, error) {
	result, token, err := s.prepareSession(user, familyID, parentTokenID)
	if err != nil {
		return AuthResult{}, err
	}
	if err := s.tokens.CreateRefreshToken(ctx, token, authEvent(user.ID, "auth.session.issued", "refresh_token_family", familyID.String(), "auth.session.issued:"+familyID.String(), "success")); err != nil {
		return AuthResult{}, fmt.Errorf("create refresh token: %w", err)
	}
	return result, nil
}

func (s *AuthService) prepareSession(user biz.User, familyID uuid.UUID, parentTokenID *uuid.UUID) (AuthResult, biz.RefreshToken, error) {
	now := s.clock.Now().UTC()
	rawToken, err := auth.NewOpaqueToken()
	if err != nil {
		return AuthResult{}, biz.RefreshToken{}, fmt.Errorf("generate refresh token: %w", err)
	}
	tokenHash, err := auth.HashOpaqueToken(rawToken, s.pepper)
	if err != nil {
		return AuthResult{}, biz.RefreshToken{}, err
	}
	accessToken, err := s.jwt.IssueAccess(user.ID.String(), string(user.Role), user.CredentialVersion)
	if err != nil {
		return AuthResult{}, biz.RefreshToken{}, err
	}
	refreshToken := biz.RefreshToken{
		ID: uuid.New(), UserID: user.ID, FamilyID: familyID, TokenHash: tokenHash, ParentTokenID: parentTokenID,
		CredentialVersion: user.CredentialVersion, ExpiresAt: now.Add(RefreshTokenTTL), CreatedAt: now,
	}
	result := AuthResult{
		User: PublicUser{ID: user.ID, Username: user.Username, Role: user.Role, Status: user.Status},
		Tokens: TokenPair{
			AccessToken: accessToken, RefreshToken: rawToken, TokenType: "Bearer",
			AccessExpiresAt: now.Add(auth.AccessTokenTTL), RefreshExpiresAt: refreshToken.ExpiresAt,
		},
	}
	return result, refreshToken, nil
}

func authEvent(userID uuid.UUID, eventType, aggregateType, aggregateID, dedupeKey, outcome string) biz.OutboxEvent {
	return biz.OutboxEvent{
		UserID: userID, EventType: eventType, AggregateType: aggregateType, AggregateID: aggregateID,
		DedupeKey: dedupeKey, Payload: map[string]any{"outcome": outcome},
	}
}
