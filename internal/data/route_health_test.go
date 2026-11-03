package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRouteHealthBreakerAndHalfOpen 验证失败阈值、熔断打开、半开单探测与成功恢复。
func TestRouteHealthBreakerAndHalfOpen(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewRouteHealthStore(client, 3, 30*time.Second, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if allowed, err := store.Allow(ctx, "provider", "model"); err != nil || !allowed {
		t.Fatalf("initial Allow() = %t, %v", allowed, err)
	}
	for i := 0; i < 2; i++ {
		if err := store.RecordFailure(ctx, "provider", "model"); err != nil {
			t.Fatal(err)
		}
	}
	if allowed, err := store.Allow(ctx, "provider", "model"); err != nil || !allowed {
		t.Fatalf("breaker must stay closed before threshold: %t, %v", allowed, err)
	}
	if err := store.RecordFailure(ctx, "provider", "model"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.Allow(ctx, "provider", "model"); err != nil || allowed {
		t.Fatalf("breaker must open at threshold: %t, %v", allowed, err)
	}

	// 冷却期结束进入半开：仅第一个探测获得放行资格。
	server.FastForward(10 * time.Second)
	first, err := store.Allow(ctx, "provider", "model")
	if err != nil || !first {
		t.Fatalf("half-open first probe = %t, %v", first, err)
	}
	second, err := store.Allow(ctx, "provider", "model")
	if err != nil || second {
		t.Fatalf("half-open second probe must be rejected: %t, %v", second, err)
	}

	if err := store.RecordSuccess(ctx, "provider", "model"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.Allow(ctx, "provider", "model"); err != nil || !allowed {
		t.Fatalf("success must close the breaker: %t, %v", allowed, err)
	}
}

// TestRouteHealthWindowDecay 验证失败计数窗口过期后自动衰减。
func TestRouteHealthWindowDecay(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewRouteHealthStore(client, 3, 5*time.Second, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.RecordFailure(ctx, "p", "m"); err != nil {
		t.Fatal(err)
	}
	server.FastForward(6 * time.Second)
	// 窗口过期后计数清零，两次失败不应触发阈值。
	for i := 0; i < 2; i++ {
		if err := store.RecordFailure(ctx, "p", "m"); err != nil {
			t.Fatal(err)
		}
	}
	if allowed, err := store.Allow(ctx, "p", "m"); err != nil || !allowed {
		t.Fatalf("expired failures must not trip breaker: %t, %v", allowed, err)
	}
}

// TestRouteHealthStateSnapshot 验证管理页面读取的失败计数、熔断与半开快照。
func TestRouteHealthStateSnapshot(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewRouteHealthStore(client, 2, 30*time.Second, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.RecordFailure(ctx, "provider", "model"); err != nil {
		t.Fatal(err)
	}
	state, err := store.RouteHealthState(ctx, "provider", "model")
	if err != nil || state.Failures != 1 || state.Open || state.HalfOpen {
		t.Fatalf("degraded state = %#v, %v", state, err)
	}
	if err := store.RecordFailure(ctx, "provider", "model"); err != nil {
		t.Fatal(err)
	}
	state, err = store.RouteHealthState(ctx, "provider", "model")
	if err != nil || !state.Open {
		t.Fatalf("open state = %#v, %v", state, err)
	}
	server.FastForward(10 * time.Second)
	if _, err := store.Allow(ctx, "provider", "model"); err != nil {
		t.Fatal(err)
	}
	state, err = store.RouteHealthState(ctx, "provider", "model")
	if err != nil || state.Open || !state.HalfOpen {
		t.Fatalf("half-open state = %#v, %v", state, err)
	}
}
