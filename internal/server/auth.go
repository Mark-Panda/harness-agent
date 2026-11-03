package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/Mark-Panda/harness-agent/internal/platform/ratelimit"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/google/uuid"
)

// 鉴权 Cookie 与 CSRF 协议常量。
const (
	// refreshCookieName 是 HttpOnly 刷新令牌 Cookie 名。
	refreshCookieName = "harness_refresh"
	// csrfCookieName 是双提交校验用的 CSRF Cookie 名，必须允许脚本读取。
	csrfCookieName = "harness_csrf"
	// csrfHeaderName 是客户端回传 CSRF 令牌的请求头。
	csrfHeaderName = "X-CSRF-Token"
	// refreshCookiePath 限制刷新 Cookie 只发送到鉴权接口。
	refreshCookiePath = "/api/v1/auth"
	// csrfCookiePath 使 SPA 全站可读取 CSRF Cookie。
	csrfCookiePath = "/"
	// authTransportCookie 表示浏览器 Cookie 模式，响应不回传刷新令牌。
	authTransportCookie = "cookie"
	// authTransportBearer 表示非浏览器 Bearer 模式，响应回传完整令牌对。
	authTransportBearer = "bearer"
)

// AuthApplication 是鉴权应用服务接口，HTTP 层只依赖该抽象便于测试替换。
type AuthApplication interface {
	// Register 注册用户并签发令牌。
	Register(ctx context.Context, username, password string) (service.AuthResult, error)
	// Login 校验密码并签发令牌。
	Login(ctx context.Context, username, password string) (service.AuthResult, error)
	// Refresh 轮换刷新令牌并返回新令牌对。
	Refresh(ctx context.Context, rawToken string) (service.AuthResult, error)
	// Authenticate 校验 Access Token 并返回调用者身份。
	Authenticate(ctx context.Context, accessToken string) (biz.Principal, error)
	// Logout 撤销当前刷新令牌。
	Logout(ctx context.Context, rawToken string) error
	// LogoutAll 撤销该用户全部令牌并提升凭据版本。
	LogoutAll(ctx context.Context, userID uuid.UUID) error
	// ChangePassword 校验当前密码后更新密码并使全部令牌失效。
	ChangePassword(ctx context.Context, userID uuid.UUID, currentPassword, newPassword string) error
	// SetUserStatus 由管理员启用或禁用用户。
	SetUserStatus(ctx context.Context, actor biz.Principal, targetID uuid.UUID, status biz.UserStatus, reason string) error
	// IssuePasswordReset 由管理员生成一次性短期改密令牌。
	IssuePasswordReset(ctx context.Context, actor biz.Principal, targetID uuid.UUID) (string, time.Time, error)
	// ResetPassword 消费一次性令牌并设置新密码。
	ResetPassword(ctx context.Context, token, newPassword string) error
	// CreateUser 由管理员邀请创建普通用户。
	CreateUser(ctx context.Context, actor biz.Principal, username, password string) (service.PublicUser, error)
}

// AuthRateLimiter 是登录/注册限流抽象，按 IP 或用户名维度计数。
type AuthRateLimiter interface {
	// Allow 判断本次请求是否放行并返回重试间隔。
	Allow(ctx context.Context, key string) (ratelimit.Result, error)
	// Reset 成功登录后清空计数。
	Reset(ctx context.Context, key string) error
}

// authCookiePolicy 控制鉴权 Cookie 的安全属性。
type authCookiePolicy struct {
	// Secure 为真时 Cookie 仅通过 HTTPS 发送；生产环境必须开启。
	Secure bool
}

type authHTTPHandler struct {
	service AuthApplication
	limiter AuthRateLimiter
	// userLimiter 是按登录用户维度的 API 限流器，可为空。
	userLimiter AuthRateLimiter
	// ipResolver 按可信代理配置解析客户端 IP，为空时只使用直连地址。
	ipResolver *clientIPResolver
	// cookies 是 Cookie 安全策略。
	cookies authCookiePolicy
}

// clientIP 返回用于限流与审计的客户端 IP，遵循可信代理策略。
func (h *authHTTPHandler) clientIP(r *http.Request) string {
	return h.ipResolver.resolve(r)
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Transport 指定令牌交付方式：cookie（默认）或 bearer。
	Transport string `json:"transport"`
}

type refreshTokenRequest struct {
	RefreshToken string `json:"refresh_token"`
	// Transport 指定令牌交付方式：cookie（默认）或 bearer。
	Transport string `json:"transport"`
}

// browserAuthResponse 是 Cookie 模式的登录响应，刻意不包含刷新令牌。
type browserAuthResponse struct {
	// User 是公开用户信息。
	User service.PublicUser `json:"user"`
	// AccessToken 是短期访问令牌，前端只能保存在内存。
	AccessToken string `json:"access_token"`
	// TokenType 固定为 Bearer。
	TokenType string `json:"token_type"`
	// AccessExpiresAt 是访问令牌过期时间。
	AccessExpiresAt time.Time `json:"access_expires_at"`
	// RefreshExpiresAt 是刷新令牌过期时间。
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	// CSRF 是本次会话的 CSRF 令牌，前端随写请求回传。
	CSRF string `json:"csrf_token"`
}

func (h *authHTTPHandler) register(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	var request credentialsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !h.allowRequest(w, r, "register:"+h.clientIP(r)) {
		return
	}
	result, err := h.service.Register(r.Context(), request.Username, request.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	h.writeSessionResult(w, http.StatusCreated, result, request.Transport)
}

func (h *authHTTPHandler) login(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	var request credentialsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rateLimitKey := "login:" + biz.NormalizeUsername(request.Username) + "|" + h.clientIP(r)
	if !h.allowRequest(w, r, rateLimitKey) {
		return
	}
	result, err := h.service.Login(r.Context(), request.Username, request.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if h.limiter != nil {
		_ = h.limiter.Reset(r.Context(), rateLimitKey)
	}
	h.writeSessionResult(w, http.StatusOK, result, request.Transport)
}

// refresh 处理刷新请求：优先读取 HttpOnly Cookie，非浏览器客户端可用请求体令牌。
func (h *authHTTPHandler) refresh(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	if cookie, err := r.Cookie(refreshCookieName); err == nil && cookie.Value != "" {
		// Cookie 鉴权的写操作必须通过双提交 CSRF 校验。
		if !h.requireCSRF(w, r) {
			return
		}
		result, refreshErr := h.service.Refresh(r.Context(), cookie.Value)
		if refreshErr != nil {
			h.clearSessionCookies(w)
			writeAuthError(w, refreshErr)
			return
		}
		// 刷新并发时复用现有 CSRF 令牌，避免多标签页因 Cookie 值不一致而互相踢下线。
		csrf := ""
		if existing, csrfErr := r.Cookie(csrfCookieName); csrfErr == nil {
			csrf = existing.Value
		}
		h.writeSessionResult(w, http.StatusOK, result, authTransportCookie, csrf)
		return
	}
	var request refreshTokenRequest
	if !decodeOptionalJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.RefreshToken) == "" {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	result, err := h.service.Refresh(r.Context(), request.RefreshToken)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	h.writeSessionResult(w, http.StatusOK, result, request.Transport)
}

// logout 处理注销：Cookie 模式撤销 Cookie 中的刷新令牌并清理会话 Cookie。
func (h *authHTTPHandler) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	if cookie, err := r.Cookie(refreshCookieName); err == nil && cookie.Value != "" {
		if !h.requireCSRF(w, r) {
			return
		}
		err := h.service.Logout(r.Context(), cookie.Value)
		h.clearSessionCookies(w)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var request refreshTokenRequest
	if !decodeOptionalJSON(w, r, &request) {
		return
	}
	if err := h.service.Logout(r.Context(), request.RefreshToken); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHTTPHandler) logoutAll(w http.ResponseWriter, r *http.Request) {
	principal, ok := biz.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.LogoutAll(r.Context(), principal.UserID); err != nil {
		writeAuthError(w, err)
		return
	}
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// passwordChangeRequest 是当前用户修改密码的请求体。
type passwordChangeRequest struct {
	// CurrentPassword 当前密码。
	CurrentPassword string `json:"current_password"`
	// NewPassword 新密码，长度 12–1024 字节。
	NewPassword string `json:"new_password"`
}

// changePassword 处理 POST /api/v1/auth/password：校验当前密码后更新密码。
func (h *authHTTPHandler) changePassword(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	principal, ok := biz.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request passwordChangeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if err := h.service.ChangePassword(r.Context(), principal.UserID, request.CurrentPassword, request.NewPassword); err != nil {
		writeAuthError(w, err)
		return
	}
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// passwordResetRequest 是匿名消费一次性重置令牌的请求体。
type passwordResetRequest struct {
	// Token 一次性重置令牌。
	Token string `json:"token"`
	// NewPassword 新密码。
	NewPassword string `json:"new_password"`
}

// resetPassword 处理 POST /api/v1/auth/password/reset：消费一次性令牌并设置新密码。
func (h *authHTTPHandler) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	var request passwordResetRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !h.allowRequest(w, r, "password_reset:"+h.clientIP(r)) {
		return
	}
	if err := h.service.ResetPassword(r.Context(), request.Token, request.NewPassword); err != nil {
		writeAuthError(w, err)
		return
	}
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// userStatusRequest 是管理员启用/禁用用户的请求体。
type userStatusRequest struct {
	// Status 目标状态：active/disabled。
	Status string `json:"status"`
	// Reason 操作理由，写入审计。
	Reason string `json:"reason"`
}

// setUserStatus 处理 POST /api/v1/admin/users/{id}/status：管理员启用或禁用用户。
func (h *authHTTPHandler) setUserStatus(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	principal, ok := biz.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var request userStatusRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if err := h.service.SetUserStatus(r.Context(), principal, id, biz.UserStatus(request.Status), request.Reason); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issuePasswordReset 处理 POST /api/v1/admin/users/{id}/password-reset：生成一次性短期改密令牌。
func (h *authHTTPHandler) issuePasswordReset(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	principal, ok := biz.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	token, expiresAt, err := h.service.IssuePasswordReset(r.Context(), principal, id)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expiresAt})
}

// createUser 处理 POST /api/v1/admin/users：管理员邀请创建普通用户。
func (h *authHTTPHandler) createUser(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeErrorCode(w, http.StatusForbidden, "cross_origin_request")
		return
	}
	principal, ok := biz.PrincipalFromContext(r.Context())
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request credentialsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	user, err := h.service.CreateUser(r.Context(), principal, request.Username, request.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

// csrf 处理 GET /api/v1/auth/csrf：下发可被脚本读取的 CSRF Cookie，供刷新前双提交。
func (h *authHTTPHandler) csrf(w http.ResponseWriter, r *http.Request) {
	// CSRF 引导接口只生成随机令牌，不消耗登录限流预算，避免正常页面刷新被误限流。
	token, err := newCSRFToken()
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal_error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: token, Path: csrfCookiePath,
		MaxAge: int((30 * 24 * time.Hour).Seconds()), HttpOnly: false,
		Secure: h.cookies.Secure, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": token})
}

// writeSessionResult 按传输模式写出令牌：Cookie 模式设置 Cookie 并省略刷新令牌。
// reuseCSRF 非空时沿用调用方已有的 CSRF 令牌，否则生成新令牌。
func (h *authHTTPHandler) writeSessionResult(w http.ResponseWriter, status int, result service.AuthResult, transport string, reuseCSRF ...string) {
	if transport == authTransportBearer {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, status, result)
		return
	}
	csrf := ""
	if len(reuseCSRF) > 0 {
		csrf = strings.TrimSpace(reuseCSRF[0])
	}
	if csrf == "" {
		token, err := newCSRFToken()
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "internal_error")
			return
		}
		csrf = token
	}
	h.setSessionCookies(w, result, csrf)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, browserAuthResponse{
		User: result.User, AccessToken: result.Tokens.AccessToken, TokenType: result.Tokens.TokenType,
		AccessExpiresAt: result.Tokens.AccessExpiresAt, RefreshExpiresAt: result.Tokens.RefreshExpiresAt, CSRF: csrf,
	})
}

// setSessionCookies 设置 HttpOnly 刷新 Cookie 与可读 CSRF Cookie。
func (h *authHTTPHandler) setSessionCookies(w http.ResponseWriter, result service.AuthResult, csrf string) {
	maxAge := int(service.RefreshTokenTTL.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: result.Tokens.RefreshToken, Path: refreshCookiePath,
		MaxAge: maxAge, HttpOnly: true, Secure: h.cookies.Secure, SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: csrf, Path: csrfCookiePath,
		MaxAge: maxAge, HttpOnly: false, Secure: h.cookies.Secure, SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookies 删除刷新与 CSRF Cookie，用于注销或令牌失效。
func (h *authHTTPHandler) clearSessionCookies(w http.ResponseWriter) {
	for _, cookie := range []struct{ name, path string }{
		{refreshCookieName, refreshCookiePath}, {csrfCookieName, csrfCookiePath},
	} {
		http.SetCookie(w, &http.Cookie{
			Name: cookie.name, Value: "", Path: cookie.path, MaxAge: -1,
			HttpOnly: cookie.name == refreshCookieName, Secure: h.cookies.Secure, SameSite: http.SameSiteStrictMode,
		})
	}
}

// requireCSRF 校验双提交令牌：Cookie 与请求头必须存在且常量时间相等。
func (h *authHTTPHandler) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		writeErrorCode(w, http.StatusForbidden, "csrf_token_missing")
		return false
	}
	header := strings.TrimSpace(r.Header.Get(csrfHeaderName))
	if header == "" {
		writeErrorCode(w, http.StatusForbidden, "csrf_token_missing")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
		writeErrorCode(w, http.StatusForbidden, "csrf_token_invalid")
		return false
	}
	return true
}

// requireAuth 校验 Bearer 访问令牌，并把调用者身份注入 Context。
func (h *authHTTPHandler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.service == nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "auth_unavailable")
			return
		}
		header := r.Header.Get("Authorization")
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		principal, err := h.service.Authenticate(r.Context(), strings.TrimSpace(parts[1]))
		if err != nil {
			writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !h.allowRequest(w, r, "api_user:"+principal.UserID.String()) {
			return
		}
		next(w, r.WithContext(biz.WithPrincipal(r.Context(), principal)))
	}
}

func (h *authHTTPHandler) allowRequest(w http.ResponseWriter, r *http.Request, key string) bool {
	limiter := h.limiter
	if strings.HasPrefix(key, "api_user:") {
		limiter = h.userLimiter
	}
	if limiter == nil {
		return true
	}
	result, err := limiter.Allow(r.Context(), key)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "rate_limit_unavailable")
		return false
	}
	if !result.Allowed {
		seconds := int(math.Ceil(result.RetryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited")
		return false
	}
	return true
}

// newCSRFToken 生成 32 字节随机 CSRF 令牌的十六进制编码。
func newCSRFToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// sameOriginRequest 拒绝明显的跨站写请求；无 Origin 的非浏览器客户端不受影响。
func sameOriginRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIPResolver 按显式配置的可信代理网段解析客户端 IP。
// 关键步骤：直连地址不在可信网段时忽略 X-Forwarded-For；
// 在可信网段内时从右向左跳过可信代理，取第一个非可信地址，防止客户端伪造来源。
type clientIPResolver struct {
	// trusted 是可信任的代理网段。
	trusted []*net.IPNet
}

// newClientIPResolver 解析可信代理 CIDR 列表；空列表返回 nil（只使用直连地址）。
func newClientIPResolver(cidrs []string) (*clientIPResolver, error) {
	resolver := &clientIPResolver{}
	for _, raw := range cidrs {
		cidr := strings.TrimSpace(raw)
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("parse trusted proxy CIDR %q: %w", cidr, err)
		}
		resolver.trusted = append(resolver.trusted, network)
	}
	if len(resolver.trusted) == 0 {
		return nil, nil
	}
	return resolver, nil
}

// contains 判断地址是否落在任一可信网段。
func (r *clientIPResolver) contains(ip net.IP) bool {
	for _, network := range r.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// resolve 返回客户端 IP；解析器为空时等价于只使用直连地址。
func (r *clientIPResolver) resolve(req *http.Request) string {
	remote := clientIP(req)
	if r == nil {
		return remote
	}
	remoteIP := net.ParseIP(remote)
	if remoteIP == nil || !r.contains(remoteIP) {
		return remote
	}
	header := req.Header.Get("X-Forwarded-For")
	if header == "" {
		return remote
	}
	parts := strings.Split(header, ",")
	// 转发链长度异常时放弃推断，回退直连地址，避免消耗解析资源。
	if len(parts) > 32 {
		return remote
	}
	// 从右向左跳过可信代理；全部可信时返回最左侧地址。
	fallback := remote
	for index := len(parts) - 1; index >= 0; index-- {
		candidate := strings.TrimSpace(parts[index])
		ip := net.ParseIP(candidate)
		if ip == nil {
			return remote
		}
		if r.contains(ip) {
			fallback = candidate
			continue
		}
		return candidate
	}
	return fallback
}

// decodeJSON 解码必填 JSON 请求体，并拒绝未知字段与超大请求。
func decodeJSON(w http.ResponseWriter, r *http.Request, target any, limits ...int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maximumBody(limits...))
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

// decodeOptionalJSON 解码可空 JSON 请求体，空请求体视为未提供字段。
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximumBody())
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeErrorCode(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

// maximumBody 返回请求体上限，默认 1MiB。
func maximumBody(limits ...int64) int64 {
	if len(limits) > 0 && limits[0] > 0 {
		return limits[0]
	}
	return int64(1 << 20)
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidCredentials),
		errors.Is(err, service.ErrInvalidRefreshToken),
		errors.Is(err, service.ErrRefreshTokenReused),
		errors.Is(err, auth.ErrInvalidAccessToken):
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, biz.ErrUserAlreadyExists):
		writeErrorCode(w, http.StatusConflict, "user_already_exists")
	case errors.Is(err, biz.ErrForbidden):
		writeErrorCode(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, service.ErrRegistrationDisabled):
		writeErrorCode(w, http.StatusForbidden, "registration_disabled")
	case errors.Is(err, biz.ErrUserNotFound):
		writeErrorCode(w, http.StatusNotFound, "user_not_found")
	case errors.Is(err, biz.ErrPasswordResetInvalid):
		writeErrorCode(w, http.StatusBadRequest, "invalid_reset_token")
	case errors.Is(err, biz.ErrPasswordResetUnavailable):
		writeErrorCode(w, http.StatusServiceUnavailable, "password_reset_unavailable")
	case errors.Is(err, biz.ErrInvalidConfig):
		writeErrorCode(w, http.StatusBadRequest, "invalid_configuration")
	case errors.Is(err, service.ErrInvalidUsername), errors.Is(err, auth.ErrWeakPassword):
		writeErrorCode(w, http.StatusBadRequest, "invalid_credentials")
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal_error")
	}
}

func writeErrorCode(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}
