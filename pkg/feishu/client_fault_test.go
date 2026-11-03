package feishu

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientSlowUpstreamFailsFast 验证飞书慢调用受 HTTP 超时约束，避免占满回复 Worker。
func TestClientSlowUpstreamFailsFast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"late","expire":7200}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, AppID: "app", AppSecret: "secret",
		HTTPClient: &http.Client{Timeout: 25 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := client.TenantAccessToken(context.Background()); err == nil {
		t.Fatal("slow upstream must fail with timeout")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("slow upstream was not abandoned in time: %s", elapsed)
	}
}

// TestClientUpstreamUnavailableReturnsClassifiedError 验证飞书不可用时返回可分类错误而不是伪成功。
func TestClientUpstreamUnavailableReturnsClassifiedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"code":0,"msg":"bad gateway"}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{BaseURL: server.URL, AppID: "app", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := client.TenantAccessToken(context.Background())
	if callErr == nil {
		t.Fatal("upstream 502 must return an error")
	}
	var apiError *APIError
	if !errors.As(callErr, &apiError) || apiError.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected error classification: %v", callErr)
	}
}
