package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRerankTestClient 构造指向测试服务器的客户端。
func newRerankTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret-key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestRerankSuccess 验证请求参数、鉴权与乱序下标的分数映射。
func TestRerankSuccess(t *testing.T) {
	client := newRerankTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"query":"查询"`) || !strings.Contains(string(body), `"top_n":3`) ||
			!strings.Contains(string(body), `"model":"rerank-model"`) {
			t.Fatalf("unexpected body %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-1","results":[{"index":2,"relevance_score":0.9},{"index":0,"relevance_score":0.5},{"index":1,"score":0.7}],"usage":{"prompt_tokens":12}}`)
	})
	response, err := client.Rerank(context.Background(), RerankRequest{
		Model: "rerank-model", Query: "查询", Documents: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 3 || response.InputTokens != 12 || response.ProviderRequestID != "req-1" {
		t.Fatalf("response = %#v", response)
	}
	scores := map[int]float64{}
	for _, hit := range response.Results {
		scores[hit.Index] = hit.Score
	}
	if scores[0] != 0.5 || scores[1] != 0.7 || scores[2] != 0.9 {
		t.Fatalf("scores = %#v", scores)
	}
}

// TestRerankGuards 验证非法输入、认证失败、可重试状态与越界下标。
func TestRerankGuards(t *testing.T) {
	client := newRerankTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"auth"}`)
	})
	if _, err := client.Rerank(context.Background(), RerankRequest{Model: "", Query: "q", Documents: []string{"a"}}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("missing model error = %v", err)
	}
	if _, err := client.Rerank(context.Background(), RerankRequest{Model: "m", Query: "", Documents: []string{"a"}}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("missing query error = %v", err)
	}
	if _, err := client.Rerank(context.Background(), RerankRequest{Model: "m", Query: "q"}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("empty documents error = %v", err)
	}
	if _, err := client.Rerank(context.Background(), RerankRequest{Model: "m", Query: "q", Documents: []string{"a"}}); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("auth error = %v", err)
	}

	retryClient := newRerankTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := retryClient.Rerank(context.Background(), RerankRequest{Model: "m", Query: "q", Documents: []string{"a"}}); !errors.Is(err, ErrRetryable) {
		t.Fatalf("retryable error = %v", err)
	}

	outOfRange := newRerankTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"index":5,"relevance_score":0.1}]}`)
	})
	if _, err := outOfRange.Rerank(context.Background(), RerankRequest{Model: "m", Query: "q", Documents: []string{"a"}}); !errors.Is(err, ErrRetryable) {
		t.Fatalf("out of range error = %v", err)
	}
}
