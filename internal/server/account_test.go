package server

import (
	"context"
	"encoding/json"
	"errors"
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

// fakeAccountSummaryRepository 是账号导出测试的摘要只读替身。
type fakeAccountSummaryRepository struct {
	// summaries 预置摘要列表。
	summaries []biz.SessionSummary
}

// GetActiveSummary 返回空结果，测试不使用。
func (f *fakeAccountSummaryRepository) GetActiveSummary(context.Context, uuid.UUID, uuid.UUID) (biz.SessionSummary, bool, error) {
	return biz.SessionSummary{}, false, nil
}

// LatestSummaryVersion 返回 0，测试不使用。
func (f *fakeAccountSummaryRepository) LatestSummaryVersion(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	return 0, nil
}

// SaveSummary 返回未应用，测试不使用。
func (f *fakeAccountSummaryRepository) SaveSummary(_ context.Context, summary biz.SessionSummary, _ int64, _ biz.OutboxEvent) (biz.SessionSummary, bool, error) {
	return summary, false, nil
}

// InvalidateActive 返回 nil，测试不使用。
func (f *fakeAccountSummaryRepository) InvalidateActive(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

// ListSummaries 返回预置摘要。
func (f *fakeAccountSummaryRepository) ListSummaries(context.Context, uuid.UUID, uuid.UUID, int) ([]biz.SessionSummary, error) {
	return f.summaries, nil
}

// ListUserSummaries 返回预置摘要。
func (f *fakeAccountSummaryRepository) ListUserSummaries(context.Context, uuid.UUID, int) ([]biz.SessionSummary, error) {
	return f.summaries, nil
}

// fakeAccountFactRepository 是账号导出测试的事实只读替身。
type fakeAccountFactRepository struct {
	// facts 预置事实列表。
	facts []biz.MemoryFact
}

// ListActiveFacts 返回预置事实。
func (f *fakeAccountFactRepository) ListActiveFacts(context.Context, uuid.UUID, int) ([]biz.MemoryFact, error) {
	return f.facts, nil
}

// GetActiveFact 返回未命中，测试不使用。
func (f *fakeAccountFactRepository) GetActiveFact(context.Context, uuid.UUID, string) (biz.MemoryFact, bool, error) {
	return biz.MemoryFact{}, false, nil
}

// SaveFact 返回未应用，测试不使用。
func (f *fakeAccountFactRepository) SaveFact(_ context.Context, fact biz.MemoryFact, _ int64) (biz.MemoryFact, bool, error) {
	return fact, false, nil
}

// ListFactVersions 返回预置事实。
func (f *fakeAccountFactRepository) ListFactVersions(context.Context, uuid.UUID, string, int) ([]biz.MemoryFact, error) {
	return f.facts, nil
}

// ListFacts 返回预置事实。
func (f *fakeAccountFactRepository) ListFacts(context.Context, uuid.UUID, int) ([]biz.MemoryFact, error) {
	return f.facts, nil
}

// DeleteFact 返回 nil，测试不使用。
func (f *fakeAccountFactRepository) DeleteFact(context.Context, uuid.UUID, string, time.Time) error {
	return nil
}

// fakeAccountVectorRepository 是账号导出测试的向量只读替身。
type fakeAccountVectorRepository struct {
	// vectors 预置向量列表。
	vectors []biz.MemoryVector
}

// UpsertVectors 返回 nil，测试不使用。
func (f *fakeAccountVectorRepository) UpsertVectors(context.Context, uuid.UUID, []biz.MemoryVector) error {
	return nil
}

// SearchVectors 返回空结果，测试不使用。
func (f *fakeAccountVectorRepository) SearchVectors(context.Context, uuid.UUID, []float32, string, int) ([]biz.MemoryMatch, error) {
	return nil, nil
}

// ListSourceHashes 返回空映射，测试不使用。
func (f *fakeAccountVectorRepository) ListSourceHashes(context.Context, uuid.UUID, string, []uuid.UUID) (map[uuid.UUID]string, error) {
	return nil, nil
}

// DeleteVectorsForSources 返回 nil，测试不使用。
func (f *fakeAccountVectorRepository) DeleteVectorsForSources(context.Context, uuid.UUID, []uuid.UUID, time.Time) error {
	return nil
}

// ListVectors 返回预置向量。
func (f *fakeAccountVectorRepository) ListVectors(context.Context, uuid.UUID, *uuid.UUID, int) ([]biz.MemoryVector, error) {
	return f.vectors, nil
}

// fakeAccountDeletionRepository 是删除请求仓储的内存替身。
type fakeAccountDeletionRepository struct{}

// Create 返回待处理请求。
func (f *fakeAccountDeletionRepository) Create(_ context.Context, request biz.DeletionRequest, _ biz.OutboxEvent) (biz.DeletionRequest, error) {
	request.Status = biz.DeletionPending
	request.Attempts = 0
	request.MaxAttempts = 5
	request.CreatedAt = time.Now().UTC()
	return request, nil
}

// Get 返回未找到，测试不使用。
func (f *fakeAccountDeletionRepository) Get(context.Context, uuid.UUID, uuid.UUID) (biz.DeletionRequest, error) {
	return biz.DeletionRequest{}, biz.ErrResourceNotFound
}

// List 返回空分页，测试不使用。
func (f *fakeAccountDeletionRepository) List(context.Context, uuid.UUID, int, string) (biz.Page[biz.DeletionRequest], error) {
	return biz.Page[biz.DeletionRequest]{}, nil
}

// Claim 返回空领取，测试不使用。
func (f *fakeAccountDeletionRepository) Claim(context.Context, string, time.Duration, time.Time) (biz.DeletionRequest, bool, error) {
	return biz.DeletionRequest{}, false, nil
}

// Renew 返回 nil，测试不使用。
func (f *fakeAccountDeletionRepository) Renew(context.Context, uuid.UUID, string, int64, time.Duration, time.Time) error {
	return nil
}

// Complete 返回 nil，测试不使用。
func (f *fakeAccountDeletionRepository) Complete(context.Context, uuid.UUID, string, int64, time.Time, biz.OutboxEvent) error {
	return nil
}

// Fail 返回 nil，测试不使用。
func (f *fakeAccountDeletionRepository) Fail(context.Context, uuid.UUID, string, int64, time.Time, string, time.Time) error {
	return nil
}

// TestAccountExportAndDeletionHTTP 验证账号导出/删除路由、脱敏、附件响应与鉴权边界。
func TestAccountExportAndDeletionHTTP(t *testing.T) {
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

	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessionID, agentID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	summaries := &fakeAccountSummaryRepository{summaries: []biz.SessionSummary{{
		ID: uuid.New(), SessionID: sessionID, UserID: principal.UserID, Version: 1,
		SummaryText: "账号摘要", Status: biz.SummaryActive, CreatedAt: now,
	}}}
	facts := &fakeAccountFactRepository{facts: []biz.MemoryFact{{
		ID: uuid.New(), SessionID: &sessionID, Key: "credential.aws",
		Value: json.RawMessage(`{"value":"AKIAIOSFODNN7EXAMPLE"}`), Confidence: 0.9,
		Version: 1, ValidFrom: now,
	}}}
	vectors := &fakeAccountVectorRepository{vectors: []biz.MemoryVector{{
		ID: uuid.New(), SessionID: &sessionID, SourceType: biz.MemorySourceMessage,
		Content: "向量内容", Version: "v1", CreatedAt: now,
	}}}
	deletionService, err := biz.NewDeletionService(&fakeAccountDeletionRepository{}, sessionRepo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	exportService, err := biz.NewExportService(sessionRepo, summaries, facts, vectors, nil, biz.WithExportAgents(agentRepo))
	if err != nil {
		t.Fatal(err)
	}
	platform.WithDeletion(deletionService, exportService)
	auth := &fakeAuthApplication{principal: principal}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)

	sessionRepo.EXPECT().ListSessions(gomock.Any(), principal.UserID, 50, "").Return(biz.Page[biz.Session]{
		Items: []biz.Session{{ID: sessionID, UserID: principal.UserID, Title: "账号会话", Status: biz.SessionActive}},
	}, nil)
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).
		Return(biz.Session{ID: sessionID, UserID: principal.UserID, Title: "账号会话", Status: biz.SessionActive}, nil)
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, 100, "").
		Return(biz.Page[biz.Message]{Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}}}, nil)
	agentRepo.EXPECT().ListAgents(gomock.Any(), principal.UserID, 50, "").Return(biz.Page[biz.Agent]{
		Items: []biz.Agent{{
			ID: agentID, UserID: principal.UserID, Name: "账号助手", Mode: biz.AgentModeReact,
			Version: 1, Status: biz.AgentPublished, CreatedAt: now, UpdatedAt: now,
			ConfigSnapshot: map[string]any{"password": "hunter2", "model": "m1"},
		}},
	}, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/account/export", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("account export returned %d %s", recorder.Code, recorder.Body.String())
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, "account-export.json") {
		t.Fatalf("content disposition = %q", disposition)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("cache control = %q", cache)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "账号会话") || !strings.Contains(body, "账号助手") || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("account export missing content or redaction: %s", body)
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("account export leaked secrets: %s", body)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/account/deletion?source=api", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("account deletion returned %d %s", recorder.Code, recorder.Body.String())
	}
	var deletion struct {
		ResourceType string `json:"resource_type"`
		Status       string `json:"status"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.ResourceType != string(biz.DeletionResourceAccount) || deletion.Status != string(biz.DeletionPending) {
		t.Fatalf("deletion response = %#v", deletion)
	}

	// 未认证请求必须被鉴权中间件拒绝，而不是进入导出逻辑。
	auth.err = errors.New("unauthenticated")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/account/export", nil)
	request.Header.Set("Authorization", "Bearer expired")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export returned %d %s", recorder.Code, recorder.Body.String())
	}
}
