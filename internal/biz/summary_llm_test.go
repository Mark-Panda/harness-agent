package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeSummaryLimiter 是供应商并发舱壁的测试替身。
type fakeSummaryLimiter struct {
	acquired bool
	err      error
	released int
}

// Acquire 实现阻塞接口但测试不使用。
func (f *fakeSummaryLimiter) Acquire(context.Context, string) (func(), error) {
	return func() {}, nil
}

// TryAcquire 返回预设的舱壁状态。
func (f *fakeSummaryLimiter) TryAcquire(context.Context, string) (func(), bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.acquired {
		return nil, false, nil
	}
	return func() { f.released++ }, true, nil
}

// summaryProviderServer 构造返回固定摘要的模型服务；handler 可自定义响应。
func summaryProviderServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

// summaryRunWithPolicy 构造带模型候选快照的 Run 分页。
func summaryRunWithPolicy(t *testing.T, providerID, modelID uuid.UUID) biz.Page[biz.Run] {
	t.Helper()
	policy := map[string]any{
		"policy_id": "policy", "policy_version": 1, "min_context_tokens": int64(1_000_000),
		"candidates": []map[string]any{{
			"priority": 1, "model_id": modelID.String(), "provider_id": providerID.String(),
			"remote_model_name": "summary-model", "context_window_tokens": int64(1_000_000),
			"max_output_tokens": 8192, "capabilities": map[string]any{"tools": true},
			"input_price_per_million": 1.0, "output_price_per_million": 2.0, "data_region": "cn",
		}},
	}
	return biz.Page[biz.Run]{Items: []biz.Run{{ID: uuid.New(), ModelPolicySnapshot: policy}}}
}

// newSummaryGenerator 构造带 Mock 依赖的 LLM 摘要生成器。
func newSummaryGenerator(t *testing.T, ctrl *gomock.Controller, baseURL string, providerID, modelID uuid.UUID,
	budgets biz.BudgetChecker, limiter model.ConcurrencyLimiter, config biz.SummaryLLMConfig) (*biz.SummaryLLMGenerator, *mocks.MockRunRepository, *mocks.MockProviderCredentialRepository) {
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
	generator, err := biz.NewSummaryLLMGenerator(runs, providers, opener, budgets, limiter, config)
	if err != nil {
		t.Fatal(err)
	}
	return generator, runs, providers
}

// TestSummaryGeneratorSuccess 验证模型候选解析、鉴权头、摘要文本与 Token 统计。
func TestSummaryGeneratorSuccess(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"req-1","choices":[{"message":{"content":"滚动摘要"}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil, biz.SummaryLLMConfig{})
	generation, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "旧摘要", []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "问题", SpeakerOpenID: "ou_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation.Text != "滚动摘要" || generation.InputTokens != 5 || generation.OutputTokens != 7 ||
		generation.ProviderID == nil || *generation.ProviderID != providerID || generation.ModelID != modelID.String() {
		t.Fatalf("generation = %#v", generation)
	}
}

// TestSummaryGeneratorChunksAndMerges 验证超长输入被分块生成并归并为最终摘要。
func TestSummaryGeneratorChunksAndMerges(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	calls := 0
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		content := "分块摘要"
		if calls == 3 {
			content = "最终摘要"
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+content+`"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil,
		biz.SummaryLLMConfig{MaxInputBytes: 30, MaxOutputTokens: 512})
	generation, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "第一段"},
		{ID: uuid.New(), Role: biz.MessageAssistant, Content: "第二段"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || generation.Text != "最终摘要" {
		t.Fatalf("calls=%d generation=%#v", calls, generation)
	}
}

// TestSummaryGeneratorRecordsUsageWhenLaterCallFails 验证后续分块或归并失败时，
// 前序成功调用以及失败响应中已返回的 usage 都会记录一次。
func TestSummaryGeneratorRecordsUsageWhenLaterCallFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		failCall int
	}{
		{name: "later chunk", failCall: 2},
		{name: "merge", failCall: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerID, modelID := uuid.New(), uuid.New()
			userID := uuid.New()
			calls := 0
			server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls == test.failCall {
					_, _ = io.WriteString(w, `{"error":{"code":"server_error"},"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"分块摘要"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
			})
			defer server.Close()
			ctrl := gomock.NewController(t)
			generator, _, _ := newSummaryGenerator(t, ctrl, server.URL, providerID, modelID, nil, nil,
				biz.SummaryLLMConfig{MaxInputBytes: 30, MaxOutputTokens: 512})
			usage := mocks.NewMockUsageRecorder(ctrl)
			for call := 1; call <= test.failCall; call++ {
				input, output := 3, 4
				if call == test.failCall {
					input, output = 5, 2
				}
				cost := float64(input)/1_000_000 + float64(output)*2/1_000_000
				usage.EXPECT().RecordUsage(gomock.Any(), userID, gomock.Nil(), gomock.Eq(&providerID),
					int64(input), int64(output), cost, gomock.Any()).Return(nil)
			}
			generator.WithUsageRecorder(usage)
			_, err := generator.Summarize(context.Background(), userID, uuid.New(), "", []biz.Message{
				{ID: uuid.New(), Role: biz.MessageUser, Content: "第一段"},
				{ID: uuid.New(), Role: biz.MessageAssistant, Content: "第二段"},
			})
			if !errors.Is(err, model.ErrRetryable) || calls != test.failCall {
				t.Fatalf("Summarize() calls=%d err=%v, want failure at call %d", calls, err, test.failCall)
			}
		})
	}
}

// TestSummaryGeneratorErrorClassification 验证供应商错误被正确分类为永久或可重试。
func TestSummaryGeneratorErrorClassification(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"message":"bad"}}`, want: biz.ErrSummaryPermanent},
		{name: "auth", status: http.StatusUnauthorized, body: `{"error":{"message":"auth"}}`, want: biz.ErrSummaryPermanent},
		{name: "server", status: http.StatusInternalServerError, body: `{"error":{"message":"oops"}}`, want: model.ErrRetryable},
		{name: "empty", status: http.StatusOK, body: `{"choices":[{"message":{"content":" "}}]}`, want: biz.ErrSummaryPermanent},
		{name: "reasoning only", status: http.StatusOK, body: `{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"stop"}]}`, want: biz.ErrSummaryPermanent},
		{name: "content filtered", status: http.StatusOK, body: `{"choices":[{"message":{"content":"partial"},"finish_reason":"content_filter"}]}`, want: biz.ErrSummaryPermanent},
		{name: "unexpected tools", status: http.StatusOK, body: `{"choices":[{"message":{"content":"summary","tool_calls":[{"id":"call","function":{"name":"bad","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, want: biz.ErrSummaryPermanent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			defer server.Close()
			generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil, biz.SummaryLLMConfig{})
			_, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{
				{ID: uuid.New(), Role: biz.MessageUser, Content: "问题"},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestSummaryGeneratorUnavailableAndGuards 验证无 Run、供应商停用、预算超限与舱壁占满的终止语义。
func TestSummaryGeneratorUnavailableAndGuards(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	// 无 Run：无法继承模型策略。
	runs := mocks.NewMockRunRepository(gomock.NewController(t))
	runs.EXPECT().ListRuns(gomock.Any(), gomock.Any(), gomock.Any(), 1, "").Return(biz.Page[biz.Run]{}, nil)
	providers := mocks.NewMockProviderCredentialRepository(gomock.NewController(t))
	generator, err := biz.NewSummaryLLMGenerator(runs, providers, mocks.NewMockCredentialOpener(gomock.NewController(t)), nil, nil, biz.SummaryLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Content: "x"}}); !errors.Is(err, biz.ErrSummaryModelUnavailable) {
		t.Fatalf("no run error = %v", err)
	}

	// 预算超限：摘要调用应终止而不是无限重试。
	ctrl := gomock.NewController(t)
	budgets := mocks.NewMockBudgetChecker(ctrl)
	budgets.EXPECT().Check(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, biz.ErrBudgetExceeded)
	generator, _, _ = newSummaryGenerator(t, ctrl, "http://127.0.0.1:1", providerID, modelID, budgets, nil, biz.SummaryLLMConfig{})
	if _, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Content: "x"}}); !errors.Is(err, biz.ErrSummaryModelUnavailable) {
		t.Fatalf("budget error = %v", err)
	}

	// 舱壁占满：返回可重试错误。
	generator, _, _ = newSummaryGenerator(t, gomock.NewController(t), "http://127.0.0.1:1", providerID, modelID, nil, &fakeSummaryLimiter{}, biz.SummaryLLMConfig{})
	if _, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Content: "x"}}); !errors.Is(err, biz.ErrSummaryProviderBusy) {
		t.Fatalf("busy error = %v", err)
	}

	if _, err := biz.NewSummaryLLMGenerator(nil, nil, nil, nil, nil, biz.SummaryLLMConfig{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestSummaryGeneratorSkipsDisabledProvider 验证停用供应商不会被选中。
func TestSummaryGeneratorSkipsDisabledProvider(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	providers := mocks.NewMockProviderCredentialRepository(ctrl)
	opener := mocks.NewMockCredentialOpener(ctrl)
	runs.EXPECT().ListRuns(gomock.Any(), gomock.Any(), gomock.Any(), 1, "").Return(summaryRunWithPolicy(t, providerID, modelID), nil)
	providers.EXPECT().GetProviderCredential(gomock.Any(), providerID).
		Return(biz.Provider{ID: providerID, Status: biz.ProviderDisabled, BaseURL: "https://example.com"}, biz.ProviderCredential{}, nil)
	generator, err := biz.NewSummaryLLMGenerator(runs, providers, opener, nil, nil, biz.SummaryLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Content: "x"}})
	if !errors.Is(err, biz.ErrSummaryModelUnavailable) {
		t.Fatalf("disabled provider error = %v", err)
	}
}

// TestSummaryGeneratorValidatesEmptyInput 验证空消息被拒绝为永久错误。
func TestSummaryGeneratorValidatesEmptyInput(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), "http://127.0.0.1:1", providerID, modelID, nil, nil, biz.SummaryLLMConfig{})
	_, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", nil)
	if !errors.Is(err, biz.ErrSummaryPermanent) || !strings.Contains(err.Error(), "at least one message") {
		t.Fatalf("empty input error = %v", err)
	}
}

type summaryBudgetEvents struct {
	events     []string
	userID     uuid.UUID
	providerID uuid.UUID
	invalid    bool
}

func (f *summaryBudgetEvents) Check(_ context.Context, userID uuid.UUID, agentID, providerID *uuid.UUID) ([]biz.BudgetAlert, error) {
	f.events = append(f.events, "check")
	if userID != f.userID || agentID != nil || providerID == nil || *providerID != f.providerID {
		f.invalid = true
	}
	return nil, nil
}

func (f *summaryBudgetEvents) RecordUsage(_ context.Context, userID uuid.UUID, agentID, providerID *uuid.UUID, _, _ int64, _ float64, _ time.Time) error {
	f.events = append(f.events, "record")
	if userID != f.userID || agentID != nil || providerID == nil || *providerID != f.providerID {
		f.invalid = true
	}
	return nil
}

func TestSummaryGeneratorChecksAndRecordsEachPaidCallInOrder(t *testing.T) {
	providerID, modelID, userID := uuid.New(), uuid.New(), uuid.New()
	events := &summaryBudgetEvents{userID: userID, providerID: providerID}
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		events.events = append(events.events, "chat")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"摘要"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, events, nil,
		biz.SummaryLLMConfig{MaxInputBytes: 30, MaxOutputTokens: 512})
	generator.WithUsageRecorder(events)
	_, err := generator.Summarize(context.Background(), userID, uuid.New(), "", []biz.Message{
		{ID: uuid.New(), Role: biz.MessageUser, Content: "第一段"},
		{ID: uuid.New(), Role: biz.MessageAssistant, Content: "第二段"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check", "chat", "record", "check", "chat", "record", "check", "chat", "record"}
	if len(events.events) != len(want) {
		t.Fatalf("events = %#v, want %#v", events.events, want)
	}
	for index := range want {
		if events.events[index] != want[index] {
			t.Fatalf("events = %#v, want %#v", events.events, want)
		}
	}
	if events.invalid {
		t.Fatal("budget check or usage record used the wrong user/provider identity")
	}
}

func TestSummaryGeneratorContinuesTruncatedTextOnlyOnce(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	calls := 0
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"前半"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"后半"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil, biz.SummaryLLMConfig{})
	generation, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Role: biz.MessageUser, Content: "问题"}})
	if err != nil || generation.Text != "前半后半" || calls != 2 || generation.InputTokens != 7 || generation.OutputTokens != 10 {
		t.Fatalf("Summarize() = %#v, %v, calls=%d", generation, err, calls)
	}
}

// summaryRequestPrompt 返回请求中的用户 Prompt，用于验证上下文超限后的输入确实缩小。
func summaryRequestPrompt(t *testing.T, r *http.Request) string {
	t.Helper()
	var request struct {
		Messages []model.ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("messages = %d, want system and user", len(request.Messages))
	}
	return request.Messages[1].Content
}

// TestSummaryGeneratorRetriesContextExceededWithStrictlySmallerChunks 验证首轮及连续多轮
// context_length_exceeded 都不会用相同 Payload 原地重试。
func TestSummaryGeneratorRetriesContextExceededWithStrictlySmallerChunks(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	calls := 0
	prompts := []string{}
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		prompts = append(prompts, summaryRequestPrompt(t, r))
		w.Header().Set("Content-Type", "application/json")
		if calls <= 2 {
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"too large"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"缩小后的摘要"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil, biz.SummaryLLMConfig{})
	generation, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{{Role: biz.MessageUser, Content: strings.Repeat("中文片段", 100)}})
	if err != nil || generation.Text != "缩小后的摘要" {
		t.Fatalf("Summarize() = %#v, %v, calls=%d", generation, err, calls)
	}
	if calls < 3 {
		t.Fatalf("calls = %d, want reduced retries after the initial request", calls)
	}
	for index := 1; index < 3; index++ {
		if len(prompts[index]) >= len(prompts[index-1]) {
			t.Fatalf("prompt did not strictly shrink at request %d", index+1)
		}
	}
}

// TestSummaryGeneratorRetriesContextExceededDuringMerge 验证归并超限只重建更小的归并批次，
// 已成功分块不会用相同 Prompt 重发。
func TestSummaryGeneratorRetriesContextExceededDuringMerge(t *testing.T) {
	providerID, modelID := uuid.New(), uuid.New()
	calls := 0
	prompts := []string{}
	server := summaryProviderServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		prompts = append(prompts, summaryRequestPrompt(t, r))
		w.Header().Set("Content-Type", "application/json")
		if calls == 3 {
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"merge too large"}}`)
			return
		}
		content := "最终摘要"
		if calls <= 2 {
			content = strings.Repeat("分段摘要", 80)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+content+`"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	defer server.Close()
	generator, _, _ := newSummaryGenerator(t, gomock.NewController(t), server.URL, providerID, modelID, nil, nil,
		biz.SummaryLLMConfig{MaxInputBytes: 30, MaxOutputTokens: 512})
	generation, err := generator.Summarize(context.Background(), uuid.New(), uuid.New(), "", []biz.Message{
		{Role: biz.MessageUser, Content: "第一段"},
		{Role: biz.MessageAssistant, Content: "第二段"},
	})
	if err != nil || generation.Text != "最终摘要" {
		t.Fatalf("Summarize() = %#v, %v, calls=%d", generation, err, calls)
	}
	if calls < 5 {
		t.Fatalf("calls = %d, want reduced merge requests", calls)
	}
	for index := 3; index < len(prompts); index++ {
		if len(prompts[index]) >= len(prompts[2]) {
			t.Fatalf("retry prompt %d was not smaller than rejected merge", index+1)
		}
	}
	for index, prompt := range prompts[:2] {
		for later := index + 1; later < len(prompts); later++ {
			if prompts[later] == prompt {
				t.Fatalf("successful chunk %d was retried with identical payload", index+1)
			}
		}
	}
}
