package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/redis/go-redis/v9"
)

// RouteHealthStore 使用 Redis 保存跨副本共享的模型熔断与半开探测状态。
// Key 按 provider_id + model_id 隔离，避免单一模型故障扩散到同供应商的其他模型。
type RouteHealthStore struct {
	// client Redis 客户端，支持单机与集群实现。
	client redis.UniversalClient
	// threshold 连续失败阈值，达到后打开熔断。
	threshold int
	// window 失败计数窗口，超时后自动清零，避免陈旧故障长期累积。
	window time.Duration
	// openFor 熔断打开时长，到期后进入半开探测。
	openFor time.Duration
}

// NewRouteHealthStore 构造熔断存储；阈值与时长必须为正，避免无界故障状态。
func NewRouteHealthStore(client redis.UniversalClient, threshold int, window, openFor time.Duration) (*RouteHealthStore, error) {
	if client == nil || threshold <= 0 || window <= 0 || openFor <= 0 {
		return nil, errors.New("route health store requires redis, positive threshold, window and open duration")
	}
	return &RouteHealthStore{client: client, threshold: threshold, window: window, openFor: openFor}, nil
}

// healthKeys 返回失败计数、熔断打开、半开探测与熔断历史四个 Key。
// wasOpenKey 的 TTL 比 openKey 长一个探测窗口，用于区分“从未熔断”和“冷却后进入半开”。
func healthKeys(providerID, modelID string) (failures, open, halfOpen, wasOpen string) {
	prefix := fmt.Sprintf("route:health:{%s}:{%s}", providerID, modelID)
	return prefix + ":failures", prefix + ":open", prefix + ":half_open", prefix + ":was_open"
}

// RouteHealthState 读取指定模型的失败计数、熔断与半开状态，供管理页面展示。
func (s *RouteHealthStore) RouteHealthState(ctx context.Context, providerID, modelID string) (biz.RouteHealthState, error) {
	failuresKey, openKey, halfOpenKey, _ := healthKeys(providerID, modelID)
	failures, err := s.client.Get(ctx, failuresKey).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return biz.RouteHealthState{}, fmt.Errorf("read route failure count: %w", err)
	}
	open, err := s.client.Exists(ctx, openKey).Result()
	if err != nil {
		return biz.RouteHealthState{}, fmt.Errorf("read route breaker: %w", err)
	}
	halfOpen, err := s.client.Exists(ctx, halfOpenKey).Result()
	if err != nil {
		return biz.RouteHealthState{}, fmt.Errorf("read half-open state: %w", err)
	}
	return biz.RouteHealthState{Failures: failures, Open: open > 0, HalfOpen: halfOpen > 0}, nil
}

// Allow 判断当前是否允许调用。
// 熔断打开期间全部拒绝；冷却结束后的半开阶段只放行一个探测请求，其余请求继续使用下一候选。
func (s *RouteHealthStore) Allow(ctx context.Context, providerID, modelID string) (bool, error) {
	_, openKey, halfOpenKey, wasOpenKey := healthKeys(providerID, modelID)
	exists, err := s.client.Exists(ctx, openKey).Result()
	if err != nil {
		return false, fmt.Errorf("read route breaker: %w", err)
	}
	if exists > 0 {
		return false, nil
	}
	// 从未熔断或已成功恢复时直接放行，不创建任何探测标记，避免健康模型被误限流。
	wasOpen, err := s.client.Exists(ctx, wasOpenKey).Result()
	if err != nil {
		return false, fmt.Errorf("read route breaker history: %w", err)
	}
	if wasOpen == 0 {
		return true, nil
	}
	// SET NX 让多副本中只有一个请求获得半开探测资格；探测结果会清理或延长该标记。
	acquired, err := s.client.SetNX(ctx, halfOpenKey, "1", s.window).Result()
	if err != nil {
		return false, fmt.Errorf("acquire half-open probe: %w", err)
	}
	return acquired, nil
}

// RecordSuccess 记录成功并关闭熔断，清理失败计数与探测标记。
func (s *RouteHealthStore) RecordSuccess(ctx context.Context, providerID, modelID string) error {
	failuresKey, openKey, halfOpenKey, wasOpenKey := healthKeys(providerID, modelID)
	if err := s.client.Del(ctx, failuresKey, openKey, halfOpenKey, wasOpenKey).Err(); err != nil {
		return fmt.Errorf("reset route breaker: %w", err)
	}
	return nil
}

// RecordFailure 记录一次健康类失败；达到阈值后打开熔断并清空失败计数。
// 失败计数使用滑动窗口：每次失败都刷新窗口 TTL，窗口内没有新失败则自动衰减。
func (s *RouteHealthStore) RecordFailure(ctx context.Context, providerID, modelID string) error {
	failuresKey, openKey, halfOpenKey, wasOpenKey := healthKeys(providerID, modelID)
	// 半开探测失败时直接重新打开熔断，避免失败的探测被当作普通失败缓慢累积。
	probeFailed, err := s.client.Exists(ctx, halfOpenKey).Result()
	if err != nil {
		return fmt.Errorf("read half-open probe: %w", err)
	}
	if probeFailed > 0 {
		reopen := s.client.TxPipeline()
		reopen.Del(ctx, halfOpenKey)
		reopen.Set(ctx, openKey, "1", s.openFor)
		reopen.Set(ctx, wasOpenKey, "1", s.openFor+s.window)
		reopen.Del(ctx, failuresKey)
		if _, err := reopen.Exec(ctx); err != nil {
			return fmt.Errorf("reopen route breaker after failed probe: %w", err)
		}
		return nil
	}
	pipeline := s.client.TxPipeline()
	countCommand := pipeline.Incr(ctx, failuresKey)
	pipeline.Expire(ctx, failuresKey, s.window)
	if _, err := pipeline.Exec(ctx); err != nil {
		return fmt.Errorf("record route failure: %w", err)
	}
	count, err := countCommand.Result()
	if err != nil {
		return fmt.Errorf("read route failure count: %w", err)
	}
	if count < int64(s.threshold) {
		return nil
	}
	// 达到阈值：打开熔断，并清空失败计数与探测标记，为冷却后的半开恢复准备干净状态。
	reopen := s.client.TxPipeline()
	reopen.Set(ctx, openKey, "1", s.openFor)
	reopen.Set(ctx, wasOpenKey, "1", s.openFor+s.window)
	reopen.Del(ctx, failuresKey, halfOpenKey)
	if _, err := reopen.Exec(ctx); err != nil {
		return fmt.Errorf("open route breaker: %w", err)
	}
	return nil
}
