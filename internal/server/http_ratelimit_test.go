package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/ratelimit"
	"github.com/google/uuid"
)

// recordingLimiter 记录每次限流调用的键，并按预设结果返回，便于验证各维度键空间。
type recordingLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error
	keys       []string
}

// Allow 记录键并返回预设结果。
func (l *recordingLimiter) Allow(_ context.Context, key string) (ratelimit.Result, error) {
	l.keys = append(l.keys, key)
	if l.err != nil {
		return ratelimit.Result{}, l.err
	}
	return ratelimit.Result{Allowed: l.allowed, RetryAfter: l.retryAfter}, nil
}

// Reset 记录限流器不参与登录重置，返回 nil。
func (l *recordingLimiter) Reset(context.Context, string) error { return nil }

// apiLimitTestHandler 构造带多维限流的最小 Handler。
func apiLimitTestHandler(application AuthApplication, limits APILimiters) http.Handler {
	return NewHandlerWithLimits(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, application, nil, limits)
}

// TestAPIRateLimitGlobalDimension 验证全局维度拦截 /api/v1 请求并返回 Retry-After，
// 且静态资源请求不消耗全局配额。
func TestAPIRateLimitGlobalDimension(t *testing.T) {
	global := &recordingLimiter{allowed: false, retryAfter: 90 * time.Second}
	handler := apiLimitTestHandler(&fakeAuthApplication{}, APILimiters{Global: global})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "90" {
		t.Fatalf("global limit returned %d retry=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if len(global.keys) != 1 || global.keys[0] != "api_global" {
		t.Fatalf("global limiter keys = %v", global.keys)
	}
	// 静态资源不属于 API 前缀，不应进入限流器。
	staticRecorder := httptest.NewRecorder()
	handler.ServeHTTP(staticRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if staticRecorder.Code != http.StatusOK || len(global.keys) != 1 {
		t.Fatalf("static request returned %d global keys=%v", staticRecorder.Code, global.keys)
	}
}

// TestAPIRateLimitIPDimension 验证来源 IP 维度使用 api_ip 前缀与 RemoteAddr 主机部分。
func TestAPIRateLimitIPDimension(t *testing.T) {
	ipLimiter := &recordingLimiter{allowed: true}
	handler := apiLimitTestHandler(&fakeAuthApplication{}, APILimiters{IP: ipLimiter})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.RemoteAddr = "192.0.2.10:54321"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ip allowed request returned %d", recorder.Code)
	}
	if len(ipLimiter.keys) != 1 || ipLimiter.keys[0] != "api_ip:192.0.2.10" {
		t.Fatalf("ip limiter keys = %v", ipLimiter.keys)
	}
	// 不同来源 IP 使用独立键，避免共享后端限流误伤其他客户端。
	blocked := &recordingLimiter{allowed: false, retryAfter: time.Minute}
	blockedHandler := apiLimitTestHandler(&fakeAuthApplication{}, APILimiters{IP: blocked})
	second := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	second.RemoteAddr = "198.51.100.7:1234"
	blockedRecorder := httptest.NewRecorder()
	blockedHandler.ServeHTTP(blockedRecorder, second)
	if blockedRecorder.Code != http.StatusTooManyRequests || len(blocked.keys) != 1 || blocked.keys[0] != "api_ip:198.51.100.7" {
		t.Fatalf("ip blocked returned %d keys=%v", blockedRecorder.Code, blocked.keys)
	}
}

// TestAPIRateLimitUserDimension 验证登录用户维度在解析 Principal 后执行，
// 未认证请求不会消耗用户配额。
func TestAPIRateLimitUserDimension(t *testing.T) {
	userID := uuid.New()
	userLimiter := &recordingLimiter{allowed: false, retryAfter: 30 * time.Second}
	application := &fakeAuthApplication{principal: biz.Principal{UserID: userID, Role: biz.RoleUser}}
	handler := apiLimitTestHandler(application, APILimiters{User: userLimiter})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout-all", nil)
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "30" {
		t.Fatalf("user limit returned %d retry=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if len(userLimiter.keys) != 1 || userLimiter.keys[0] != "api_user:"+userID.String() {
		t.Fatalf("user limiter keys = %v", userLimiter.keys)
	}
	// 认证失败发生在限流之前，不应消耗用户维度配额。
	unauthenticated := &fakeAuthApplication{err: errors.New("invalid token")}
	unauthorizedHandler := apiLimitTestHandler(unauthenticated, APILimiters{User: userLimiter})
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout-all", nil)
	unauthorizedRequest.Header.Set("Authorization", "Bearer access")
	unauthorizedRecorder := httptest.NewRecorder()
	unauthorizedHandler.ServeHTTP(unauthorizedRecorder, unauthorizedRequest)
	if unauthorizedRecorder.Code != http.StatusUnauthorized || len(userLimiter.keys) != 1 {
		t.Fatalf("unauthenticated returned %d user keys=%v", unauthorizedRecorder.Code, userLimiter.keys)
	}
}

// TestAPIRateLimitUnavailable 验证限流依赖故障时失败关闭，返回 503。
func TestAPIRateLimitUnavailable(t *testing.T) {
	failing := &recordingLimiter{err: errors.New("redis unavailable")}
	handler := apiLimitTestHandler(&fakeAuthApplication{}, APILimiters{Global: failing})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable limiter returned %d", recorder.Code)
	}
}

// TestAPIRateLimitDisengaged 验证三个维度均为空时请求不受影响。
func TestAPIRateLimitDisengaged(t *testing.T) {
	handler := apiLimitTestHandler(&fakeAuthApplication{}, APILimiters{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disabled limits returned %d", recorder.Code)
	}
}

// TestClientIPResolverInvalidCIDR 验证非法可信代理网段在启动前被拒绝。
func TestClientIPResolverInvalidCIDR(t *testing.T) {
	if _, err := newClientIPResolver([]string{"10.0.0.0/8", "not-a-cidr"}); err == nil {
		t.Fatal("invalid trusted proxy CIDR accepted")
	}
	if resolver, err := newClientIPResolver(nil); err != nil || resolver != nil {
		t.Fatalf("empty trusted proxies = %v err=%v", resolver, err)
	}
}

// TestClientIPResolverUntrustedSourceIgnoresForwardedFor 验证非可信来源的转发头被忽略。
func TestClientIPResolverUntrustedSourceIgnoresForwardedFor(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.5:4321"
	request.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := resolver.resolve(request); got != "203.0.113.5" {
		t.Fatalf("untrusted source resolved %q", got)
	}
}

// TestClientIPResolverTrustedProxyChain 验证从右向左跳过可信代理并取真实客户端 IP。
func TestClientIPResolverTrustedProxyChain(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.4")
	if got := resolver.resolve(request); got != "203.0.113.9" {
		t.Fatalf("trusted chain resolved %q", got)
	}
	// 全部为可信代理时取最左侧地址，保持转发链语义。
	allTrusted := httptest.NewRequest(http.MethodGet, "/", nil)
	allTrusted.RemoteAddr = "10.0.0.5:4321"
	allTrusted.Header.Set("X-Forwarded-For", "10.0.0.2, 10.0.0.3")
	if got := resolver.resolve(allTrusted); got != "10.0.0.2" {
		t.Fatalf("all-trusted chain resolved %q", got)
	}
	// 链中出现非法地址时回退直连地址，不接受不可信输入。
	malformed := httptest.NewRequest(http.MethodGet, "/", nil)
	malformed.RemoteAddr = "10.0.0.5:4321"
	malformed.Header.Set("X-Forwarded-For", "203.0.113.9, garbage")
	if got := resolver.resolve(malformed); got != "10.0.0.5" {
		t.Fatalf("malformed chain resolved %q", got)
	}
	// 无转发头时直接使用直连地址。
	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "10.0.0.5:4321"
	if got := resolver.resolve(direct); got != "10.0.0.5" {
		t.Fatalf("direct resolved %q", got)
	}
}

// TestAPIRateLimitUsesTrustedProxyIP 验证 IP 维度限流键使用可信代理链中的真实客户端地址。
func TestAPIRateLimitUsesTrustedProxyIP(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	ipLimiter := &recordingLimiter{allowed: true}
	handler := newHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, &fakeAuthApplication{}, nil, authCookiePolicy{}, APILimiters{IP: ipLimiter}, resolver)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if len(ipLimiter.keys) != 1 || ipLimiter.keys[0] != "api_ip:203.0.113.9" {
		t.Fatalf("proxy ip limiter keys = %v", ipLimiter.keys)
	}
}
