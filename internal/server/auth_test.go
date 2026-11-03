package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/ratelimit"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/google/uuid"
	"testing/fstest"
)

type fakeAuthApplication struct {
	result        service.AuthResult
	err           error
	principal     biz.Principal
	logoutAllUser uuid.UUID
	loginCalls    int
	refreshToken  string
	logoutToken   string
	resetToken    string
	resetExpires  time.Time
	// operationErr 仅用于 SetUserStatus，避免污染 Authenticate 结果。
	operationErr error
	// feishuEnabled 控制飞书登录入口测试开关。
	feishuEnabled bool
}

func (f *fakeAuthApplication) Register(context.Context, string, string) (service.AuthResult, error) {
	return f.result, f.err
}
func (f *fakeAuthApplication) Login(context.Context, string, string) (service.AuthResult, error) {
	f.loginCalls++
	return f.result, f.err
}
func (f *fakeAuthApplication) Refresh(_ context.Context, token string) (service.AuthResult, error) {
	f.refreshToken = token
	return f.result, f.err
}
func (f *fakeAuthApplication) Authenticate(context.Context, string) (biz.Principal, error) {
	return f.principal, f.err
}
func (f *fakeAuthApplication) Logout(_ context.Context, token string) error {
	f.logoutToken = token
	return f.err
}
func (f *fakeAuthApplication) LogoutAll(_ context.Context, userID uuid.UUID) error {
	f.logoutAllUser = userID
	return f.err
}

// ChangePassword 返回预设错误，测试只验证路由与参数传递。
func (f *fakeAuthApplication) ChangePassword(context.Context, uuid.UUID, string, string) error {
	return f.err
}

// SetUserStatus 返回单独配置的操作错误。
func (f *fakeAuthApplication) SetUserStatus(context.Context, biz.Principal, uuid.UUID, biz.UserStatus, string) error {
	return f.operationErr
}

// IssuePasswordReset 返回预设令牌或错误。
func (f *fakeAuthApplication) IssuePasswordReset(context.Context, biz.Principal, uuid.UUID) (string, time.Time, error) {
	return f.resetToken, f.resetExpires, f.err
}

// ResetPassword 返回预设错误。
func (f *fakeAuthApplication) ResetPassword(context.Context, string, string) error {
	return f.err
}

// CreateUser 返回预设结果。
func (f *fakeAuthApplication) CreateUser(context.Context, biz.Principal, string, string) (service.PublicUser, error) {
	return f.result.User, f.err
}

// ListUsers 返回空分页或预设错误，测试只验证路由与管理权限。
func (f *fakeAuthApplication) ListUsers(context.Context, biz.Principal, biz.UserFilter, int, string) (biz.Page[service.AdminUser], error) {
	return biz.Page[service.AdminUser]{}, f.err
}

// SetUserRole 返回预设操作错误。
func (f *fakeAuthApplication) SetUserRole(context.Context, biz.Principal, uuid.UUID, biz.Role, string) error {
	return f.operationErr
}

// FeishuLoginEnabled 返回测试配置的开关。
func (f *fakeAuthApplication) FeishuLoginEnabled() bool {
	return f.feishuEnabled
}

// FeishuAuthorizeURL 生成测试授权地址。
func (f *fakeAuthApplication) FeishuAuthorizeURL(state string) (string, bool) {
	if !f.feishuEnabled {
		return "", false
	}
	return "https://accounts.example.com/authorize?state=" + state, true
}

// LoginWithFeishu 返回预设会话或错误。
func (f *fakeAuthApplication) LoginWithFeishu(context.Context, string) (service.AuthResult, error) {
	return f.result, f.err
}

func authTestHandler(application AuthApplication) http.Handler {
	return NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, application, nil)
}

type fakeRateLimiter struct {
	result   ratelimit.Result
	resetKey string
}

func (f *fakeRateLimiter) Allow(context.Context, string) (ratelimit.Result, error) {
	return f.result, nil
}
func (f *fakeRateLimiter) Reset(_ context.Context, key string) error {
	f.resetKey = key
	return nil
}

func TestAdminUserDirectoryRoute(t *testing.T) {
	application := &fakeAuthApplication{principal: biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}}
	handler := authTestHandler(application)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users?limit=5&status=active&q=ali", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"items"`) {
		t.Fatalf("list users returned %d %s", recorder.Code, recorder.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users?limit=0", nil)
	invalid.Header.Set("Authorization", "Bearer valid-token")
	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit returned %d", invalidRecorder.Code)
	}
}

// TestFeishuLoginRoutes 验证飞书登录状态、授权跳转与 state 校验。
func TestFeishuLoginRoutes(t *testing.T) {
	application := &fakeAuthApplication{
		feishuEnabled: true,
		result:        service.AuthResult{Tokens: service.TokenPair{RefreshToken: "refresh"}},
	}
	handler := authTestHandler(application)

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/auth/feishu/status", nil))
	if statusRecorder.Code != http.StatusOK || !strings.Contains(statusRecorder.Body.String(), `"enabled":true`) {
		t.Fatalf("feishu status returned %d %s", statusRecorder.Code, statusRecorder.Body.String())
	}

	startRecorder := httptest.NewRecorder()
	handler.ServeHTTP(startRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/auth/feishu/start", nil))
	if startRecorder.Code != http.StatusFound || !strings.Contains(startRecorder.Header().Get("Location"), "state=") {
		t.Fatalf("feishu start returned %d %s", startRecorder.Code, startRecorder.Header().Get("Location"))
	}
	stateCookie := startRecorder.Result().Cookies()
	if len(stateCookie) == 0 || stateCookie[0].Name != feishuStateCookieName {
		t.Fatalf("feishu start did not set state cookie")
	}

	mismatch := httptest.NewRequest(http.MethodGet, "/api/v1/auth/feishu/callback?code=code-1&state=wrong", nil)
	mismatch.AddCookie(&http.Cookie{Name: feishuStateCookieName, Value: stateCookie[0].Value})
	mismatchRecorder := httptest.NewRecorder()
	handler.ServeHTTP(mismatchRecorder, mismatch)
	if mismatchRecorder.Code != http.StatusFound || !strings.Contains(mismatchRecorder.Header().Get("Location"), "feishu_error=state") {
		t.Fatalf("state mismatch returned %d %s", mismatchRecorder.Code, mismatchRecorder.Header().Get("Location"))
	}

	callback := httptest.NewRequest(http.MethodGet, "/api/v1/auth/feishu/callback?code=code-1&state="+stateCookie[0].Value, nil)
	callback.AddCookie(&http.Cookie{Name: feishuStateCookieName, Value: stateCookie[0].Value})
	callbackRecorder := httptest.NewRecorder()
	handler.ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusFound || callbackRecorder.Header().Get("Location") != "/" {
		t.Fatalf("feishu callback returned %d %s", callbackRecorder.Code, callbackRecorder.Header().Get("Location"))
	}
	foundSessionCookie := false
	for _, cookie := range callbackRecorder.Result().Cookies() {
		if cookie.Name == refreshCookieName && cookie.Value != "" {
			foundSessionCookie = true
		}
	}
	if !foundSessionCookie {
		t.Fatalf("feishu callback did not set session cookies")
	}
}

func TestLoginRateLimited(t *testing.T) {
	application := &fakeAuthApplication{}
	limiter := &fakeRateLimiter{result: ratelimit.Result{Allowed: false, RetryAfter: time.Minute}}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, application, limiter)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"bad"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "60" || application.loginCalls != 0 {
		t.Fatalf("rate limit returned %d retry=%q calls=%d", recorder.Code, recorder.Header().Get("Retry-After"), application.loginCalls)
	}
}

func TestLoginSuccessResetsRateLimit(t *testing.T) {
	application := &fakeAuthApplication{result: service.AuthResult{}}
	limiter := &fakeRateLimiter{result: ratelimit.Result{Allowed: true}}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, application, limiter)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"Alice","password":"correct horse battery"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(limiter.resetKey, "login:alice|") {
		t.Fatalf("login reset returned %d key=%q", recorder.Code, limiter.resetKey)
	}
}

func TestLoginEndpoint(t *testing.T) {
	application := &fakeAuthApplication{result: service.AuthResult{
		User:   service.PublicUser{ID: uuid.New(), Username: "alice", Role: biz.RoleUser, Status: biz.UserStatusActive},
		Tokens: service.TokenPair{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer"},
	}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"correct horse battery"}`))
	authTestHandler(application).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "access") {
		t.Fatalf("login returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthEndpointErrors(t *testing.T) {
	application := &fakeAuthApplication{err: service.ErrInvalidCredentials}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"bad"}`))
	authTestHandler(application).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login returned %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"bad","user_id":"attack"}`))
	authTestHandler(application).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown user_id field returned %d", recorder.Code)
	}
}

func TestLogoutAllRequiresPrincipal(t *testing.T) {
	principal, _ := biz.NewPrincipal(uuid.New(), biz.RoleUser)
	application := &fakeAuthApplication{principal: principal}
	handler := authTestHandler(application)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout-all", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("logout-all without token returned %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout-all", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || application.logoutAllUser != principal.UserID {
		t.Fatalf("logout-all returned %d user=%s", recorder.Code, application.logoutAllUser)
	}
}

func TestRegisterConflict(t *testing.T) {
	application := &fakeAuthApplication{err: biz.ErrUserAlreadyExists}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewBufferString(`{"username":"alice","password":"correct horse battery"}`))
	authTestHandler(application).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("duplicate registration returned %d", recorder.Code)
	}
}

// cookieAuthFixture 构造带刷新令牌的鉴权结果。
func cookieAuthFixture() service.AuthResult {
	now := time.Now().UTC()
	return service.AuthResult{
		User: service.PublicUser{ID: uuid.New(), Username: "alice", Role: biz.RoleUser, Status: biz.UserStatusActive},
		Tokens: service.TokenPair{
			AccessToken: "access-token", RefreshToken: "refresh-token", TokenType: "Bearer",
			AccessExpiresAt: now.Add(15 * time.Minute), RefreshExpiresAt: now.Add(30 * 24 * time.Hour),
		},
	}
}

// responseCookie 从响应中查找指定名称的 Cookie。
func responseCookie(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q not found in %v", name, recorder.Header().Values("Set-Cookie"))
	return nil
}

func TestLoginUsesSecureCookieTransport(t *testing.T) {
	handler := newHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, &fakeAuthApplication{result: cookieAuthFixture()}, nil, authCookiePolicy{Secure: true}, APILimiters{}, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"correct horse battery"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login returned %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "refresh-token") {
		t.Fatal("cookie transport leaked refresh token in response body")
	}
	refresh := responseCookie(t, recorder, refreshCookieName)
	if !refresh.HttpOnly || !refresh.Secure || refresh.SameSite != http.SameSiteStrictMode || refresh.Path != refreshCookiePath {
		t.Fatalf("refresh cookie attributes = %#v", refresh)
	}
	csrf := responseCookie(t, recorder, csrfCookieName)
	if csrf.HttpOnly || !csrf.Secure || csrf.Path != csrfCookiePath {
		t.Fatalf("csrf cookie attributes = %#v", csrf)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth response must not be cached, got %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestLoginBearerTransportReturnsRefreshToken(t *testing.T) {
	handler := authTestHandler(&fakeAuthApplication{result: cookieAuthFixture()})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"correct horse battery","transport":"bearer"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "refresh-token") {
		t.Fatalf("bearer login returned %d %s", recorder.Code, recorder.Body.String())
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("bearer transport must not set cookies: %v", recorder.Header().Values("Set-Cookie"))
	}
}

func TestRefreshRequiresCSRFAndRotatesCookie(t *testing.T) {
	application := &fakeAuthApplication{result: cookieAuthFixture()}
	handler := authTestHandler(application)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	request.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old-refresh"})
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || application.refreshToken != "" {
		t.Fatalf("refresh without csrf returned %d token=%q", recorder.Code, application.refreshToken)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	request.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old-refresh"})
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	request.Header.Set(csrfHeaderName, "csrf-value")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || application.refreshToken != "old-refresh" {
		t.Fatalf("refresh returned %d token=%q", recorder.Code, application.refreshToken)
	}
	if strings.Contains(recorder.Body.String(), "refresh-token") {
		t.Fatal("refresh response leaked refresh token")
	}
	if rotated := responseCookie(t, recorder, refreshCookieName); rotated.Value != "refresh-token" {
		t.Fatalf("refresh cookie was not rotated: %#v", rotated)
	}
	// 并发刷新必须复用同一 CSRF 令牌，避免多标签页互相失效。
	if csrfCookie := responseCookie(t, recorder, csrfCookieName); csrfCookie.Value != "csrf-value" {
		t.Fatalf("refresh rotated csrf token unexpectedly: %#v", csrfCookie)
	}
}

func TestRefreshReuseClearsCookies(t *testing.T) {
	application := &fakeAuthApplication{err: service.ErrRefreshTokenReused}
	handler := authTestHandler(application)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	request.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "stolen"})
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	request.Header.Set(csrfHeaderName, "csrf-value")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("reuse refresh returned %d", recorder.Code)
	}
	if cleared := responseCookie(t, recorder, refreshCookieName); cleared.MaxAge >= 0 {
		t.Fatalf("reuse did not clear refresh cookie: %#v", cleared)
	}
}

func TestLogoutClearsSessionCookies(t *testing.T) {
	application := &fakeAuthApplication{}
	handler := authTestHandler(application)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "refresh-to-revoke"})
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	request.Header.Set(csrfHeaderName, "csrf-value")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || application.logoutToken != "refresh-to-revoke" {
		t.Fatalf("logout returned %d token=%q", recorder.Code, application.logoutToken)
	}
	for _, name := range []string{refreshCookieName, csrfCookieName} {
		if cleared := responseCookie(t, recorder, name); cleared.MaxAge >= 0 {
			t.Fatalf("logout did not clear %s: %#v", name, cleared)
		}
	}
}

func TestCSRFEndpointIssuesToken(t *testing.T) {
	handler := authTestHandler(&fakeAuthApplication{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/auth/csrf", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "csrf_token") {
		t.Fatalf("csrf endpoint returned %d %s", recorder.Code, recorder.Body.String())
	}
	if cookie := responseCookie(t, recorder, csrfCookieName); cookie.HttpOnly || cookie.Value == "" {
		t.Fatalf("csrf cookie = %#v", cookie)
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	handler := authTestHandler(&fakeAuthApplication{result: cookieAuthFixture()})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"alice","password":"correct horse battery"}`))
	request.Header.Set("Origin", "https://evil.example")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login returned %d", recorder.Code)
	}
}

func TestSecurityHeadersApplied(t *testing.T) {
	handler := authTestHandler(&fakeAuthApplication{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	header := recorder.Header()
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if header.Get(name) != want {
			t.Fatalf("%s = %q, want %q", name, header.Get(name), want)
		}
	}
	if !strings.Contains(header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("content security policy = %q", header.Get("Content-Security-Policy"))
	}
}

// TestPasswordAndUserStatusRoutes 验证改密、重置令牌与管理员用户状态路由及错误映射。
func TestPasswordAndUserStatusRoutes(t *testing.T) {
	userID := uuid.New()
	application := &fakeAuthApplication{principal: biz.Principal{UserID: userID, Role: biz.RoleUser}}
	handler := authTestHandler(application)

	request := func(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer access")
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if recorder := request(http.MethodPost, "/api/v1/auth/password", `{"current_password":"old","new_password":"new password 123"}`, true); recorder.Code != http.StatusNoContent {
		t.Fatalf("change password returned %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := request(http.MethodPost, "/api/v1/auth/password", `{"current_password":"old","new_password":"new password 123"}`, false); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous change password returned %d", recorder.Code)
	}
	if recorder := request(http.MethodPost, "/api/v1/auth/password/reset", `{"token":"token","new_password":"new password 123"}`, false); recorder.Code != http.StatusNoContent {
		t.Fatalf("reset password returned %d %s", recorder.Code, recorder.Body.String())
	}
	application.err = biz.ErrPasswordResetInvalid
	if recorder := request(http.MethodPost, "/api/v1/auth/password/reset", `{"token":"bad","new_password":"new password 123"}`, false); recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid reset token returned %d %s", recorder.Code, recorder.Body.String())
	}
	application.err = nil
	application.operationErr = biz.ErrForbidden
	if recorder := request(http.MethodPost, "/api/v1/admin/users/"+uuid.NewString()+"/status", `{"status":"disabled","reason":"manual"}`, true); recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin status returned %d %s", recorder.Code, recorder.Body.String())
	}

	application.err = nil
	application.principal = biz.Principal{UserID: userID, Role: biz.RoleAdmin}
	application.resetToken, application.resetExpires = "one-time-token", time.Now().UTC().Add(15*time.Minute)
	recorder := request(http.MethodPost, "/api/v1/admin/users/"+uuid.NewString()+"/password-reset", "", true)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), "one-time-token") {
		t.Fatalf("issue reset returned %d %s", recorder.Code, recorder.Body.String())
	}
}
