package biz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

func TestPlanDirectCallsUseSharedResponseGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		want      string
		wantErr   error
	}{
		{name: "empty", responses: []string{`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`}, wantErr: react.ErrEmptyResponse},
		{name: "reasoning only", responses: []string{`{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"stop"}]}`}, wantErr: react.ErrReasoningOnly},
		{name: "filtered", responses: []string{`{"choices":[{"message":{"content":"partial"},"finish_reason":"content_filter"}]}`}, wantErr: react.ErrContentFiltered},
		{name: "unexpected tools", responses: []string{`{"choices":[{"message":{"content":"answer","tool_calls":[{"id":"call","function":{"name":"bad","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`}, wantErr: react.ErrIncompleteToolCall},
		{name: "continued", responses: []string{
			`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
			`{"choices":[{"message":{"content":" tail"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
		}, want: "partial tail"},
		{name: "continuation truncated", responses: []string{
			`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
			`{"choices":[{"message":{"content":"tail"},"finish_reason":"length"}]}`,
		}, wantErr: react.ErrStepLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			worker, _, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.responses[calls])
				calls++
			})
			plan := Plan{ID: uuid.New(), Goal: "goal", Steps: []PlanStep{{StepNo: 1, Title: "step", Status: PlanStepCompleted, Result: map[string]any{"content": "result"}}}}
			content, completion, err := worker.summarizePlan(context.Background(), router, plan)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("summarizePlan() = %q, %v; want %v", content, err, tc.wantErr)
			}
			if tc.wantErr == nil && (content != tc.want || calls != 2 || completion.Usage.InputTokens != 7 || completion.Usage.OutputTokens != 10) {
				t.Fatalf("summarizePlan() = %q, completion=%#v, calls=%d", content, completion, calls)
			}
		})
	}
}

func TestGeneratePlanRejectsUnexpectedToolCalls(t *testing.T) {
	worker, _, router, _ := planWorkerFixture(t, func(_ string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"{}","tool_calls":[{"id":"call","function":{"name":"bad","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	if _, _, err := worker.generatePlan(context.Background(), router, []react.Message{{Role: react.RoleUser, Content: "task"}}, emptyToolRegistry{}); !errors.Is(err, react.ErrIncompleteToolCall) {
		t.Fatalf("generatePlan() error = %v", err)
	}
}

func TestPlanSupervisorDraftUsesTextResponseGuard(t *testing.T) {
	calls := 0
	complete := func([]react.Message) (react.Completion, error) {
		calls++
		if calls == 1 {
			return react.Completion{Content: `{"workers":[`, FinishReason: "length"}, nil
		}
		return react.Completion{Content: `{"name":"A","task":"task","tools":[]}]}`, FinishReason: "stop"}, nil
	}
	draft, completion, err := planSupervisorDraft(context.Background(), []react.Message{{Role: react.RoleUser, Content: "task"}}, complete, 2, emptyToolRegistry{})
	if err != nil || len(draft.Workers) != 1 || calls != 2 {
		t.Fatalf("planSupervisorDraft(context.Background(), ) = %#v, %#v, %v, calls=%d", draft, completion, err, calls)
	}

	if _, _, err := planSupervisorDraft(context.Background(), nil, func([]react.Message) (react.Completion, error) {
		return react.Completion{Content: "not json"}, nil
	}, 2, emptyToolRegistry{}); err == nil {
		t.Fatal("invalid JSON must fail")
	}
	if _, _, err := planSupervisorDraft(context.Background(), nil, func([]react.Message) (react.Completion, error) {
		return react.Completion{Content: "{}", ToolCalls: []react.ToolCall{{ID: "call"}}}, nil
	}, 2, emptyToolRegistry{}); !errors.Is(err, react.ErrIncompleteToolCall) {
		t.Fatalf("unexpected tool call error = %v", err)
	}
}

func TestSkillLLMExtractorRejectsAbnormalResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty", body: `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`},
		{name: "reasoning only", body: `{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"stop"}]}`},
		{name: "unexpected tools", body: `{"choices":[{"message":{"content":"{}","tool_calls":[{"id":"call","function":{"name":"bad","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{name: "filtered", body: `{"choices":[{"message":{"content":"{}"},"finish_reason":"content_filter"}]}`},
		{name: "invalid JSON", body: `{"choices":[{"message":{"content":"not json"},"finish_reason":"stop"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			providerID := uuid.New()
			catalogModel := skillLLMModel(providerID, "model")
			policyID := uuid.New()
			policies := &fakeSkillPolicyRepository{
				policies: map[uuid.UUID]ModelPolicy{policyID: skillLLMPolicy(policyID, PolicyCandidate{ModelID: catalogModel.ID, Priority: 1})},
				models:   map[uuid.UUID]Model{catalogModel.ID: catalogModel},
			}
			providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
				providerID: {provider: Provider{ID: providerID, Status: ProviderEnabled, BaseURL: server.URL}},
			}}
			userID := uuid.New()
			version := 1
			settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
				userID: {UserID: userID, Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version},
			}}
			extractor, err := NewSkillLLMExtractor(settings, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{})
			if err != nil {
				t.Fatal(err)
			}
			sessions, messages := skillLLMConversations()
			candidate, err := extractor.Extract(context.Background(), userID, sessions, messages)
			if !errors.Is(err, ErrUserSkillSynthesisUnavailable) || candidate.ShouldCreate {
				t.Fatalf("Extract() = %#v, %v", candidate, err)
			}
		})
	}
}

func TestSkillLLMExtractorContinuesTruncatedJSON(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"should_create\":"},"finish_reason":"length"}]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "不要调用工具") {
			t.Fatalf("continuation request missing shared instruction: %s", raw)
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":" false}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	providerID := uuid.New()
	catalogModel := skillLLMModel(providerID, "model")
	policyID := uuid.New()
	policies := &fakeSkillPolicyRepository{
		policies: map[uuid.UUID]ModelPolicy{policyID: skillLLMPolicy(policyID, PolicyCandidate{ModelID: catalogModel.ID, Priority: 1})},
		models:   map[uuid.UUID]Model{catalogModel.ID: catalogModel},
	}
	providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
		providerID: {provider: Provider{ID: providerID, Status: ProviderEnabled, BaseURL: server.URL}},
	}}
	userID := uuid.New()
	version := 1
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		userID: {UserID: userID, Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version},
	}}
	extractor, err := NewSkillLLMExtractor(settings, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, messages := skillLLMConversations()
	candidate, err := extractor.Extract(context.Background(), userID, sessions, messages)
	if err != nil || candidate.ShouldCreate || calls != 2 {
		t.Fatalf("Extract() = %#v, %v, calls=%d", candidate, err, calls)
	}
}
