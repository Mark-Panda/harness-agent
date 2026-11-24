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

	"github.com/google/uuid"
)

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
			candidate, err := extractor.Extract(context.Background(), userID, sessions, messages, nil)
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
	candidate, err := extractor.Extract(context.Background(), userID, sessions, messages, nil)
	if err != nil || candidate.ShouldCreate || calls != 2 {
		t.Fatalf("Extract() = %#v, %v, calls=%d", candidate, err, calls)
	}
}
