//go:build integration

package data

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestRedisRefreshReplayStoreRoundTrip 验证刷新重放结果的保存、读取与 TTL 过期。
func TestRedisRefreshReplayStoreRoundTrip(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", Password: "harness"})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("requires Redis: %v", err)
	}
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	store, err := NewRefreshReplayStore(client)
	if err != nil {
		t.Fatal(err)
	}

	tokenHash := sha256.Sum256([]byte("refresh-" + uuid.NewString()))
	replay := service.RefreshReplay{
		Result: service.AuthResult{
			User: service.PublicUser{ID: uuid.New(), Username: "replay-user"},
			Tokens: service.TokenPair{
				AccessToken: "access-token", RefreshToken: "refresh-token", TokenType: "Bearer",
				AccessExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
			},
		},
		SuccessorHash: []byte("successor-hash-bytes"),
	}
	defer func() { _ = client.Del(ctx, replayKey(tokenHash[:])).Err() }()

	// 未保存的哈希读取为 miss。
	if _, found, err := store.LoadRefreshReplay(ctx, tokenHash[:]); err != nil || found {
		t.Fatalf("empty LoadRefreshReplay found=%v err=%v", found, err)
	}
	if err := store.SaveRefreshReplay(ctx, tokenHash[:], replay, time.Minute); err != nil {
		t.Fatalf("SaveRefreshReplay() = %v", err)
	}
	loaded, found, err := store.LoadRefreshReplay(ctx, tokenHash[:])
	if err != nil || !found {
		t.Fatalf("LoadRefreshReplay found=%v err=%v", found, err)
	}
	if loaded.Result.Tokens.AccessToken != "access-token" || loaded.Result.User.Username != "replay-user" {
		t.Fatalf("loaded replay = %#v", loaded)
	}
	if string(loaded.SuccessorHash) != string(replay.SuccessorHash) {
		t.Fatalf("successor hash mismatch")
	}

	// 极短 TTL 过期后读取为 miss。
	shortHash := sha256.Sum256([]byte("short-" + uuid.NewString()))
	if err := store.SaveRefreshReplay(ctx, shortHash[:], replay, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, found, err := store.LoadRefreshReplay(ctx, shortHash[:]); err != nil || found {
		t.Fatalf("expired LoadRefreshReplay found=%v err=%v", found, err)
	}
}
