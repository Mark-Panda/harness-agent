package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// 长期记忆错误。
var (
	// ErrEmbeddingModelUnavailable 当前没有可用的 embedding 模型或凭证，向量化任务应终止。
	ErrEmbeddingModelUnavailable = errors.New("embedding model is unavailable")
	// ErrMemoryIndexNotReady staging 索引覆盖度不足，暂不能切换为 active。
	ErrMemoryIndexNotReady = errors.New("memory index is not ready")
)

// EmbeddingIndexStatus 是向量索引版本的生命周期状态。
type EmbeddingIndexStatus string

const (
	// EmbeddingIndexStaging 正在回填、尚未参与检索。
	EmbeddingIndexStaging EmbeddingIndexStatus = "staging"
	// EmbeddingIndexActive 当前参与检索的索引。
	EmbeddingIndexActive EmbeddingIndexStatus = "active"
	// EmbeddingIndexRetired 已退役的索引，不再写入或检索。
	EmbeddingIndexRetired EmbeddingIndexStatus = "retired"
	// MemorySourceMessage 来源为会话消息。
	MemorySourceMessage = "message"
	// MemorySourceSummary 来源为会话摘要。
	MemorySourceSummary = "summary"
	// MemorySourceFact 来源为结构化事实。
	MemorySourceFact = "fact"
	// MemoryEmbedJobType 异步向量化任务类型。
	MemoryEmbedJobType = "memory.embed"
	// EmbeddingDimension 是当前强制向量维度。
	EmbeddingDimension = 1536
)

// EmbeddingIndex 是平台级向量索引版本元数据。
type EmbeddingIndex struct {
	// ID 索引唯一标识。
	ID uuid.UUID
	// Name 索引名称，平台内唯一。
	Name string
	// ProviderID 生成向量使用的供应商。
	ProviderID uuid.UUID
	// Model 供应商侧 embedding 模型名。
	Model string
	// Dimension 向量维度，当前固定 1536。
	Dimension int
	// Version 索引版本，写入 memory_vectors.embedding_version。
	Version string
	// Status 索引生命周期状态。
	Status EmbeddingIndexStatus
	// DualWrite 是否存在 staging 版本时同时写入 active 与 staging。
	DualWrite bool
	// Metadata 索引配置与校验信息，禁止包含凭证。
	Metadata map[string]any
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// MemoryVector 是用户私有的长期记忆向量。
type MemoryVector struct {
	// ID 向量唯一标识。
	ID uuid.UUID
	// UserID 所属用户，所有查询强制携带。
	UserID uuid.UUID
	// SessionID 来源会话，可为空。
	SessionID *uuid.UUID
	// SourceType 来源类型：message/summary/fact。
	SourceType string
	// SourceID 来源实体 ID。
	SourceID *uuid.UUID
	// Content 来源公开文本，不包含隐藏思维链或凭证。
	Content string
	// ContentHash 内容哈希，内容变更时触发重建。
	ContentHash string
	// Embedding 向量值，维度必须等于索引维度。
	Embedding []float32
	// Metadata 检索展示所需的来源元数据。
	Metadata map[string]any
	// ProviderID 生成向量的供应商。
	ProviderID uuid.UUID
	// Model 生成向量的模型名。
	Model string
	// Dimension 向量维度。
	Dimension int
	// Version 索引版本。
	Version string
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
}

// MemoryMatch 是长期记忆检索结果。
type MemoryMatch struct {
	// VectorID 向量唯一标识。
	VectorID uuid.UUID
	// SourceType 来源类型。
	SourceType string
	// SourceID 来源实体 ID，可为空。
	SourceID *uuid.UUID
	// SessionID 来源会话，可为空。
	SessionID *uuid.UUID
	// Content 记忆文本。
	Content string
	// Metadata 来源元数据，如发言人、角色与消息时间。
	Metadata map[string]any
	// Score 与查询的余弦相似度，取值通常为 [0,1]。
	Score float64
	// Vector 记忆向量，用于 MMR 多样性计算。
	Vector []float32
}

// EmbeddingIndexRepository 定义索引版本与维护统计契约。
type EmbeddingIndexRepository interface {
	// ListIndexes 按创建时间列出全部索引版本。
	ListIndexes(ctx context.Context) ([]EmbeddingIndex, error)
	// GetIndex 按版本读取索引。
	GetIndex(ctx context.Context, version string) (EmbeddingIndex, error)
	// CreateIndex 创建 staging 索引；版本或模型组合冲突时返回冲突错误。
	CreateIndex(ctx context.Context, index EmbeddingIndex) (EmbeddingIndex, error)
	// ActivateIndex 原子切换 active 索引，旧 active 变为 retired，staging 变为 active。
	ActivateIndex(ctx context.Context, version string) (EmbeddingIndex, error)
	// SetDualWrite 设置索引双写开关。
	SetDualWrite(ctx context.Context, version string, enabled bool) error
	// CountVectors 统计指定版本的向量总数，维护任务使用系统作用域。
	CountVectors(ctx context.Context, version string) (int64, error)
	// CountEligibleMessages 统计可向量化的消息总数（仅 user 且未删除）。
	CountEligibleMessages(ctx context.Context) (int64, error)
	// ListUnembeddedMessages 读取尚未写入指定版本的候选用户消息，按时间正序。
	ListUnembeddedMessages(ctx context.Context, version string, limit int) ([]Message, error)
}

// MemoryVectorRepository 定义用户私有向量的写入与检索契约。
type MemoryVectorRepository interface {
	// UpsertVectors 幂等写入或更新向量；同一来源与版本只保留一条有效记录。
	UpsertVectors(ctx context.Context, userID uuid.UUID, vectors []MemoryVector) error
	// SearchVectors 在指定用户与索引版本内按 cosine 距离取 Top-K。
	SearchVectors(ctx context.Context, userID uuid.UUID, query []float32, version string, limit int) ([]MemoryMatch, error)
	// ListSourceHashes 读取指定来源在目标版本已写入的内容哈希，用于跳过未变更内容。
	ListSourceHashes(ctx context.Context, userID uuid.UUID, version string, sourceIDs []uuid.UUID) (map[uuid.UUID]string, error)
	// DeleteVectorsForSources 删除来源对应的向量，失败重试由调用方负责。
	DeleteVectorsForSources(ctx context.Context, userID uuid.UUID, sourceIDs []uuid.UUID, now time.Time) error
	// ListVectors 按创建时间倒序列出用户向量内容与元数据，供导出与审计；不要求返回向量数值。
	ListVectors(ctx context.Context, userID uuid.UUID, sessionID *uuid.UUID, limit int) ([]MemoryVector, error)
}

// EmbeddingResult 是一次向量化调用的结果。
type EmbeddingResult struct {
	// Vectors 与输入文本顺序一致的向量列表。
	Vectors [][]float32
	// InputTokens 输入 Token 数。
	InputTokens int
}

// EmbeddingGenerator 使用指定索引的模型生成向量。
type EmbeddingGenerator interface {
	Embed(ctx context.Context, index EmbeddingIndex, inputs []string) (EmbeddingResult, error)
}

// MemoryRetrieverPort 是上下文组装使用的长期记忆检索端口。
type MemoryRetrieverPort interface {
	// Retrieve 返回与当前输入语义相关的长期记忆，失败时由调用方降级。
	Retrieve(ctx context.Context, userID, sessionID uuid.UUID, query string) ([]MemoryMatch, error)
}

// MemoryRetrieverConfig 是长期记忆检索与 MMR 配置。
type MemoryRetrieverConfig struct {
	// TopK 向量检索候选数，默认 30。
	TopK int
	// MMRTop MMR 最终返回数量，默认 8。
	MMRTop int
	// Lambda MMR 相关性权重，默认 0.7。
	Lambda float64
	// MinSimilarity 最低余弦相似度，默认 0。
	MinSimilarity float64
	// RerankEnabled 是否在向量召回后使用 rerank 模型重新排序。
	RerankEnabled bool
}

// normalize 补齐检索配置安全默认值。
func (c MemoryRetrieverConfig) normalize() MemoryRetrieverConfig {
	if c.TopK <= 0 {
		c.TopK = 30
	}
	if c.TopK > 200 {
		c.TopK = 200
	}
	if c.MMRTop <= 0 {
		c.MMRTop = 8
	}
	if c.MMRTop > c.TopK {
		c.MMRTop = c.TopK
	}
	if c.Lambda < 0 || c.Lambda > 1 {
		c.Lambda = 0.7
	}
	return c
}

// MemoryRetriever 负责查询向量化、Top-K 检索、可选 rerank 与 MMR 多样性选择。
type MemoryRetriever struct {
	indexes   EmbeddingIndexRepository
	vectors   MemoryVectorRepository
	generator EmbeddingGenerator
	reranker  Reranker
	usage     UsageRecorder
	config    MemoryRetrieverConfig
	logger    *slog.Logger
}

// NewMemoryRetriever 构造长期记忆检索器；reranker 为空表示不使用 rerank。
func NewMemoryRetriever(indexes EmbeddingIndexRepository, vectors MemoryVectorRepository, generator EmbeddingGenerator,
	reranker Reranker, config MemoryRetrieverConfig, logger *slog.Logger) (*MemoryRetriever, error) {
	if indexes == nil || vectors == nil || generator == nil {
		return nil, invalid("memory retriever requires index, vector and embedding dependencies")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !config.RerankEnabled {
		reranker = nil
	}
	return &MemoryRetriever{indexes: indexes, vectors: vectors, generator: generator, reranker: reranker, config: config.normalize(), logger: logger}, nil
}

// WithUsageRecorder 注入长期记忆 rerank 用量记录器。
func (r *MemoryRetriever) WithUsageRecorder(recorder UsageRecorder) *MemoryRetriever {
	r.usage = recorder
	return r
}

// Retrieve 返回与当前输入语义相关的长期记忆；没有 active 索引或查询为空时返回空结果。
func (r *MemoryRetriever) Retrieve(ctx context.Context, userID, sessionID uuid.UUID, query string) ([]MemoryMatch, error) {
	if userID == uuid.Nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	indexes, err := r.indexes.ListIndexes(ctx)
	if err != nil {
		return nil, err
	}
	var active *EmbeddingIndex
	for position := range indexes {
		if indexes[position].Status == EmbeddingIndexActive {
			active = &indexes[position]
			break
		}
	}
	if active == nil {
		return nil, nil
	}
	result, err := r.generator.Embed(ctx, *active, []string{query})
	if err != nil {
		return nil, err
	}
	if len(result.Vectors) != 1 {
		return nil, fmt.Errorf("%w: query embedding count mismatch", ErrInvalidConfig)
	}
	if err := validateVectorDimension(result.Vectors[0], active.Dimension); err != nil {
		return nil, err
	}
	matches, err := r.vectors.SearchVectors(ctx, userID, result.Vectors[0], active.Version, r.config.TopK)
	if err != nil {
		return nil, err
	}
	filtered := make([]MemoryMatch, 0, len(matches))
	for _, match := range matches {
		if match.Score >= r.config.MinSimilarity {
			filtered = append(filtered, match)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if r.reranker != nil && len(filtered) > 1 {
		// rerank 失败时保留向量分数，确保长期记忆检索仍可用。
		var scores []float64
		var rerankErr error
		if scoped, ok := r.reranker.(RerankerWithUsage); ok {
			var usage RerankUsage
			scores, usage, rerankErr = scoped.RerankWithUsage(ctx, query, memoryContents(filtered))
			if rerankErr == nil && r.usage != nil && usage.InputTokens > 0 {
				providerID := usage.ProviderID
				if err := r.usage.RecordUsage(ctx, userID, nil, &providerID, int64(usage.InputTokens), 0, 0, time.Now().UTC()); err != nil {
					r.logger.Warn("record rerank usage failed", "session_id", sessionID, "error", err)
				}
			}
		} else {
			scores, rerankErr = r.reranker.Rerank(ctx, query, memoryContents(filtered))
		}
		if rerankErr != nil {
			r.logger.Warn("long-term memory rerank failed", "session_id", sessionID, "error", rerankErr)
		} else if len(scores) == len(filtered) {
			for index := range filtered {
				filtered[index].Score = scores[index]
			}
			sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Score > filtered[j].Score })
		}
	}
	candidates := make([]memory.VectorCandidate, 0, len(filtered))
	byID := make(map[string]MemoryMatch, len(filtered))
	now := time.Now().UTC()
	for _, match := range filtered {
		id := match.VectorID.String()
		// 旧记忆同样语义相关时优先选择较新的内容；按 30 天半衰期衰减，
		// 但保留至少 70% 的基础相关性，避免完全压掉长期稳定的用户偏好。
		candidates = append(candidates, memory.VectorCandidate{
			ID: id, Relevance: match.Score * recencyWeight(match.Metadata, now), Vector: match.Vector,
		})
		byID[id] = match
	}
	selected := memory.SelectMMR(candidates, r.config.MMRTop, r.config.Lambda)
	resultMatches := make([]MemoryMatch, 0, len(selected))
	for _, candidate := range selected {
		if match, exists := byID[candidate.ID]; exists {
			resultMatches = append(resultMatches, match)
		}
	}
	r.logger.Info("long-term memory retrieved", "session_id", sessionID, "candidates", len(filtered), "selected", len(resultMatches))
	return resultMatches, nil
}

// memoryContents 提取候选记忆文本用于 rerank。
func memoryContents(matches []MemoryMatch) []string {
	contents := make([]string, len(matches))
	for index := range matches {
		contents[index] = matches[index].Content
	}
	return contents
}

// recencyWeight 按 30 天半衰期对记忆相关性做时间衰减，最低保留 0.7 权重。
func recencyWeight(metadata map[string]any, now time.Time) float64 {
	raw, exists := metadata["created_at"]
	if !exists {
		return 1
	}
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return 1
	}
	createdAt, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 1
	}
	age := now.Sub(createdAt)
	if age <= 0 {
		return 1
	}
	const halfLife = 30 * 24 * time.Hour
	decay := math.Pow(0.5, age.Hours()/halfLife.Hours())
	return 0.7 + 0.3*decay
}

// validateVectorDimension 校验向量维度与索引声明一致。
func validateVectorDimension(vector []float32, dimension int) error {
	if len(vector) == 0 {
		return fmt.Errorf("%w: empty embedding vector", ErrInvalidConfig)
	}
	if dimension > 0 && len(vector) != dimension {
		return fmt.Errorf("%w: embedding dimension %d does not match index %d", ErrInvalidConfig, len(vector), dimension)
	}
	return nil
}

// 编译期断言长期记忆检索器实现。
var _ MemoryRetrieverPort = (*MemoryRetriever)(nil)
