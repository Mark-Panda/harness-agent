package ratelimit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Result struct {
	Allowed    bool
	RetryAfter time.Duration
}

type RedisLimiter struct {
	client *redis.Client
	prefix string
	limit  int
	window time.Duration
}

var increment = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
return {count, ttl}
`)

func NewRedisLimiter(client *redis.Client, prefix string, limit int, window time.Duration) (*RedisLimiter, error) {
	if client == nil || prefix == "" || limit <= 0 || window <= 0 {
		return nil, errors.New("redis client, prefix, limit and window are required")
	}
	return &RedisLimiter{client: client, prefix: prefix, limit: limit, window: window}, nil
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (Result, error) {
	if key == "" {
		return Result{}, errors.New("rate limit key is required")
	}
	redisKey := l.redisKey(key)
	values, err := increment.Run(ctx, l.client, []string{redisKey}, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(values) != 2 {
		return Result{}, errors.New("unexpected redis rate limit response")
	}
	result := Result{Allowed: values[0] <= int64(l.limit)}
	if !result.Allowed {
		result.RetryAfter = time.Duration(values[1]) * time.Millisecond
		if result.RetryAfter < 0 {
			result.RetryAfter = l.window
		}
	}
	return result, nil
}

func (l *RedisLimiter) Reset(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	return l.client.Del(ctx, l.redisKey(key)).Err()
}

func (l *RedisLimiter) redisKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("ratelimit:%s:%x", l.prefix, hash)
}
