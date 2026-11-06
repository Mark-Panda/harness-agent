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
	if err := store.Save(context.Background(), userID, sessionID, []biz.Message{message}); err != nil {
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

// TestHotMessageStoreRejectsPartialWindow 验证缓存缺失或残缺时不会被单条追加伪造成完整窗口。
// 关键约束：Redis 重启、TTL 过期或飞书链路清理后，缓存未命中必须回源 PostgreSQL，不能静默丢历史。
func TestHotMessageStoreRejectsPartialWindow(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewHotMessageStore(client, 20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, sessionID := uuid.New(), uuid.New()

	// 缓存不存在时追加是空操作：不得创建只有一条消息的残窗。
	userMessage := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "hello", SpeakerOpenID: "ou_speaker", CreatedAt: time.Now().UTC()}
	if err := store.Append(ctx, userID, sessionID, userMessage); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
		t.Fatalf("append on missing cache must stay miss, hit=%v err=%v", hit, err)
	}

	// 没有用户消息的窗口视为残缺，读取时清理并要求回源。
	if err := store.Save(ctx, userID, sessionID, []biz.Message{{ID: uuid.New(), Role: biz.MessageAssistant, Content: "answer"}}); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
		t.Fatalf("window without user message must be miss, hit=%v err=%v", hit, err)
	}
	if _, _, err := store.Load(ctx, userID, sessionID); err != nil {
		t.Fatal(err)
	}

	// 正常窗口保留群聊发言人信息。
	if err := store.Save(ctx, userID, sessionID, []biz.Message{userMessage}); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.Load(ctx, userID, sessionID)
	if err != nil || !hit || len(loaded) != 1 || loaded[0].SpeakerOpenID != "ou_speaker" || !loaded[0].CreatedAt.Equal(userMessage.CreatedAt) {
		t.Fatalf("speaker round trip = %#v hit=%v err=%v", loaded, hit, err)
	}
}
