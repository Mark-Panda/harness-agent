//go:build integration

// Package memoryeval_test 运行数据驱动的记忆质量评测：检索、多语言、群聊归属、
// 长周期冲突、摘要忠实度与删除后不可召回。
package memoryeval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// memoryDataset 是版本化记忆评测数据集。
type memoryDataset struct {
	Version  int          `json:"version"`
	Baseline string       `json:"baseline"`
	Cases    []memoryCase `json:"cases"`
}

// memoryCase 是单个评测场景。
type memoryCase struct {
	ID            string            `json:"id"`
	Category      string            `json:"category"`
	Locale        string            `json:"locale"`
	Policy        string            `json:"policy"`
	Query         string            `json:"query"`
	QueryGroup    string            `json:"query_group"`
	ExpectedTop   string            `json:"expected_top"`
	Messages      []memoryMessage   `json:"messages"`
	Vectors       []memoryVector    `json:"vectors"`
	Relevant      []string          `json:"relevant"`
	Forbidden     []string          `json:"forbidden"`
	Summary       *memorySummary    `json:"summary"`
	Facts         []memoryFact      `json:"facts"`
	ExpectedFacts map[string]string `json:"expected_facts"`
	DeleteSession bool              `json:"delete_session"`
	SeedSummary   bool              `json:"seed_summary"`
}

// memoryMessage 是评测消息。
type memoryMessage struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Speaker string `json:"speaker"`
	Content string `json:"content"`
	AgeDays int    `json:"age_days"`
}

// memoryVector 是评测向量种子；group 由 runner 转换为确定性 1536 维向量。
type memoryVector struct {
	MessageID string `json:"message_id"`
	Group     string `json:"group"`
	Speaker   string `json:"speaker"`
	AgeDays   int    `json:"age_days"`
	OtherUser bool   `json:"other_user"`
}

// memorySummary 是摘要评测期望。
type memorySummary struct {
	Generated       string            `json:"generated"`
	Sources         []string          `json:"sources"`
	ExpectedClaims  []string          `json:"expected_claims"`
	ForbiddenClaims []string          `json:"forbidden_claims"`
	Speakers        map[string]string `json:"speakers"`
}

// memoryFact 是事实冲突评测种子。
type memoryFact struct {
	Key        string  `json:"key"`
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
	Correction bool    `json:"correction"`
}

// memoryBaseline 是记忆评测门禁。
type memoryBaseline struct {
	Version        int `json:"version"`
	DatasetVersion int `json:"dataset_version"`
	Thresholds     struct {
		RecallAt8                  float64 `json:"recall_at_8"`
		PrecisionAt8               float64 `json:"precision_at_8"`
		MRR                        float64 `json:"mrr"`
		SummaryFaithfulness        float64 `json:"summary_faithfulness"`
		SummaryClaimRecall         float64 `json:"summary_claim_recall"`
		UnsupportedClaimRate       float64 `json:"unsupported_claim_rate"`
		SpeakerAttributionAccuracy float64 `json:"speaker_attribution_accuracy"`
		ConflictResolutionAccuracy float64 `json:"conflict_resolution_accuracy"`
		CrossUserLeak              int     `json:"cross_user_leak"`
		StalePreferenceLeak        int     `json:"stale_preference_leak"`
		DeletedMemoryLeak          int     `json:"deleted_memory_leak"`
	} `json:"thresholds"`
}

// evalContext 是套件共享的数据库与仓储。
type evalContext struct {
	ctx         context.Context
	adminDB     *gorm.DB
	appDB       *gorm.DB
	authDB      *gorm.DB
	users       *data.UserRepository
	sessions    *data.SessionRepository
	memories    *data.MemoryRepository
	summaries   *data.SummaryRepository
	facts       *biz.FactService
	index       biz.EmbeddingIndex
	providerID  uuid.UUID
	modelID     uuid.UUID
	adminUserID uuid.UUID
}

// queryGroupEmbedder 把查询 group 映射为确定性向量，保证检索可复现。
type queryGroupEmbedder struct {
	group string
}

// Embed 返回与输入数量一致的查询向量。
func (e queryGroupEmbedder) Embed(_ context.Context, _ biz.EmbeddingIndex, inputs []string) (biz.EmbeddingResult, error) {
	vector := groupVector(e.group)
	vectors := make([][]float32, 0, len(inputs))
	for range inputs {
		vectors = append(vectors, append([]float32(nil), vector...))
	}
	return biz.EmbeddingResult{Vectors: vectors}, nil
}

// groupVector 生成 1536 维 one-hot 向量；同组余弦相似度为 1，不同组为 0。
func groupVector(group string) []float32 {
	vector := make([]float32, biz.EmbeddingDimension)
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(group))
	vector[int(hash.Sum32())%64] = 1
	return vector
}

// TestMemoryEvaluationSuite 执行全部记忆评测场景并应用版本化门禁。
func TestMemoryEvaluationSuite(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	dataset := loadDataset(t)
	baseline := loadBaseline(t)
	if baseline.DatasetVersion != dataset.Version {
		t.Fatalf("baseline dataset version = %d, dataset = %d", baseline.DatasetVersion, dataset.Version)
	}
	env := newEvalContext(t)
	defer env.cleanup(t)
	for _, evalCase := range dataset.Cases {
		evalCase := evalCase
		t.Run(evalCase.ID, func(t *testing.T) {
			runMemoryCase(t, env, baseline, evalCase)
		})
	}
}

func loadDataset(t *testing.T) memoryDataset {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "dataset.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dataset memoryDataset
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatal(err)
	}
	if dataset.Version <= 0 || len(dataset.Cases) == 0 {
		t.Fatalf("dataset is incomplete: %#v", dataset)
	}
	return dataset
}

func loadBaseline(t *testing.T) memoryBaseline {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "baseline.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var baseline memoryBaseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	if baseline.Version <= 0 || baseline.DatasetVersion <= 0 {
		t.Fatalf("baseline is incomplete: %#v", baseline)
	}
	return baseline
}

func newEvalContext(t *testing.T) *evalContext {
	t.Helper()
	ctx := context.Background()
	adminDB := openDB(t, "TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable")
	appDB := openDB(t, "TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable")
	authDB := openDB(t, "TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable")
	users := data.NewUserRepository(appDB, authDB)
	suffix := uniqueSuffix()
	adminUser, err := users.CreateUser(ctx, biz.User{
		Username: "mem_eval_admin_" + suffix, PasswordHash: "hash", Role: biz.RoleAdmin,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, evalEvent(uuid.Nil, "test.register", "user", suffix))
	if err != nil {
		t.Fatal(err)
	}
	providerID, modelID := uuid.New(), uuid.New()
	catalog := data.NewCatalogRepository(appDB)
	if _, err := catalog.CreateProvider(ctx, adminUser.ID, biz.Provider{
		ID: providerID, Name: "mem_eval_provider_" + suffix, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("encrypted"), KeyVersion: "v1"},
		evalEvent(adminUser.ID, "provider.created", "llm_provider", providerID.String())); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelName := "mem-eval-embedding-" + suffix
	if _, err := catalog.CreateModel(ctx, adminUser.ID, biz.Model{
		ID: modelID, ProviderID: providerID, RemoteModelName: modelName, Kind: biz.ModelKindEmbedding,
		ContextWindowTokens: 32768, MaxOutputTokens: 1, Status: biz.ModelEnabled,
		VerificationSource: "memory-eval", VerifiedAt: &now,
	}, evalEvent(adminUser.ID, "model.created", "llm_model", modelID.String())); err != nil {
		t.Fatal(err)
	}
	memories := data.NewMemoryRepository(appDB)
	index := biz.EmbeddingIndex{
		ID: uuid.New(), Name: "mem-eval-index-" + suffix, ProviderID: providerID, Model: modelName,
		Dimension: biz.EmbeddingDimension, Version: "mem-eval-" + suffix,
		Status: biz.EmbeddingIndexStaging, DualWrite: true,
	}
	if _, err := memories.CreateIndex(ctx, index); err != nil {
		t.Fatal(err)
	}
	if _, err := memories.ActivateIndex(ctx, index.Version); err != nil {
		t.Fatal(err)
	}
	factService, err := biz.NewFactService(data.NewFactRepository(appDB), biz.FactPolicy{
		MinConfidence: 0.6, CorrectionGuard: 0.9, MaxFactsPerUser: 100,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &evalContext{
		ctx: ctx, adminDB: adminDB, appDB: appDB, authDB: authDB, users: users,
		sessions: data.NewSessionRepository(appDB), memories: memories,
		summaries: data.NewSummaryRepository(appDB), facts: factService, index: index,
		providerID: providerID, modelID: modelID, adminUserID: adminUser.ID,
	}
}

func (e *evalContext) cleanup(t *testing.T) {
	t.Helper()
	_ = e.adminDB.Exec("DELETE FROM embedding_indexes WHERE provider_id = ?", e.providerID).Error
	_ = e.adminDB.Exec("DELETE FROM llm_models WHERE provider_id = ?", e.providerID).Error
	_ = e.adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", e.providerID).Error
	_ = e.adminDB.Exec("DELETE FROM users WHERE id = ?", e.adminUserID).Error
}

func runMemoryCase(t *testing.T, env *evalContext, baseline memoryBaseline, evalCase memoryCase) {
	t.Helper()
	owner, err := env.users.CreateUser(env.ctx, biz.User{
		Username: evalUsername("owner", evalCase.ID), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, evalEvent(uuid.Nil, "test.register", "user", evalCase.ID+uniqueSuffix()))
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.users.CreateUser(env.ctx, biz.User{
		Username: evalUsername("other", evalCase.ID), PasswordHash: "hash",
		Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1,
	}, evalEvent(uuid.Nil, "test.register", "user", evalCase.ID+uniqueSuffix()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = env.adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error
	}()

	policy := biz.SessionMemoryPrivate
	if evalCase.Policy == string(biz.SessionMemoryGroup) {
		policy = biz.SessionMemoryGroup
	}
	session := biz.Session{
		ID: uuid.New(), UserID: owner.ID, Title: evalCase.ID, Source: biz.SessionSourceWeb,
		MemoryPolicy: policy, Status: biz.SessionActive, Version: 1,
	}
	if _, err := env.sessions.CreateSession(env.ctx, session, evalEvent(owner.ID, "test.session.created", "session", session.ID.String())); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	messageIDs := make(map[string]uuid.UUID, len(evalCase.Messages))
	messageByID := make(map[string]biz.Message, len(evalCase.Messages))
	for _, seed := range evalCase.Messages {
		messageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(evalCase.ID+":"+seed.ID))
		role := biz.MessageUser
		if seed.Role == "assistant" {
			role = biz.MessageAssistant
		}
		message := biz.Message{
			ID: messageID, SessionID: session.ID, UserID: owner.ID, Role: role,
			Content: seed.Content, SpeakerOpenID: seed.Speaker,
			CreatedAt: now.Add(-time.Duration(seed.AgeDays) * 24 * time.Hour),
		}
		created, err := env.sessions.AppendMessage(env.ctx, message, evalEvent(owner.ID, "test.message.created", "message", messageID.String()))
		if err != nil {
			t.Fatal(err)
		}
		messageIDs[seed.ID] = created.ID
		messageByID[seed.ID] = created
	}

	ownerVectors, otherVectors := make([]biz.MemoryVector, 0), make([]biz.MemoryVector, 0)
	for _, seed := range evalCase.Vectors {
		userID := owner.ID
		sourceID := messageIDs[seed.MessageID]
		if seed.OtherUser {
			userID = other.ID
			sourceID = uuid.New()
		}
		if sourceID == uuid.Nil {
			sourceID = uuid.New()
		}
		metadata := map[string]any{
			"created_at": now.Add(-time.Duration(seed.AgeDays) * 24 * time.Hour).Format(time.RFC3339Nano),
			"category":   evalCase.Category,
		}
		if seed.Speaker != "" {
			metadata["speaker_open_id"] = seed.Speaker
		}
		var sessionPtr *uuid.UUID
		if !seed.OtherUser {
			sessionPtr = &session.ID
		}
		vector := biz.MemoryVector{
			ID: uuid.New(), UserID: userID, SessionID: sessionPtr,
			SourceType: biz.MemorySourceMessage, SourceID: &sourceID,
			Content: seed.MessageID, ContentHash: biz.MemoryContentHash(seed.MessageID),
			Embedding: groupVector(seed.Group), Metadata: metadata,
			ProviderID: env.providerID, Model: env.index.Model, Dimension: biz.EmbeddingDimension,
			Version: env.index.Version, CreatedAt: now.Add(-time.Duration(seed.AgeDays) * 24 * time.Hour),
		}
		if seed.OtherUser {
			otherVectors = append(otherVectors, vector)
		} else {
			ownerVectors = append(ownerVectors, vector)
		}
	}
	if len(ownerVectors) > 0 {
		if err := env.memories.UpsertVectors(env.ctx, owner.ID, ownerVectors); err != nil {
			t.Fatal(err)
		}
	}
	if len(otherVectors) > 0 {
		if err := env.memories.UpsertVectors(env.ctx, other.ID, otherVectors); err != nil {
			t.Fatal(err)
		}
		// 确认跨用户种子真实落库，避免测试因向量被策略跳过而静默通过。
		var otherCount int64
		if err := env.adminDB.Raw("SELECT count(*) FROM memory_vectors WHERE user_id = ?", other.ID).
			Scan(&otherCount).Error; err != nil {
			t.Fatal(err)
		}
		if otherCount == 0 {
			t.Fatal("other-user memory seed was not persisted")
		}
	}
	if evalCase.Category == "group" {
		// 群聊会话禁止写入个人长期向量；UpsertVectors 会在事务内跳过非 private 会话。
		var vectorCount int64
		if err := env.adminDB.Raw("SELECT count(*) FROM memory_vectors WHERE user_id = ? AND session_id = ?", owner.ID, session.ID).
			Scan(&vectorCount).Error; err != nil {
			t.Fatal(err)
		}
		if vectorCount != 0 {
			t.Fatalf("group session persisted %d personal vectors", vectorCount)
		}
	}

	// 事实冲突：按顺序写入旧事实与显式纠正。
	for _, seed := range evalCase.Facts {
		var sourceID *uuid.UUID
		if len(evalCase.Messages) > 0 {
			id := messageIDs[evalCase.Messages[len(evalCase.Messages)-1].ID]
			sourceID = &id
		}
		if _, err := env.facts.Apply(env.ctx, owner.ID, session.ID, []biz.FactCandidate{{
			Key: seed.Key, Value: json.RawMessage(seed.Value), Confidence: seed.Confidence,
			Correction: seed.Correction, SourceMessageID: sourceID, SourceKind: biz.FactSourceUser,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if evalCase.SeedSummary {
		first := messageByID[evalCase.Messages[0].ID]
		endAt := first.CreatedAt
		summary := biz.SessionSummary{
			ID: uuid.New(), SessionID: session.ID, UserID: owner.ID, Version: 1,
			SummaryText: "待删除摘要", SourceMessageIDs: []uuid.UUID{first.ID},
			SourceEndMessageID: &first.ID, SourceEndAt: &endAt, CoveredMessageCount: 1,
		}
		if _, applied, err := env.summaries.SaveSummary(env.ctx, summary, 0, evalEvent(owner.ID, "test.summary.created", "summary", summary.ID.String())); err != nil || !applied {
			t.Fatalf("seed summary applied=%v err=%v", applied, err)
		}
	}

	if evalCase.DeleteSession {
		runDeletionCase(t, env, evalCase, owner, session, messageIDs)
		return
	}

	ranked, err := retrieveIDs(env, owner.ID, session.ID, evalCase.QueryGroup, evalCase.Query)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ranked=%v", ranked)
	if evalCase.Category == "group" && len(ranked) != 0 {
		// 群聊共享会话不得写入/召回个人长期向量；上下文只来自会话内消息与摘要。
		t.Fatalf("group session must not retrieve personal long-term vectors: %v", ranked)
	}
	forbidden := make(map[string]struct{}, len(evalCase.Forbidden))
	for _, id := range evalCase.Forbidden {
		forbidden[id] = struct{}{}
	}
	for _, id := range ranked {
		if _, blocked := forbidden[id]; blocked {
			t.Fatalf("forbidden memory %q was retrieved: %v", id, ranked)
		}
	}
	relevant := make(map[string]struct{}, len(evalCase.Relevant))
	for _, id := range evalCase.Relevant {
		relevant[id] = struct{}{}
	}
	if len(evalCase.Relevant) > 0 {
		precision := memory.PrecisionAtK(ranked, relevant, 8)
		recall := memory.RecallAtK(ranked, relevant, 8)
		mrr := memory.MRR(ranked, relevant)
		ndcg := memory.NDCGAtK(ranked, relevant, 8)
		t.Logf("precision@8=%.3f recall@8=%.3f mrr=%.3f ndcg@8=%.3f", precision, recall, mrr, ndcg)
		if precision+1e-9 < baseline.Thresholds.PrecisionAt8 {
			t.Fatalf("precision@8 = %.3f below %.3f", precision, baseline.Thresholds.PrecisionAt8)
		}
		if recall+1e-9 < baseline.Thresholds.RecallAt8 {
			t.Fatalf("recall@8 = %.3f below %.3f", recall, baseline.Thresholds.RecallAt8)
		}
		if mrr+1e-9 < baseline.Thresholds.MRR {
			t.Fatalf("mrr = %.3f below %.3f", mrr, baseline.Thresholds.MRR)
		}
	}
	if evalCase.ExpectedTop != "" && (len(ranked) == 0 || ranked[0] != evalCase.ExpectedTop) {
		t.Fatalf("top memory = %v, want %s", ranked, evalCase.ExpectedTop)
	}

	if evalCase.Summary != nil {
		checkSummary(t, baseline, *evalCase.Summary)
	}
	if len(evalCase.ExpectedFacts) > 0 {
		active, err := activeFactValues(env, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		accuracy := memory.ConflictResolutionAccuracy(active, evalCase.ExpectedFacts)
		t.Logf("conflict accuracy=%.3f active=%v", accuracy, active)
		if accuracy+1e-9 < baseline.Thresholds.ConflictResolutionAccuracy {
			t.Fatalf("conflict accuracy = %.3f below %.3f", accuracy, baseline.Thresholds.ConflictResolutionAccuracy)
		}
	}
	if evalCase.Category == "group" && evalCase.Summary != nil {
		// 群聊场景必须保留全部发言人的归属，不能只出现部分发言人。
		rate := memory.SpeakerAttributionRate(evalCase.Summary.Generated, mapKeys(evalCase.Summary.Speakers))
		if rate+1e-9 < baseline.Thresholds.SpeakerAttributionAccuracy {
			t.Fatalf("speaker rate = %.3f below %.3f", rate, baseline.Thresholds.SpeakerAttributionAccuracy)
		}
	}
	if evalCase.Category == "retrieval" || evalCase.Category == "multilingual" || evalCase.Category == "group" {
		if len(evalCase.Forbidden) > 0 && baseline.Thresholds.CrossUserLeak == 0 {
			for _, id := range ranked {
				if _, blocked := forbidden[id]; blocked {
					t.Fatalf("cross-user leak: %s", id)
				}
			}
		}
	}
}

func runDeletionCase(t *testing.T, env *evalContext, evalCase memoryCase, owner biz.User, session biz.Session, messageIDs map[string]uuid.UUID) {
	t.Helper()
	executor := data.NewDeletionExecutor(env.appDB, nil)
	if err := executor.DeleteSession(env.ctx, owner.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	queryVector := groupVector(evalCase.QueryGroup)
	matches, err := env.memories.SearchVectors(env.ctx, owner.ID, queryVector, env.index.Version, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("deleted vectors still recallable: %#v", matches)
	}
	if _, found, err := env.summaries.GetActiveSummary(env.ctx, owner.ID, session.ID); err != nil || found {
		t.Fatalf("deleted summary found=%v err=%v", found, err)
	}
	activeFacts, err := env.facts.ListActiveFacts(env.ctx, owner.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeFacts) != 0 {
		t.Fatalf("deleted facts still active: %#v", activeFacts)
	}
	page, err := env.sessions.ListMessages(env.ctx, owner.ID, session.ID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("deleted session still has messages: %d", len(page.Items))
	}
	for _, id := range evalCase.Relevant {
		if _, exists := messageIDs[id]; !exists {
			t.Fatalf("test data missing message %s", id)
		}
	}
}

func retrieveIDs(env *evalContext, userID, sessionID uuid.UUID, group, query string) ([]string, error) {
	retriever, err := biz.NewMemoryRetriever(env.memories, env.memories, queryGroupEmbedder{group: group}, nil, biz.MemoryRetrieverConfig{
		TopK: 30, MMRTop: 8, Lambda: 0.7, MinSimilarity: 0,
	}, slog.Default())
	if err != nil {
		return nil, err
	}
	matches, err := retriever.Retrieve(env.ctx, userID, sessionID, query)
	if err != nil {
		return nil, err
	}
	ranked := make([]string, 0, len(matches))
	for _, match := range matches {
		ranked = append(ranked, match.Content)
	}
	return ranked, nil
}

func checkSummary(t *testing.T, baseline memoryBaseline, expectation memorySummary) {
	t.Helper()
	faithfulness := memory.SummaryFaithfulness(expectation.Generated, expectation.Sources)
	claimRecall := memory.SummaryClaimRecall(expectation.Generated, expectation.ExpectedClaims)
	unsupported := memory.UnsupportedClaimRate(expectation.Generated, expectation.ForbiddenClaims)
	t.Logf("summary faithfulness=%.3f claim_recall=%.3f unsupported=%.3f", faithfulness, claimRecall, unsupported)
	if faithfulness+1e-9 < baseline.Thresholds.SummaryFaithfulness {
		t.Fatalf("summary faithfulness = %.3f below %.3f", faithfulness, baseline.Thresholds.SummaryFaithfulness)
	}
	if claimRecall+1e-9 < baseline.Thresholds.SummaryClaimRecall {
		t.Fatalf("summary claim recall = %.3f below %.3f", claimRecall, baseline.Thresholds.SummaryClaimRecall)
	}
	if unsupported > baseline.Thresholds.UnsupportedClaimRate+1e-9 {
		t.Fatalf("unsupported claim rate = %.3f above %.3f", unsupported, baseline.Thresholds.UnsupportedClaimRate)
	}
	if len(expectation.Speakers) > 0 {
		accuracy := memory.SpeakerAttributionAccuracy(expectation.Speakers, expectation.Generated)
		if accuracy+1e-9 < baseline.Thresholds.SpeakerAttributionAccuracy {
			t.Fatalf("speaker attribution accuracy = %.3f below %.3f", accuracy, baseline.Thresholds.SpeakerAttributionAccuracy)
		}
	}
}

func activeFactValues(env *evalContext, userID uuid.UUID) (map[string]string, error) {
	facts, err := env.facts.ListActiveFacts(env.ctx, userID, 100)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(facts))
	for _, fact := range facts {
		var value string
		if err := json.Unmarshal(fact.Value, &value); err != nil {
			value = string(fact.Value)
		}
		values[fact.Key] = value
	}
	return values, nil
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func openDB(t *testing.T, envName, fallback string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(envName)
	if dsn == "" {
		dsn = fallback
	}
	db, err := data.OpenPostgres(dsn, 1)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func uniqueSuffix() string {
	return fmt.Sprintf("%d_%s", time.Now().UnixNano(), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}

// evalUsername 生成满足长度与字符约束的唯一用户名。
func evalUsername(prefix, caseID string) string {
	normalized := strings.ReplaceAll(strings.ToLower(caseID), "_", "")
	if len(normalized) > 12 {
		normalized = normalized[:12]
	}
	return fmt.Sprintf("mem_%s_%s_%s", prefix, normalized, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}

func evalEvent(userID uuid.UUID, eventType, aggregateType, aggregateID string) biz.OutboxEvent {
	return biz.OutboxEvent{
		UserID: userID, EventType: eventType, AggregateType: aggregateType, AggregateID: aggregateID,
		DedupeKey: eventType + ":" + aggregateID + ":" + uuid.NewString(),
		Payload:   map[string]any{"source": "memory-eval"},
	}
}
