package data

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// concurrencyScript 原子清理过期成员、检查容量并占用槽位。
// 使用有序集合保存成员租约，Worker 崩溃后槽位会在 lease 到期后自动回收。
var concurrencyScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local lease = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - lease)
local count = redis.call('ZCARD', KEYS[1])
if count < limit then
  redis.call('ZADD', KEYS[1], now, member)
  redis.call('PEXPIRE', KEYS[1], lease)
  return 1
end
return 0
`)

// RedisConcurrencyLimiter 使用 Redis 有序集合实现跨副本并发舱壁。
type RedisConcurrencyLimiter struct {
	// client Redis 客户端。
	client redis.UniversalClient
	// limit 每个 Key 允许的最大并发数。
	limit int
	// lease 槽位租约；持有者崩溃后自动回收。
	lease time.Duration
	// waitInterval Acquire 的轮询间隔。
	waitInterval time.Duration
}

// NewRedisConcurrencyLimiter 构造舱壁；参数非法时返回错误。
func NewRedisConcurrencyLimiter(client redis.UniversalClient, limit int, lease, waitInterval time.Duration) (*RedisConcurrencyLimiter, error) {
	if client == nil || limit <= 0 || lease <= 0 {
		return nil, errors.New("concurrency limiter requires redis, positive limit and lease")
	}
	if waitInterval <= 0 {
		waitInterval = 50 * time.Millisecond
	}
	return &RedisConcurrencyLimiter{client: client, limit: limit, lease: lease, waitInterval: waitInterval}, nil
}

// Acquire 阻塞等待槽位，直到获取成功或上下文取消。
func (l *RedisConcurrencyLimiter) Acquire(ctx context.Context, key string) (func(), error) {
	for {
		release, acquired, err := l.TryAcquire(ctx, key)
		if err != nil {
			return nil, err
		}
		if acquired {
			return release, nil
		}
		timer := time.NewTimer(l.waitInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// TryAcquire 非阻塞尝试占用槽位，返回是否获得以及释放函数。
func (l *RedisConcurrencyLimiter) TryAcquire(ctx context.Context, key string) (func(), bool, error) {
	member := uuid.NewString()
	redisKey := "concurrency:" + key
	now := time.Now().UTC().UnixMilli()
	result, err := concurrencyScript.Run(ctx, l.client, []string{redisKey},
		now, l.lease.Milliseconds(), l.limit, member,
	).Int()
	if err != nil {
		return nil, false, err
	}
	if result == 0 {
		return nil, false, nil
	}
	return func() {
		// 释放使用独立上下文，避免 Run 取消后槽位泄漏到租约到期。
		releaseContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.client.ZRem(releaseContext, redisKey, member).Err()
	}, true, nil
}
