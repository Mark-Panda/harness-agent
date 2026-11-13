package server

import (
	"bytes"
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

// planTestFixture 聚合 Plan 控制面测试所需的 mock 与处理器。
type planTestFixture struct {
	handler   http.Handler
	principal biz.Principal
	runRepo   *mocks.MockRunRepository
	planRepo  *mocks.MockPlanRepository
	enqueuer  *mocks.MockRunEnqueuer
}

// newPlanTestFixture 装配带 Plan 能力的 HTTP Handler，复用 gomock 仓储与内存鉴权。
func newPlanTestFixture(t *testing.T) *planTestFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	enqueuer := mocks.NewMockRunEnqueuer(ctrl)
	runs, err := biz.NewRunService(runRepo, sessionRepo, biz.WithRunEnqueuer(enqueuer))
	if err != nil {
		t.Fatal(err)
	}
	planRepo := mocks.NewMockPlanRepository(ctrl)
	plans, err := biz.NewPlanService(planRepo, runs)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{},
		&biz.SessionService{}, runs, &biz.IdempotencyService{},
	)
	if err != nil {
		t.Fatal(err)
	}
	platform.WithPlans(plans)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, &fakeAuthApplication{principal: principal}, nil, platform)
	return &planTestFixture{handler: handler, principal: principal, runRepo: runRepo, planRepo: planRepo, enqueuer: enqueuer}
}

// doPlanRequest 以认证用户身份发起 Plan 接口调用并返回响应。
func (f *planTestFixture) doPlanRequest(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Buffer
	if body == "" {
		reader = bytes.NewBuffer(nil)
	} else {
		reader = bytes.NewBufferString(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer access")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// TestPlanAPIGetPlan 验证读取最新计划的成功路径与 Run 缺失时的 404。
func TestPlanAPIGetPlan(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()
	planID := uuid.New()
	sessionID := uuid.New()

	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Run{ID: runID, UserID: fixture.principal.UserID, SessionID: sessionID, Status: biz.RunRunning}, nil,
	)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(biz.Plan{
		ID: planID, UserID: fixture.principal.UserID, RunID: runID, SessionID: sessionID,
		Version: 1, Status: biz.PlanExecuting, Goal: "build", MaxSteps: 50, Revision: 1,
		Steps: []biz.PlanStep{
			{ID: uuid.New(), PlanID: planID, StepNo: 1, Title: "collect", Status: biz.PlanStepRunning, Attempts: 1, MaxAttempts: 2},
		},
	}, nil)
	recorder := fixture.doPlanRequest(t, http.MethodGet, "/api/v1/runs/"+runID.String()+"/plan", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get plan returned %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, fragment := range []string{`"id":"` + planID.String() + `"`, `"run_id":"` + runID.String() + `"`, `"status":"executing"`, `"step_no":1`, `"title":"collect"`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("get plan response missing %s: %s", fragment, body)
		}
	}

	// Run 不存在（或跨用户）必须映射为 404 resource_not_found。
	missingRunID := uuid.New()
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, missingRunID).Return(biz.Run{}, biz.ErrResourceNotFound)
	recorder = fixture.doPlanRequest(t, http.MethodGet, "/api/v1/runs/"+missingRunID.String()+"/plan", "")
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "resource_not_found") {
		t.Fatalf("missing run returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlanAPIListPlanVersions 验证计划版本历史列表。
func TestPlanAPIListPlanVersions(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()

	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunRunning}, nil,
	)
	fixture.planRepo.EXPECT().ListPlanVersions(gomock.Any(), fixture.principal.UserID, runID).Return([]biz.Plan{
		{ID: uuid.New(), RunID: runID, Version: 2, Status: biz.PlanExecuting, Goal: "v2"},
		{ID: uuid.New(), RunID: runID, Version: 1, Status: biz.PlanCancelled, Goal: "v1"},
	}, nil)
	recorder := fixture.doPlanRequest(t, http.MethodGet, "/api/v1/runs/"+runID.String()+"/plan/versions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list plan versions returned %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"items"`) || !strings.Contains(body, `"version":2`) || !strings.Contains(body, `"version":1`) {
		t.Fatalf("list plan versions response = %s", body)
	}
}

// TestPlanAPIPausePlan 验证暂停成功路径与非执行中计划的 409 冲突。
func TestPlanAPIPausePlan(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()
	planID := uuid.New()

	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunRunning}, nil,
	)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: planID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanExecuting, Revision: 1}, nil,
	)
	fixture.planRepo.EXPECT().UpdatePlan(gomock.Any(), fixture.principal.UserID, planID, biz.PlanExecuting, biz.PlanPaused, int64(1), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, _ biz.PlanStatus, _ int64, _ biz.PlanFence, update biz.PlanUpdate) (biz.Plan, error) {
			if update.ErrorCode != "plan_paused" {
				t.Fatalf("unexpected pause update: %#v", update)
			}
			return biz.Plan{ID: planID, RunID: runID, Status: biz.PlanPaused, Revision: 2, ErrorCode: update.ErrorCode}, nil
		},
	)
	recorder := fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/pause", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause plan returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 已暂停的计划不能再次暂停，返回 409 resource_conflict。
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunRunning}, nil,
	)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: planID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanPaused, Revision: 2}, nil,
	)
	recorder = fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/pause", "")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resource_conflict") {
		t.Fatalf("pause paused plan returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlanAPIResumePlan 验证恢复成功路径与非暂停计划的 409 冲突。
func TestPlanAPIResumePlan(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()
	planID := uuid.New()

	// UpdatedAt 置于未来，使暂停时长补偿为零，避免触碰截止时间延长分支。
	queuedRun := biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunQueued}
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(queuedRun, nil).Times(2)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(biz.Plan{
		ID: planID, UserID: fixture.principal.UserID, RunID: runID,
		Status: biz.PlanPaused, Revision: 3, UpdatedAt: time.Now().Add(time.Hour).UTC(),
	}, nil)
	fixture.planRepo.EXPECT().UpdatePlan(gomock.Any(), fixture.principal.UserID, planID, biz.PlanPaused, biz.PlanExecuting, int64(3), gomock.Any(), gomock.Any()).Return(
		biz.Plan{ID: planID, RunID: runID, Status: biz.PlanExecuting, Revision: 4}, nil,
	)
	fixture.enqueuer.EXPECT().EnqueueRun(gomock.Any(), fixture.principal.UserID, runID, gomock.Any()).Return(nil)
	recorder := fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/resume", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"executing"`) {
		t.Fatalf("resume plan returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 执行中的计划不能恢复，返回 409 resource_conflict。
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(queuedRun, nil)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: planID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanExecuting, Revision: 4}, nil,
	)
	recorder = fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/resume", "")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resource_conflict") {
		t.Fatalf("resume executing plan returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlanAPICancelPlan 验证人工终止成功路径与终态计划的 409 冲突。
func TestPlanAPICancelPlan(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()
	planID := uuid.New()

	runningRun := biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunRunning}
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(runningRun, nil).Times(2)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: planID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanExecuting, Revision: 1}, nil,
	)
	fixture.planRepo.EXPECT().UpdatePlan(gomock.Any(), fixture.principal.UserID, planID, biz.PlanExecuting, biz.PlanCancelled, int64(1), gomock.Any(), gomock.Any()).Return(
		biz.Plan{ID: planID, RunID: runID, Status: biz.PlanCancelled, Revision: 2, ErrorCode: "plan_cancelled"}, nil,
	)
	fixture.runRepo.EXPECT().TransitionRun(gomock.Any(), fixture.principal.UserID, runID, biz.RunRunning, biz.RunCancelled, gomock.Any()).Return(
		biz.Run{ID: runID, Status: biz.RunCancelled}, nil,
	)
	recorder := fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/cancel", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel plan returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 终态计划不能终止，返回 409 resource_conflict，且不再触碰仓储写操作。
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(runningRun, nil)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: planID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanCompleted, Revision: 9}, nil,
	)
	recorder = fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/cancel", "")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resource_conflict") {
		t.Fatalf("cancel terminal plan returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlanAPIRevisePlan 验证修订成功路径（201 新版本）与非法输入。
func TestPlanAPIRevisePlan(t *testing.T) {
	fixture := newPlanTestFixture(t)
	runID := uuid.New()
	sessionID := uuid.New()
	oldPlanID := uuid.New()

	queuedRun := biz.Run{ID: runID, UserID: fixture.principal.UserID, Status: biz.RunQueued}
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(queuedRun, nil).Times(3)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(biz.Plan{
		ID: oldPlanID, UserID: fixture.principal.UserID, RunID: runID, SessionID: sessionID,
		Version: 1, Status: biz.PlanPaused, Revision: 5, Goal: "old goal",
		UpdatedAt: time.Now().Add(time.Hour).UTC(),
	}, nil)
	fixture.planRepo.EXPECT().ReplacePlan(gomock.Any(), fixture.principal.UserID, gomock.Any(), int64(5), biz.PlanFence{}, gomock.Any()).DoAndReturn(
		func(_ context.Context, userID uuid.UUID, plan biz.Plan, _ int64, _ biz.PlanFence, event biz.OutboxEvent) (biz.Plan, error) {
			if plan.Version != 2 || plan.ParentPlanID == nil || *plan.ParentPlanID != oldPlanID ||
				plan.Status != biz.PlanExecuting || plan.Goal != "revised goal" || plan.SessionID != sessionID {
				t.Fatalf("revised plan = %#v", plan)
			}
			if len(plan.Steps) != 2 || plan.Steps[0].MaxAttempts != 2 || plan.Steps[1].MaxAttempts != 1 ||
				plan.Steps[1].ToolName != "search" || plan.Steps[0].UserID != userID {
				t.Fatalf("revised steps = %#v", plan.Steps)
			}
			if event.EventType != "plan.revised" {
				t.Fatalf("revised event = %#v", event)
			}
			return plan, nil
		},
	)
	fixture.enqueuer.EXPECT().EnqueueRun(gomock.Any(), fixture.principal.UserID, runID, gomock.Any()).Return(nil)
	recorder := fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/revise",
		`{"goal":"revised goal","steps":[{"step_no":1,"title":"collect"},{"step_no":2,"title":"search web","depends_on":[1],"tool_name":"search","arguments":{"q":"x"}}]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("revise plan returned %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"goal":"revised goal"`) || !strings.Contains(body, `"version":2`) || !strings.Contains(body, `"status":"executing"`) {
		t.Fatalf("revise plan response = %s", body)
	}

	// 执行中的计划不能修订，返回 409 resource_conflict。
	fixture.runRepo.EXPECT().GetRun(gomock.Any(), fixture.principal.UserID, runID).Return(queuedRun, nil)
	fixture.planRepo.EXPECT().GetPlanByRun(gomock.Any(), fixture.principal.UserID, runID).Return(
		biz.Plan{ID: oldPlanID, UserID: fixture.principal.UserID, RunID: runID, Status: biz.PlanExecuting, Revision: 6}, nil,
	)
	recorder = fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/revise",
		`{"goal":"another","steps":[{"step_no":1,"title":"collect"}]}`)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resource_conflict") {
		t.Fatalf("revise executing plan returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 请求体非法时在解码阶段返回 400，不触达领域层。
	recorder = fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/"+runID.String()+"/plan/revise", `{"goal":`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_json") {
		t.Fatalf("invalid revise body returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlanAPIRejectsInvalidRunID 验证非法路径参数与未认证请求在路由层被拦截。
func TestPlanAPIRejectsInvalidRunID(t *testing.T) {
	fixture := newPlanTestFixture(t)

	recorder := fixture.doPlanRequest(t, http.MethodPost, "/api/v1/runs/not-a-uuid/plan/pause", "")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_resource_id") {
		t.Fatalf("invalid run id returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 未携带 Token 的请求必须返回 401，且不触达业务层。
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+uuid.NewString()+"/plan/pause", nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pause returned %d", recorder.Code)
	}
}
