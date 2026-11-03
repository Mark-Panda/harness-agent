package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// memoryIdempotencyRepository 是内存幂等仓库，用于验证 HTTP 层的回放与冲突语义。
type memoryIdempotencyRepository struct {
	mu      sync.Mutex
	records map[string]memoryIdempotencyRecord
}

type memoryIdempotencyRecord struct {
	hash     []byte
	status   string
	envelope []byte
}

func newMemoryIdempotencyRepository() *memoryIdempotencyRepository {
	return &memoryIdempotencyRepository{records: map[string]memoryIdempotencyRecord{}}
}

func (r *memoryIdempotencyRepository) Begin(_ context.Context, userID uuid.UUID, key string, hash []byte, _ string, _ time.Time) (biz.IdempotencyOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	composite := userID.String() + ":" + key
	record, exists := r.records[composite]
	if !exists {
		r.records[composite] = memoryIdempotencyRecord{hash: hash, status: "in_progress"}
		return biz.IdempotencyOutcome{}, nil
	}
	if !bytes.Equal(record.hash, hash) {
		return biz.IdempotencyOutcome{}, biz.ErrIdempotencyConflict
	}
	switch record.status {
	case "completed":
		return biz.ReplayOutcome(record.envelope)
	case "failed":
		r.records[composite] = memoryIdempotencyRecord{hash: hash, status: "in_progress"}
		return biz.IdempotencyOutcome{}, nil
	default:
		return biz.IdempotencyOutcome{}, biz.ErrIdempotencyConflict
	}
}

func (r *memoryIdempotencyRepository) Complete(_ context.Context, userID uuid.UUID, key string, envelope []byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	composite := userID.String() + ":" + key
	record := r.records[composite]
	record.status = "completed"
	record.envelope = envelope
	r.records[composite] = record
	return nil
}

func (r *memoryIdempotencyRepository) Fail(_ context.Context, userID uuid.UUID, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	composite := userID.String() + ":" + key
	record := r.records[composite]
	record.status = "failed"
	r.records[composite] = record
	return nil
}

// TestIdempotentWriteReplaysAndRejectsMismatch 验证同键同体回放、异体冲突与单次执行。
func TestIdempotentWriteReplaysAndRejectsMismatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	sessions, _ := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	idempotency, _ := biz.NewIdempotencyService(newMemoryIdempotencyRepository())
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{}, sessions, &biz.RunService{}, idempotency,
	)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	auth := &fakeAuthApplication{principal: principal}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)

	sessionRepo.EXPECT().CountSessions(gomock.Any(), principal.UserID).Return(int64(0), nil).Times(1)
	sessionRepo.EXPECT().CreateSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			return session, nil
		},
	).Times(1)

	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"title":"once"}`))
	request.Header.Set("Authorization", "Bearer access")
	request.Header.Set("Idempotency-Key", "create-session-1")
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create returned %d %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"title":"once"}`))
	request.Header.Set("Authorization", "Bearer access")
	request.Header.Set("Idempotency-Key", "create-session-1")
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusCreated || second.Header().Get("Idempotency-Replayed") != "true" || strings.TrimSpace(second.Body.String()) != strings.TrimSpace(first.Body.String()) {
		t.Fatalf("replay returned %d %q %s", second.Code, second.Header().Get("Idempotency-Replayed"), second.Body.String())
	}

	conflict := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"title":"different"}`))
	request.Header.Set("Authorization", "Bearer access")
	request.Header.Set("Idempotency-Key", "create-session-1")
	handler.ServeHTTP(conflict, request)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "idempotency_conflict") {
		t.Fatalf("mismatched replay returned %d %s", conflict.Code, conflict.Body.String())
	}
}
