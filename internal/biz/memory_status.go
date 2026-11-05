package biz

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// VectorStatus 描述长期记忆向量能力当前是否真正生效，供前端提示配置缺口。
type VectorStatus struct {
	// VectorEnabled 配置项 memory.vector.enabled 是否开启。
	VectorEnabled bool `json:"vector_enabled"`
	// ModelConfigured 是否存在已启用供应商下的 embedding 模型。
	ModelConfigured bool `json:"model_configured"`
	// ProviderID embedding 模型所属供应商。
	ProviderID string `json:"provider_id,omitempty"`
	// ProviderName 供应商展示名称。
	ProviderName string `json:"provider_name,omitempty"`
	// Model embedding 模型名。
	Model string `json:"model,omitempty"`
	// Dimension 向量维度。
	Dimension int `json:"dimension,omitempty"`
	// IndexVersion 当前 active 或 staging 索引版本。
	IndexVersion string `json:"index_version,omitempty"`
	// IndexStatus 索引生命周期状态：active/staging/retired。
	IndexStatus string `json:"index_status,omitempty"`
	// VectorCount 已写入向量的数量。
	VectorCount int64 `json:"vector_count"`
	// Healthy 配置开启、模型已启用且 active 索引与模型一致。
	Healthy bool `json:"healthy"`
	// Warning 未生效时的可操作提示，为空表示无需提醒。
	Warning string `json:"warning,omitempty"`
}

// EmbeddingProviderReader 读取 embedding 索引绑定的供应商元数据。
type EmbeddingProviderReader interface {
	// GetProvider 按 ID 读取供应商。
	GetProvider(ctx context.Context, providerID uuid.UUID) (Provider, error)
}

// VectorStatusService 汇总配置开关、embedding 模型与索引状态，给出是否生效的结论。
type VectorStatusService struct {
	enabled   bool
	models    EmbeddingModelSource
	indexes   EmbeddingIndexRepository
	providers EmbeddingProviderReader
}

// NewVectorStatusService 构造向量状态查询服务；缺失仓储时返回错误。
func NewVectorStatusService(models EmbeddingModelSource, indexes EmbeddingIndexRepository,
	providers EmbeddingProviderReader, enabled bool) (*VectorStatusService, error) {
	if models == nil || indexes == nil || providers == nil {
		return nil, invalid("vector status service requires model, index and provider dependencies")
	}
	return &VectorStatusService{enabled: enabled, models: models, indexes: indexes, providers: providers}, nil
}

// Status 返回向量能力状态；只有配置开启、模型启用且 active 索引与模型一致才算生效。
func (s *VectorStatusService) Status(ctx context.Context) (VectorStatus, error) {
	status := VectorStatus{VectorEnabled: s.enabled}
	providerID, modelName, dimension, err := s.models.GetEnabledEmbeddingModel(ctx)
	switch {
	case err == nil:
		status.ModelConfigured = true
		status.ProviderID = providerID.String()
		status.Model = modelName
		status.Dimension = dimension
		// 供应商名称仅用于展示，读取失败不阻断状态查询。
		if provider, lookupErr := s.providers.GetProvider(ctx, providerID); lookupErr == nil {
			status.ProviderName = provider.Name
		}
	case errors.Is(err, ErrResourceNotFound):
		// 未配置 embedding 模型属于正常提示场景。
	default:
		return status, fmt.Errorf("read enabled embedding model: %w", err)
	}

	indexes, err := s.indexes.ListIndexes(ctx)
	if err != nil {
		return status, err
	}
	var active, staging *EmbeddingIndex
	for position := range indexes {
		switch indexes[position].Status {
		case EmbeddingIndexActive:
			if active == nil {
				active = &indexes[position]
			}
		case EmbeddingIndexStaging:
			if staging == nil {
				staging = &indexes[position]
			}
		}
	}
	// 优先展示 active；仅有 staging 时展示回填进度。
	switch {
	case active != nil:
		status.IndexVersion = active.Version
		status.IndexStatus = string(active.Status)
	case staging != nil:
		status.IndexVersion = staging.Version
		status.IndexStatus = string(staging.Status)
	}
	if status.IndexVersion != "" {
		count, err := s.indexes.CountVectors(ctx, status.IndexVersion)
		if err != nil {
			return status, err
		}
		status.VectorCount = count
	}

	modelMatched := status.ModelConfigured && active != nil &&
		active.Model == modelName && active.ProviderID == providerID
	status.Healthy = s.enabled && modelMatched
	if !status.Healthy {
		status.Warning = s.warningMessage(status, active, staging, modelMatched)
	}
	return status, nil
}

// warningMessage 生成未生效时的可操作提示文案。
func (s *VectorStatusService) warningMessage(status VectorStatus, active, staging *EmbeddingIndex, modelMatched bool) string {
	switch {
	case !s.enabled:
		return "长期记忆向量能力已在配置中关闭（memory.vector.enabled=false），不会生成或检索向量记忆。"
	case !status.ModelConfigured:
		return "尚未配置生效的向量模型：请到“模型目录”登记用途为 embedding 的模型，并在其模型服务中启用。向量维度需为 1536。"
	case active == nil && staging != nil:
		return fmt.Sprintf("向量模型 %s 已配置，索引 %s 正在回填，完成后自动生效。", status.Model, staging.Version)
	case active == nil:
		return fmt.Sprintf("向量模型 %s 已配置，但还没有生效索引；后台维护会在约 1 分钟内自动创建。", status.Model)
	case !modelMatched:
		return fmt.Sprintf("已配置的向量模型 %s 与当前生效索引（%s）不一致，正在重建新索引；重建完成前检索仍使用旧模型。", status.Model, active.Model)
	default:
		return ""
	}
}
