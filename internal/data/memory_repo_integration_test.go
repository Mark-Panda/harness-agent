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
)

// memoryIntegrationVector 构造一个只在指定维度为 1 的单位向量，便于排序断言。
func memoryIntegrationVector(primary, second int) []float32 {
	vector := make([]float32, biz.EmbeddingDimension)
	if primary >= 0 && primary < len(vector) {
		vector[primary] = 1
	}
	if second >= 0 && second < len(vector) {
		vector[second] = 0.5
	}
	return vector
}

// TestMemoryRepositoryVectorsAndIndexes 验证向量写入幂等、用户隔离检索、HNSW 排序与索引切换。
func TestMemoryRepositoryVectorsAndIndexes(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "memory_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "memory_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "memory_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	providerID, modelID := uuid.New(), uuid.New()
	defer func() {
		_ = adminDB.Exec("DELETE FROM embedding_indexes WHERE provider_id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_models WHERE id = ?", modelID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?, ?)", admin.ID, owner.ID, other.ID).Error
	}()
	catalog := NewCatalogRepository(appDB)
	if _, err := catalog.CreateProvider(ctx, admin.ID, biz.Provider{
		ID: providerID, Name: "memory_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("encrypted"), KeyVersion: "v1"},
		catalogEvent(admin.ID, "provider.created", "llm_provider", providerID)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := catalog.CreateModel(ctx, admin.ID, biz.Model{
		ID: modelID, ProviderID: providerID, RemoteModelName: "embedding-model", Kind: biz.ModelKindEmbedding,
		ContextWindowTokens: 32768, MaxOutputTokens: 1, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(admin.ID, "model.created", "llm_model", modelID)); err != nil {
		t.Fatal(err)
	}

	memory := NewMemoryRepository(appDB)
	provider, modelName, dimension, err := memory.GetEnabledEmbeddingModel(ctx)
	if err != nil || provider != providerID || modelName != "embedding-model" || dimension != biz.EmbeddingDimension {
		t.Fatalf("embedding model = %s %s %d err=%v", provider, modelName, dimension, err)
	}

	index := biz.EmbeddingIndex{
		ID: uuid.New(), Name: "index-" + suffix, ProviderID: providerID, Model: "embedding-model",
		Dimension: biz.EmbeddingDimension, Version: "v1-" + suffix, Status: biz.EmbeddingIndexActive, DualWrite: true,
	}
	if _, err := memory.CreateIndex(ctx, index); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ActivateIndex(ctx, index.Version); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(appDB)
	ownerSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "owner-"+suffix)
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, other.ID, "other-"+suffix)
	ownerMessages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 3)
	otherMessages := appendMemoryIntegrationMessages(t, ctx, sessions, other.ID, otherSession, 1)

	ownerVectors := []biz.MemoryVector{
		memoryVectorFor(owner.ID, ownerSession, ownerMessages[0], index, memoryIntegrationVector(0, -1)),
		memoryVectorFor(owner.ID, ownerSession, ownerMessages[1], index, memoryIntegrationVector(0, 1)),
	}
	if err := memory.UpsertVectors(ctx, owner.ID, ownerVectors); err != nil {
		t.Fatal(err)
	}
	if err := memory.UpsertVectors(ctx, other.ID, []biz.MemoryVector{
		memoryVectorFor(other.ID, otherSession, otherMessages[0], index, memoryIntegrationVector(0, -1)),
	}); err != nil {
		t.Fatal(err)
	}

	matches, err := memory.SearchVectors(ctx, owner.ID, memoryIntegrationVector(0, -1), index.Version, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %#v", matches)
	}
	// 高维单位向量下，[1,0.5] 与查询 [1,0] 的余弦相似度高于 [1,0]，排序应稳定。
	if matches[0].Content != ownerMessages[1].Content {
		t.Fatalf("top match = %#v", matches[0])
	}
	if matches[0].Score <= matches[1].Score {
		t.Fatalf("scores are not ordered: %#v", matches)
	}
	// 检索必须只返回当前用户的向量；另一用户只能看到自己的记忆。
	if cross, err := memory.SearchVectors(ctx, other.ID, memoryIntegrationVector(0, -1), index.Version, 30); err != nil || len(cross) != 1 || *cross[0].SourceID != otherMessages[0].ID {
		t.Fatalf("cross user matches = %#v err=%v", cross, err)
	}

	// 重复写入同一来源与版本只更新内容，不产生重复向量。
	updated := ownerVectors[0]
	updated.Content = "更新后的内容"
	updated.ContentHash = biz.MemoryContentHash(updated.Content)
	if err := memory.UpsertVectors(ctx, owner.ID, []biz.MemoryVector{updated}); err != nil {
		t.Fatal(err)
	}
	hashes, err := memory.ListSourceHashes(ctx, owner.ID, index.Version, []uuid.UUID{*updated.SourceID})
	if err != nil || hashes[*updated.SourceID] != biz.MemoryContentHash("更新后的内容") {
		t.Fatalf("hashes = %#v err=%v", hashes, err)
	}
	if count, err := memory.CountVectors(ctx, index.Version); err != nil || count != 3 {
		t.Fatalf("vector count = %d err=%v", count, err)
	}
	if count, err := memory.CountEligibleMessages(ctx); err != nil || count < 4 {
		t.Fatalf("eligible messages = %d err=%v", count, err)
	}
	unembedded, err := memory.ListUnembeddedMessages(ctx, index.Version, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range unembedded {
		if message.ID == ownerMessages[0].ID || message.ID == ownerMessages[1].ID {
			t.Fatalf("embedded message returned as unembedded: %s", message.ID)
		}
	}

	// 删除来源向量后不再可召回。
	if err := memory.DeleteVectorsForSources(ctx, owner.ID, []uuid.UUID{ownerMessages[0].ID}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	matches, err = memory.SearchVectors(ctx, owner.ID, memoryIntegrationVector(0, -1), index.Version, 30)
	if err != nil || len(matches) != 1 {
		t.Fatalf("after delete matches = %#v err=%v", matches, err)
	}

	// staging 索引切换后旧 active 变为 retired。
	staging := biz.EmbeddingIndex{
		ID: uuid.New(), Name: "index-v2-" + suffix, ProviderID: providerID, Model: "embedding-model",
		Dimension: biz.EmbeddingDimension, Version: "v2-" + suffix, Status: biz.EmbeddingIndexStaging, DualWrite: true,
	}
	if _, err := memory.CreateIndex(ctx, staging); err != nil {
		t.Fatal(err)
	}
	activated, err := memory.ActivateIndex(ctx, staging.Version)
	if err != nil || activated.Status != biz.EmbeddingIndexActive {
		t.Fatalf("activate = %#v err=%v", activated, err)
	}
	indexes, err := memory.ListIndexes(ctx)
	if err != nil || len(indexes) < 2 {
		t.Fatalf("indexes = %#v err=%v", indexes, err)
	}
	activeCount := 0
	for _, item := range indexes {
		if item.Status == biz.EmbeddingIndexActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("active indexes = %d", activeCount)
	}
}

// createMemoryIntegrationSession 创建长期记忆测试会话。
func createMemoryIntegrationSession(t *testing.T, ctx context.Context, sessions *SessionRepository, userID uuid.UUID, title string) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: userID, Title: title, Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(userID, "session.created")); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

// appendMemoryIntegrationMessages 追加指定数量的消息并保证时间递增。
func appendMemoryIntegrationMessages(t *testing.T, ctx context.Context, sessions *SessionRepository, userID, sessionID uuid.UUID, count int) []biz.Message {
	t.Helper()
	messages := make([]biz.Message, 0, count)
	for index := 0; index < count; index++ {
		created, err := sessions.AppendMessage(ctx, biz.Message{
			ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser,
			Content: "记忆消息",
		}, testOutboxEvent(userID, "message.created"))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, created)
		time.Sleep(2 * time.Millisecond)
	}
	return messages
}

// memoryVectorFor 构造一条测试向量记录。
func memoryVectorFor(userID, sessionID uuid.UUID, message biz.Message, index biz.EmbeddingIndex, embedding []float32) biz.MemoryVector {
	messageID := message.ID
	return biz.MemoryVector{
		ID: uuid.New(), UserID: userID, SessionID: &sessionID, SourceType: biz.MemorySourceMessage,
		SourceID: &messageID, Content: message.Content, ContentHash: biz.MemoryContentHash(message.Content),
		Embedding: embedding, Metadata: map[string]any{"role": string(message.Role)},
		ProviderID: index.ProviderID, Model: index.Model, Dimension: index.Dimension,
		Version: index.Version, CreatedAt: time.Now().UTC(),
	}
}
