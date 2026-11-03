package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

func TestPlatformAPIAuthenticationAdmissionAndSecretRedaction(t *testing.T) {
	ctrl := gomock.NewController(t)
	providerRepo := mocks.NewMockProviderRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	modelRepo := mocks.NewMockModelRepository(ctrl)
	policyRepo := mocks.NewMockPolicyRepository(ctrl)
	agentRepo := mocks.NewMockAgentRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	providers, _ := biz.NewProviderService(providerRepo, cipher)
	models, _ := biz.NewModelService(modelRepo)
	policies, _ := biz.NewPolicyService(policyRepo)
	agents, _ := biz.NewAgentService(agentRepo, biz.QuotaLimits{})
	sessions, _ := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	platform, _ := service.NewPlatformService(providers, models, policies, &biz.SkillService{}, &biz.MCPService{}, agents, sessions, &biz.RunService{}, &biz.IdempotencyService{})
	admin := biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}
	auth := &fakeAuthApplication{principal: admin}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)

	cipher.EXPECT().Seal([]byte("top-secret")).Return([]byte("encrypted"), "v1", nil)
	providerRepo.EXPECT().CreateProvider(gomock.Any(), admin.UserID, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, provider biz.Provider, _ biz.ProviderCredential, _ biz.OutboxEvent) (biz.Provider, error) {
			provider.KeyVersion = "v1"
			return provider, nil
		},
	)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers", bytes.NewBufferString(`{"name":"primary","base_url":"https://models.example.com","api_key":"top-secret"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || strings.Contains(recorder.Body.String(), "top-secret") || strings.Contains(recorder.Body.String(), "api_key") || !strings.Contains(recorder.Body.String(), "key_version") {
		t.Fatalf("provider create returned %d %s", recorder.Code, recorder.Body.String())
	}

	auth.principal = biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/providers", bytes.NewBufferString(`{"name":"forbidden","base_url":"https://models.example.com","api_key":"secret"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin provider create returned %d %s", recorder.Code, recorder.Body.String())
	}

	auth.principal = admin
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/models", bytes.NewBufferString(`{"provider_id":"`+uuid.NewString()+`","remote_model_name":"small","kind":"chat","context_window_tokens":128000,"max_output_tokens":4096,"status":"disabled"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_configuration") {
		t.Fatalf("low-context model returned %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionAndMessageAPIUsesAuthenticatedIdentity(t *testing.T) {
	ctrl := gomock.NewController(t)
	providerRepo := mocks.NewMockProviderRepository(ctrl)
	cipher := mocks.NewMockCredentialCipher(ctrl)
	modelRepo := mocks.NewMockModelRepository(ctrl)
	policyRepo := mocks.NewMockPolicyRepository(ctrl)
	agentRepo := mocks.NewMockAgentRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	providers, _ := biz.NewProviderService(providerRepo, cipher)
	models, _ := biz.NewModelService(modelRepo)
	policies, _ := biz.NewPolicyService(policyRepo)
	agents, _ := biz.NewAgentService(agentRepo, biz.QuotaLimits{})
	sessions, _ := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	platform, _ := service.NewPlatformService(providers, models, policies, &biz.SkillService{}, &biz.MCPService{}, agents, sessions, &biz.RunService{}, &biz.IdempotencyService{})
	// 飞书解绑路由需在构造 Handler 前装配，否则不会注册。
	conversations := mocks.NewMockFeishuConversationRepository(ctrl)
	bindings, err := biz.NewFeishuBindingService(conversations, sessionRepo)
	if err != nil {
		t.Fatal(err)
	}
	platform.WithFeishuBindings(bindings)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	auth := &fakeAuthApplication{principal: principal}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)

	sessionRepo.EXPECT().CountSessions(gomock.Any(), principal.UserID).Return(int64(0), nil)
	sessionRepo.EXPECT().CreateSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.UserID != principal.UserID {
				t.Fatalf("session accepted a different user: %#v", session)
			}
			return session, nil
		},
	)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"title":"private"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("session create returned %d %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&created); err != nil || created.ID == uuid.Nil {
		t.Fatalf("decode session: %v %#v", err, created)
	}

	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, created.ID).Return(biz.Session{ID: created.ID, UserID: principal.UserID, Status: biz.SessionActive}, nil)
	sessionRepo.EXPECT().AppendMessage(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			return message, nil
		},
	)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID.String()+"/messages", bytes.NewBufferString(`{"content":"hello"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"role":"user"`) {
		t.Fatalf("message create returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 记忆策略接口只能作用于当前 Token 用户的会话，并返回最新策略。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, created.ID).Return(biz.Session{
		ID: created.ID, UserID: principal.UserID, Status: biz.SessionActive,
		MemoryPolicy: biz.SessionMemoryPrivate, Version: 1,
	}, nil)
	sessionRepo.EXPECT().UpdateSessionMemoryPolicy(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.UserID != principal.UserID || session.MemoryPolicy != biz.SessionMemoryDisabled || session.Version != 2 {
				t.Fatalf("memory policy accepted a different mutation: %#v", session)
			}
			return session, nil
		},
	)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPut, "/api/v1/sessions/"+created.ID.String()+"/memory-policy", bytes.NewBufferString(`{"memory_policy":"disabled"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"memory_policy":"disabled"`) {
		t.Fatalf("memory policy update returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 飞书话题解绑：按当前 Token 用户校验归属，并返回受影响绑定数。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, created.ID).Return(biz.Session{
		ID: created.ID, UserID: principal.UserID, Status: biz.SessionActive,
	}, nil)
	conversations.EXPECT().ReleaseConversation(gomock.Any(), principal.UserID, created.ID, biz.FeishuBindingUnbound, gomock.Any()).Return(1, nil)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID.String()+"/feishu-unbind", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"released":1`) {
		t.Fatalf("feishu unbind returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 飞书话题关闭/归档：复用归属校验，映射到对应绑定状态。
	for _, testCase := range []struct {
		path   string
		status string
	}{
		{path: "/feishu-close", status: biz.FeishuBindingClosed},
		{path: "/feishu-archive", status: biz.FeishuBindingArchived},
	} {
		sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, created.ID).Return(biz.Session{
			ID: created.ID, UserID: principal.UserID, Status: biz.SessionActive,
		}, nil)
		conversations.EXPECT().ReleaseConversation(gomock.Any(), principal.UserID, created.ID, testCase.status, gomock.Any()).Return(1, nil)
		recorder = httptest.NewRecorder()
		request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID.String()+testCase.path, nil)
		request.Header.Set("Authorization", "Bearer access")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"affected":1`) {
			t.Fatalf("feishu %s returned %d %s", testCase.path, recorder.Code, recorder.Body.String())
		}
	}

	// 飞书话题恢复：调用 RestoreConversation 恢复已关闭或已归档的绑定。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, created.ID).Return(biz.Session{
		ID: created.ID, UserID: principal.UserID, Status: biz.SessionActive,
	}, nil)
	conversations.EXPECT().RestoreConversation(gomock.Any(), principal.UserID, created.ID, gomock.Any()).Return(1, nil)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID.String()+"/feishu-restore", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"affected":1`) {
		t.Fatalf("feishu restore returned %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"user_id":"`+uuid.NewString()+`"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("client supplied user_id returned %d %s", recorder.Code, recorder.Body.String())
	}
}
