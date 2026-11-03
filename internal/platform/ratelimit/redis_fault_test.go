package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisLimiterFailsClosedThenRecovers 验证 Redis 丢失期间限流检查报错、恢复后自动继续工作。
// 关键约束：限流不可用时由鉴权层返回 503，禁止静默放行；Redis 恢复后无需重启应用。
func TestRedisLimiterFailsClosedThenRecovers(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(), MaxRetries: 2, MinRetryBackoff: 10 * time.Millisecond, MaxRetryBackoff: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := NewRedisLimiter(client, "login", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if result, err := limiter.Allow(ctx, "alice"); err != nil || !result.Allowed {
		t.Fatalf("initial allow = %#v, %v", result, err)
	}
	server.Close()
	if _, err := limiter.Allow(ctx, "alice"); err == nil {
		t.Fatal("limiter must report redis unavailability")
	}
	if err := server.Restart(); err != nil {
		t.Fatal(err)
	}
	if result, err := limiter.Allow(ctx, "alice"); err != nil || !result.Allowed {
		t.Fatalf("recovered allow = %#v, %v", result, err)
	}
}
