package biz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeRouteHealth 是测试用熔断状态读取器。
type fakeRouteHealth struct {
	states map[string]biz.RouteHealthState
	err    error
}

// RouteHealthState 返回预置状态。
func (f *fakeRouteHealth) RouteHealthState(_ context.Context, providerID, modelID string) (biz.RouteHealthState, error) {
	if f.err != nil {
		return biz.RouteHealthState{}, f.err
	}
	return f.states[providerID+"/"+modelID], nil
}

func TestRoutingHealthRequiresAdminAndMapsStates(t *testing.T) {
	ctrl := gomock.NewController(t)
	models := mocks.NewMockModelRepository(ctrl)
	service, err := biz.NewRoutingHealthService(models, &fakeRouteHealth{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin list error = %v", err)
	}
	providerID, modelID := uuid.New(), uuid.New()
	models.EXPECT().ListModels(gomock.Any(), (*uuid.UUID)(nil)).Return([]biz.Model{
		{ID: modelID, ProviderID: providerID, RemoteModelName: "e2e-chat"},
	}, nil).Times(3)
	health := &fakeRouteHealth{states: map[string]biz.RouteHealthState{
		providerID.String() + "/" + modelID.String(): {Failures: 2},
	}}
	service, err = biz.NewRoutingHealthService(models, health)
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.List(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "degraded" || items[0].Failures != 2 {
		t.Fatalf("unexpected health items: %#v", items)
	}

	// 熔断打开优先展示 open，未知错误标记 unknown。
	health.states[providerID.String()+"/"+modelID.String()] = biz.RouteHealthState{Open: true}
	items, _ = service.List(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin})
	if items[0].Status != "open" {
		t.Fatalf("open state = %q", items[0].Status)
	}
	health.err = errors.New("redis unavailable")
	items, _ = service.List(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin})
	if items[0].Status != "unknown" {
		t.Fatalf("unknown state = %q", items[0].Status)
	}
	if _, err := biz.NewRoutingHealthService(nil, health); err == nil {
		t.Fatal("nil model repository was accepted")
	}
}
