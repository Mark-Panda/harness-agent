package data_test

import (
	"context"
	"fmt"
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

func newHotMessageConcurrencyStore(t *testing.T) (*data.HotMessageRepository, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := data.NewHotMessageStore(client, 20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

func hotMessageCacheKeys(userID, sessionID uuid.UUID) (string, string) {
	key := fmt.Sprintf("session:%s:%s:messages", userID, sessionID)
	return key, key + ":metadata"
}

// TestHotMessageStoreSaveDoesNotOverwriteConcurrentAppend 验证旧 PG 快照不能覆盖 Append 推进的高水位。
func TestHotMessageStoreSaveDoesNotOverwriteConcurrentAppend(t *testing.T) {
	store, client := newHotMessageConcurrencyStore(t)
	ctx := context.Background()
	userID, sessionID := uuid.New(), uuid.New()
	base := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	first := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "first", CreatedAt: base}
	second := biz.Message{ID: uuid.New(), Role: biz.MessageAssistant, Content: "second", CreatedAt: base.Add(time.Second)}
	concurrent := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "concurrent", CreatedAt: base.Add(2 * time.Second)}
	if err := store.Save(ctx, userID, sessionID, []biz.Message{first, second}); err != nil {
		t.Fatal(err)
	}
	// PG 已返回 first/second 后、Save 重建前，另一个请求持久化并 Append 了 concurrent。
	if err := store.Append(ctx, userID, sessionID, concurrent); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, userID, sessionID, []biz.Message{first, second}); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.Load(ctx, userID, sessionID)
	if err != nil || !hit || len(loaded) != 3 || loaded[2].ID != concurrent.ID {
		t.Fatalf("concurrent append was lost: loaded=%#v hit=%v err=%v", loaded, hit, err)
	}
	_, metadataKey := hotMessageCacheKeys(userID, sessionID)
	version, err := client.HGet(ctx, metadataKey, "version").Int64()
	if err != nil || version != 2 {
		t.Fatalf("stale save advanced cache version: version=%d err=%v", version, err)
	}
	// 包含当前高水位的新快照允许重建，并继续推进版本。
	if err := store.Save(ctx, userID, sessionID, []biz.Message{first, second, concurrent}); err != nil {
		t.Fatal(err)
	}
	version, err = client.HGet(ctx, metadataKey, "version").Int64()
	if err != nil || version != 3 {
		t.Fatalf("fresh save did not advance cache version: version=%d err=%v", version, err)
	}
}

// TestHotMessageStoreMissingWindowKeepsAppendHighWater 验证空缓存 Append 不会被旧快照伪造成完整窗口。
func TestHotMessageStoreMissingWindowKeepsAppendHighWater(t *testing.T) {
	store, client := newHotMessageConcurrencyStore(t)
	ctx := context.Background()
	userID, sessionID := uuid.New(), uuid.New()
	base := time.Now().UTC()
	stale := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "stale", CreatedAt: base}
	concurrent := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "new", CreatedAt: base.Add(time.Second)}
	if err := store.Append(ctx, userID, sessionID, concurrent); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, userID, sessionID, []biz.Message{stale}); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
		t.Fatalf("stale snapshot must stay a cache miss: hit=%v err=%v", hit, err)
	}
	listKey, metadataKey := hotMessageCacheKeys(userID, sessionID)
	if newest, err := client.HGet(ctx, metadataKey, "newest").Result(); err != nil || newest != concurrent.ID.String() {
		t.Fatalf("append high water was not preserved: newest=%q err=%v", newest, err)
	}
	if exists, err := client.Exists(ctx, listKey).Result(); err != nil || exists != 0 {
		t.Fatalf("partial list was created: exists=%d err=%v", exists, err)
	}
	// 下次 PG 回源包含并发消息后，才允许重建并命中完整窗口。
	if err := store.Save(ctx, userID, sessionID, []biz.Message{stale, concurrent}); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := store.Load(ctx, userID, sessionID)
	if err != nil || !hit || len(loaded) != 2 || loaded[1].ID != concurrent.ID {
		t.Fatalf("fresh postgres snapshot did not recover messages: loaded=%#v hit=%v err=%v", loaded, hit, err)
	}
}

// TestHotMessageStoreRejectsInvalidVersionedWindow 验证元数据缺失、重复与乱序窗口全部回源。
func TestHotMessageStoreRejectsInvalidVersionedWindow(t *testing.T) {
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	newMessages := func() []biz.Message {
		return []biz.Message{
			{ID: uuid.New(), Role: biz.MessageUser, Content: "first", CreatedAt: base},
			{ID: uuid.New(), Role: biz.MessageAssistant, Content: "second", CreatedAt: base.Add(time.Second)},
		}
	}
	t.Run("missing metadata", func(t *testing.T) {
		store, client := newHotMessageConcurrencyStore(t)
		ctx := context.Background()
		userID, sessionID := uuid.New(), uuid.New()
		if err := store.Save(ctx, userID, sessionID, newMessages()); err != nil {
			t.Fatal(err)
		}
		_, metadataKey := hotMessageCacheKeys(userID, sessionID)
		if err := client.Del(ctx, metadataKey).Err(); err != nil {
			t.Fatal(err)
		}
		if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
			t.Fatalf("missing metadata must miss: hit=%v err=%v", hit, err)
		}
	})
	t.Run("duplicate message", func(t *testing.T) {
		store, _ := newHotMessageConcurrencyStore(t)
		ctx := context.Background()
		userID, sessionID := uuid.New(), uuid.New()
		messages := newMessages()
		if err := store.Save(ctx, userID, sessionID, messages); err != nil {
			t.Fatal(err)
		}
		duplicate := messages[1]
		duplicate.Content = "different payload with duplicate id"
		if err := store.Append(ctx, userID, sessionID, duplicate); err != nil {
			t.Fatal(err)
		}
		if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
			t.Fatalf("duplicate message must miss: hit=%v err=%v", hit, err)
		}
	})
	t.Run("out of order message", func(t *testing.T) {
		store, _ := newHotMessageConcurrencyStore(t)
		ctx := context.Background()
		userID, sessionID := uuid.New(), uuid.New()
		if err := store.Save(ctx, userID, sessionID, newMessages()); err != nil {
			t.Fatal(err)
		}
		older := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "older", CreatedAt: base.Add(-time.Second)}
		if err := store.Append(ctx, userID, sessionID, older); err != nil {
			t.Fatal(err)
		}
		if _, hit, err := store.Load(ctx, userID, sessionID); err != nil || hit {
			t.Fatalf("out-of-order message must miss: hit=%v err=%v", hit, err)
		}
	})
}
