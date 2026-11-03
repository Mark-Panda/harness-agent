package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestConcurrencyLimiterTryAcquireAndRelease 验证容量、释放与租约自动回收。
func TestConcurrencyLimiterTryAcquireAndRelease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := data.NewRedisConcurrencyLimiter(client, 1, 50*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	release, acquired, err := limiter.TryAcquire(ctx, "provider:p1")
	if err != nil || !acquired {
		t.Fatalf("first acquire = %t, %v", acquired, err)
	}
	if _, acquired, err := limiter.TryAcquire(ctx, "provider:p1"); err != nil || acquired {
		t.Fatalf("second acquire = %t, %v", acquired, err)
	}
	// 不同 Key 互不影响。
	if _, acquired, err := limiter.TryAcquire(ctx, "provider:p2"); err != nil || !acquired {
		t.Fatalf("different key acquire = %t, %v", acquired, err)
	}
	// 租约到期后槽位自动回收。
	time.Sleep(80 * time.Millisecond)
	if _, acquired, err := limiter.TryAcquire(ctx, "provider:p1"); err != nil || !acquired {
		t.Fatalf("expired lease acquire = %t, %v", acquired, err)
	}
	release()
}

// TestConcurrencyLimiterAcquireWaitsForRelease 验证阻塞等待与上下文取消。
func TestConcurrencyLimiterAcquireWaitsForRelease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := data.NewRedisConcurrencyLimiter(client, 1, time.Minute, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, acquired, err := limiter.TryAcquire(context.Background(), "user:u1")
	if err != nil || !acquired {
		t.Fatalf("initial acquire = %t, %v", acquired, err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		release()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	blockedRelease, err := limiter.Acquire(ctx, "user:u1")
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	blockedRelease()

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := limiter.Acquire(cancelled, "user:u1"); err == nil {
		t.Fatal("cancelled Acquire() did not fail")
	}
}

// TestConcurrencyLimiterValidation 验证构造参数与 Redis 故障错误。
func TestConcurrencyLimiterValidation(t *testing.T) {
	if _, err := data.NewRedisConcurrencyLimiter(nil, 1, time.Minute, time.Millisecond); err == nil {
		t.Fatal("nil redis was accepted")
	}
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	limiter, _ := data.NewRedisConcurrencyLimiter(client, 1, time.Minute, time.Millisecond)
	server.Close()
	if _, _, err := limiter.TryAcquire(context.Background(), "user:u1"); err == nil {
		t.Fatal("closed redis did not return an error")
	}
	_ = client.Close()
}
