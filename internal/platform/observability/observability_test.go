package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetricsRenderIncludesCounterAndHistogram 验证 Prometheus 文本输出包含计数器、直方图与 le 分桶。
func TestMetricsRenderIncludesCounterAndHistogram(t *testing.T) {
	metrics := NewMetrics()
	metrics.IncCounter("agent_llm_tokens_total", map[string]string{"agent_mode": "react", "type": "input"}, 12)
	metrics.ObserveDuration("agent_tool_call_duration_seconds",
		map[string]string{"agent_mode": "react", "tool": `say"hi`, "status": "succeeded"}, 0.02)
	output := metrics.Render()
	for _, expected := range []string{
		`# TYPE agent_llm_tokens_total counter`,
		`agent_llm_tokens_total{agent_mode="react",type="input"} 12`,
		`# TYPE agent_tool_call_duration_seconds histogram`,
		`agent_tool_call_duration_seconds_bucket{agent_mode="react",status="succeeded",tool="say\"hi",le="0.025"} 1`,
		`agent_tool_call_duration_seconds_count{agent_mode="react",status="succeeded",tool="say\"hi"} 1`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("render missing %q:\n%s", expected, output)
		}
	}
}

// TestTracingMiddlewareReusesInboundTraceparent 验证入站 W3C traceparent 会被复用并回写响应头。
func TestTracingMiddlewareReusesInboundTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	handler := TracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TraceIDFrom(r.Context()) != traceID {
			t.Fatalf("context trace id = %q", TraceIDFrom(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-Trace-Id"); got != traceID {
		t.Fatalf("response trace id = %q", got)
	}
}

// TestTracingMiddlewareGeneratesTraceID 验证缺少 traceparent 时仍生成可关联的 TraceID。
func TestTracingMiddlewareGeneratesTraceID(t *testing.T) {
	handler := TracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(TraceIDFrom(r.Context())) != 32 {
			t.Fatalf("generated trace id = %q", TraceIDFrom(r.Context()))
		}
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if len(recorder.Header().Get("X-Trace-Id")) != 32 {
		t.Fatalf("response trace id = %q", recorder.Header().Get("X-Trace-Id"))
	}
}

// TestMetricsHandlerContentType 验证 /metrics 使用 Prometheus 文本内容类型。
func TestMetricsHandlerContentType(t *testing.T) {
	recorder := httptest.NewRecorder()
	Default().MetricsHandler(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("content type = %q", contentType)
	}
}

// TestSpanNameNormalizesResourceIDs 验证资源标识被归一为占位符，避免 Span 名称高基数。
func TestSpanNameNormalizesResourceIDs(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{"/api/v1/agents/6f1a1b8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f/runs", "GET /api/v1/agents/{id}/runs"},
		{"/api/v1/runs/12345/events", "GET /api/v1/runs/{id}/events"},
		{"/api/v1/agents/deadbeefdeadbeef", "GET /api/v1/agents/{id}"},
		{"/health/live", "GET /health/live"},
		{"/api/v1/agents/v1", "GET /api/v1/agents/v1"},
	}
	for _, testCase := range cases {
		if got := spanName(httptest.NewRequest(http.MethodGet, testCase.target, nil)); got != testCase.want {
			t.Fatalf("span name for %q = %q, want %q", testCase.target, got, testCase.want)
		}
	}
}

// TestSpanNameTruncatesLongPath 验证超长路径被截断到上限，避免异常 Span 名称。
func TestSpanNameTruncatesLongPath(t *testing.T) {
	longPath := "/api/v1/sessions/" + strings.Repeat("segment/", 40)
	if got := spanName(httptest.NewRequest(http.MethodGet, longPath, nil)); len([]rune(got)) > maxSpanNameLength {
		t.Fatalf("span name length = %d, want <= %d", len([]rune(got)), maxSpanNameLength)
	}
}
