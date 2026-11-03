package biz_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// newFactExtractor 构造带 Mock 模型依赖的事实提取器。
func newFactExtractor(t *testing.T, ctrl *gomock.Controller, baseURL string, providerID, modelID uuid.UUID) *biz.FactLLMExtractor {
	t.Helper()
	runs := mocks.NewMockRunRepository(ctrl)
	providers := mocks.NewMockProviderCredentialRepository(ctrl)
	opener := mocks.NewMockCredentialOpener(ctrl)
	runs.EXPECT().ListRuns(gomock.Any(), gomock.Any(), gomock.Any(), 1, "").
		Return(summaryRunWithPolicy(t, providerID, modelID), nil).AnyTimes()
	providers.EXPECT().GetProviderCredential(gomock.Any(), providerID).
		Return(biz.Provider{ID: providerID, Status: biz.ProviderEnabled, BaseURL: baseURL},
			biz.ProviderCredential{KeyVersion: "v1", Encrypted: []byte("cipher")}, nil).AnyTimes()
	opener.EXPECT().Open("v1", []byte("cipher")).Return([]byte("secret-key"), nil).AnyTimes()
	extractor, err := biz.NewFactLLMExtractor(runs, providers, opener, biz.FactLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

// TestFactLLMExtractorSuccess 验证事实 JSON 解析、来源映射与纠正标记。
func TestFactLLMExtractorSuccess(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-1","choices":[{"message":{"content":"{\"facts\":[{\"key\":\"preference.language\",\"value\":\"中文\",\"confidence\":0.9,\"correction\":false,\"source_index\":1},{\"key\":\"profile.timezone\",\"value\":\"Asia/Shanghai\",\"confidence\":0.8,\"correction\":true,\"source_index\":2}]}"}}]}`)
	}))
	defer server.Close()
	extractor := newFactExtractor(t, gomock.NewController(t), server.URL, providerID, modelID)
	messages := []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "我偏好中文"},
		{ID: uuid.New(), Role: biz.MessageUser, Content: "时区是上海"},
	}
	candidates, err := extractor.Extract(context.Background(), uuid.New(), uuid.New(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if candidates[0].SourceMessageID == nil || *candidates[0].SourceMessageID != messages[0].ID || candidates[0].SourceKind != biz.FactSourceUser {
		t.Fatalf("candidate source = %#v", candidates[0])
	}
	if !candidates[1].Correction || candidates[1].SourceKind != biz.FactSourceCorrection {
		t.Fatalf("correction candidate = %#v", candidates[1])
	}
}

// TestFactLLMExtractorErrors 验证空输出、不可重试错误与无模型语义。
func TestFactLLMExtractorErrors(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	// 空输出表示没有事实，不算失败。
	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":" "}}]}`)
	}))
	defer emptyServer.Close()
	extractor := newFactExtractor(t, gomock.NewController(t), emptyServer.URL, providerID, modelID)
	candidates, err := extractor.Extract(context.Background(), uuid.New(), uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}})
	if err != nil || len(candidates) != 0 {
		t.Fatalf("empty output candidates=%#v err=%v", candidates, err)
	}

	// 非法 JSON 属于永久错误。
	invalidServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}]}`)
	}))
	defer invalidServer.Close()
	extractor = newFactExtractor(t, gomock.NewController(t), invalidServer.URL, providerID, modelID)
	if _, err := extractor.Extract(context.Background(), uuid.New(), uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}}); !errors.Is(err, biz.ErrFactPermanent) {
		t.Fatalf("invalid json error = %v", err)
	}

	// 无 Run 时继承不到模型，返回不可用。
	runs := mocks.NewMockRunRepository(gomock.NewController(t))
	runs.EXPECT().ListRuns(gomock.Any(), gomock.Any(), gomock.Any(), 1, "").Return(biz.Page[biz.Run]{}, nil)
	noModel, err := biz.NewFactLLMExtractor(runs, mocks.NewMockProviderCredentialRepository(gomock.NewController(t)),
		mocks.NewMockCredentialOpener(gomock.NewController(t)), biz.FactLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noModel.Extract(context.Background(), uuid.New(), uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}}); !errors.Is(err, biz.ErrFactExtractionUnavailable) {
		t.Fatalf("no model error = %v", err)
	}

	if _, err := biz.NewFactLLMExtractor(nil, nil, nil, biz.FactLLMConfig{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestFactLLMExtractorRetryable 验证供应商 5xx 保留可重试错误。
func TestFactLLMExtractorRetryable(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	extractor := newFactExtractor(t, gomock.NewController(t), server.URL, providerID, modelID)
	_, err := extractor.Extract(context.Background(), uuid.New(), uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}})
	if err == nil || errors.Is(err, biz.ErrFactPermanent) || errors.Is(err, biz.ErrFactExtractionUnavailable) {
		t.Fatalf("retryable error = %v", err)
	}
}
