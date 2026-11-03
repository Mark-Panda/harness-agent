package biz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// newBudgetService 构造带 Mock 仓储的预算服务。
func newBudgetService(t *testing.T, ctrl *gomock.Controller, policy biz.BudgetPolicy, clock func() time.Time) (*biz.BudgetService, *mocks.MockBudgetRepository) {
	t.Helper()
	repo := mocks.NewMockBudgetRepository(ctrl)
	service, err := biz.NewBudgetService(repo, policy, clock)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

// TestBudgetPolicyNormalize 验证负数上限被归零、告警阈值回退默认值。
func TestBudgetPolicyNormalize(t *testing.T) {
	policy := biz.BudgetPolicy{
		UserDailyTokens: -1, AgentDailyTokens: -1, ProviderDailyTokens: -1,
		UserDailyCost: -1, AgentDailyCost: -1, ProviderDailyCost: -1, AlertThreshold: 2,
	}.Normalize()
	if policy.UserDailyTokens != 0 || policy.AgentDailyTokens != 0 || policy.ProviderDailyTokens != 0 ||
		policy.UserDailyCost != 0 || policy.AgentDailyCost != 0 || policy.ProviderDailyCost != 0 {
		t.Fatalf("negative limits were not normalized: %#v", policy)
	}
	if policy.AlertThreshold != 0.8 {
		t.Fatalf("alert threshold = %f", policy.AlertThreshold)
	}
	if _, err := biz.NewBudgetService(nil, policy, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil repository error = %v", err)
	}
}

// TestBudgetWindowStart 验证预算窗口为 UTC 自然日零点。
func TestBudgetWindowStart(t *testing.T) {
	service, _ := newBudgetService(t, gomock.NewController(t), biz.BudgetPolicy{}, func() time.Time {
		return time.Date(2026, time.November, 2, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	})
	window := service.WindowStart(time.Date(2026, time.November, 2, 23, 30, 0, 0, time.FixedZone("CST", 8*3600)))
	if window != time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("window = %v", window)
	}
}

// TestBudgetCheckUserExceeded 验证用户 Token 超限时拒绝并返回告警明细。
func TestBudgetCheckUserExceeded(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID := uuid.New()
	service, repo := newBudgetService(t, ctrl, biz.BudgetPolicy{UserDailyTokens: 1000, UserDailyCost: 1, AlertThreshold: 0.8}, nil)
	repo.EXPECT().GetUsage(gomock.Any(), biz.BudgetScopeUser, userID, gomock.Any()).Return(biz.BudgetUsage{
		InputTokens: 800, OutputTokens: 300, EstimatedCost: 0.5,
	}, nil)
	alerts, err := service.Check(context.Background(), userID, nil, nil)
	if !errors.Is(err, biz.ErrBudgetExceeded) {
		t.Fatalf("Check() error = %v", err)
	}
	if len(alerts) != 1 || alerts[0].Scope != biz.BudgetScopeUser || !alerts[0].Exceeded {
		t.Fatalf("alerts = %#v", alerts)
	}
}

// TestBudgetCheckAlertBelowLimit 验证接近阈值但未超限时仅告警不拒绝。
func TestBudgetCheckAlertBelowLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, agentID, providerID := uuid.New(), uuid.New(), uuid.New()
	service, repo := newBudgetService(t, ctrl, biz.BudgetPolicy{
		UserDailyTokens: 1000, UserDailyCost: 2, AgentDailyTokens: 500, ProviderDailyCost: 3, AlertThreshold: 0.8,
	}, nil)
	repo.EXPECT().GetUsage(gomock.Any(), biz.BudgetScopeUser, userID, gomock.Any()).Return(biz.BudgetUsage{InputTokens: 850}, nil)
	repo.EXPECT().GetUsage(gomock.Any(), biz.BudgetScopeAgent, agentID, gomock.Any()).Return(biz.BudgetUsage{InputTokens: 100}, nil)
	repo.EXPECT().GetUsage(gomock.Any(), biz.BudgetScopeProvider, providerID, gomock.Any()).Return(biz.BudgetUsage{EstimatedCost: 2.5}, nil)
	alerts, err := service.Check(context.Background(), userID, &agentID, &providerID)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %#v", alerts)
	}
	for _, alert := range alerts {
		if alert.Exceeded {
			t.Fatalf("alert must not be exceeded: %#v", alert)
		}
	}
}

// TestRunCreateRejectsExhaustedBudget 验证预算超限时 Run 创建在触达会话仓储前被拒绝。
func TestRunCreateRejectsExhaustedBudget(t *testing.T) {
	ctrl := gomock.NewController(t)
	runs := mocks.NewMockRunRepository(ctrl)
	sessions := mocks.NewMockSessionRepository(ctrl)
	checker := mocks.NewMockBudgetChecker(ctrl)
	service, err := biz.NewRunService(runs, sessions, biz.WithBudgets(checker))
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	checker.EXPECT().Check(gomock.Any(), principal.UserID, gomock.Any(), gomock.Any()).Return(nil, biz.ErrBudgetExceeded)
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{SessionID: uuid.New(), Mode: biz.AgentModeReact}); !errors.Is(err, biz.ErrBudgetExceeded) {
		t.Fatalf("Create() error = %v", err)
	}
}

// TestBudgetCheckNoLimitAndErrors 验证零上限表示不限制且不查询仓储，仓储故障原样返回。
func TestBudgetCheckNoLimitAndErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID := uuid.New()
	service, _ := newBudgetService(t, ctrl, biz.BudgetPolicy{}, nil)
	if alerts, err := service.Check(context.Background(), userID, nil, nil); err != nil || len(alerts) != 0 {
		t.Fatalf("unlimited Check() = %#v, %v", alerts, err)
	}
	if _, err := service.Check(context.Background(), uuid.Nil, nil, nil); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("nil user error = %v", err)
	}
	limited, repo := newBudgetService(t, ctrl, biz.BudgetPolicy{UserDailyTokens: 1000}, nil)
	repo.EXPECT().GetUsage(gomock.Any(), biz.BudgetScopeUser, userID, gomock.Any()).Return(biz.BudgetUsage{}, errors.New("database down"))
	if _, err := limited.Check(context.Background(), userID, nil, nil); err == nil {
		t.Fatal("repository failure was swallowed")
	}
}
