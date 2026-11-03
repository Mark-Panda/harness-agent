package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// RouteHealthState 是模型熔断状态的只读快照。
type RouteHealthState struct {
	// Failures 当前窗口内的失败计数。
	Failures int64
	// Open 熔断是否打开。
	Open bool
	// HalfOpen 是否处于半开探测阶段。
	HalfOpen bool
}

// RouteHealthReader 读取跨副本共享的模型熔断状态。
type RouteHealthReader interface {
	// RouteHealthState 返回指定供应商与模型的熔断快照。
	RouteHealthState(ctx context.Context, providerID, modelID string) (RouteHealthState, error)
}

// ModelHealth 是模型目录中的健康与熔断展示项。
type ModelHealth struct {
	// ModelID 模型 ID。
	ModelID uuid.UUID
	// ProviderID 所属供应商。
	ProviderID uuid.UUID
	// RemoteModelName 供应商侧模型名。
	RemoteModelName string
	// Status 健康状态：healthy/degraded/half_open/open/unknown。
	Status string
	// Failures 当前失败计数。
	Failures int64
	// CheckedAt 状态读取时间。
	CheckedAt time.Time
}

// RoutingHealthService 汇聚模型目录与熔断状态，供管理员页面展示。
type RoutingHealthService struct {
	models ModelRepository
	health RouteHealthReader
	clock  func() time.Time
}

// NewRoutingHealthService 构造路由健康服务。
func NewRoutingHealthService(models ModelRepository, health RouteHealthReader) (*RoutingHealthService, error) {
	if models == nil || health == nil {
		return nil, errors.New("routing health service requires model repository and health reader")
	}
	return &RoutingHealthService{models: models, health: health, clock: func() time.Time { return time.Now().UTC() }}, nil
}

// List 返回全部模型的健康状态；仅管理员可读。
func (s *RoutingHealthService) List(ctx context.Context, principal Principal) ([]ModelHealth, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	models, err := s.models.ListModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	result := make([]ModelHealth, 0, len(models))
	for _, model := range models {
		state, stateErr := s.health.RouteHealthState(ctx, model.ProviderID.String(), model.ID.String())
		item := ModelHealth{
			ModelID: model.ID, ProviderID: model.ProviderID, RemoteModelName: model.RemoteModelName,
			Status: statusFromRouteHealth(state), Failures: state.Failures, CheckedAt: now,
		}
		if stateErr != nil {
			// 缓存不可用时明确标记 unknown，避免把未知状态展示为健康。
			item.Status = "unknown"
			item.Failures = 0
		}
		result = append(result, item)
	}
	return result, nil
}

// statusFromRouteHealth 将熔断快照映射为稳定状态字符串。
func statusFromRouteHealth(state RouteHealthState) string {
	switch {
	case state.Open:
		return "open"
	case state.HalfOpen:
		return "half_open"
	case state.Failures > 0:
		return "degraded"
	default:
		return "healthy"
	}
}
