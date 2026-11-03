//go:build integration

package data

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// integrationRedisClient 构造集成测试 Redis 客户端。
func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	address := os.Getenv("REDIS_ADDR")
	if address == "" {
		address = "localhost:6379"
	}
	password := os.Getenv("REDIS_PASSWORD")
	if password == "" {
		password = "harness"
	}
	client := redis.NewClient(&redis.Options{Addr: address, Password: password})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestHotMessageStoreLifecycle 验证 Redis 热窗口的读写、幂等追加、裁剪、TTL 与损坏数据清理。
func TestHotMessageStoreLifecycle(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires Redis")
	}
	client := integrationRedisClient(t)
	ctx := context.Background()
	userID, sessionID := uuid.New(), uuid.New()
	otherSession := uuid.New()
	store, err := NewHotMessageStore(client, 3, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = store.Invalidate(ctx, userID, sessionID)
		_ = store.Invalidate(ctx, userID, otherSession)
	}()

	base := time.Now().UTC()
	messages := []biz.Message{
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "第一条", CreatedAt: base},
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageAssistant, Content: "第二条", ToolCalls: []map[string]any{{"id": "call-1", "name": "search", "arguments": "{}"}}, CreatedAt: base.Add(time.Second)},
		{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageTool, Content: `{"ok":true}`, ToolCallID: "call-1", ToolName: "search", CreatedAt: base.Add(2 * time.Second)},
	}
	if err := store.Save(ctx, userID, sessionID, messages); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.Load(ctx, userID, sessionID)
	if err != nil || !hit || len(loaded) != 3 {
		t.Fatalf("Load() = %d, %t, %v", len(loaded), hit, err)
	}
	if loaded[1].Content != "第二条" || len(loaded[1].ToolCalls) != 1 || loaded[2].ToolCallID != "call-1" {
		t.Fatalf("loaded messages = %#v", loaded)
	}
	if ttl, err := client.TTL(ctx, hotMessageKey(userID, sessionID)).Result(); err != nil || ttl <= 0 {
		t.Fatalf("ttl = %v, %v", ttl, err)
	}

	// 幂等追加：相同载荷重复写入不会产生重复消息。
	appended := biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "第三条", CreatedAt: base.Add(3 * time.Second)}
	for index := 0; index < 2; index++ {
		if err := store.Append(ctx, userID, sessionID, appended); err != nil {
			t.Fatal(err)
		}
	}
	loaded, hit, err = store.Load(ctx, userID, sessionID)
	if err != nil || !hit || len(loaded) != 3 {
		t.Fatalf("after append Load() = %d, %t, %v", len(loaded), hit, err)
	}
	if loaded[2].Content != "第三条" {
		t.Fatalf("appended message not at tail: %#v", loaded)
	}

	// 长度上限为 3：追加第四条后最旧的消息被裁剪。
	if err := store.Append(ctx, userID, sessionID, biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "第四条", CreatedAt: base.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err = store.Load(ctx, userID, sessionID)
	if err != nil || len(loaded) != 3 || loaded[0].Role != biz.MessageTool || loaded[1].Content != "第三条" || loaded[2].Content != "第四条" {
		t.Fatalf("trimmed messages = %#v, %v", loaded, err)
	}

	// 空列表必须清理缓存而不是缓存空值。
	if err := store.Save(ctx, userID, sessionID, nil); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
		t.Fatalf("empty save Load() hit = %t, %v", hit, err)
	}
	// 不同会话的 Key 不共享数据。
	if err := store.Save(ctx, userID, otherSession, messages[:1]); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
		t.Fatalf("cross-session cache hit = %t, %v", hit, err)
	}
	other, hit, err := store.Load(ctx, userID, otherSession)
	if err != nil || !hit || len(other) != 1 {
		t.Fatalf("other session Load() = %#v, %t, %v", other, hit, err)
	}

	// 损坏数据必须被清理并返回错误，而不是把坏数据注入上下文。
	if err := client.RPush(ctx, hotMessageKey(userID, sessionID), "not-json").Err(); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err == nil || hit {
		t.Fatalf("corrupt cache Load() hit = %t, err = %v", hit, err)
	}
	if exists, _ := client.Exists(ctx, hotMessageKey(userID, sessionID)).Result(); exists != 0 {
		t.Fatalf("corrupt cache was not invalidated")
	}
	if err := store.Append(ctx, uuid.Nil, sessionID, appended); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("nil user append error = %v", err)
	}
}
