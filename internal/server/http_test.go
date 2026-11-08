package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
)

func testHandler() http.Handler {
	return NewHandler(fstest.MapFS{
		"index.html":        {Data: []byte("<html><body>Harness Agent</body></html>")},
		"assets/app-123.js": {Data: []byte("console.log('ok')")},
	}, nil, nil)
}

func TestHealthAndSSEProbe(t *testing.T) {
	handler := testHandler()
	for _, path := range []string{"/health/live", "/health/ready", "/healthz", "/readyz", "/api/v1/health"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		// 入站请求 ID 必须原样回写，便于日志与 Trace 关联。
		request.Header.Set("X-Request-ID", "req-test-1")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" ||
			!strings.Contains(recorder.Body.String(), "status") {
			t.Fatalf("%s returned %d %s", path, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("X-Request-ID") != "req-test-1" {
			t.Fatalf("%s request id = %q", path, recorder.Header().Get("X-Request-ID"))
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sse/probe", nil))
	if recorder.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(recorder.Body.String(), "event: heartbeat") {
		t.Fatalf("unexpected SSE response: %v %s", recorder.Header(), recorder.Body.String())
	}
	// readiness 摘流期间必须返回 503，避免负载均衡继续转发新请求。
	SetReady(false)
	t.Cleanup(func() { SetReady(true) })
	draining := httptest.NewRecorder()
	handler.ServeHTTP(draining, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if draining.Code != http.StatusServiceUnavailable || !strings.Contains(draining.Body.String(), "draining") {
		t.Fatalf("draining readiness = %d %s", draining.Code, draining.Body.String())
	}
	SetReady(true)
}

func TestSPAFallbackAndAssetCaching(t *testing.T) {
	handler := testHandler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/example", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Harness Agent") {
		t.Fatalf("SPA fallback returned %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unexpected index cache policy %q", recorder.Header().Get("Cache-Control"))
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/app-123.js", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("unexpected asset response %d %q", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
}

func TestMissingAPIAndAssetReturnNotFound(t *testing.T) {
	handler := testHandler()
	for _, path := range []string{"/api/v1/missing", "/assets/missing.js", "/health/missing"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}
}

var _ fs.FS = fstest.MapFS{}

// TestNewHTTPServerRejectsInvalidTLSFiles 验证启用 TLS 但证书不可读时启动失败，禁止静默降级为 HTTP。
func TestNewHTTPServerRejectsInvalidTLSFiles(t *testing.T) {
	cfg := config.ServerConfig{
		HTTP: config.HTTPServerConfig{
			Address: ":0", Timeout: config.Duration{Duration: time.Second},
			TLS: config.HTTPTLSConfig{Enabled: true, CertFile: "missing.crt", KeyFile: "missing.key", MinVersion: "1.3"},
		},
	}
	if _, err := NewHTTPServer(cfg, fstest.MapFS{}, nil, nil); err == nil {
		t.Fatal("invalid TLS certificate must fail server construction")
	}
}

// TestMetricsEndpointRequiresBearerToken 验证配置令牌后 /metrics 强制 Bearer 鉴权。
func TestMetricsEndpointRequiresBearerToken(t *testing.T) {
	handler := newHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, nil, nil, authCookiePolicy{}, APILimiters{}, nil, "metrics-secret")
	for _, testCase := range []struct {
		name          string
		authorization string
		want          int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong scheme", authorization: "Basic metrics-secret", want: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer other", want: http.StatusUnauthorized},
		{name: "valid", authorization: "Bearer metrics-secret", want: http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if testCase.authorization != "" {
			request.Header.Set("Authorization", testCase.authorization)
		}
		handler.ServeHTTP(recorder, request)
		if recorder.Code != testCase.want {
			t.Fatalf("%s: status = %d, want %d", testCase.name, recorder.Code, testCase.want)
		}
	}
}

// TestMetricsEndpointAnonymousWithoutToken 验证未配置令牌时保持匿名只读。
func TestMetricsEndpointAnonymousWithoutToken(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("status = %d, content type = %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}
