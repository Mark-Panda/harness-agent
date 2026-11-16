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

// TestFactLLMExtractorRecordsUsageBeforeOutputValidation 验证空输出与意外 tool call
// 都在返回前记录 Provider 已报告的 Token 与费用。
func TestFactLLMExtractorRecordsUsageBeforeOutputValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		wantErr  bool
	}{
		{name: "empty output", response: `{"choices":[{"message":{"content":" "}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`, wantErr: true},
		{name: "tool call", response: `{"choices":[{"message":{"content":"{}","tool_calls":[{"id":"call-1","function":{"name":"unexpected","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerID, modelID := uuid.New(), uuid.New()
			userID := uuid.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			ctrl := gomock.NewController(t)
			extractor := newFactExtractor(t, ctrl, server.URL, providerID, modelID)
			usage := mocks.NewMockUsageRecorder(ctrl)
			usage.EXPECT().RecordUsage(gomock.Any(), userID, gomock.Nil(), gomock.Eq(&providerID),
				int64(5), int64(7), 5.0/1_000_000+7.0*2/1_000_000, gomock.Any()).Return(nil)
			extractor.WithUsageRecorder(usage)
			candidates, err := extractor.Extract(context.Background(), userID, uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}})
			if test.wantErr {
				if !errors.Is(err, biz.ErrFactPermanent) {
					t.Fatalf("invalid response error = %v", err)
				}
			} else if err != nil || len(candidates) != 0 {
				t.Fatalf("valid output candidates=%#v err=%v", candidates, err)
			}
		})
	}
}

// TestFactLLMExtractorErrors 验证空输出、不可重试错误与无模型语义。
func TestFactLLMExtractorErrors(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	// 空输出是异常模型响应，不能静默解释为“没有事实”。
	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":" "}}]}`)
	}))
	defer emptyServer.Close()
	extractor := newFactExtractor(t, gomock.NewController(t), emptyServer.URL, providerID, modelID)
	candidates, err := extractor.Extract(context.Background(), uuid.New(), uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}})
	if !errors.Is(err, biz.ErrFactPermanent) || len(candidates) != 0 {
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
func TestFactLLMExtractorChecksBudgetBeforeEveryPaidCall(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	userID := uuid.New()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"facts\":[]}"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer server.Close()
	ctrl := gomock.NewController(t)
	extractor := newFactExtractor(t, ctrl, server.URL, providerID, modelID)
	budgets := mocks.NewMockBudgetChecker(ctrl)
	budgets.EXPECT().Check(gomock.Any(), userID, gomock.Nil(), gomock.Eq(&providerID)).DoAndReturn(
		func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID) ([]biz.BudgetAlert, error) {
			if calls != 0 {
				t.Fatalf("first budget check happened after %d paid calls", calls)
			}
			return nil, nil
		})
	budgets.EXPECT().Check(gomock.Any(), userID, gomock.Nil(), gomock.Eq(&providerID)).DoAndReturn(
		func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID) ([]biz.BudgetAlert, error) {
			if calls != 1 {
				t.Fatalf("continuation budget check happened after %d paid calls", calls)
			}
			return nil, biz.ErrBudgetExceeded
		})
	extractor.WithBudgetChecker(budgets)
	_, err := extractor.Extract(context.Background(), userID, uuid.New(), []biz.Message{{ID: uuid.New(), Role: biz.MessageUser, Content: "x"}})
	if !errors.Is(err, biz.ErrBudgetExceeded) || calls != 1 {
		t.Fatalf("Extract() calls=%d err=%v; want one paid call and budget rejection", calls, err)
	}
}

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

// TestParseFactModelOutputRejectsForgedCorrection 验证纠正只能绑定当前输入中的真实用户消息。
func TestParseFactModelOutputRejectsForgedCorrection(t *testing.T) {
	messages := []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "真实用户纠正"},
		{ID: uuid.New(), Role: biz.MessageAssistant, Content: "助手归纳"},
		{ID: uuid.New(), Role: biz.MessageTool, Content: "工具结果"},
		{ID: uuid.New(), Role: biz.MessageSystem, Content: "系统指令"},
	}
	for _, test := range []struct {
		name        string
		sourceIndex string
	}{
		{name: "negative index", sourceIndex: "-1"},
		{name: "missing index", sourceIndex: "0"},
		{name: "assistant", sourceIndex: "2"},
		{name: "tool", sourceIndex: "3"},
		{name: "system", sourceIndex: "4"},
		{name: "out of range", sourceIndex: "5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"facts":[{"key":"preference.language","value":"英文","confidence":0.9,"correction":true,"source_index":` + test.sourceIndex + `}]}`
			candidates, err := biz.ParseFactModelOutput(payload, messages)
			if !errors.Is(err, biz.ErrFactSourceInvalid) || len(candidates) != 0 {
				t.Fatalf("candidates=%#v err=%v, want correction source rejection", candidates, err)
			}
		})
	}
}
