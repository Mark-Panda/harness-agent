package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// embeddingIndexModel 对应 embedding_indexes 表：平台级向量索引版本。
type embeddingIndexModel struct {
	// ID 索引主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name 索引名称。
	Name string
	// ProviderID 供应商 ID。
	ProviderID uuid.UUID `gorm:"type:uuid"`
	// EmbeddingModel embedding 模型名。
	EmbeddingModel string
	// Dimension 向量维度。
	Dimension int
	// Version 索引版本。
	Version string
	// Status 索引状态。
	Status string
	// DualWrite 双写开关。
	DualWrite bool
	// Metadata 索引元数据 JSON。
	Metadata datatypes.JSON
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定索引表名。
func (embeddingIndexModel) TableName() string { return "embedding_indexes" }

// memoryVectorModel 对应 memory_vectors 表：用户长期记忆向量。
type memoryVectorModel struct {
	// ID 向量主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// SessionID 来源会话。
	SessionID *uuid.UUID `gorm:"type:uuid"`
	// Content 来源文本。
	Content string
	// Embedding 向量文本，仅用于读取。
	EmbeddingText string
	// Metadata 来源元数据。
	Metadata datatypes.JSON
	// EmbeddingProvider 供应商 ID。
	EmbeddingProvider uuid.UUID `gorm:"type:uuid"`
	// EmbeddingModel 模型名。
	EmbeddingModel string
	// EmbeddingDimension 维度。
	EmbeddingDimension int
	// EmbeddingVersion 索引版本。
	EmbeddingVersion string
	// SourceType 来源类型。
	SourceType string
	// SourceID 来源 ID。
	SourceID *uuid.UUID `gorm:"type:uuid"`
	// ContentHash 内容哈希。
	ContentHash string
	// CreatedAt 创建时间。
	CreatedAt time.Time
}

// TableName 指定向量表名。
func (memoryVectorModel) TableName() string { return "memory_vectors" }

// memorySearchRow 是向量检索的投影行。
type memorySearchRow struct {
	// ID 向量主键。
	ID uuid.UUID
	// SessionID 来源会话。
	SessionID *uuid.UUID
	// SourceType 来源类型。
	SourceType string
	// SourceID 来源 ID。
	SourceID *uuid.UUID
	// Content 记忆文本。
	Content string
	// Metadata 来源元数据。
	Metadata datatypes.JSON
	// Score 余弦相似度。
	Score float64
	// EmbeddingText 向量文本。
	EmbeddingText string
}

// MemoryRepository 是长期记忆索引与向量的 GORM 实现。
type MemoryRepository struct {
	db *gorm.DB
}

// NewMemoryRepository 构造长期记忆仓库。
func NewMemoryRepository(db *gorm.DB) *MemoryRepository {
	return &MemoryRepository{db: db}
}

// ListIndexes 按创建时间列出全部索引版本。
func (r *MemoryRepository) ListIndexes(ctx context.Context) ([]biz.EmbeddingIndex, error) {
	var records []embeddingIndexModel
	if err := r.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, catalogError("list embedding indexes", err)
	}
	indexes := make([]biz.EmbeddingIndex, 0, len(records))
	for _, record := range records {
		index, err := toBizIndex(record)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

// GetIndex 按版本读取索引。
func (r *MemoryRepository) GetIndex(ctx context.Context, version string) (biz.EmbeddingIndex, error) {
	if strings.TrimSpace(version) == "" {
		return biz.EmbeddingIndex{}, fmt.Errorf("%w: index version is required", biz.ErrInvalidConfig)
	}
	var record embeddingIndexModel
	err := r.db.WithContext(ctx).Where("version = ?", version).First(&record).Error
	if err != nil {
		return biz.EmbeddingIndex{}, catalogError("get embedding index", err)
	}
	return toBizIndex(record)
}

// CreateIndex 创建索引版本；版本或模型组合冲突时返回资源冲突。
func (r *MemoryRepository) CreateIndex(ctx context.Context, index biz.EmbeddingIndex) (biz.EmbeddingIndex, error) {
	if index.ID == uuid.Nil || index.Version == "" || index.ProviderID == uuid.Nil || index.Model == "" {
		return biz.EmbeddingIndex{}, fmt.Errorf("%w: index identity is required", biz.ErrInvalidConfig)
	}
	if index.Dimension != biz.EmbeddingDimension {
		return biz.EmbeddingIndex{}, fmt.Errorf("%w: embedding dimension must be %d", biz.ErrInvalidConfig, biz.EmbeddingDimension)
	}
	if index.Status == "" {
		index.Status = biz.EmbeddingIndexStaging
	}
	metadata, err := json.Marshal(nonNilMap(index.Metadata))
	if err != nil {
		return biz.EmbeddingIndex{}, err
	}
	now := time.Now().UTC()
	record := embeddingIndexModel{
		ID: index.ID, Name: index.Name, ProviderID: index.ProviderID, EmbeddingModel: index.Model,
		Dimension: index.Dimension, Version: index.Version, Status: string(index.Status),
		DualWrite: index.DualWrite, Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	err = r.db.WithContext(ctx).Create(&record).Error
	if err != nil {
		return biz.EmbeddingIndex{}, catalogError("create embedding index", err)
	}
	return toBizIndex(record)
}

// ActivateIndex 原子切换 active 索引，旧 active 变为 retired。
func (r *MemoryRepository) ActivateIndex(ctx context.Context, version string) (biz.EmbeddingIndex, error) {
	var target embeddingIndexModel
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("version = ?", version).First(&target).Error; err != nil {
			return err
		}
		if biz.EmbeddingIndexStatus(target.Status) == biz.EmbeddingIndexRetired {
			return fmt.Errorf("%w: retired index cannot be activated", biz.ErrResourceConflict)
		}
		if biz.EmbeddingIndexStatus(target.Status) == biz.EmbeddingIndexActive {
			return nil
		}
		// 先退役旧 active 再激活目标，避免触发单维度唯一 active 索引。
		if err := tx.Model(&embeddingIndexModel{}).
			Where("dimension = ? AND status = ? AND id <> ?", target.Dimension, biz.EmbeddingIndexActive, target.ID).
			Updates(map[string]any{"status": biz.EmbeddingIndexRetired, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		if err := tx.Model(&embeddingIndexModel{}).Where("id = ?", target.ID).
			Updates(map[string]any{"status": biz.EmbeddingIndexActive, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", target.ID).First(&target).Error
	})
	if err != nil {
		return biz.EmbeddingIndex{}, catalogError("activate embedding index", err)
	}
	return toBizIndex(target)
}

// SetDualWrite 设置索引双写开关。
func (r *MemoryRepository) SetDualWrite(ctx context.Context, version string, enabled bool) error {
	result := r.db.WithContext(ctx).Model(&embeddingIndexModel{}).Where("version = ?", version).
		Updates(map[string]any{"dual_write": enabled, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return catalogError("set index dual write", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: embedding index not found", biz.ErrResourceNotFound)
	}
	return nil
}

// CountVectors 统计指定版本的向量总数；维护任务使用系统作用域跨用户读取。
func (r *MemoryRepository) CountVectors(ctx context.Context, version string) (int64, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&memoryVectorModel{}).
			Where("embedding_version = ? AND deleted_at IS NULL", version).Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count memory vectors", err)
	}
	return count, nil
}

// CountEligibleMessages 统计可向量化消息总数。
func (r *MemoryRepository) CountEligibleMessages(ctx context.Context) (int64, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&messageModel{}).
			Where("deleted_at IS NULL AND role = ?", string(biz.MessageUser)).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count eligible messages", err)
	}
	return count, nil
}

// ListUnembeddedMessages 读取尚未写入指定版本的候选消息。
func (r *MemoryRepository) ListUnembeddedMessages(ctx context.Context, version string, limit int) ([]biz.Message, error) {
	if strings.TrimSpace(version) == "" || limit <= 0 {
		return nil, fmt.Errorf("%w: version and limit are required", biz.ErrInvalidConfig)
	}
	if limit > 1000 {
		limit = 1000
	}
	var records []messageModel
	query := `
SELECT m.* FROM messages m
WHERE m.deleted_at IS NULL AND m.role = 'user'
AND NOT EXISTS (
  SELECT 1 FROM memory_vectors v
  WHERE v.user_id = m.user_id AND v.source_type = 'message' AND v.source_id = m.id
    AND v.embedding_version = ? AND v.deleted_at IS NULL
)
ORDER BY m.created_at ASC, m.id ASC
LIMIT ?`
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Raw(query, version, limit).Scan(&records).Error
	})
	if err != nil {
		return nil, catalogError("list unembedded messages", err)
	}
	return toBizMessages(records)
}

// UpsertVectors 幂等写入或更新向量，同一来源与版本只保留一条有效记录。
func (r *MemoryRepository) UpsertVectors(ctx context.Context, userID uuid.UUID, vectors []biz.MemoryVector) error {
	if userID == uuid.Nil || len(vectors) == 0 {
		return fmt.Errorf("%w: user and vectors are required", biz.ErrInvalidConfig)
	}
	const statement = `
INSERT INTO memory_vectors (id, user_id, session_id, content, embedding, metadata, embedding_provider,
  embedding_model, embedding_dimension, embedding_version, source_type, source_id, content_hash, created_at)
VALUES (?, ?, ?, ?, ?::vector, ?::jsonb, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, source_type, source_id, embedding_version)
  WHERE deleted_at IS NULL AND source_id IS NOT NULL
DO UPDATE SET content = EXCLUDED.content, embedding = EXCLUDED.embedding, metadata = EXCLUDED.metadata,
  content_hash = EXCLUDED.content_hash`
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		for _, vector := range vectors {
			if vector.Dimension != biz.EmbeddingDimension || len(vector.Embedding) != vector.Dimension {
				return fmt.Errorf("%w: invalid vector dimension", biz.ErrInvalidConfig)
			}
			metadata, err := json.Marshal(nonNilMap(vector.Metadata))
			if err != nil {
				return err
			}
			if err := tx.Exec(statement, vector.ID, userID, vector.SessionID, vector.Content,
				formatVector(vector.Embedding), metadata, vector.ProviderID, vector.Model,
				vector.Dimension, vector.Version, vector.SourceType, vector.SourceID,
				vector.ContentHash, vector.CreatedAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return catalogError("upsert memory vectors", err)
	}
	return nil
}

// SearchVectors 在指定用户与索引版本内按 cosine 距离取 Top-K。
func (r *MemoryRepository) SearchVectors(ctx context.Context, userID uuid.UUID, query []float32, version string, limit int) ([]biz.MemoryMatch, error) {
	if userID == uuid.Nil || len(query) != biz.EmbeddingDimension || strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("%w: user, 1536-dim query and version are required", biz.ErrInvalidConfig)
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	text := formatVector(query)
	var rows []memorySearchRow
	sql := `
SELECT id, session_id, source_type, source_id, content, metadata,
  1 - (embedding <=> ?::vector) AS score, embedding::text AS embedding_text
FROM memory_vectors
WHERE user_id = ? AND embedding_version = ? AND deleted_at IS NULL
ORDER BY embedding <=> ?::vector
LIMIT ?`
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Raw(sql, text, userID, version, text, limit).Scan(&rows).Error
	})
	if err != nil {
		return nil, catalogError("search memory vectors", err)
	}
	matches := make([]biz.MemoryMatch, 0, len(rows))
	for _, row := range rows {
		vector, err := parseVectorText(row.EmbeddingText)
		if err != nil {
			return nil, err
		}
		var metadata map[string]any
		if len(row.Metadata) > 0 {
			if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
				return nil, err
			}
		}
		matches = append(matches, biz.MemoryMatch{
			VectorID: row.ID, SourceType: row.SourceType, SourceID: row.SourceID, SessionID: row.SessionID,
			Content: row.Content, Metadata: metadata, Score: row.Score, Vector: vector,
		})
	}
	return matches, nil
}

// ListSourceHashes 读取指定来源在目标版本已写入的内容哈希。
func (r *MemoryRepository) ListSourceHashes(ctx context.Context, userID uuid.UUID, version string, sourceIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if userID == uuid.Nil || len(sourceIDs) == 0 {
		return nil, fmt.Errorf("%w: user and source ids are required", biz.ErrInvalidConfig)
	}
	type hashRow struct {
		SourceID    uuid.UUID
		ContentHash string
	}
	var rows []hashRow
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&memoryVectorModel{}).
			Select("source_id, content_hash").
			Where("user_id = ? AND embedding_version = ? AND source_type = ? AND deleted_at IS NULL AND source_id IN ?",
				userID, version, biz.MemorySourceMessage, sourceIDs).
			Scan(&rows).Error
	})
	if err != nil {
		return nil, catalogError("list source hashes", err)
	}
	hashes := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		hashes[row.SourceID] = row.ContentHash
	}
	return hashes, nil
}

// DeleteVectorsForSources 软删除来源对应的向量。
func (r *MemoryRepository) DeleteVectorsForSources(ctx context.Context, userID uuid.UUID, sourceIDs []uuid.UUID, now time.Time) error {
	if userID == uuid.Nil || len(sourceIDs) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&memoryVectorModel{}).
			Where("user_id = ? AND source_type = ? AND source_id IN ? AND deleted_at IS NULL",
				userID, biz.MemorySourceMessage, sourceIDs).
			Updates(map[string]any{"deleted_at": now}).Error
	})
	return catalogError("delete memory vectors", err)
}

// ListVectors 按创建时间倒序列出用户向量内容与元数据，供导出与审计；不返回向量数值。
func (r *MemoryRepository) ListVectors(ctx context.Context, userID uuid.UUID, sessionID *uuid.UUID, limit int) ([]biz.MemoryVector, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	var records []memoryVectorModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND deleted_at IS NULL", userID)
		if sessionID != nil {
			query = query.Where("session_id = ?", *sessionID)
		}
		return query.Order("created_at DESC, id DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list memory vectors", err)
	}
	vectors := make([]biz.MemoryVector, 0, len(records))
	for _, record := range records {
		var metadata map[string]any
		if len(record.Metadata) > 0 {
			if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
				return nil, err
			}
		}
		vectors = append(vectors, biz.MemoryVector{
			ID: record.ID, UserID: record.UserID, SessionID: record.SessionID,
			SourceType: record.SourceType, SourceID: record.SourceID, Content: record.Content,
			ContentHash: record.ContentHash, Metadata: metadata, ProviderID: record.EmbeddingProvider,
			Model: record.EmbeddingModel, Dimension: record.EmbeddingDimension, Version: record.EmbeddingVersion,
			CreatedAt: record.CreatedAt,
		})
	}
	return vectors, nil
}

// GetEnabledEmbeddingModel 返回第一个启用供应商下的 embedding 模型。
func (r *MemoryRepository) GetEnabledEmbeddingModel(ctx context.Context) (uuid.UUID, string, int, error) {
	var record struct {
		ProviderID      uuid.UUID
		RemoteModelName string
	}
	err := r.db.WithContext(ctx).Raw(`
SELECT m.provider_id, m.remote_model_name
FROM llm_models m
JOIN llm_providers p ON p.id = m.provider_id
WHERE m.model_kind = 'embedding' AND m.status = 'enabled' AND p.status = 'enabled'
ORDER BY m.created_at ASC, m.id ASC
LIMIT 1`).Scan(&record).Error
	if err != nil {
		return uuid.Nil, "", 0, catalogError("get embedding model", err)
	}
	if record.ProviderID == uuid.Nil || record.RemoteModelName == "" {
		return uuid.Nil, "", 0, fmt.Errorf("%w: no enabled embedding model", biz.ErrResourceNotFound)
	}
	return record.ProviderID, record.RemoteModelName, biz.EmbeddingDimension, nil
}

// GetEnabledRerankModel 返回第一个启用供应商下的 rerank 模型。
func (r *MemoryRepository) GetEnabledRerankModel(ctx context.Context) (uuid.UUID, string, error) {
	var record struct {
		ProviderID      uuid.UUID
		RemoteModelName string
	}
	err := r.db.WithContext(ctx).Raw(`
SELECT m.provider_id, m.remote_model_name
FROM llm_models m
JOIN llm_providers p ON p.id = m.provider_id
WHERE m.model_kind = 'rerank' AND m.status = 'enabled' AND p.status = 'enabled'
ORDER BY m.created_at ASC, m.id ASC
LIMIT 1`).Scan(&record).Error
	if err != nil {
		return uuid.Nil, "", catalogError("get rerank model", err)
	}
	if record.ProviderID == uuid.Nil || record.RemoteModelName == "" {
		return uuid.Nil, "", fmt.Errorf("%w: no enabled rerank model", biz.ErrResourceNotFound)
	}
	return record.ProviderID, record.RemoteModelName, nil
}

// toBizIndex 把索引行转换为领域对象。
func toBizIndex(record embeddingIndexModel) (biz.EmbeddingIndex, error) {
	var metadata map[string]any
	if len(record.Metadata) > 0 {
		if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
			return biz.EmbeddingIndex{}, err
		}
	}
	return biz.EmbeddingIndex{
		ID: record.ID, Name: record.Name, ProviderID: record.ProviderID, Model: record.EmbeddingModel,
		Dimension: record.Dimension, Version: record.Version, Status: biz.EmbeddingIndexStatus(record.Status),
		DualWrite: record.DualWrite, Metadata: metadata, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// formatVector 把向量编码为 pgvector 文本字面量。
func formatVector(vector []float32) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range vector {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	builder.WriteByte(']')
	return builder.String()
}

// parseVectorText 解析 pgvector 的文本输出。
func parseVectorText(text string) ([]float32, error) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return nil, fmt.Errorf("invalid vector text")
	}
	parts := strings.Split(trimmed[1:len(trimmed)-1], ",")
	values := make([]float32, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vector value: %w", err)
		}
		values = append(values, float32(value))
	}
	if len(values) != biz.EmbeddingDimension {
		return nil, fmt.Errorf("vector dimension %d does not match %d", len(values), biz.EmbeddingDimension)
	}
	return values, nil
}

// 编译期断言长期记忆仓储实现。
var (
	_ biz.EmbeddingIndexRepository = (*MemoryRepository)(nil)
	_ biz.MemoryVectorRepository   = (*MemoryRepository)(nil)
	_ biz.EmbeddingModelSource     = (*MemoryRepository)(nil)
)
