package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/redis/go-redis/v9"
)

// refreshReplayPrefix 是刷新并发重放记录的 Redis Key 前缀。
const refreshReplayPrefix = "auth:refresh:replay:"

// RefreshReplayStore 基于 Redis 实现刷新轮换短时重放。
// Key 使用令牌哈希的二次哈希，避免原始哈希与令牌信息出现在缓存键中。
type RefreshReplayStore struct {
	client *redis.Client
}

// NewRefreshReplayStore 构造刷新重放存储。
func NewRefreshReplayStore(client *redis.Client) (*RefreshReplayStore, error) {
	if client == nil {
		return nil, fmt.Errorf("redis client is required")
	}
	return &RefreshReplayStore{client: client}, nil
}

// SaveRefreshReplay 写入重放结果并设置 TTL；记录永不落日志。
func (s *RefreshReplayStore) SaveRefreshReplay(ctx context.Context, tokenHash []byte, replay service.RefreshReplay, ttl time.Duration) error {
	encoded, err := json.Marshal(replay)
	if err != nil {
		return fmt.Errorf("encode refresh replay: %w", err)
	}
	if err := s.client.Set(ctx, replayKey(tokenHash), encoded, ttl).Err(); err != nil {
		return fmt.Errorf("store refresh replay: %w", err)
	}
	return nil
}

// LoadRefreshReplay 读取重放结果；缓存未命中返回 found=false。
func (s *RefreshReplayStore) LoadRefreshReplay(ctx context.Context, tokenHash []byte) (service.RefreshReplay, bool, error) {
	raw, err := s.client.Get(ctx, replayKey(tokenHash)).Bytes()
	if err == redis.Nil {
		return service.RefreshReplay{}, false, nil
	}
	if err != nil {
		return service.RefreshReplay{}, false, fmt.Errorf("load refresh replay: %w", err)
	}
	var replay service.RefreshReplay
	if err := json.Unmarshal(raw, &replay); err != nil {
		return service.RefreshReplay{}, false, fmt.Errorf("decode refresh replay: %w", err)
	}
	return replay, true, nil
}

// replayKey 计算重放键：对令牌哈希再做一次 SHA-256，避免缓存键泄露原始哈希。
func replayKey(tokenHash []byte) string {
	sum := sha256.Sum256(tokenHash)
	return refreshReplayPrefix + hex.EncodeToString(sum[:])
}

var _ service.RefreshReplayStore = (*RefreshReplayStore)(nil)
