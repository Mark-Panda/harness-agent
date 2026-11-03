package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestHTTPMiddlewareUsesRouteTemplate 验证请求指标使用路由模板而不是含 ID 的原始路径。
func TestHTTPMiddlewareUsesRouteTemplate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := Middleware(mux)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents/8f14e45f-ea3c-4c2e-9d1a-2b3c4d5e6f70", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d", recorder.Code)
	}
	value := testutil.ToFloat64(apiRequestsTotal.WithLabelValues("GET", "GET /api/v1/agents/{id}", "201"))
	if value < 1 {
		t.Fatalf("route metric = %v", value)
	}
	// 原始路径不得成为标签值，否则会按用户/会话 ID 形成高基数。
	if testutil.ToFloat64(apiRequestsTotal.WithLabelValues("GET", "/api/v1/agents/8f14e45f-ea3c-4c2e-9d1a-2b3c4d5e6f70", "201")) != 0 {
		t.Fatal("raw path leaked into metric labels")
	}
}

// TestMiddlewareSkipsMetricsPathAndNormalizesMethod 验证抓取路径与非常见方法不会污染指标。
func TestMiddlewareSkipsMetricsPathAndNormalizesMethod(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	before := testutil.CollectAndCount(apiRequestsTotal)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if after := testutil.CollectAndCount(apiRequestsTotal); after != before {
		t.Fatalf("metrics path changed metric count from %d to %d", before, after)
	}
	request := httptest.NewRequest("CUSTOM-METHOD", "/api/v1/probe", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if testutil.ToFloat64(apiRequestsTotal.WithLabelValues("OTHER", "unmatched", "200")) < 1 {
		t.Fatal("custom method was not normalized")
	}
}

// TestAgentMetricsHelpers 验证 Run、Token、工具、任务、Outbox、SSE 与记忆指标可观测。
func TestAgentMetricsHelpers(t *testing.T) {
	IncRunCreated("react")
	ObserveRunFinished("react", "completed", 2*time.Second)
	ObserveLLM("react", 100, 20, 0.0004)
	ObserveTool("search", "completed", 100*time.Millisecond)
	finish := StartJob("agent.run")
	finish("success")
	if testutil.ToFloat64(jobsInflight.WithLabelValues("agent.run")) != 0 {
		t.Fatal("inflight jobs must return to zero")
	}
	IncOutbox("audit", "success")
	AddSSEConnection(1)
	IncSSEEvent("run.completed")
	AddSSEConnection(-1)
	ObserveMemory("retrieve", "success")

	if testutil.ToFloat64(runsTotal.WithLabelValues("react", "queued")) < 1 ||
		testutil.ToFloat64(runsTotal.WithLabelValues("react", "completed")) < 1 {
		t.Fatal("run counters not increased")
	}
	if testutil.ToFloat64(llmTokensTotal.WithLabelValues("react", "input")) < 100 ||
		testutil.ToFloat64(llmCostTotal.WithLabelValues("react")) <= 0 {
		t.Fatal("token or cost counters not increased")
	}
	if testutil.ToFloat64(toolCallsTotal.WithLabelValues("search", "completed")) < 1 {
		t.Fatal("tool counter not increased")
	}
	if testutil.ToFloat64(jobsTotal.WithLabelValues("agent.run", "success")) < 1 {
		t.Fatal("job counter not increased")
	}
	if testutil.ToFloat64(outboxEventsTotal.WithLabelValues("audit", "success")) < 1 {
		t.Fatal("outbox counter not increased")
	}
	if testutil.ToFloat64(sseConnections) != 0 {
		t.Fatal("sse connections must return to zero")
	}
	if testutil.ToFloat64(sseEventsTotal.WithLabelValues("run.completed")) < 1 {
		t.Fatal("sse event counter not increased")
	}
	if testutil.ToFloat64(memoryOperationsTotal.WithLabelValues("retrieve", "success")) < 1 {
		t.Fatal("memory counter not increased")
	}
}

// TestHandlerExposesUSEMetricsAndRejectsHighCardinalityLabels 验证 /metrics 暴露资源指标且不含用户维度标签。
func TestHandlerExposesUSEMetricsAndRejectsHighCardinalityLabels(t *testing.T) {
	ObserveMemory("summary", "success")
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total", "harness_agent_runs_total", "harness_agent_jobs_total"} {
		if !strings.Contains(body, name) {
			t.Fatalf("metrics output missing %s", name)
		}
	}
	// 高基数或敏感标识不得作为标签出现。
	for _, label := range []string{"user_id=", "session_id=", "run_id="} {
		if strings.Contains(body, label) {
			t.Fatalf("metrics output contains forbidden label %s", label)
		}
	}
}
