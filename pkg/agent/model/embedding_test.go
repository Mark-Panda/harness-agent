package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbedSuccess 验证请求路径、鉴权、输入与向量解析。
func TestEmbedSuccess(t *testing.T) {
	var received struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-1","data":[
			{"index":0,"embedding":[0.1,0.2,0.3]},
			{"index":1,"embedding":[0.4,0.5,0.6]}
		],"usage":{"prompt_tokens":12}}`)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret-key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Embed(context.Background(), EmbeddingRequest{
		Model: "embedding-model", Inputs: []string{"第一段", "第二段"}, Dimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "embedding-model" || len(received.Input) != 2 || received.Dimensions != 3 {
		t.Fatalf("request = %#v", received)
	}
	if len(response.Embeddings) != 2 || len(response.Embeddings[0]) != 3 || response.Embeddings[0][0] != 0.1 {
		t.Fatalf("embeddings = %#v", response.Embeddings)
	}
	if response.InputTokens != 12 || response.ProviderRequestID != "req-1" {
		t.Fatalf("response = %#v", response)
	}
}

// TestEmbedValidationAndErrors 验证条数、维度与供应商错误分类。
func TestEmbedValidationAndErrors(t *testing.T) {
	client, err := NewClient("https://models.example.com", "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Embed(context.Background(), EmbeddingRequest{Model: "m"}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("empty inputs error = %v", err)
	}
	if _, err := client.Embed(context.Background(), EmbeddingRequest{Model: "", Inputs: []string{"x"}}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("model error = %v", err)
	}

	_, err = client.Embed(context.Background(), EmbeddingRequest{}) // 触发输入校验
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("validation error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"oops"}`)
	}))
	defer server.Close()
	retryClient, err := NewClient(server.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retryClient.Embed(context.Background(), EmbeddingRequest{Model: "m", Inputs: []string{"x"}}); !errors.Is(err, ErrRetryable) {
		t.Fatalf("server error = %v", err)
	}

	// 维度不一致属于永久错误。
	dimServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	}))
	defer dimServer.Close()
	dimClient, err := NewClient(dimServer.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dimClient.Embed(context.Background(), EmbeddingRequest{Model: "m", Inputs: []string{"x"}, Dimensions: 3}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("dimension error = %v", err)
	}

	// 返回条数不足属于可重试错误。
	countServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer countServer.Close()
	countClient, err := NewClient(countServer.URL, "secret-key", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := countClient.Embed(context.Background(), EmbeddingRequest{Model: "m", Inputs: []string{"x"}}); !errors.Is(err, ErrRetryable) {
		t.Fatalf("count error = %v", err)
	}
}
