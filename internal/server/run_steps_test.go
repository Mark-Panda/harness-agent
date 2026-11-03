package server

import (
	"context"
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

// TestRunStepsAPI 验证执行树接口返回步骤与按需载荷，并拒绝非法步骤过滤参数。
func TestRunStepsAPI(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	runs, err := biz.NewRunService(runRepo, sessionRepo, biz.WithExecutionSteps(steps))
	if err != nil {
		t.Fatal(err)
	}
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{},
		&biz.SessionService{}, runs, &biz.IdempotencyService{},
	)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, &fakeAuthApplication{principal: principal}, nil, platform)
	runID := uuid.New()
	stepID := uuid.New()
	// 载荷查阅会依次经过步骤读取、审计写入与载荷读取三次 Run 归属校验。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil).Times(3)
	steps.EXPECT().ListSteps(gomock.Any(), principal.UserID, runID, 500).Return([]biz.ExecutionStep{
		{ID: stepID, RunID: runID, UserID: principal.UserID, StepSeq: 1, Kind: biz.StepKindModel, Name: "generation", Status: biz.StepCompleted},
	}, nil)
	runRepo.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, event biz.RunEvent) (biz.RunEvent, error) {
		if event.EventType != "audit.payload.viewed" || event.Payload["reason"] != "review failure" {
			t.Fatalf("unexpected payload audit event: %#v", event)
		}
		return event, nil
	})
	steps.EXPECT().ListPayloads(gomock.Any(), principal.UserID, runID, (*uuid.UUID)(nil), 200).Return([]biz.ExecutionPayload{
		{ID: uuid.New(), RunID: runID, UserID: principal.UserID, Type: biz.PayloadModelRequest, CaptureMode: biz.CaptureMetadataOnly, ContentJSON: map[string]any{"attempt_no": 1}},
	}, nil)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/steps?include_payloads=1&reason=review+failure", nil)
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("run steps returned %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"step_seq":1`) || !strings.Contains(body, `"payloads"`) {
		t.Fatalf("unexpected run steps body: %s", body)
	}

	// 非法 step_id 必须在读取载荷前返回 400。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil)
	steps.EXPECT().ListSteps(gomock.Any(), principal.UserID, runID, 500).Return([]biz.ExecutionStep{}, nil)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/steps?include_payloads=1&reason=review&step_id=invalid", nil)
	request.Header.Set("Authorization", "Bearer access")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid step id returned %d", recorder.Code)
	}

	// 缺少查阅理由时必须拒绝读取载荷。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil)
	steps.EXPECT().ListSteps(gomock.Any(), principal.UserID, runID, 500).Return([]biz.ExecutionStep{}, nil)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/steps?include_payloads=1&step_id="+stepID.String(), nil)
	request.Header.Set("Authorization", "Bearer access")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing payload reason returned %d", recorder.Code)
	}
}
