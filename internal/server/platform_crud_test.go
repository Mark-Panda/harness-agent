package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// platformCRUDFixture 汇总平台 CRUD 测试所需的 Mock 仓储与 Handler。
type platformCRUDFixture struct {
	handler   http.Handler
	auth      *fakeAuthApplication
	principal biz.Principal
	platform  *service.PlatformService
	providers *mocks.MockProviderRepository
	cipher    *mocks.MockCredentialCipher
	models    *mocks.MockModelRepository
	policies  *mocks.MockPolicyRepository
	agents    *mocks.MockAgentRepository
	sessions  *mocks.MockSessionRepository
}

// newPlatformCRUDFixture 复用既有测试的装配方式：gomock 仓储 + 真实 biz/service + fakeAuth。
func newPlatformCRUDFixture(t *testing.T, role biz.Role) *platformCRUDFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	fixture := &platformCRUDFixture{
		providers: mocks.NewMockProviderRepository(ctrl),
		cipher:    mocks.NewMockCredentialCipher(ctrl),
		models:    mocks.NewMockModelRepository(ctrl),
		policies:  mocks.NewMockPolicyRepository(ctrl),
		agents:    mocks.NewMockAgentRepository(ctrl),
		sessions:  mocks.NewMockSessionRepository(ctrl),
	}
	providers, err := biz.NewProviderService(fixture.providers, fixture.cipher)
	if err != nil {
		t.Fatal(err)
	}
	models, err := biz.NewModelService(fixture.models)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := biz.NewPolicyService(fixture.policies)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := biz.NewAgentService(fixture.agents, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := biz.NewSessionService(fixture.sessions, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := service.NewPlatformService(providers, models, policies, &biz.SkillService{}, &biz.MCPService{}, agents, sessions, &biz.RunService{}, &biz.IdempotencyService{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.principal = biz.Principal{UserID: uuid.New(), Role: role}
	fixture.auth = &fakeAuthApplication{principal: fixture.principal}
	fixture.platform = platform
	fixture.handler = NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, fixture.auth, nil, platform)
	return fixture
}

// do 发起带 Bearer Token 的请求并返回响应记录器。
func (f *platformCRUDFixture) do(method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestProviderCRUDEndpoints(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleAdmin)
	adminID := fixture.principal.UserID
	providerID := uuid.New()

	// 列表：返回全部供应商。
	fixture.providers.EXPECT().ListProviders(gomock.Any()).Return([]biz.Provider{
		{ID: providerID, Name: "primary", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled},
	}, nil)
	recorder := fixture.do(http.MethodGet, "/api/v1/providers", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"primary"`) {
		t.Fatalf("list providers returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取单个供应商。
	fixture.providers.EXPECT().GetProvider(gomock.Any(), providerID).Return(biz.Provider{
		ID: providerID, Name: "primary", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/providers/"+providerID.String(), "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"`+providerID.String()+`"`) {
		t.Fatalf("get provider returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取不存在：领域错误映射为 404。
	fixture.providers.EXPECT().GetProvider(gomock.Any(), providerID).Return(biz.Provider{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/providers/"+providerID.String(), "")
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "resource_not_found") {
		t.Fatalf("get missing provider returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 路径 ID 非法：400。
	recorder = fixture.do(http.MethodGet, "/api/v1/providers/not-a-uuid", "")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_resource_id") {
		t.Fatalf("invalid provider id returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 更新且不轮换密钥：credential 必须为 nil，cipher 不应被调用。
	fixture.providers.EXPECT().UpdateProvider(gomock.Any(), adminID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.Provider, error) {
			if credential != nil {
				t.Fatalf("credential must be nil when api_key is empty: %#v", credential)
			}
			return provider, nil
		},
	)
	recorder = fixture.do(http.MethodPut, "/api/v1/providers/"+providerID.String(),
		`{"name":"primary","base_url":"https://models.example.com","status":"enabled"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"primary"`) {
		t.Fatalf("update provider returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 更新并轮换密钥：先加密再落库，响应包含新 key_version。
	fixture.cipher.EXPECT().Seal([]byte("rotated-secret")).Return([]byte("enc2"), "v2", nil)
	fixture.providers.EXPECT().UpdateProvider(gomock.Any(), adminID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, credential *biz.ProviderCredential, _ biz.OutboxEvent) (biz.Provider, error) {
			if credential == nil || credential.KeyVersion != "v2" || provider.KeyVersion != "v2" {
				t.Fatalf("rotation lost credential: %#v %#v", provider, credential)
			}
			return provider, nil
		},
	)
	recorder = fixture.do(http.MethodPut, "/api/v1/providers/"+providerID.String(),
		`{"name":"primary","base_url":"https://models.example.com","api_key":"rotated-secret","status":"enabled"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"key_version":"v2"`) {
		t.Fatalf("rotate provider key returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 删除：204 且无响应体。
	fixture.providers.EXPECT().DeleteProvider(gomock.Any(), adminID, providerID, gomock.Any()).Return(nil)
	recorder = fixture.do(http.MethodDelete, "/api/v1/providers/"+providerID.String(), "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete provider returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestModelCRUDEndpoints(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleAdmin)
	adminID := fixture.principal.UserID
	providerID := uuid.New()
	modelID := uuid.New()

	// 创建已启用模型：需要核验元数据且会校验供应商处于启用状态。
	fixture.models.EXPECT().GetProvider(gomock.Any(), providerID).Return(biz.Provider{ID: providerID, Status: biz.ProviderEnabled}, nil)
	fixture.models.EXPECT().CreateModel(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, model biz.Model, _ biz.OutboxEvent) (biz.Model, error) {
			if model.ContextWindowTokens != 1_000_000 || model.Status != biz.ModelEnabled {
				t.Fatalf("model admission dropped: %#v", model)
			}
			return model, nil
		},
	)
	recorder := fixture.do(http.MethodPost, "/api/v1/models",
		`{"provider_id":"`+providerID.String()+`","remote_model_name":"gpt-x","kind":"chat","context_window_tokens":1000000,"max_output_tokens":4096,"status":"enabled","verification_source":"admin","verified_at":"2026-01-01T00:00:00Z"}`)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"remote_model_name":"gpt-x"`) {
		t.Fatalf("create model returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表：无过滤条件。
	fixture.models.EXPECT().ListModels(gomock.Any(), nil).Return([]biz.Model{
		{ID: modelID, ProviderID: providerID, RemoteModelName: "gpt-x", Kind: biz.ModelKindChat, ContextWindowTokens: 1_000_000, Status: biz.ModelEnabled},
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/models", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"remote_model_name":"gpt-x"`) {
		t.Fatalf("list models returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表：provider_id 过滤参数透传到仓储。
	fixture.models.EXPECT().ListModels(gomock.Any(), &providerID).Return([]biz.Model{}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/models?provider_id="+providerID.String(), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list models by provider returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表：provider_id 非法时 400。
	recorder = fixture.do(http.MethodGet, "/api/v1/models?provider_id=bad", "")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_provider_id") {
		t.Fatalf("invalid provider_id filter returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取单个模型。
	fixture.models.EXPECT().GetModel(gomock.Any(), modelID).Return(biz.Model{
		ID: modelID, ProviderID: providerID, RemoteModelName: "gpt-x", Kind: biz.ModelKindChat, ContextWindowTokens: 1_000_000, Status: biz.ModelEnabled,
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/models/"+modelID.String(), "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"`+modelID.String()+`"`) {
		t.Fatalf("get model returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取不存在：404。
	fixture.models.EXPECT().GetModel(gomock.Any(), modelID).Return(biz.Model{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/models/"+modelID.String(), "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("get missing model returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 更新为停用状态：不触发供应商一致性校验。
	fixture.models.EXPECT().UpdateModel(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, model biz.Model, _ biz.OutboxEvent) (biz.Model, error) {
			if model.ID != modelID || model.Status != biz.ModelDisabled {
				t.Fatalf("update lost identity or status: %#v", model)
			}
			return model, nil
		},
	)
	recorder = fixture.do(http.MethodPut, "/api/v1/models/"+modelID.String(),
		`{"provider_id":"`+providerID.String()+`","remote_model_name":"gpt-x","kind":"chat","context_window_tokens":1000000,"max_output_tokens":4096,"status":"disabled"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"disabled"`) {
		t.Fatalf("update model returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 创建非法 kind：领域校验 400，仓储不被调用。
	recorder = fixture.do(http.MethodPost, "/api/v1/models",
		`{"provider_id":"`+providerID.String()+`","remote_model_name":"bad","kind":"bogus","context_window_tokens":1000000,"max_output_tokens":4096}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_configuration") {
		t.Fatalf("invalid kind model returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 删除：204。
	fixture.models.EXPECT().DeleteModel(gomock.Any(), adminID, modelID, gomock.Any()).Return(nil)
	recorder = fixture.do(http.MethodDelete, "/api/v1/models/"+modelID.String(), "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete model returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPolicyCRUDEndpoints(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleAdmin)
	adminID := fixture.principal.UserID
	policyID := uuid.New()
	modelID := uuid.New()
	verifiedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 创建草稿策略：候选存在性延迟到发布时校验，草稿创建不查模型目录。
	fixture.policies.EXPECT().CreatePolicy(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, policy biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if policy.Version != 1 || policy.Status != biz.PolicyDraft || len(policy.Candidates) != 1 {
				t.Fatalf("policy draft invalid: %#v", policy)
			}
			return policy, nil
		},
	)
	recorder := fixture.do(http.MethodPost, "/api/v1/model-policies",
		`{"name":"routing","min_context_tokens":1000000,"candidates":[{"model_id":"`+modelID.String()+`","priority":1}]}`)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"status":"draft"`) {
		t.Fatalf("create policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表。
	fixture.policies.EXPECT().ListPolicies(gomock.Any()).Return([]biz.ModelPolicy{
		{ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyDraft},
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/model-policies", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"routing"`) {
		t.Fatalf("list policies returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取单个策略。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyDraft,
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/model-policies/"+policyID.String(), "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"version":1`) {
		t.Fatalf("get policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取不存在：404。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/model-policies/"+policyID.String(), "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("get missing policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 更新草稿：先读当前策略，名称与版本由服务端保持。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyDraft, MinContextTokens: 1_000_000,
	}, nil)
	fixture.policies.EXPECT().UpdatePolicy(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, policy biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if policy.ID != policyID || policy.Name != "routing" || policy.Version != 1 {
				t.Fatalf("update lost server-kept fields: %#v", policy)
			}
			return policy, nil
		},
	)
	recorder = fixture.do(http.MethodPut, "/api/v1/model-policies/"+policyID.String(),
		`{"name":"renamed-ignored","min_context_tokens":1000000}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"routing"`) {
		t.Fatalf("update policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 更新已发布策略：409 冲突。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyPublished,
	}, nil)
	recorder = fixture.do(http.MethodPut, "/api/v1/model-policies/"+policyID.String(),
		`{"name":"routing","min_context_tokens":1000000}`)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resource_conflict") {
		t.Fatalf("update published policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 创建新版本：复制源策略并递增版本号。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyPublished, MinContextTokens: 1_000_000,
	}, nil)
	fixture.policies.EXPECT().NextPolicyVersion(gomock.Any(), "routing").Return(2, nil)
	fixture.policies.EXPECT().CreatePolicy(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, policy biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if policy.Version != 2 || policy.Status != biz.PolicyDraft || policy.ID == policyID {
				t.Fatalf("new version invalid: %#v", policy)
			}
			return policy, nil
		},
	)
	recorder = fixture.do(http.MethodPost, "/api/v1/model-policies/"+policyID.String()+"/versions", "")
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"version":2`) {
		t.Fatalf("create policy version returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 发布草稿：候选模型必须是已启用、已核验的 1M+ 生成模型。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyDraft, MinContextTokens: 1_000_000,
		Candidates: []biz.PolicyCandidate{{ModelID: modelID, Priority: 1}},
	}, nil)
	fixture.policies.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{
		{ID: modelID, Kind: biz.ModelKindChat, Status: biz.ModelEnabled, ContextWindowTokens: 1_000_000, VerificationSource: "admin", VerifiedAt: &verifiedAt},
	}, nil)
	fixture.policies.EXPECT().UpdatePolicy(gomock.Any(), adminID, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, policy biz.ModelPolicy, _ biz.OutboxEvent) (biz.ModelPolicy, error) {
			if policy.Status != biz.PolicyPublished {
				t.Fatalf("policy not published: %#v", policy)
			}
			return policy, nil
		},
	)
	recorder = fixture.do(http.MethodPost, "/api/v1/model-policies/"+policyID.String()+"/publish", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"published"`) {
		t.Fatalf("publish policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 发布非草稿：409。
	fixture.policies.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyPublished,
	}, nil)
	recorder = fixture.do(http.MethodPost, "/api/v1/model-policies/"+policyID.String()+"/publish", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("publish non-draft policy returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 删除：204。
	fixture.policies.EXPECT().DeletePolicy(gomock.Any(), adminID, policyID, gomock.Any()).Return(nil)
	recorder = fixture.do(http.MethodDelete, "/api/v1/model-policies/"+policyID.String(), "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete policy returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAgentCRUDEndpoints(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleUser)
	userID := fixture.principal.UserID
	agentID := uuid.New()
	policyID := uuid.New()
	policyVersion := 1

	// 创建草稿：配额计数后落库，user_id 只能来自 Token。
	fixture.agents.EXPECT().CountAgents(gomock.Any(), userID).Return(int64(0), nil)
	fixture.agents.EXPECT().CreateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) {
			if agent.UserID != userID || agent.Version != 1 || agent.Status != biz.AgentDraft {
				t.Fatalf("agent draft invalid: %#v", agent)
			}
			return agent, nil
		},
	)
	recorder := fixture.do(http.MethodPost, "/api/v1/agents", `{"name":"assistant","mode":"react"}`)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"name":"assistant"`) {
		t.Fatalf("create agent returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 创建非法模式：领域校验 400（配额计数仍会执行）。
	fixture.agents.EXPECT().CountAgents(gomock.Any(), userID).Return(int64(0), nil)
	recorder = fixture.do(http.MethodPost, "/api/v1/agents", `{"name":"assistant","mode":"bogus"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_configuration") {
		t.Fatalf("invalid mode agent returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表：默认排除已归档版本。
	fixture.agents.EXPECT().ListAgents(gomock.Any(), userID, false, 20, "").Return(biz.Page[biz.Agent]{
		Items: []biz.Agent{{ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentDraft}},
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/agents", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"assistant"`) {
		t.Fatalf("list agents returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取单个 Agent：草稿会额外读取可继承的自动技能。
	draft := biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentDraft,
	}
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(draft, nil).Times(2)
	fixture.agents.EXPECT().ListPublishedAgentIDsByName(gomock.Any(), userID, "assistant").Return(nil, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/agents/"+agentID.String(), "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"`+agentID.String()+`"`) ||
		!strings.Contains(recorder.Body.String(), `"inheritable_user_skills"`) {
		t.Fatalf("get agent returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取不存在：404。
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(biz.Agent{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/agents/"+agentID.String(), "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("get missing agent returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 创建新版本：读源版本、配额计数、取下一版本号后落库。
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentDraft,
	}, nil)
	fixture.agents.EXPECT().CountAgents(gomock.Any(), userID).Return(int64(1), nil)
	fixture.agents.EXPECT().NextAgentVersion(gomock.Any(), userID, "assistant").Return(2, nil)
	fixture.agents.EXPECT().CreateAgentVersion(gomock.Any(), gomock.Any(), agentID, gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ uuid.UUID, _ biz.OutboxEvent) (biz.Agent, error) {
			if agent.Version != 2 || agent.ID == agentID || agent.Status != biz.AgentDraft {
				t.Fatalf("new agent version invalid: %#v", agent)
			}
			return agent, nil
		},
	)
	recorder = fixture.do(http.MethodPost, "/api/v1/agents/"+agentID.String()+"/versions", "")
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"version":2`) {
		t.Fatalf("create agent version returned %d %s", recorder.Code, recorder.Body.String())
	}
	// 发布：校验策略已发布并固化配置快照；validate 与 buildSnapshot 各读一次策略。
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentDraft,
		ModelPolicyID: &policyID, ModelPolicyVersion: &policyVersion,
	}, nil)
	fixture.agents.EXPECT().GetPolicy(gomock.Any(), policyID).Return(biz.ModelPolicy{
		ID: policyID, Name: "routing", Version: 1, Status: biz.PolicyPublished, MinContextTokens: 1_000_000,
	}, nil).Times(2)
	fixture.agents.EXPECT().ListPublishedAgentIDsByName(gomock.Any(), userID, "assistant").Return(nil, nil)
	fixture.agents.EXPECT().ListModelsByIDs(gomock.Any(), gomock.Any()).Return([]biz.Model{}, nil)
	fixture.agents.EXPECT().ListSkillsByIDs(gomock.Any(), gomock.Any()).Return([]biz.Skill{}, nil)
	fixture.agents.EXPECT().ListMCPsByIDs(gomock.Any(), gomock.Any()).Return([]biz.MCP{}, nil)
	fixture.agents.EXPECT().UpdateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) {
			if agent.Status != biz.AgentPublished || agent.ConfigSnapshot == nil {
				t.Fatalf("publish lost snapshot: %#v", agent)
			}
			return agent, nil
		},
	)
	recorder = fixture.do(http.MethodPost, "/api/v1/agents/"+agentID.String()+"/publish", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"published"`) {
		t.Fatalf("publish agent returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 发布缺少策略的草稿：400。
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentDraft,
	}, nil)
	fixture.agents.EXPECT().ListPublishedAgentIDsByName(gomock.Any(), userID, "assistant").Return(nil, nil)
	recorder = fixture.do(http.MethodPost, "/api/v1/agents/"+agentID.String()+"/publish", "")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_configuration") {
		t.Fatalf("publish policy-less agent returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionListGetArchiveAndMessages(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleUser)
	userID := fixture.principal.UserID
	sessionID := uuid.New()
	messageID := uuid.New()

	// 列表：游标分页。
	fixture.sessions.EXPECT().ListSessions(gomock.Any(), userID, 20, "").Return(biz.Page[biz.Session]{
		Items: []biz.Session{{ID: sessionID, UserID: userID, Title: "chat", Status: biz.SessionActive}},
	}, nil)
	recorder := fixture.do(http.MethodGet, "/api/v1/sessions", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"title":"chat"`) {
		t.Fatalf("list sessions returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 列表分页参数非法：400。
	recorder = fixture.do(http.MethodGet, "/api/v1/sessions?limit=0", "")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_page_limit") {
		t.Fatalf("invalid page limit returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取单个会话。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: userID, Title: "chat", Status: biz.SessionActive,
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/sessions/"+sessionID.String(), "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"`+sessionID.String()+`"`) {
		t.Fatalf("get session returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 读取不存在或跨用户：404。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/sessions/"+sessionID.String(), "")
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "resource_not_found") {
		t.Fatalf("get missing session returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 归档活跃会话：乐观锁版本号递增。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: userID, Title: "chat", Status: biz.SessionActive, Version: 1,
	}, nil)
	fixture.sessions.EXPECT().ArchiveSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.Status != biz.SessionArchived || session.Version != 2 {
				t.Fatalf("archive mutation invalid: %#v", session)
			}
			return session, nil
		},
	)
	recorder = fixture.do(http.MethodPost, "/api/v1/sessions/"+sessionID.String()+"/archive", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"archived"`) {
		t.Fatalf("archive session returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 归档非活跃会话：409 冲突。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: userID, Title: "chat", Status: biz.SessionArchived, Version: 2,
	}, nil)
	recorder = fixture.do(http.MethodPost, "/api/v1/sessions/"+sessionID.String()+"/archive", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("archive inactive session returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 消息列表：先校验会话归属，再分页返回。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: userID, Title: "chat", Status: biz.SessionActive,
	}, nil)
	fixture.sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 20, "").Return(biz.Page[biz.Message]{
		Items: []biz.Message{{ID: messageID, SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "hello"}},
	}, nil)
	recorder = fixture.do(http.MethodGet, "/api/v1/sessions/"+sessionID.String()+"/messages", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("list messages returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 消息列表：会话不存在时 404。
	fixture.sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).Return(biz.Session{}, biz.ErrResourceNotFound)
	recorder = fixture.do(http.MethodGet, "/api/v1/sessions/"+sessionID.String()+"/messages", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("list messages of missing session returned %d %s", recorder.Code, recorder.Body.String())
	}
}
