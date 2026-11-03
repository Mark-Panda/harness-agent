package service

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeRefreshReplayStore 是内存版刷新重放存储，用于验证并发刷新语义。
type fakeRefreshReplayStore struct {
	mu      sync.Mutex
	records map[string]RefreshReplay
	saveErr error
	loadErr error
}

// newFakeRefreshReplayStore 构造内存重放存储。
func newFakeRefreshReplayStore() *fakeRefreshReplayStore {
	return &fakeRefreshReplayStore{records: map[string]RefreshReplay{}}
}

// replayKey 将令牌哈希编码为测试用键。
func replayKey(hash []byte) string { return hex.EncodeToString(hash) }

// SaveRefreshReplay 保存轮换结果。
func (s *fakeRefreshReplayStore) SaveRefreshReplay(_ context.Context, tokenHash []byte, replay RefreshReplay, _ time.Duration) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[replayKey(tokenHash)] = replay
	return nil
}

// LoadRefreshReplay 读取轮换结果。
func (s *fakeRefreshReplayStore) LoadRefreshReplay(_ context.Context, tokenHash []byte) (RefreshReplay, bool, error) {
	if s.loadErr != nil {
		return RefreshReplay{}, false, s.loadErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	replay, ok := s.records[replayKey(tokenHash)]
	return replay, ok, nil
}

// replayServiceFixture 在基础夹具上装配刷新重放存储。
func replayServiceFixture(t *testing.T, store RefreshReplayStore) *authServiceFixture {
	t.Helper()
	fixture := newAuthServiceFixture(t)
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithRefreshReplay(store, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = service
	return fixture
}

// replayFixture 构造一条带有效后继哈希的重放记录。
func replayFixture(t *testing.T, fixture *authServiceFixture, user biz.User) (RefreshReplay, []byte) {
	t.Helper()
	successorRaw := "successor-" + uuid.NewString()
	successorHash, err := auth.HashOpaqueToken(successorRaw, fixture.pepper)
	if err != nil {
		t.Fatal(err)
	}
	replay := RefreshReplay{
		Result: AuthResult{
			User: PublicUser{ID: user.ID, Username: user.Username, Role: user.Role, Status: user.Status},
			Tokens: TokenPair{
				AccessToken: "winner-access", RefreshToken: successorRaw, TokenType: "Bearer",
				AccessExpiresAt: fixture.clock.Now().Add(time.Minute), RefreshExpiresAt: fixture.clock.Now().Add(24 * time.Hour),
			},
		},
		SuccessorHash: successorHash,
	}
	return replay, successorHash
}

func TestRefreshReplayForAlreadyRotatedToken(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	rawToken := "concurrent-refresh"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	rotatedAt := fixture.clock.Now().Add(-time.Second)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1, RotatedAt: &rotatedAt}
	replayed, successorHash := replayFixture(t, fixture, user)
	store.records[replayKey(hash)] = replayed
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), successorHash).Return(biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: oldToken.FamilyID, TokenHash: successorHash}, nil)
	result, err := fixture.service.Refresh(context.Background(), rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tokens.RefreshToken != replayed.Result.Tokens.RefreshToken {
		t.Fatalf("expected replayed token pair, got %#v", result.Tokens)
	}
}

func TestRefreshRotationConflictReplaysWinnerResult(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	rawToken := "conflict-refresh"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1, ExpiresAt: fixture.clock.Now().Add(time.Hour)}
	replayed, successorHash := replayFixture(t, fixture, user)
	store.records[replayKey(hash)] = replayed
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RotateRefreshToken(gomock.Any(), user.ID, oldToken.ID, hash, gomock.Any(), gomock.Any()).Return(biz.ErrRefreshTokenConflict)
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), successorHash).Return(biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: oldToken.FamilyID, TokenHash: successorHash}, nil)
	result, err := fixture.service.Refresh(context.Background(), rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tokens.RefreshToken != replayed.Result.Tokens.RefreshToken {
		t.Fatalf("expected winner token pair, got %#v", result.Tokens)
	}
}

func TestRefreshReplayRejectedAfterLogout(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	rawToken := "logged-out-refresh"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	rotatedAt := fixture.clock.Now().Add(-time.Minute)
	revokedAt := fixture.clock.Now()
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1, RotatedAt: &rotatedAt}
	replayed, successorHash := replayFixture(t, fixture, user)
	store.records[replayKey(hash)] = replayed
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), successorHash).Return(biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: oldToken.FamilyID, TokenHash: successorHash, RevokedAt: &revokedAt}, nil).Times(4)
	fixture.tokens.EXPECT().RevokeFamily(gomock.Any(), user.ID, oldToken.FamilyID, fixture.clock.Now().UTC(), true, gomock.Any()).Return(nil)
	if _, err := fixture.service.Refresh(context.Background(), rawToken); !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("replay after logout error = %v", err)
	}
}

func TestRefreshSuccessStoresReplay(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	rawToken := "rotate-refresh"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1, ExpiresAt: fixture.clock.Now().Add(time.Hour)}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RotateRefreshToken(gomock.Any(), user.ID, oldToken.ID, hash, gomock.Any(), gomock.Any()).Return(nil)
	result, err := fixture.service.Refresh(context.Background(), rawToken)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := store.records[replayKey(hash)]
	if !ok || stored.Result.Tokens.RefreshToken != result.Tokens.RefreshToken || len(stored.SuccessorHash) != 32 {
		t.Fatalf("replay record missing or mismatched: %#v", stored)
	}
}

// TestWaitReplayRejectsInvalidRecords 验证重放记录的用户、令牌与后继校验。
func TestWaitReplayRejectsInvalidRecords(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	oldToken := biz.RefreshToken{UserID: user.ID}
	hash := []byte("replay-hash")
	ctx := context.Background()
	if _, ok := fixture.service.waitReplay(ctx, oldToken, hash); ok {
		t.Fatal("missing replay record was accepted")
	}
	store.records[replayKey(hash)] = RefreshReplay{
		Result:        AuthResult{User: PublicUser{ID: uuid.New()}, Tokens: TokenPair{AccessToken: "a", RefreshToken: "b"}},
		SuccessorHash: []byte("successor"),
	}
	if _, ok := fixture.service.waitReplay(ctx, oldToken, hash); ok {
		t.Fatal("replay for another user was accepted")
	}
	store.records[replayKey(hash)] = RefreshReplay{
		Result: AuthResult{User: PublicUser{ID: user.ID}, Tokens: TokenPair{AccessToken: "a", RefreshToken: "b"}},
	}
	if _, ok := fixture.service.waitReplay(ctx, oldToken, hash); ok {
		t.Fatal("replay without successor hash was accepted")
	}
	store.records[replayKey(hash)] = RefreshReplay{
		Result:        AuthResult{User: PublicUser{ID: user.ID}, Tokens: TokenPair{AccessToken: "a"}},
		SuccessorHash: []byte("successor"),
	}
	if _, ok := fixture.service.waitReplay(ctx, oldToken, hash); ok {
		t.Fatal("incomplete token pair was accepted")
	}
	store.loadErr = errors.New("redis down")
	if _, ok := fixture.service.waitReplay(ctx, oldToken, hash); ok {
		t.Fatal("replay store error was treated as a hit")
	}
}

// TestWaitReplayHonorsContextCancellation 验证取消上下文会立即结束重放等待。
func TestWaitReplayHonorsContextCancellation(t *testing.T) {
	store := newFakeRefreshReplayStore()
	fixture := replayServiceFixture(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := fixture.service.waitReplay(ctx, biz.RefreshToken{UserID: uuid.New()}, []byte("hash")); ok {
		t.Fatal("canceled replay wait returned a record")
	}
}

// TestWithRefreshReplayDefaultTTL 验证零值 TTL 回退到默认窗口。
func TestWithRefreshReplayDefaultTTL(t *testing.T) {
	fixture := newAuthServiceFixture(t)
	service, err := NewAuthService(fixture.users, fixture.tokens, fixture.jwt, fixture.pepper, fixture.clock, WithRefreshReplay(newFakeRefreshReplayStore(), 0))
	if err != nil {
		t.Fatal(err)
	}
	if service.replayTTL != DefaultRefreshReplayTTL {
		t.Fatalf("default replay ttl = %s", service.replayTTL)
	}
}

// TestRefreshReplaySaveFailureIsNonFatal 验证重放写入失败不影响已完成的轮换。
func TestRefreshReplaySaveFailureIsNonFatal(t *testing.T) {
	store := newFakeRefreshReplayStore()
	store.saveErr = errors.New("redis down")
	fixture := replayServiceFixture(t, store)
	user := activeUser(t)
	rawToken := "rotate-despite-replay-error"
	hash, _ := auth.HashOpaqueToken(rawToken, fixture.pepper)
	oldToken := biz.RefreshToken{ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: hash, CredentialVersion: 1, ExpiresAt: fixture.clock.Now().Add(time.Hour)}
	fixture.tokens.EXPECT().GetRefreshTokenByHash(gomock.Any(), hash).Return(oldToken, nil)
	fixture.users.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	fixture.tokens.EXPECT().RotateRefreshToken(gomock.Any(), user.ID, oldToken.ID, hash, gomock.Any(), gomock.Any()).Return(nil)
	result, err := fixture.service.Refresh(context.Background(), rawToken)
	if err != nil || result.Tokens.RefreshToken == "" {
		t.Fatalf("Refresh() = %#v, %v", result, err)
	}
}
