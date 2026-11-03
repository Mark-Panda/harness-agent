package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReadinessCriticalDependencyFailure 验证关键依赖故障使实例退出流量。
func TestReadinessCriticalDependencyFailure(t *testing.T) {
	SetDependencyProbes(
		DependencyProbe{Name: "postgres", Critical: true, Check: func(context.Context) error {
			return errors.New("connection refused")
		}},
		DependencyProbe{Name: "redis", Critical: false, Check: func(context.Context) error { return nil }},
	)
	t.Cleanup(func() { SetDependencyProbes() })
	SetReady(true)

	recorder := httptest.NewRecorder()
	readinessHandler(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("critical failure status = %d, want 503", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"unavailable"`) || !strings.Contains(body, `"postgres":"unavailable"`) {
		t.Fatalf("unexpected readiness body: %s", body)
	}
	// 错误细节不得回显到探针响应，避免泄露连接串或内部地址。
	if strings.Contains(body, "connection refused") {
		t.Fatalf("readiness leaked dependency error: %s", body)
	}
}

// TestReadinessRedisFailureDegradesWithoutDraining 验证 Redis 故障只降级不摘流。
func TestReadinessRedisFailureDegradesWithoutDraining(t *testing.T) {
	SetDependencyProbes(
		DependencyProbe{Name: "postgres", Critical: true, Check: func(context.Context) error { return nil }},
		DependencyProbe{Name: "redis", Critical: false, Check: func(context.Context) error {
			return errors.New("redis down")
		}},
	)
	t.Cleanup(func() { SetDependencyProbes() })
	SetReady(true)

	recorder := httptest.NewRecorder()
	readinessHandler(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("redis failure status = %d, want 200 degraded", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"redis":"unavailable"`) ||
		!strings.Contains(body, `"postgres":"ok"`) {
		t.Fatalf("unexpected degraded readiness body: %s", body)
	}
}

// TestReadinessRecoversAfterDependencyRestore 验证依赖恢复后探针回到 ready。
func TestReadinessRecoversAfterDependencyRestore(t *testing.T) {
	failing := true
	SetDependencyProbes(DependencyProbe{Name: "postgres", Critical: true, Check: func(context.Context) error {
		if failing {
			return errors.New("down")
		}
		return nil
	}})
	t.Cleanup(func() { SetDependencyProbes() })
	SetReady(true)

	failed := httptest.NewRecorder()
	readinessHandler(failed, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed status = %d", failed.Code)
	}
	failing = false
	recovered := httptest.NewRecorder()
	readinessHandler(recovered, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), `"status":"ready"`) {
		t.Fatalf("recovered status = %d %s", recovered.Code, recovered.Body.String())
	}
}

// TestReadinessWithoutProbesStaysReady 验证未注册探针时不额外阻塞就绪。
func TestReadinessWithoutProbesStaysReady(t *testing.T) {
	SetDependencyProbes()
	t.Cleanup(func() { SetDependencyProbes() })
	SetReady(true)
	recorder := httptest.NewRecorder()
	readinessHandler(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ready"`) {
		t.Fatalf("probe-less readiness = %d %s", recorder.Code, recorder.Body.String())
	}
}
