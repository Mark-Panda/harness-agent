package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	return NewHandler(fstest.MapFS{
		"index.html":        {Data: []byte("<html><body>Harness Agent</body></html>")},
		"assets/app-123.js": {Data: []byte("console.log('ok')")},
	})
}

func TestHealthAndSSEProbe(t *testing.T) {
	handler := testHandler()
	for _, path := range []string{"/health/live", "/health/ready", "/api/v1/health"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "status") {
			t.Fatalf("%s returned %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sse/probe", nil))
	if recorder.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(recorder.Body.String(), "event: heartbeat") {
		t.Fatalf("unexpected SSE response: %v %s", recorder.Header(), recorder.Body.String())
	}
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
