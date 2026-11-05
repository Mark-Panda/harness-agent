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
	for _, path := range []string{"/health/live", "/health/ready", "/api/v1/health"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		// 入站请求 ID 必须原样回写，便于日志与 Trace 关联。
		request.Header.Set("X-Request-ID", "req-test-1")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "status") {
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
	for _, path := range []string{"/api/v1/missing", "/assets/missing.js"} {
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
