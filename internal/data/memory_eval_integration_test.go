//go:build integration

package data

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// evalVector 构造指定前两维的 1536 维单位向量，便于构造可控相似度。
func evalVector(first, second float32) []float32 {
	vector := make([]float32, biz.EmbeddingDimension)
	vector[0] = first
	vector[1] = second
	return vector
}

// TestMemoryQualityEvaluation 验证检索 Precision/Recall、MMR 多样性、跨用户零泄漏与删除后不可召回。
func TestMemoryQualityEvaluation(t *testing.T) {
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
		Username: "eval_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "eval_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{
		Username: "eval_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
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
		ID: providerID, Name: "eval_provider_" + suffix, ProviderType: "openai_compatible",
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
	repository := NewMemoryRepository(appDB)
	index := biz.EmbeddingIndex{
		ID: uuid.New(), Name: "eval-index-" + suffix, ProviderID: providerID, Model: "embedding-model",
		Dimension: biz.EmbeddingDimension, Version: "eval-v1-" + suffix, Status: biz.EmbeddingIndexStaging, DualWrite: true,
	}
	if _, err := repository.CreateIndex(ctx, index); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ActivateIndex(ctx, index.Version); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(appDB)
	ownerSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "eval-owner-"+suffix)
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, other.ID, "eval-other-"+suffix)
	ownerMessages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 4)
	otherMessages := appendMemoryIntegrationMessages(t, ctx, sessions, other.ID, otherSession, 1)

	// 给消息设置可识别内容，并按相似度构造向量。
	vectors := []biz.MemoryVector{
		evalVectorMessage(owner.ID, ownerSession, ownerMessages[0], index, "relevant-a", evalVector(1, 0)),
		evalVectorMessage(owner.ID, ownerSession, ownerMessages[1], index, "relevant-b", evalVector(0.9, float32(math.Sqrt(1-0.81)))), //nolint:gocritic
		evalVectorMessage(owner.ID, ownerSession, ownerMessages[2], index, "diverse-c", evalVector(0.6, 0.8)),
		evalVectorMessage(owner.ID, ownerSession, ownerMessages[3], index, "diverse-d", evalVector(0.5, float32(math.Sqrt(0.75)))), //nolint:gocritic
		evalVectorMessage(other.ID, otherSession, otherMessages[0], index, "other-user", evalVector(1, 0)),
	}
	if err := repository.UpsertVectors(ctx, owner.ID, vectors[:4]); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpsertVectors(ctx, other.ID, vectors[4:]); err != nil {
		t.Fatal(err)
	}

	query := evalVector(1, 0)
	matches, err := repository.SearchVectors(ctx, owner.ID, query, index.Version, 30)
	if err != nil {
		t.Fatal(err)
	}
	ranked := make([]string, 0, len(matches))
	for _, match := range matches {
		if match.Content == "other-user" {
			t.Fatal("cross-user memory leaked into owner retrieval")
		}
		ranked = append(ranked, match.Content)
	}
	relevant := map[string]struct{}{"relevant-a": {}, "relevant-b": {}}
	if precision := memory.PrecisionAtK(ranked, relevant, 4); math.Abs(precision-0.5) > 1e-9 {
		t.Fatalf("precision@4 = %f ranked=%v", precision, ranked)
	}
	if recall := memory.RecallAtK(ranked, relevant, 2); recall != 1 {
		t.Fatalf("recall@2 = %f ranked=%v", recall, ranked)
	}
	if len(matches) > 0 && matches[0].Content != "relevant-a" {
		t.Fatalf("top match = %s", matches[0].Content)
	}

	// MMR 低 lambda 应避免选择与查询几乎同向的冗余记忆。
	candidates := make([]memory.VectorCandidate, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, memory.VectorCandidate{ID: match.Content, Relevance: match.Score, Vector: match.Vector})
	}
	selected := memory.SelectMMR(candidates, 2, 0.3)
	if len(selected) != 2 || selected[0].ID != "relevant-a" {
		t.Fatalf("selected = %#v", selected)
	}
	if selected[1].ID == "relevant-b" {
		t.Fatalf("MMR selected a redundant memory: %#v", selected)
	}
	selectedVectors := make([][]float32, 0, len(selected))
	for _, candidate := range selected {
		selectedVectors = append(selectedVectors, candidate.Vector)
	}
	if diversity := memory.MMRDiversity(selectedVectors); diversity < 0.2 {
		t.Fatalf("MMR diversity = %f", diversity)
	}

	// 删除来源后不得再被召回。
	if err := repository.DeleteVectorsForSources(ctx, owner.ID, []uuid.UUID{ownerMessages[0].ID}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	matches, err = repository.SearchVectors(ctx, owner.ID, query, index.Version, 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if match.Content == "relevant-a" {
			t.Fatal("deleted memory was recalled")
		}
	}

	// 摘要忠实度与群聊发言人归属使用同一条评测工具链。
	sources := []string{"用户偏好中文回答，部署窗口是周末。", "群聊发言人 ou_a 确认发布计划。"}
	if score := memory.SummaryFaithfulness("用户偏好中文回答，部署窗口周末。", sources); score < 0.6 {
		t.Fatalf("summary faithfulness = %f", score)
	}
	transcript := memory.FormatSummaryTranscript([]memory.SummaryMessage{{Role: "user", Speaker: "ou_a", Content: "确认"}}, 0)
	if rate := memory.SpeakerAttributionRate(transcript, []string{"ou_a"}); rate != 1 {
		t.Fatalf("speaker attribution = %f", rate)
	}
}

// evalVectorMessage 构造评测用向量记录。
func evalVectorMessage(userID, sessionID uuid.UUID, message biz.Message, index biz.EmbeddingIndex, content string, embedding []float32) biz.MemoryVector {
	messageID := message.ID
	return biz.MemoryVector{
		ID: uuid.New(), UserID: userID, SessionID: &sessionID, SourceType: biz.MemorySourceMessage,
		SourceID: &messageID, Content: content, ContentHash: biz.MemoryContentHash(content),
		Embedding: embedding, Metadata: map[string]any{"role": "user"},
		ProviderID: index.ProviderID, Model: index.Model, Dimension: index.Dimension,
		Version: index.Version, CreatedAt: time.Now().UTC(),
	}
}
