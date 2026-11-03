package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOAuthClientExchangeAndAuthorizeURL 验证授权地址生成与授权码换取 open_id。
func TestOAuthClientExchangeAndAuthorizeURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/authen/v2/oauth/token":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") == "" {
				t.Fatalf("unexpected token request: %s %s", r.Method, r.Header.Get("Content-Type"))
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode token payload: %v", err)
			}
			if payload["code"] != "code-1" || payload["client_id"] != "app-id" || payload["redirect_uri"] != "https://example.com/callback" {
				t.Fatalf("unexpected token payload: %#v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "access_token": "user-token", "token_type": "Bearer",
			})
		case "/open-apis/authen/v1/user_info":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer user-token" {
				t.Fatalf("unexpected user info request: %s %s", r.Method, r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]string{"open_id": "ou_alice", "tenant_key": "tenant_1", "name": "Alice"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewOAuthClient(OAuthConfig{AppID: "app-id", AppSecret: "app-secret", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	authorizeURL := client.AuthorizeURL("https://example.com/callback", "state-1")
	for _, expected := range []string{"app_id=app-id", "state=state-1", "redirect_uri=https%3A%2F%2Fexample.com%2Fcallback"} {
		if !strings.Contains(authorizeURL, expected) {
			t.Fatalf("authorize URL %q missing %q", authorizeURL, expected)
		}
	}
	openID, tenantKey, name, err := client.Exchange(context.Background(), "code-1", "https://example.com/callback")
	if err != nil || openID != "ou_alice" || tenantKey != "tenant_1" || name != "Alice" {
		t.Fatalf("Exchange() = %q %q %q, %v", openID, tenantKey, name, err)
	}
}

// TestOAuthClientRejectsInvalidConfigAndResponses 验证缺少凭据与上游错误时的失败路径。
func TestOAuthClientRejectsInvalidConfigAndResponses(t *testing.T) {
	if _, err := NewOAuthClient(OAuthConfig{AppID: "app-id"}); err == nil {
		t.Fatal("missing app secret should fail")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 10003, "msg": "invalid code"})
	}))
	defer server.Close()
	client, err := NewOAuthClient(OAuthConfig{AppID: "app-id", AppSecret: "app-secret", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.Exchange(context.Background(), "bad", "https://example.com/callback"); err == nil {
		t.Fatal("rejected token exchange should fail")
	}
	if _, _, _, err := client.Exchange(context.Background(), "", "https://example.com/callback"); err == nil {
		t.Fatal("empty authorization code should fail")
	}
}
