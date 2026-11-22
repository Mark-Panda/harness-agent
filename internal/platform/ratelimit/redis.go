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

// tokenBucket 是跨实例原子令牌桶：按经过时间补充令牌，避免固定窗口在边界处放行双倍请求。
// KEYS[1] 桶键；ARGV[1] 当前毫秒；ARGV[2] 每毫秒补充速率；ARGV[3] 容量；ARGV[4] 过期毫秒。
var tokenBucket = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local capacity = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local bucket = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(bucket[1])
local ts = tonumber(bucket[2])
if tokens == nil or ts == nil then
  tokens = capacity
  ts = now
end
-- 时钟回拨时按零补充处理，避免出现负增量放大令牌。
local elapsed = now - ts
if elapsed < 0 then
  elapsed = 0
end
tokens = math.min(capacity, tokens + elapsed * rate)
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', key, ttl)
local retry = 0
if allowed == 0 then
  retry = math.ceil((1 - tokens) / rate)
end
return {allowed, retry}
`)

func NewRedisLimiter(client *redis.Client, prefix string, limit int, window time.Duration) (*RedisLimiter, error) {
	if client == nil || prefix == "" || limit <= 0 || window <= 0 {
		return nil, errors.New("redis client, prefix, limit and window are required")
	}
	// 令牌补充速率按毫秒计算；窗口小于 1ms 会被除零放大为 +Inf，统一抬到 1ms。
	if window < time.Millisecond {
		window = time.Millisecond
	}
	return &RedisLimiter{client: client, prefix: prefix, limit: limit, window: window}, nil
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (Result, error) {
	if key == "" {
		return Result{}, errors.New("rate limit key is required")
	}
	redisKey := l.redisKey(key)
	now := time.Now().UnixMilli()
	// 速率以“每毫秒补充令牌数”表示，容量等于窗口内允许的请求数。
	rate := float64(l.limit) / float64(l.window.Milliseconds())
	values, err := tokenBucket.Run(ctx, l.client, []string{redisKey},
		now, rate, l.limit, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(values) != 2 {
		return Result{}, errors.New("unexpected redis rate limit response")
	}
	result := Result{Allowed: values[0] == 1}
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
