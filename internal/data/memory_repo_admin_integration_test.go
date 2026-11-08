//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestMemoryRepositoryAdminPaths 验证索引读取、双写开关、向量导出与 Rerank 模型发现。
func TestMemoryRepositoryAdminPaths(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	admin, err := users.CreateUser(ctx, biz.User{
		Username: "mem_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "mem_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	providerID := uuid.New()
	defer func() {
		// 清理顺序必须满足外键：先索引与模型，再供应商，避免阻断删除造成跨测试污染。
		_ = adminDB.Exec("DELETE FROM embedding_indexes WHERE provider_id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_models WHERE provider_id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
		_ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", admin.ID, owner.ID).Error
	}()

	catalog := NewCatalogRepository(appDB)
	if _, err := catalog.CreateProvider(ctx, admin.ID, biz.Provider{
		ID: providerID, Name: "mem_admin_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("encrypted"), KeyVersion: "v1"},
		catalogEvent(admin.ID, "provider.created", "llm_provider", providerID)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	embeddingModelID := uuid.New()
	if _, err := catalog.CreateModel(ctx, admin.ID, biz.Model{
		ID: embeddingModelID, ProviderID: providerID, RemoteModelName: "mem-admin-embedding", Kind: biz.ModelKindEmbedding,
		ContextWindowTokens: 32768, MaxOutputTokens: 1, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(admin.ID, "model.created", "llm_model", embeddingModelID)); err != nil {
		t.Fatal(err)
	}
	rerankModelID := uuid.New()
	if _, err := catalog.CreateModel(ctx, admin.ID, biz.Model{
		ID: rerankModelID, ProviderID: providerID, RemoteModelName: "mem-admin-rerank", Kind: biz.ModelKindRerank,
		ContextWindowTokens: 32768, MaxOutputTokens: 1, Status: biz.ModelEnabled,
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, catalogEvent(admin.ID, "model.created", "llm_model", rerankModelID)); err != nil {
		t.Fatal(err)
	}

	memory := NewMemoryRepository(appDB)
	// GetEnabledRerankModel 发现启用的 rerank 模型；供应商停用后不再返回。
	rerankProvider, rerankName, err := memory.GetEnabledRerankModel(ctx)
	if err != nil || rerankProvider != providerID || rerankName != "mem-admin-rerank" {
		t.Fatalf("GetEnabledRerankModel() = %s %s %v", rerankProvider, rerankName, err)
	}
	if err := adminDB.Exec("UPDATE llm_providers SET status = 'disabled' WHERE id = ?", providerID).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.GetEnabledRerankModel(ctx); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("disabled provider rerank error = %v", err)
	}
	if err := adminDB.Exec("UPDATE llm_providers SET status = 'enabled' WHERE id = ?", providerID).Error; err != nil {
		t.Fatal(err)
	}

	// 索引：GetIndex 按版本读取，SetDualWrite 开关持久化，缺失版本返回未命中。
	index := biz.EmbeddingIndex{
		ID: uuid.New(), Name: "admin-index-" + suffix, ProviderID: providerID, Model: "mem-admin-embedding",
		Dimension: biz.EmbeddingDimension, Version: "v1-" + suffix, Status: biz.EmbeddingIndexActive,
	}
	if _, err := memory.CreateIndex(ctx, index); err != nil {
		t.Fatal(err)
	}
	got, err := memory.GetIndex(ctx, index.Version)
	if err != nil || got.ID != index.ID || got.DualWrite {
		t.Fatalf("GetIndex() = %#v, %v", got, err)
	}
	if _, err := memory.GetIndex(ctx, "missing-"+suffix); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("GetIndex(missing) error = %v", err)
	}
	if _, err := memory.GetIndex(ctx, " "); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("GetIndex(blank) error = %v", err)
	}
	if err := memory.SetDualWrite(ctx, index.Version, true); err != nil {
		t.Fatalf("SetDualWrite() = %v", err)
	}
	if got, err := memory.GetIndex(ctx, index.Version); err != nil || !got.DualWrite {
		t.Fatalf("dual write not persisted: %#v, %v", got, err)
	}
	if err := memory.SetDualWrite(ctx, "missing-"+suffix, true); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("SetDualWrite(missing) error = %v", err)
	}

	// ListVectors：按用户导出，可按会话过滤，跨用户不可见。
	sessions := NewSessionRepository(appDB)
	ownerSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "mem-admin-"+suffix)
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "mem-admin-other-"+suffix)
	messages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 2)
	vectors := []biz.MemoryVector{
		memoryVectorFor(owner.ID, ownerSession, messages[0], index, memoryIntegrationVector(0, -1)),
		memoryVectorFor(owner.ID, ownerSession, messages[1], index, memoryIntegrationVector(1, -1)),
	}
	if err := memory.UpsertVectors(ctx, owner.ID, vectors); err != nil {
		t.Fatal(err)
	}
	all, err := memory.ListVectors(ctx, owner.ID, nil, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListVectors(all) = %#v, %v", all, err)
	}
	scoped, err := memory.ListVectors(ctx, owner.ID, &ownerSession, 10)
	if err != nil || len(scoped) != 2 {
		t.Fatalf("ListVectors(session) = %#v, %v", scoped, err)
	}
	if scoped, err := memory.ListVectors(ctx, owner.ID, &otherSession, 10); err != nil || len(scoped) != 0 {
		t.Fatalf("ListVectors(other session) = %#v, %v", scoped, err)
	}
	strangerID := uuid.New()
	if scoped, err := memory.ListVectors(ctx, strangerID, nil, 10); err != nil || len(scoped) != 0 {
		t.Fatalf("ListVectors(stranger) = %#v, %v", scoped, err)
	}
	if _, err := memory.ListVectors(ctx, uuid.Nil, nil, 10); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("ListVectors(nil user) error = %v", err)
	}
}
