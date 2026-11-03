package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestHotMessageStoreRecoversAfterRedisRestart 验证 Redis 丢失时热缓存报错、恢复后自动重建连接并保留数据。
// 关键约束：Redis 故障只降级为 PostgreSQL 回源，不得让进程崩溃；恢复后无需重启应用。
func TestHotMessageStoreRecoversAfterRedisRestart(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(), MaxRetries: 2, MinRetryBackoff: 10 * time.Millisecond, MaxRetryBackoff: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewHotMessageStore(client, 20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	userID, sessionID := uuid.New(), uuid.New()
	message := biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "hello"}
	if err := store.Append(context.Background(), userID, sessionID, message); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.Load(context.Background(), userID, sessionID)
	if err != nil || !hit || len(loaded) != 1 || loaded[0].Content != "hello" {
		t.Fatalf("initial load = %#v hit=%v err=%v", loaded, hit, err)
	}

	// 模拟 Redis 进程丢失：读取必须返回错误，由 Worker 降级到 PostgreSQL。
	server.Close()
	if _, _, err := store.Load(context.Background(), userID, sessionID); err == nil {
		t.Fatal("load must fail while redis is down")
	}

	// 同端口恢复后客户端自动重连，缓存数据保留。
	if err := server.Restart(); err != nil {
		t.Fatal(err)
	}
	recovered, hit, err := store.Load(context.Background(), userID, sessionID)
	if err != nil || !hit || len(recovered) != 1 || recovered[0].Content != "hello" {
		t.Fatalf("recovered load = %#v hit=%v err=%v", recovered, hit, err)
	}
}
