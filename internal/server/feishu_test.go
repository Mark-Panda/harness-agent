package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
)

// serverFeishuEventStore 是回调测试用的事件仓储占位实现。
type serverFeishuEventStore struct {
	biz.FeishuEventStore
}

func (serverFeishuEventStore) RecordEvent(_ context.Context, _ biz.FeishuEventRecord) (bool, error) {
	return true, nil
}

// serverFeishuEnqueuer 是回调测试用的事件入队占位实现。
type serverFeishuEnqueuer struct {
	biz.FeishuEventEnqueuer
}

func (serverFeishuEnqueuer) EnqueueFeishuEvent(_ context.Context, _ string, _ time.Time) error {
	return nil
}

// newFeishuTestHandler 构造只装配飞书回调的平台路由，验证其不依赖用户 JWT。
func newFeishuTestHandler(t *testing.T) http.Handler {
	t.Helper()
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{VerificationToken: "verify-token"})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	ingest, err := biz.NewFeishuIngestService(verifier, serverFeishuEventStore{}, serverFeishuEnqueuer{}, biz.FeishuIngestConfig{MaxBodyBytes: 1024})
	if err != nil {
		t.Fatalf("new ingest: %v", err)
	}
	platform := &service.PlatformService{Idempotency: &biz.IdempotencyService{}}
	platform.WithFeishuIngest(ingest)
	return NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, nil, nil, platform)
}

// TestFeishuEventCallbackWithoutAuth 覆盖 URL 验证、伪造 Token 与快速 ACK。
func TestFeishuEventCallbackWithoutAuth(t *testing.T) {
	handler := newFeishuTestHandler(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feishu/events", bytes.NewBufferString(`{"type":"url_verification","token":"verify-token","challenge":"abc"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"challenge":"abc"`) {
		t.Fatalf("challenge returned %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/feishu/events", bytes.NewBufferString(`{"type":"url_verification","token":"forged","challenge":"abc"}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("forged token returned %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/feishu/events", bytes.NewBufferString(`{"schema":"2.0","header":{"event_id":"evt-1","event_type":"im.message.receive_v1","tenant_key":"tenant","token":"verify-token"},"event":{}}`))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"code":0`) {
		t.Fatalf("event ack returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestFeishuEventCallbackRejectsOversizedBody 覆盖请求体上限。
func TestFeishuEventCallbackRejectsOversizedBody(t *testing.T) {
	handler := newFeishuTestHandler(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/feishu/events", bytes.NewBufferString(strings.Repeat("x", 2048)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body returned %d %s", recorder.Code, recorder.Body.String())
	}
}
