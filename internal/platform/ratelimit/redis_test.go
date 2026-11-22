package ratelimit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisLimiterRoundsSubMillisecondWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	limiter, err := NewRedisLimiter(client, "tiny", 1, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	result, err := limiter.Allow(context.Background(), "key")
	if err != nil || !result.Allowed {
		t.Fatalf("sub-millisecond window = %#v, %v", result, err)
	}
}

func TestRedisLimiterRefillsOverTime(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	limiter, err := NewRedisLimiter(client, "refill", 1, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if result, err := limiter.Allow(ctx, "key"); err != nil || !result.Allowed {
		t.Fatalf("first = %#v, %v", result, err)
	}
	if result, err := limiter.Allow(ctx, "key"); err != nil || result.Allowed {
		t.Fatalf("second = %#v, %v", result, err)
	}
	time.Sleep(60 * time.Millisecond)
	if result, err := limiter.Allow(ctx, "key"); err != nil || !result.Allowed {
		t.Fatalf("after refill = %#v, %v", result, err)
	}
}

func TestRedisLimiterLimitAndReset(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	limiter, err := NewRedisLimiter(client, "login", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		result, err := limiter.Allow(ctx, "alice|127.0.0.1")
		if err != nil || !result.Allowed {
			t.Fatalf("attempt %d = %#v, %v", attempt, result, err)
		}
	}
	result, err := limiter.Allow(ctx, "alice|127.0.0.1")
	if err != nil || result.Allowed || result.RetryAfter <= 0 {
		t.Fatalf("third attempt = %#v, %v", result, err)
	}
	for _, key := range client.Keys(ctx, "*").Val() {
		if strings.Contains(key, "alice") || strings.Contains(key, "127.0.0.1") {
			t.Fatalf("rate limit key contains plaintext identity: %s", key)
		}
	}
	if err := limiter.Reset(ctx, "alice|127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	result, err = limiter.Allow(ctx, "alice|127.0.0.1")
	if err != nil || !result.Allowed {
		t.Fatalf("after reset = %#v, %v", result, err)
	}
}
