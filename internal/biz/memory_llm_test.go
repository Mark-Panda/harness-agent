package biz_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// embeddingVectorJSON 构造 1536 维向量的 JSON 片段。
func embeddingVectorJSON(value string) string {
	return "[" + strings.Repeat(value+",", biz.EmbeddingDimension-1) + value + "]"
}

// newEmbeddingGenerator 构造带 Mock 凭证依赖的 LLM embedding 生成器。
func newEmbeddingGenerator(t *testing.T, ctrl *gomock.Controller, baseURL string, providerID uuid.UUID,
	status biz.ProviderStatus, openerErr error) (*biz.LLMEmbeddingGenerator, *mocks.MockProviderCredentialRepository) {
	t.Helper()
	providers := mocks.NewMockProviderCredentialRepository(ctrl)
	opener := mocks.NewMockCredentialOpener(ctrl)
	providers.EXPECT().GetProviderCredential(gomock.Any(), providerID).
		Return(biz.Provider{ID: providerID, Status: status, BaseURL: baseURL},
			biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("cipher")}, nil).AnyTimes()
	if openerErr != nil {
		opener.EXPECT().Open("v1", []byte("cipher")).Return(nil, openerErr).AnyTimes()
	} else {
		opener.EXPECT().Open("v1", []byte("cipher")).Return([]byte("secret-key"), nil).AnyTimes()
	}
	generator, err := biz.NewLLMEmbeddingGenerator(providers, opener, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return generator, providers
}

// TestLLMEmbeddingGeneratorSuccess 验证请求鉴权、维度与用量映射。
func TestLLMEmbeddingGeneratorSuccess(t *testing.T) {
	providerID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-1","data":[{"index":0,"embedding":`+embeddingVectorJSON("0.1")+`}],"usage":{"prompt_tokens":9}}`)
	}))
	defer server.Close()
	generator, _ := newEmbeddingGenerator(t, gomock.NewController(t), server.URL, providerID, biz.ProviderEnabled, nil)
	index := biz.EmbeddingIndex{ProviderID: providerID, Model: "embedding", Dimension: biz.EmbeddingDimension, Version: "v1"}
	result, err := generator.Embed(context.Background(), index, []string{"文本"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Vectors) != 1 || len(result.Vectors[0]) != biz.EmbeddingDimension || result.InputTokens != 9 {
		t.Fatalf("result = %#v", result)
	}
}

// TestLLMEmbeddingGeneratorGuards 验证停用供应商、解密失败、认证错误与非法输入。
func TestLLMEmbeddingGeneratorGuards(t *testing.T) {
	providerID := uuid.New()
	index := biz.EmbeddingIndex{ProviderID: providerID, Model: "embedding", Dimension: biz.EmbeddingDimension}

	generator, _ := newEmbeddingGenerator(t, gomock.NewController(t), "https://example.com", providerID, biz.ProviderDisabled, nil)
	if _, err := generator.Embed(context.Background(), index, []string{"文本"}); !errors.Is(err, biz.ErrEmbeddingModelUnavailable) {
		t.Fatalf("disabled provider error = %v", err)
	}

	generator, _ = newEmbeddingGenerator(t, gomock.NewController(t), "https://example.com", providerID, biz.ProviderEnabled, errors.New("bad cipher"))
	if _, err := generator.Embed(context.Background(), index, []string{"文本"}); !errors.Is(err, biz.ErrEmbeddingModelUnavailable) {
		t.Fatalf("decrypt error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"auth"}`)
	}))
	defer server.Close()
	generator, _ = newEmbeddingGenerator(t, gomock.NewController(t), server.URL, providerID, biz.ProviderEnabled, nil)
	if _, err := generator.Embed(context.Background(), index, []string{"文本"}); !errors.Is(err, biz.ErrEmbeddingModelUnavailable) {
		t.Fatalf("auth error = %v", err)
	}

	if _, err := generator.Embed(context.Background(), index, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty inputs error = %v", err)
	}
	if _, err := generator.Embed(context.Background(), biz.EmbeddingIndex{ProviderID: providerID, Dimension: 768}, []string{"文本"}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("dimension error = %v", err)
	}
	if _, err := biz.NewLLMEmbeddingGenerator(nil, nil, 0); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// fakeRerankModelSource 返回固定 rerank 模型或错误。
type fakeRerankModelSource struct {
	providerID uuid.UUID
	model      string
	err        error
}

// GetEnabledRerankModel 返回预置 rerank 模型。
func (f fakeRerankModelSource) GetEnabledRerankModel(context.Context) (uuid.UUID, string, error) {
	return f.providerID, f.model, f.err
}

// newReranker 构造带 Mock 凭证依赖的 LLM rerank 生成器。
func newReranker(t *testing.T, ctrl *gomock.Controller, baseURL string, providerID uuid.UUID,
	status biz.ProviderStatus, models biz.RerankModelSource) *biz.LLMReranker {
	t.Helper()
	providers := mocks.NewMockProviderCredentialRepository(ctrl)
	opener := mocks.NewMockCredentialOpener(ctrl)
	providers.EXPECT().GetProviderCredential(gomock.Any(), providerID).
		Return(biz.Provider{ID: providerID, Status: status, BaseURL: baseURL},
			biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("cipher")}, nil).AnyTimes()
	opener.EXPECT().Open("v1", []byte("cipher")).Return([]byte("secret-key"), nil).AnyTimes()
	reranker, err := biz.NewLLMReranker(providers, models, opener, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return reranker
}

// TestLLMRerankerSuccess 验证 rerank 结果按下标映射到候选分数。
func TestLLMRerankerSuccess(t *testing.T) {
	providerID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.2}]}`)
	}))
	defer server.Close()
	reranker := newReranker(t, gomock.NewController(t), server.URL, providerID, biz.ProviderEnabled,
		fakeRerankModelSource{providerID: providerID, model: "rerank-model"})
	scores, err := reranker.Rerank(context.Background(), "查询", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0] != 0.2 || scores[1] != 0.9 {
		t.Fatalf("scores = %#v", scores)
	}
}

// TestLLMRerankerGuards 验证模型缺失、供应商停用与非法输入。
func TestLLMRerankerGuards(t *testing.T) {
	providerID := uuid.New()
	reranker := newReranker(t, gomock.NewController(t), "https://example.com", providerID, biz.ProviderEnabled,
		fakeRerankModelSource{err: errors.New("no model")})
	if _, err := reranker.Rerank(context.Background(), "查询", []string{"a"}); !errors.Is(err, biz.ErrRerankModelUnavailable) {
		t.Fatalf("missing model error = %v", err)
	}
	reranker = newReranker(t, gomock.NewController(t), "https://example.com", providerID, biz.ProviderDisabled,
		fakeRerankModelSource{providerID: providerID, model: "rerank-model"})
	if _, err := reranker.Rerank(context.Background(), "查询", []string{"a"}); !errors.Is(err, biz.ErrRerankModelUnavailable) {
		t.Fatalf("disabled provider error = %v", err)
	}
	if _, err := reranker.Rerank(context.Background(), "查询", nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty documents error = %v", err)
	}
	if _, err := biz.NewLLMReranker(nil, nil, nil, 0); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestLLMEmbeddingGeneratorRetryable 验证供应商 5xx 保留可重试分类。
func TestLLMEmbeddingGeneratorRetryable(t *testing.T) {
	providerID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	generator, _ := newEmbeddingGenerator(t, gomock.NewController(t), server.URL, providerID, biz.ProviderEnabled, nil)
	_, err := generator.Embed(context.Background(), biz.EmbeddingIndex{
		ProviderID: providerID, Model: "embedding", Dimension: biz.EmbeddingDimension,
	}, []string{"文本"})
	if err == nil || errors.Is(err, biz.ErrEmbeddingModelUnavailable) {
		t.Fatalf("retryable error = %v", err)
	}
}
