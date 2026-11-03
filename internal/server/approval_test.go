package server

import (
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

// newApprovalHTTPHandler 构造绑定审批仓储的 HTTP 处理器。
func newApprovalHTTPHandler(t *testing.T, ctrl *gomock.Controller, principal biz.Principal) (http.Handler, *mocks.MockRunRepository, *mocks.MockToolApprovalRepository) {
	t.Helper()
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	approvals := mocks.NewMockToolApprovalRepository(ctrl)
	runs, err := biz.NewRunService(runRepo, sessionRepo, biz.WithToolApprovals(approvals))
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
	handler := NewHandler(fstest.MapFS{"index.html": {Data: []byte("Harness")}}, &fakeAuthApplication{principal: principal}, nil, platform)
	return handler, runRepo, approvals
}

// TestRunApprovalsAPIRequiresAuthAndLists 验证审批查询需要鉴权并按用户隔离。
func TestRunApprovalsAPIRequiresAuthAndLists(t *testing.T) {
	ctrl := gomock.NewController(t)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler, runRepo, approvals := newApprovalHTTPHandler(t, ctrl, principal)
	runID := uuid.New()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/approvals", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approvals returned %d", recorder.Code)
	}

	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().ListApprovals(gomock.Any(), principal.UserID, runID).Return([]biz.ToolApproval{{
		ID: uuid.New(), RunID: runID, UserID: principal.UserID, ToolCallID: "call-1",
		ToolName: "mcp_admin_delete", RiskLevel: "high", Status: biz.ApprovalPending,
	}}, nil)
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/approvals", nil)
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "mcp_admin_delete") ||
		!strings.Contains(recorder.Body.String(), "pending") {
		t.Fatalf("approval list returned %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestRunApprovalDecisionAPI 验证批准与拒绝决策的路由、错误映射与幂等语义。
func TestRunApprovalDecisionAPI(t *testing.T) {
	ctrl := gomock.NewController(t)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler, runRepo, approvals := newApprovalHTTPHandler(t, ctrl, principal)
	runID, approvalID := uuid.New(), uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), principal.UserID, approvalID).Return(biz.ToolApproval{
		ID: approvalID, RunID: runID, UserID: principal.UserID, ToolCallID: "call-1",
		ToolName: "mcp_admin_delete", Status: biz.ApprovalPending,
	}, nil)
	approved := biz.ToolApproval{ID: approvalID, RunID: runID, UserID: principal.UserID, Status: biz.ApprovalApproved}
	approvals.EXPECT().DecideApproval(gomock.Any(), principal.UserID, approvalID, biz.ApprovalDecisionApprove, principal.UserID, gomock.Any()).Return(approved, nil)
	runRepo.EXPECT().AppendEvent(gomock.Any(), gomock.Any()).Return(biz.RunEvent{}, nil)
	approvals.EXPECT().CountPendingApprovals(gomock.Any(), principal.UserID, runID).Return(0, nil)
	runRepo.EXPECT().ResumeRun(gomock.Any(), principal.UserID, runID, gomock.Any()).Return(biz.Run{ID: runID, Status: biz.RunRunning}, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/approvals/"+approvalID.String(),
		strings.NewReader(`{"decision":"approve"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "approved") {
		t.Fatalf("approve returned %d %s", recorder.Code, recorder.Body.String())
	}

	// 非法决策在触达仓储前被拒绝。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/approvals/"+approvalID.String(),
		strings.NewReader(`{"decision":"maybe"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision returned %d", recorder.Code)
	}

	// 审批不属于该 Run 时返回冲突，防止跨 Run 决策。
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), principal.UserID, approvalID).Return(biz.ToolApproval{
		ID: approvalID, RunID: uuid.New(), UserID: principal.UserID, Status: biz.ApprovalPending,
	}, nil)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/approvals/"+approvalID.String(),
		strings.NewReader(`{"decision":"deny"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("cross-run decision returned %d", recorder.Code)
	}

	// 非法审批 ID 返回 400。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/approvals/not-a-uuid",
		strings.NewReader(`{"decision":"deny"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid approval id returned %d", recorder.Code)
	}
}

// TestRunApprovalDecisionNotFound 验证审批不存在时映射为 404。
func TestRunApprovalDecisionNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	handler, runRepo, approvals := newApprovalHTTPHandler(t, ctrl, principal)
	runID, approvalID := uuid.New(), uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID, Status: biz.RunWaitingApproval}, nil)
	approvals.EXPECT().GetApproval(gomock.Any(), principal.UserID, approvalID).Return(biz.ToolApproval{}, biz.ErrResourceNotFound)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID.String()+"/approvals/"+approvalID.String(),
		strings.NewReader(`{"decision":"approve"}`))
	request.Header.Set("Authorization", "Bearer access")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing approval returned %d", recorder.Code)
	}
}
