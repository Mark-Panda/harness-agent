package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// TestWritePlatformErrorMapsDependencyTimeoutToServiceUnavailable 验证依赖超时返回可重试提示。
func TestWritePlatformErrorMapsDependencyTimeoutToServiceUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePlatformError(recorder, fmt.Errorf("query sessions: %w", context.DeadlineExceeded))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("retry-after = %q", recorder.Header().Get("Retry-After"))
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "dependency_timeout") {
		t.Fatalf("unexpected error code: %s", body)
	}
}

// TestWritePlatformErrorMapsQueueSaturatedToTooManyRequests 验证排队已满返回可重试的 429。
func TestWritePlatformErrorMapsQueueSaturatedToTooManyRequests(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePlatformError(recorder, fmt.Errorf("enqueue run: %w", biz.ErrQueueSaturated))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("retry-after = %q", recorder.Header().Get("Retry-After"))
	}
	if !strings.Contains(recorder.Body.String(), "run_queue_saturated") {
		t.Fatalf("unexpected error code: %s", recorder.Body.String())
	}
}

// TestWritePlatformErrorKeepsUnknownErrorsInternal 验证未知错误不泄露内部细节。
func TestWritePlatformErrorKeepsUnknownErrorsInternal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePlatformError(recorder, errors.New("connection string leaked"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "connection string") {
		t.Fatalf("internal error leaked detail: %s", recorder.Body.String())
	}
}
