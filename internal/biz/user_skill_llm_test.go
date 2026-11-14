package biz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseSkillCandidateShouldCreateFalse(t *testing.T) {
	candidate, err := parseSkillCandidate(`{"should_create": false}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if candidate.ShouldCreate {
		t.Fatal("expected should_create false")
	}
}

func TestParseSkillCandidateFull(t *testing.T) {
	raw := `{
		"should_create": true,
		"confidence": 0.85,
		"name": "daily_report",
		"description": "生成每日工作报告",
		"prompt_template": "请总结今日工作：{{content}}",
		"parameters": {"content": "string"}
	}`
	candidate, err := parseSkillCandidate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !candidate.ShouldCreate || candidate.Confidence != 0.85 || candidate.Name != "daily_report" {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
	if !strings.Contains(candidate.PromptTemplate, "{{content}}") {
		t.Fatalf("prompt template missing placeholder: %s", candidate.PromptTemplate)
	}
	if candidate.Parameters["content"] != "string" {
		t.Fatalf("unexpected parameters: %#v", candidate.Parameters)
	}
}

func TestParseSkillCandidateMarkdownWrapped(t *testing.T) {
	raw := "```json\n{\"should_create\": true, \"confidence\": 0.7, \"name\": \"x\", \"prompt_template\": \"p\"}\n```"
	candidate, err := parseSkillCandidate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !candidate.ShouldCreate || candidate.Name != "x" {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
}

func TestParseSkillCandidateInvalidJSON(t *testing.T) {
	if _, err := parseSkillCandidate("not json"); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseSkillCandidateMissingNameWhenShouldCreate(t *testing.T) {
	raw := `{"should_create": true, "confidence": 0.9, "prompt_template": "p"}`
	if _, err := parseSkillCandidate(raw); err == nil {
		t.Fatal("expected error when name is missing")
	}
}

type fakeSkillProviderCredential struct {
	provider   Provider
	credential ProviderCredential
	err        error
}

type fakeSkillProviderCredentials struct {
	items map[uuid.UUID]fakeSkillProviderCredential
}

func (f *fakeSkillProviderCredentials) GetProviderCredential(_ context.Context, providerID uuid.UUID) (Provider, ProviderCredential, error) {
	item, ok := f.items[providerID]
	if !ok {
		return Provider{}, ProviderCredential{}, ErrResourceNotFound
	}
	return item.provider, item.credential, item.err
}

type skillLLMOpener struct{}

func (skillLLMOpener) Open(string, []byte) ([]byte, error) { return []byte("test-key"), nil }

type skillUsageRecord struct {
	userID       uuid.UUID
	providerID   *uuid.UUID
	inputTokens  int64
	outputTokens int64
	cost         float64
}

type fakeSkillUsageRecorder struct {
	records []skillUsageRecord
}

func (f *fakeSkillUsageRecorder) RecordUsage(_ context.Context, userID uuid.UUID, _, providerID *uuid.UUID, inputTokens, outputTokens int64, cost float64, _ time.Time) error {
	f.records = append(f.records, skillUsageRecord{userID: userID, providerID: providerID, inputTokens: inputTokens, outputTokens: outputTokens, cost: cost})
	return nil
}

type skillLLMRequestLog struct {
	models []string
}

func newSkillLLMServer(t *testing.T, statuses map[string]int) (*httptest.Server, *skillLLMRequestLog) {
	t.Helper()
	log := &skillLLMRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode chat request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.models = append(log.models, request.Model)
		if status := statuses[request.Model]; status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"failure"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "skill-test",
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": `{"should_create":true,"confidence":0.9,"name":"reuse","prompt_template":"do {{thing}}"}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
		})
	}))
	t.Cleanup(server.Close)
	return server, log
}

func skillLLMModel(providerID uuid.UUID, remoteName string) Model {
	now := time.Now().UTC()
	return Model{
		ID: uuid.New(), ProviderID: providerID, RemoteModelName: remoteName,
		Kind: ModelKindChat, Status: ModelEnabled, ContextWindowTokens: MinGenerationContextTokens,
		MaxOutputTokens: 4096, VerificationSource: "test", VerifiedAt: &now,
		InputPricePerMillion: 10, OutputPricePerMillion: 20,
	}
}

func skillLLMPolicy(id uuid.UUID, candidates ...PolicyCandidate) ModelPolicy {
	return ModelPolicy{ID: id, Version: 1, Status: PolicyPublished, MinContextTokens: MinGenerationContextTokens, Candidates: candidates}
}

func skillLLMConversations() ([]Session, map[uuid.UUID][]Message) {
	sessionID := uuid.New()
	return []Session{{ID: sessionID, Title: "work"}}, map[uuid.UUID][]Message{
		sessionID: {{Role: MessageUser, Content: "帮我整理周报"}, {Role: MessageAssistant, Content: "可以"}},
	}
}

func TestSkillLLMExtractorPolicySelection(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		configureUser bool
		expectedModel string
	}{
		{name: "user policy takes precedence", configureUser: true, expectedModel: "user-model"},
		{name: "platform default when user policy unset", configureUser: false, expectedModel: "default-model"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, requests := newSkillLLMServer(t, nil)
			providerID := uuid.New()
			userModel := skillLLMModel(providerID, "user-model")
			defaultModel := skillLLMModel(providerID, "default-model")
			userPolicyID := uuid.New()
			defaultPolicyID := uuid.New()
			policies := &fakeSkillPolicyRepository{
				policies: map[uuid.UUID]ModelPolicy{
					userPolicyID:    skillLLMPolicy(userPolicyID, PolicyCandidate{ModelID: userModel.ID, Priority: 1}),
					defaultPolicyID: skillLLMPolicy(defaultPolicyID, PolicyCandidate{ModelID: defaultModel.ID, Priority: 1}),
				},
				models: map[uuid.UUID]Model{userModel.ID: userModel, defaultModel.ID: defaultModel},
			}
			providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
				providerID: {provider: Provider{ID: providerID, Status: ProviderEnabled, BaseURL: server.URL}},
			}}
			userID := uuid.New()
			version := 1
			settings := UserSkillSynthesisSettings{UserID: userID, Enabled: true}
			if testCase.configureUser {
				settings.ModelPolicyID = &userPolicyID
				settings.ModelPolicyVersion = &version
			}
			settingsRepo := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{userID: settings}}
			usage := &fakeSkillUsageRecorder{}
			extractor, err := NewSkillLLMExtractor(settingsRepo, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{
				DefaultModelPolicyID: &defaultPolicyID, DefaultModelPolicyVersion: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			extractor.WithUsageRecorder(usage)
			sessions, messages := skillLLMConversations()
			candidate, err := extractor.Extract(context.Background(), userID, sessions, messages)
			if err != nil || !candidate.ShouldCreate {
				t.Fatalf("Extract() = %#v, %v", candidate, err)
			}
			if len(requests.models) != 1 || requests.models[0] != testCase.expectedModel {
				t.Fatalf("expected only %s, got %#v", testCase.expectedModel, requests.models)
			}
			if len(usage.records) != 1 || usage.records[0].providerID == nil || *usage.records[0].providerID != providerID {
				t.Fatalf("expected usage attributed to provider, got %#v", usage.records)
			}
			if usage.records[0].inputTokens != 100 || usage.records[0].outputTokens != 20 || usage.records[0].cost <= 0 {
				t.Fatalf("unexpected usage record: %#v", usage.records[0])
			}
		})
	}
}

func TestSkillLLMExtractorSkipsDisabledOrUnconfiguredUser(t *testing.T) {
	userID := uuid.New()
	for _, testCase := range []struct {
		name     string
		settings UserSkillSynthesisSettings
	}{
		{name: "disabled", settings: UserSkillSynthesisSettings{UserID: userID, Enabled: false}},
		{name: "no user or default policy", settings: UserSkillSynthesisSettings{UserID: userID, Enabled: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{userID: testCase.settings}}
			extractor, err := NewSkillLLMExtractor(settings, &fakeSkillPolicyRepository{}, &fakeSkillProviderCredentials{}, skillLLMOpener{}, UserSkillLLMConfig{})
			if err != nil {
				t.Fatal(err)
			}
			sessions, messages := skillLLMConversations()
			if _, err := extractor.Extract(context.Background(), userID, sessions, messages); !errors.Is(err, ErrUserSkillSynthesisUnavailable) {
				t.Fatalf("expected permanent unavailable error, got %v", err)
			}
		})
	}
}

func TestSkillLLMExtractorDoesNotFallbackWhenUserPolicyIsStale(t *testing.T) {
	server, requests := newSkillLLMServer(t, nil)
	providerID := uuid.New()
	userModel := skillLLMModel(providerID, "user-model")
	defaultModel := skillLLMModel(providerID, "default-model")
	userPolicyID := uuid.New()
	defaultPolicyID := uuid.New()
	userPolicy := skillLLMPolicy(userPolicyID, PolicyCandidate{ModelID: userModel.ID, Priority: 1})
	userPolicy.Version = 2
	policies := &fakeSkillPolicyRepository{
		policies: map[uuid.UUID]ModelPolicy{
			userPolicyID: userPolicy, defaultPolicyID: skillLLMPolicy(defaultPolicyID, PolicyCandidate{ModelID: defaultModel.ID, Priority: 1}),
		},
		models: map[uuid.UUID]Model{userModel.ID: userModel, defaultModel.ID: defaultModel},
	}
	providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
		providerID: {provider: Provider{ID: providerID, Status: ProviderEnabled, BaseURL: server.URL}},
	}}
	userID := uuid.New()
	staleVersion := 1
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		userID: {UserID: userID, Enabled: true, ModelPolicyID: &userPolicyID, ModelPolicyVersion: &staleVersion},
	}}
	extractor, err := NewSkillLLMExtractor(settings, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{DefaultModelPolicyID: &defaultPolicyID, DefaultModelPolicyVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	sessions, messages := skillLLMConversations()
	if _, err := extractor.Extract(context.Background(), userID, sessions, messages); !errors.Is(err, ErrUserSkillSynthesisUnavailable) {
		t.Fatalf("expected stale user policy to be unavailable, got %v", err)
	}
	if len(requests.models) != 0 || len(policies.getPolicyCalls) != 1 || policies.getPolicyCalls[0] != userPolicyID {
		t.Fatalf("stale user policy must not fall back to platform default: requests=%#v policies=%#v", requests.models, policies.getPolicyCalls)
	}
}

func TestSkillLLMExtractorUsesCandidatePriorityAndRecordsActualProvider(t *testing.T) {
	server, requests := newSkillLLMServer(t, map[string]int{"first-model": http.StatusInternalServerError})
	firstProviderID := uuid.New()
	secondProviderID := uuid.New()
	firstModel := skillLLMModel(firstProviderID, "first-model")
	secondModel := skillLLMModel(secondProviderID, "second-model")
	policyID := uuid.New()
	policy := skillLLMPolicy(policyID,
		PolicyCandidate{ModelID: secondModel.ID, Priority: 2},
		PolicyCandidate{ModelID: firstModel.ID, Priority: 1},
	)
	policies := &fakeSkillPolicyRepository{
		policies: map[uuid.UUID]ModelPolicy{policyID: policy},
		models:   map[uuid.UUID]Model{firstModel.ID: firstModel, secondModel.ID: secondModel},
	}
	providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
		firstProviderID:  {provider: Provider{ID: firstProviderID, Status: ProviderEnabled, BaseURL: server.URL}},
		secondProviderID: {provider: Provider{ID: secondProviderID, Status: ProviderEnabled, BaseURL: server.URL}},
	}}
	userID := uuid.New()
	version := 1
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		userID: {UserID: userID, Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version},
	}}
	usage := &fakeSkillUsageRecorder{}
	extractor, err := NewSkillLLMExtractor(settings, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	extractor.WithUsageRecorder(usage)
	sessions, messages := skillLLMConversations()
	if _, err := extractor.Extract(context.Background(), userID, sessions, messages); err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(requests.models) != 2 || requests.models[0] != "first-model" || requests.models[1] != "second-model" {
		t.Fatalf("unexpected candidate order: %#v", requests.models)
	}
	if len(usage.records) != 1 || usage.records[0].providerID == nil || *usage.records[0].providerID != secondProviderID {
		t.Fatalf("expected usage for successful second provider, got %#v", usage.records)
	}
}

func TestSkillLLMExtractorReturnsTemporaryErrorWhenAllCandidatesFail(t *testing.T) {
	server, _ := newSkillLLMServer(t, map[string]int{"first-model": http.StatusInternalServerError})
	providerID := uuid.New()
	catalogModel := skillLLMModel(providerID, "first-model")
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
	if _, err := extractor.Extract(context.Background(), userID, sessions, messages); err == nil || errors.Is(err, ErrUserSkillSynthesisUnavailable) {
		t.Fatalf("temporary provider failure must remain retryable, got %v", err)
	}
}

func TestSkillLLMExtractorValidatesConfigAndSettingsReadError(t *testing.T) {
	policyID := uuid.New()
	nilPolicyID := uuid.Nil
	for _, testCase := range []struct {
		name   string
		config UserSkillLLMConfig
	}{
		{name: "default policy missing version", config: UserSkillLLMConfig{DefaultModelPolicyID: &policyID}},
		{name: "default policy missing id", config: UserSkillLLMConfig{DefaultModelPolicyVersion: 1}},
		{name: "default policy nil id", config: UserSkillLLMConfig{DefaultModelPolicyID: &nilPolicyID, DefaultModelPolicyVersion: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewSkillLLMExtractor(&fakeSkillSynthesisSettingsStore{}, &fakeSkillPolicyRepository{}, &fakeSkillProviderCredentials{}, skillLLMOpener{}, testCase.config); err == nil {
				t.Fatal("expected invalid default policy configuration to be rejected")
			}
		})
	}

	readErr := errors.New("settings read failed")
	userID := uuid.New()
	settings := &fakeSkillSynthesisSettingsStore{getErr: readErr}
	extractor, err := NewSkillLLMExtractor(settings, &fakeSkillPolicyRepository{}, &fakeSkillProviderCredentials{}, skillLLMOpener{}, UserSkillLLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate, err := extractor.Extract(context.Background(), userID, nil, nil); err != nil || candidate.ShouldCreate {
		t.Fatalf("empty conversations should not require settings or a model: %#v, %v", candidate, err)
	}
	sessions, messages := skillLLMConversations()
	if _, err := extractor.Extract(context.Background(), userID, sessions, messages); !errors.Is(err, readErr) {
		t.Fatalf("settings read error must remain retryable, got %v", err)
	}
}

func TestSkillLLMExtractorRejectsUnavailableCandidates(t *testing.T) {
	userID := uuid.New()
	policyID := uuid.New()
	providerID := uuid.New()
	disabledModel := skillLLMModel(providerID, "disabled-model")
	disabledModel.Status = ModelDisabled
	version := 1
	for _, testCase := range []struct {
		name     string
		policy   ModelPolicy
		models   map[uuid.UUID]Model
		provider Provider
	}{
		{name: "policy without candidates", policy: skillLLMPolicy(policyID)},
		{name: "disabled model", policy: skillLLMPolicy(policyID, PolicyCandidate{ModelID: disabledModel.ID, Priority: 1}), models: map[uuid.UUID]Model{disabledModel.ID: disabledModel}},
		{name: "disabled provider", policy: skillLLMPolicy(policyID, PolicyCandidate{ModelID: disabledModel.ID, Priority: 1}), models: map[uuid.UUID]Model{disabledModel.ID: func() Model {
			model := disabledModel
			model.Status = ModelEnabled
			return model
		}()}, provider: Provider{ID: providerID, Status: ProviderDisabled, BaseURL: "http://unused"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
				userID: {UserID: userID, Enabled: true, ModelPolicyID: &policyID, ModelPolicyVersion: &version},
			}}
			policies := &fakeSkillPolicyRepository{policies: map[uuid.UUID]ModelPolicy{policyID: testCase.policy}, models: testCase.models}
			providers := &fakeSkillProviderCredentials{items: map[uuid.UUID]fakeSkillProviderCredential{
				providerID: {provider: testCase.provider},
			}}
			extractor, err := NewSkillLLMExtractor(settings, policies, providers, skillLLMOpener{}, UserSkillLLMConfig{})
			if err != nil {
				t.Fatal(err)
			}
			sessions, messages := skillLLMConversations()
			if _, err := extractor.Extract(context.Background(), userID, sessions, messages); !errors.Is(err, ErrUserSkillSynthesisUnavailable) {
				t.Fatalf("expected unavailable candidate error, got %v", err)
			}
		})
	}
}

func TestBuildSkillExtractionPrompt(t *testing.T) {
	sessions := []Session{{ID: uuid.New(), Title: "work"}, {ID: uuid.New(), Title: "personal"}}
	messagesBySession := map[uuid.UUID][]Message{
		sessions[0].ID: {
			{Role: MessageUser, Content: "帮我写周报"},
			{Role: MessageAssistant, Content: "好的，请提供本周内容"},
		},
	}
	system, user, err := buildSkillExtractionPrompt(sessions, messagesBySession, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(system, "should_create") {
		t.Fatal("system prompt missing should_create instruction")
	}
	if !strings.Contains(user, "work") {
		t.Fatal("user prompt missing session title")
	}
	if !strings.Contains(user, "帮我写周报") {
		t.Fatal("user prompt missing message content")
	}
}

func TestBuildSkillExtractionPromptTruncatesLongMessage(t *testing.T) {
	sessions := []Session{{ID: uuid.New()}}
	longContent := strings.Repeat("a", 200)
	messagesBySession := map[uuid.UUID][]Message{
		sessions[0].ID: {{Role: MessageUser, Content: longContent}},
	}
	_, user, err := buildSkillExtractionPrompt(sessions, messagesBySession, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(user, "已截断") {
		t.Fatal("expected truncation marker")
	}
}

func TestBuildSkillExtractionPromptEmpty(t *testing.T) {
	if _, _, err := buildSkillExtractionPrompt(nil, nil, 1024); err == nil {
		t.Fatal("expected error for empty conversations")
	}
}
