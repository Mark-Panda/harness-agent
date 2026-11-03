package biz

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MinGenerationContextTokens 是生成类模型的上下文硬准入下限（1M Token）。
// 该限制不适用于 Embedding/Rerank 模型。
const MinGenerationContextTokens int64 = 1_000_000

// ModelKind 模型用途分类，决定上下文窗口准入规则。
type ModelKind string

// ModelStatus 模型启停状态。
type ModelStatus string

const (
	// ModelKindChat 生成类模型，参与 Chat/ReAct/Plan/Supervisor 调用。
	ModelKindChat ModelKind = "chat"
	// ModelKindEmbedding 向量化模型，不受 1M 上下文限制。
	ModelKindEmbedding ModelKind = "embedding"
	// ModelKindRerank 重排序模型，不受 1M 上下文限制。
	ModelKindRerank ModelKind = "rerank"
	// ModelEnabled 模型已启用，可进入路由策略。
	ModelEnabled ModelStatus = "enabled"
	// ModelDisabled 模型已停用，不参与新 Run 的候选。
	ModelDisabled ModelStatus = "disabled"
)

// ModelCapabilities 记录模型的协议能力，路由前用于硬过滤。
type ModelCapabilities struct {
	// Streaming 是否支持流式输出。
	Streaming bool `json:"streaming"`
	// Tools 是否支持工具调用。
	Tools bool `json:"tools"`
	// StructuredOutput 是否支持结构化 JSON 输出。
	StructuredOutput bool `json:"structured_output"`
}

// Model 是某个供应商下的具体部署，保存上下文窗口、能力、价格与核验信息。
type Model struct {
	// ID 模型唯一标识。
	ID uuid.UUID `json:"id"`
	// ProviderID 所属供应商 ID。
	ProviderID uuid.UUID `json:"provider_id"`
	// RemoteModelName 供应商侧真实模型名。
	RemoteModelName string `json:"remote_model_name"`
	// Kind 模型用途分类。
	Kind ModelKind `json:"kind"`
	// ContextWindowTokens 上下文窗口 Token 数，生成类必须大于等于 1M。
	ContextWindowTokens int64 `json:"context_window_tokens"`
	// MaxOutputTokens 单次最大输出 Token 数。
	MaxOutputTokens int `json:"max_output_tokens"`
	// Capabilities 流式、工具与结构化输出能力。
	Capabilities ModelCapabilities `json:"capabilities"`
	// InputPricePerMillion 每百万输入 Token 价格。
	InputPricePerMillion float64 `json:"input_price_per_million"`
	// OutputPricePerMillion 每百万输出 Token 价格。
	OutputPricePerMillion float64 `json:"output_price_per_million"`
	// DataRegion 数据区域，用于合规过滤。
	DataRegion string `json:"data_region"`
	// VerificationSource 上下文窗口等元数据的核验来源，启用时必填。
	VerificationSource string `json:"verification_source"`
	// VerifiedAt 管理员核验时间，启用时必填。
	VerifiedAt *time.Time `json:"verified_at"`
	// Status 模型启停状态。
	Status ModelStatus `json:"status"`
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time `json:"updated_at"`
}

// ModelRepository 定义模型目录持久化契约，写操作与审计事件同事务提交。
type ModelRepository interface {
	// CreateModel 创建模型并写入审计事件。
	CreateModel(ctx context.Context, actorID uuid.UUID, model Model, event OutboxEvent) (Model, error)
	// UpdateModel 更新模型并写入审计事件。
	UpdateModel(ctx context.Context, actorID uuid.UUID, model Model, event OutboxEvent) (Model, error)
	// DeleteModel 删除模型并写入审计事件。
	DeleteModel(ctx context.Context, actorID, modelID uuid.UUID, event OutboxEvent) error
	// GetModel 按 ID 读取模型。
	GetModel(ctx context.Context, modelID uuid.UUID) (Model, error)
	// ListModels 列出模型，providerID 为 nil 时返回全部。
	ListModels(ctx context.Context, providerID *uuid.UUID) ([]Model, error)
	// GetProvider 读取模型所属供应商，用于启用时的一致性校验。
	GetProvider(ctx context.Context, providerID uuid.UUID) (Provider, error)
}

// ModelService 实现模型目录管理用例，并把 1M+ 与核验准入收敛到领域层。
type ModelService struct {
	repo ModelRepository
}

// NewModelService 构造模型目录服务。
func NewModelService(repo ModelRepository) (*ModelService, error) {
	if repo == nil {
		return nil, invalid("model repository is required")
	}
	return &ModelService{repo: repo}, nil
}

// Create 创建模型；默认草稿停用状态，需显式核验后才能启用。
func (s *ModelService) Create(ctx context.Context, principal Principal, model Model) (Model, error) {
	if err := RequireAdmin(principal); err != nil {
		return Model{}, err
	}
	model.ID = uuid.New()
	if model.Status == "" {
		model.Status = ModelDisabled
	}
	if err := s.validate(ctx, &model); err != nil {
		return Model{}, err
	}
	return s.repo.CreateModel(ctx, principal.UserID, model, mutationEvent(principal.UserID, "model.created", "llm_model", model.ID))
}

// Update 更新模型；启用状态的变更同样需要通过硬准入校验。
func (s *ModelService) Update(ctx context.Context, principal Principal, model Model) (Model, error) {
	if err := RequireAdmin(principal); err != nil {
		return Model{}, err
	}
	if err := s.validate(ctx, &model); err != nil {
		return Model{}, err
	}
	return s.repo.UpdateModel(ctx, principal.UserID, model, mutationEvent(principal.UserID, "model.updated", "llm_model", model.ID))
}

// Delete 删除模型，管理员专用。
func (s *ModelService) Delete(ctx context.Context, principal Principal, modelID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	if modelID == uuid.Nil {
		return invalid("model id is required")
	}
	return s.repo.DeleteModel(ctx, principal.UserID, modelID, mutationEvent(principal.UserID, "model.deleted", "llm_model", modelID))
}

// Get 读取模型，管理员专用。
func (s *ModelService) Get(ctx context.Context, principal Principal, modelID uuid.UUID) (Model, error) {
	if err := RequireAdmin(principal); err != nil {
		return Model{}, err
	}
	return s.repo.GetModel(ctx, modelID)
}

// List 列出模型，管理员专用；可按供应商过滤。
func (s *ModelService) List(ctx context.Context, principal Principal, providerID *uuid.UUID) ([]Model, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListModels(ctx, providerID)
}

// validate 执行模型硬准入：1M+ 上下文、能力核验、价格非负与供应商启用一致性。
func (s *ModelService) validate(ctx context.Context, model *Model) error {
	if model.ID == uuid.Nil || model.ProviderID == uuid.Nil {
		return invalid("model and provider ids are required")
	}
	model.RemoteModelName = strings.TrimSpace(model.RemoteModelName)
	if model.RemoteModelName == "" || len(model.RemoteModelName) > 256 {
		return invalid("remote model name must contain between 1 and 256 bytes")
	}
	if model.ContextWindowTokens <= 0 || model.MaxOutputTokens <= 0 || model.MaxOutputTokens > 1_000_000 {
		return invalid("model context and output token limits must be positive")
	}
	// 生成类模型必须达到 1M 上下文；Embedding/Rerank 不受此限制。
	if model.Kind == ModelKindChat && model.ContextWindowTokens < MinGenerationContextTokens {
		return invalid("generation models require at least %d context tokens", MinGenerationContextTokens)
	}
	if model.Kind != ModelKindChat && model.Kind != ModelKindEmbedding && model.Kind != ModelKindRerank {
		return invalid("unsupported model kind")
	}
	if model.Status != ModelEnabled && model.Status != ModelDisabled {
		return invalid("unsupported model status")
	}
	if model.InputPricePerMillion < 0 || model.OutputPricePerMillion < 0 {
		return invalid("model prices cannot be negative")
	}
	// 启用模型必须有可信核验来源与核验时间，且供应商处于启用状态。
	if model.Status == ModelEnabled {
		if strings.TrimSpace(model.VerificationSource) == "" || model.VerifiedAt == nil || model.VerifiedAt.After(time.Now().UTC().Add(time.Minute)) {
			return invalid("enabled models require trusted metadata verification")
		}
		provider, err := s.repo.GetProvider(ctx, model.ProviderID)
		if err != nil {
			return err
		}
		if provider.Status != ProviderEnabled {
			return invalid("a model cannot be enabled while its provider is disabled")
		}
	}
	return nil
}
