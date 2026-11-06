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

// TestRunAPIAuthenticationAndCancellation 验证 Run 接口鉴权、身份隔离与取消语义。
func TestRunAPIAuthenticationAndCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	runs, _ := biz.NewRunService(runRepo, sessionRepo)
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{},
		&biz.SessionService{}, runs, &biz.IdempotencyService{},
	)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	auth := &fakeAuthApplication{principal: principal}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)

	// 未携带 Token 时必须被拦截，且不触达业务层。
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.NewString(), nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated run lookup returned %d", recorder.Code)
	}

	sessionID := uuid.New()
	triggerMessageID := uuid.New()
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{ID: sessionID, Status: biz.SessionActive}, nil)
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").Return(
		biz.Page[biz.Message]{Items: []biz.Message{{ID: triggerMessageID, Role: biz.MessageUser, CreatedAt: time.Now().UTC()}}}, nil,
	)
	runRepo.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, run biz.Run) (biz.Run, error) {
			if run.UserID != principal.UserID || run.Status != biz.RunQueued || run.SessionID != sessionID {
				t.Fatalf("invalid run created from API: %#v", run)
			}
			if run.TriggerMessageID == nil || *run.TriggerMessageID != triggerMessageID {
				t.Fatalf("trigger message was not bound from API: %#v", run)
			}
			return run, nil
		},
	)
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID.String()+"/runs",
		strings.NewReader(`{"mode":"react","trigger_message_id":"`+triggerMessageID.String()+`"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), "queued") {
		t.Fatalf("run create returned %d %s", recorder.Code, recorder.Body.String())
	}

	runID := uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunRunning}, nil)
	runRepo.EXPECT().TransitionRun(gomock.Any(), principal.UserID, runID, biz.RunRunning, biz.RunCancelled, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCancelled}, nil,
	)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "cancelled") {
		t.Fatalf("run cancel returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestRunEventStreamReplayAndTerminalClose 验证 SSE 事件回放、游标与终态关闭。
func TestRunEventStreamReplayAndTerminalClose(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	runs, _ := biz.NewRunService(runRepo, sessionRepo)
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{},
		&biz.SessionService{}, runs, &biz.IdempotencyService{},
	)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	auth := &fakeAuthApplication{principal: principal}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, auth, nil, platform)
	runID := uuid.New()

	// GetRun 供建立流前的归属校验与 ListEvents 内部归属校验使用；循环内状态判断使用轻量查询。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, Status: biz.RunCompleted}, nil).AnyTimes()
	runRepo.EXPECT().GetRunStatus(gomock.Any(), principal.UserID, runID).Return(biz.RunCompleted, nil).AnyTimes()
	// 注入即时通知源，验证通知驱动路径无需等待兜底轮询。
	SetRunEventStream(immediateRunEventStream{})
	t.Cleanup(func() { SetRunEventStream(nil) })
	runRepo.EXPECT().ListEvents(gomock.Any(), principal.UserID, runID, int64(2), 200).Return([]biz.RunEvent{
		{RunID: runID, Seq: 3, EventType: "message.delta", Payload: map[string]any{"text": "hi"}},
	}, nil)
	// 终态后事件为空，服务端应主动关闭连接。
	runRepo.EXPECT().ListEvents(gomock.Any(), principal.UserID, runID, int64(3), 200).Return(nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/events?after=2", nil)
	request.Header.Set("Authorization", "Bearer access")
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, request)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not terminate on run completion")
	}
	body := recorder.Body.String()
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected content type: %q", recorder.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "id: 3") || !strings.Contains(body, "event: message.delta") || !strings.Contains(body, `{"text":"hi"}`) {
		t.Fatalf("event was not replayed: %s", body)
	}
	// 非法游标必须在建立流之前被拒绝。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/events?after=-1", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor returned %d", recorder.Code)
	}
}
