package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// providerModel 对应 llm_providers 表：供应商连接与加密凭证。
type providerModel struct {
	// ID 供应商主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name 供应商名称，全局唯一。
	Name string
	// ProviderType 供应商标识类型，当前固定为 openai_compatible。
	ProviderType string
	// BaseURL 服务基础地址（HTTPS）。
	BaseURL string
	// APIKeyEncrypted API Key 的 AES-256-GCM 密文，禁止明文入库。
	APIKeyEncrypted []byte
	// KeyVersion 密文对应的密钥版本，用于轮换解密。
	KeyVersion string
	// Status 启停状态：enabled/disabled。
	Status string
	// DataRegion 数据区域，供合规模块过滤。
	DataRegion string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定供应商表名。
func (providerModel) TableName() string { return "llm_providers" }

// llmModel 对应 llm_models 表：供应商下的模型部署与能力元数据。
type llmModel struct {
	// ID 模型主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// ProviderID 所属供应商。
	ProviderID uuid.UUID `gorm:"type:uuid"`
	// RemoteModelName 供应商侧真实模型名。
	RemoteModelName string
	// ModelKind 模型用途：chat/embedding/rerank。
	ModelKind string
	// ContextWindowTokens 上下文窗口 Token 数，生成类需大于等于 1M。
	ContextWindowTokens int64
	// MaxOutputTokens 最大输出 Token 数。
	MaxOutputTokens int
	// Capabilities 流式、工具、结构化输出能力的 JSON。
	Capabilities datatypes.JSON
	// InputPricePerMillion 每百万输入 Token 价格。
	InputPricePerMillion float64
	// OutputPricePerMillion 每百万输出 Token 价格。
	OutputPricePerMillion float64
	// DataRegion 数据区域。
	DataRegion string
	// VerificationSource 元数据核验来源，启用时必填。
	VerificationSource string
	// VerifiedAt 管理员核验时间，启用时必填。
	VerifiedAt *time.Time
	// Status 启停状态。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定模型表名。
func (llmModel) TableName() string { return "llm_models" }

// policyModel 对应 model_policies 表：版本化路由策略。
type policyModel struct {
	// ID 策略主键（每个版本独立一行）。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name 策略名称，与版本共同唯一。
	Name string
	// Version 策略版本号。
	Version int
	// MinContextTokens 策略要求的最小上下文窗口。
	MinContextTokens int64
	// Rules 路由规则的 JSON，如健康优先级与成本约束。
	Rules datatypes.JSON
	// Status 生命周期：draft/published/disabled。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定策略表名。
func (policyModel) TableName() string { return "model_policies" }

// policyCandidateModel 对应 model_policy_candidates 表：有序候选模型。
type policyCandidateModel struct {
	// PolicyID 所属策略。
	PolicyID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// ModelID 候选模型。
	ModelID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Priority 显式优先级，同一策略内唯一。
	Priority int `gorm:"primaryKey"`
	// Constraints 单候选约束的 JSON。
	Constraints datatypes.JSON
}

// TableName 指定策略候选表名。
func (policyCandidateModel) TableName() string { return "model_policy_candidates" }

// CatalogRepository 是平台级 Provider、模型与策略的 GORM 实现。
// 平台表为全局共享配置，读操作使用 system scope，写操作要求管理员并附加审计事件。
type CatalogRepository struct {
	db *gorm.DB
}

// NewCatalogRepository 构造模型目录仓库。
func NewCatalogRepository(db *gorm.DB) *CatalogRepository {
	return &CatalogRepository{db: db}
}

// CreateProvider 在 system scope 事务内写入供应商密文并追加审计 Outbox 事件。
func (r *CatalogRepository) CreateProvider(ctx context.Context, actorID uuid.UUID, provider biz.Provider, credential biz.ProviderCredential, event biz.OutboxEvent) (biz.Provider, error) {
	model := providerModel{
		ID: provider.ID, Name: provider.Name, ProviderType: provider.ProviderType, BaseURL: provider.BaseURL,
		APIKeyEncrypted: credential.Encrypted, KeyVersion: credential.KeyVersion, Status: string(provider.Status), DataRegion: provider.DataRegion,
	}
	// 平台表写操作需要 system scope，且业务写入与审计事件同事务提交。
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Provider{}, catalogError("create provider", err)
	}
	return toBizProvider(model), nil
}

// UpdateProvider 更新供应商元数据；credential 非空时同步替换密文与密钥版本。
func (r *CatalogRepository) UpdateProvider(ctx context.Context, actorID uuid.UUID, provider biz.Provider, credential *biz.ProviderCredential, event biz.OutboxEvent) (biz.Provider, error) {
	updates := map[string]any{
		"name": provider.Name, "base_url": provider.BaseURL, "status": string(provider.Status),
		"data_region": provider.DataRegion, "updated_at": time.Now().UTC(),
	}
	if credential != nil {
		updates["api_key_encrypted"] = credential.Encrypted
		updates["key_version"] = credential.KeyVersion
	}
	var model providerModel
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&providerModel{}).Where("id = ?", provider.ID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.First(&model, "id = ?", provider.ID).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Provider{}, catalogError("update provider", err)
	}
	return toBizProvider(model), nil
}

// DeleteProvider 删除供应商；行不存在时返回 ErrResourceNotFound。
func (r *CatalogRepository) DeleteProvider(ctx context.Context, actorID, providerID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&providerModel{}, "id = ?", providerID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete provider", err)
}

// GetProvider 读取供应商元数据，不返回明文密钥。
func (r *CatalogRepository) GetProvider(ctx context.Context, providerID uuid.UUID) (biz.Provider, error) {
	var model providerModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&model, "id = ?", providerID).Error
	})
	if err != nil {
		return biz.Provider{}, catalogError("get provider", err)
	}
	return toBizProvider(model), nil
}

// GetProviderCredential 读取供应商元数据与加密凭证，仅供执行 Worker 解密使用。
// 密文与密钥版本绝不返回给 HTTP/gRPC 调用方，也不得写入事件载荷。
func (r *CatalogRepository) GetProviderCredential(ctx context.Context, providerID uuid.UUID) (biz.Provider, biz.ProviderCredential, error) {
	var record providerModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", providerID).Error
	})
	if err != nil {
		return biz.Provider{}, biz.ProviderCredential{}, catalogError("get provider credential", err)
	}
	credential := biz.ProviderCredential{Encrypted: append([]byte(nil), record.APIKeyEncrypted...), KeyVersion: record.KeyVersion}
	return toBizProvider(record), credential, nil
}

// ListProviders 列出全部供应商，最多 1000 条，避免无界查询。
func (r *CatalogRepository) ListProviders(ctx context.Context) ([]biz.Provider, error) {
	var models []providerModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Order("name, id").Limit(1000).Find(&models).Error
	})
	if err != nil {
		return nil, catalogError("list providers", err)
	}
	providers := make([]biz.Provider, 0, len(models))
	for _, model := range models {
		providers = append(providers, toBizProvider(model))
	}
	return providers, nil
}

// CreateModel 创建模型并与审计事件同事务提交；数据库 CHECK 作为 1M+ 准入的最后防线。
func (r *CatalogRepository) CreateModel(ctx context.Context, actorID uuid.UUID, model biz.Model, event biz.OutboxEvent) (biz.Model, error) {
	record, err := fromBizModel(model)
	if err != nil {
		return biz.Model{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Model{}, catalogError("create model", err)
	}
	return toBizModel(record)
}

// UpdateModel 更新模型元数据并追加审计事件。
func (r *CatalogRepository) UpdateModel(ctx context.Context, actorID uuid.UUID, model biz.Model, event biz.OutboxEvent) (biz.Model, error) {
	record, err := fromBizModel(model)
	if err != nil {
		return biz.Model{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&llmModel{}).Where("id = ?", model.ID).Updates(map[string]any{
			"provider_id": record.ProviderID, "remote_model_name": record.RemoteModelName, "model_kind": record.ModelKind,
			"context_window_tokens": record.ContextWindowTokens, "max_output_tokens": record.MaxOutputTokens,
			"capabilities": record.Capabilities, "input_price_per_million": record.InputPricePerMillion,
			"output_price_per_million": record.OutputPricePerMillion, "data_region": record.DataRegion,
			"verification_source": record.VerificationSource, "verified_at": record.VerifiedAt,
			"status": record.Status, "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.Model{}, catalogError("update model", err)
	}
	return r.GetModel(ctx, model.ID)
}

// DeleteModel 删除模型；仍被策略引用时由外键返回冲突。
func (r *CatalogRepository) DeleteModel(ctx context.Context, actorID, modelID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&llmModel{}, "id = ?", modelID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete model", err)
}

// GetModel 按 ID 读取模型。
func (r *CatalogRepository) GetModel(ctx context.Context, modelID uuid.UUID) (biz.Model, error) {
	var model llmModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&model, "id = ?", modelID).Error
	})
	if err != nil {
		return biz.Model{}, catalogError("get model", err)
	}
	return toBizModel(model)
}

// ListModels 列出模型；providerID 非空时按供应商过滤，最多 1000 条。
func (r *CatalogRepository) ListModels(ctx context.Context, providerID *uuid.UUID) ([]biz.Model, error) {
	var models []llmModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		query := tx.Order("provider_id, remote_model_name, model_kind, id").Limit(1000)
		if providerID != nil {
			query = query.Where("provider_id = ?", *providerID)
		}
		return query.Find(&models).Error
	})
	if err != nil {
		return nil, catalogError("list models", err)
	}
	result := make([]biz.Model, 0, len(models))
	for _, model := range models {
		converted, err := toBizModel(model)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

// ListModelsByIDs 批量读取候选模型，供策略发布准入校验。
func (r *CatalogRepository) ListModelsByIDs(ctx context.Context, modelIDs []uuid.UUID) ([]biz.Model, error) {
	if len(modelIDs) == 0 {
		return []biz.Model{}, nil
	}
	var models []llmModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Where("id IN ?", modelIDs).Find(&models).Error
	})
	if err != nil {
		return nil, catalogError("list candidate models", err)
	}
	result := make([]biz.Model, 0, len(models))
	for _, model := range models {
		converted, err := toBizModel(model)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

// CreatePolicy 在同一事务内创建策略与候选，并追加审计事件。
func (r *CatalogRepository) CreatePolicy(ctx context.Context, actorID uuid.UUID, policy biz.ModelPolicy, event biz.OutboxEvent) (biz.ModelPolicy, error) {
	record, candidates, err := fromBizPolicy(policy)
	if err != nil {
		return biz.ModelPolicy{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if len(candidates) > 0 {
			if err := tx.Create(&candidates).Error; err != nil {
				return err
			}
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.ModelPolicy{}, catalogError("create policy", err)
	}
	return r.GetPolicy(ctx, policy.ID)
}

// UpdatePolicy 仅允许更新草稿策略：先按状态条件更新，再整体替换候选。
func (r *CatalogRepository) UpdatePolicy(ctx context.Context, actorID uuid.UUID, policy biz.ModelPolicy, event biz.OutboxEvent) (biz.ModelPolicy, error) {
	record, candidates, err := fromBizPolicy(policy)
	if err != nil {
		return biz.ModelPolicy{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&policyModel{}).Where("id = ? AND status = ?", policy.ID, biz.PolicyDraft).Updates(map[string]any{
			"min_context_tokens": record.MinContextTokens, "rules": record.Rules, "status": record.Status, "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.Delete(&policyCandidateModel{}, "policy_id = ?", policy.ID).Error; err != nil {
			return err
		}
		if len(candidates) > 0 {
			if err := tx.Create(&candidates).Error; err != nil {
				return err
			}
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.ModelPolicy{}, catalogError("update policy", err)
	}
	return r.GetPolicy(ctx, policy.ID)
}

// DeletePolicy 删除策略；候选随外键级联删除。
func (r *CatalogRepository) DeletePolicy(ctx context.Context, actorID, policyID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&policyModel{}, "id = ?", policyID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete policy", err)
}

// GetPolicy 读取策略及其按优先级排序的候选。
func (r *CatalogRepository) GetPolicy(ctx context.Context, policyID uuid.UUID) (biz.ModelPolicy, error) {
	var record policyModel
	var candidates []policyCandidateModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ?", policyID).Error; err != nil {
			return err
		}
		return tx.Where("policy_id = ?", policyID).Order("priority").Find(&candidates).Error
	})
	if err != nil {
		return biz.ModelPolicy{}, catalogError("get policy", err)
	}
	return toBizPolicy(record, candidates)
}

// ListPolicies 列出全部策略，按名称与版本倒序，最多 1000 条。
func (r *CatalogRepository) ListPolicies(ctx context.Context) ([]biz.ModelPolicy, error) {
	var records []policyModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Order("name, version DESC").Limit(1000).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list policies", err)
	}
	policies := make([]biz.ModelPolicy, 0, len(records))
	for _, record := range records {
		policy, err := r.GetPolicy(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

// NextPolicyVersion 返回同名策略的最大版本号加一。
func (r *CatalogRepository) NextPolicyVersion(ctx context.Context, name string) (int, error) {
	var maximum int
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&policyModel{}).Select("COALESCE(MAX(version), 0)").Where("name = ?", name).Scan(&maximum).Error
	})
	return maximum + 1, catalogError("get next policy version", err)
}

// ValidateCatalog 是启动期硬准入校验：启用模型必须已核验且满足 1M+，
// 已发布策略的候选必须是同上下文下限的生成模型。运行期的供应商停用不阻断启动。
func (r *CatalogRepository) ValidateCatalog(ctx context.Context) error {
	return WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		var invalidModels int64
		if err := tx.Model(&llmModel{}).
			Where(`status = 'enabled' AND (
				verified_at IS NULL OR btrim(verification_source) = '' OR
				(model_kind = 'chat' AND context_window_tokens < ?)
			)`, biz.MinGenerationContextTokens).Count(&invalidModels).Error; err != nil {
			return err
		}
		if invalidModels > 0 {
			return fmt.Errorf("%d enabled models fail startup admission", invalidModels)
		}
		var invalidCandidates int64
		if err := tx.Table("model_policy_candidates AS candidates").
			Joins("JOIN model_policies AS policies ON policies.id = candidates.policy_id").
			Joins("JOIN llm_models AS models ON models.id = candidates.model_id").
			Where(`policies.status = 'published' AND (
				models.model_kind <> 'chat' OR models.context_window_tokens < policies.min_context_tokens
			)`).Count(&invalidCandidates).Error; err != nil {
			return err
		}
		if invalidCandidates > 0 {
			return fmt.Errorf("%d published policy candidates fail startup admission", invalidCandidates)
		}
		return nil
	})
}

// catalogError 将 GORM 与 PostgreSQL 错误映射为领域错误，Service 层据此返回稳定业务码。
func catalogError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var postgresError *pgconn.PgError
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("%s: %w", operation, biz.ErrResourceNotFound)
	case errors.Is(err, gorm.ErrDuplicatedKey), errors.As(err, &postgresError) && postgresError.Code == "23503":
		return fmt.Errorf("%s: %w", operation, biz.ErrResourceConflict)
	case errors.As(err, &postgresError) && postgresError.Code == "23514":
		return fmt.Errorf("%s: %w", operation, biz.ErrInvalidConfig)
	default:
		return fmt.Errorf("%s: %w", operation, err)
	}
}

// toBizProvider 将数据库模型转换为领域实体，刻意不映射密文。
func toBizProvider(model providerModel) biz.Provider {
	return biz.Provider{
		ID: model.ID, Name: model.Name, ProviderType: model.ProviderType, BaseURL: model.BaseURL,
		KeyVersion: model.KeyVersion, Status: biz.ProviderStatus(model.Status), DataRegion: model.DataRegion,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

// fromBizModel 将领域模型序列化为数据库行，能力 JSON 在此编码。
func fromBizModel(model biz.Model) (llmModel, error) {
	capabilities, err := json.Marshal(model.Capabilities)
	if err != nil {
		return llmModel{}, err
	}
	return llmModel{
		ID: model.ID, ProviderID: model.ProviderID, RemoteModelName: model.RemoteModelName, ModelKind: string(model.Kind),
		ContextWindowTokens: model.ContextWindowTokens, MaxOutputTokens: model.MaxOutputTokens, Capabilities: datatypes.JSON(capabilities),
		InputPricePerMillion: model.InputPricePerMillion, OutputPricePerMillion: model.OutputPricePerMillion,
		DataRegion: model.DataRegion, VerificationSource: model.VerificationSource, VerifiedAt: model.VerifiedAt, Status: string(model.Status),
	}, nil
}

// toBizModel 将数据库行转换为领域模型，并解码能力 JSON。
func toBizModel(model llmModel) (biz.Model, error) {
	var capabilities biz.ModelCapabilities
	if err := json.Unmarshal(model.Capabilities, &capabilities); err != nil {
		return biz.Model{}, fmt.Errorf("decode model capabilities: %w", err)
	}
	return biz.Model{
		ID: model.ID, ProviderID: model.ProviderID, RemoteModelName: model.RemoteModelName, Kind: biz.ModelKind(model.ModelKind),
		ContextWindowTokens: model.ContextWindowTokens, MaxOutputTokens: model.MaxOutputTokens, Capabilities: capabilities,
		InputPricePerMillion: model.InputPricePerMillion, OutputPricePerMillion: model.OutputPricePerMillion,
		DataRegion: model.DataRegion, VerificationSource: model.VerificationSource, VerifiedAt: model.VerifiedAt,
		Status: biz.ModelStatus(model.Status), CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

// fromBizPolicy 将策略与候选转换为数据库行，规则与约束 JSON 在此编码。
func fromBizPolicy(policy biz.ModelPolicy) (policyModel, []policyCandidateModel, error) {
	rules, err := json.Marshal(nonNilMap(policy.Rules))
	if err != nil {
		return policyModel{}, nil, err
	}
	candidates := make([]policyCandidateModel, 0, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		constraints, err := json.Marshal(nonNilMap(candidate.Constraints))
		if err != nil {
			return policyModel{}, nil, err
		}
		candidates = append(candidates, policyCandidateModel{PolicyID: policy.ID, ModelID: candidate.ModelID, Priority: candidate.Priority, Constraints: datatypes.JSON(constraints)})
	}
	return policyModel{ID: policy.ID, Name: policy.Name, Version: policy.Version, MinContextTokens: policy.MinContextTokens, Rules: datatypes.JSON(rules), Status: string(policy.Status)}, candidates, nil
}

// toBizPolicy 将策略行与候选行转换为领域聚合。
func toBizPolicy(record policyModel, candidates []policyCandidateModel) (biz.ModelPolicy, error) {
	var rules map[string]any
	if err := json.Unmarshal(record.Rules, &rules); err != nil {
		return biz.ModelPolicy{}, err
	}
	result := biz.ModelPolicy{
		ID: record.ID, Name: record.Name, Version: record.Version, MinContextTokens: record.MinContextTokens,
		Rules: rules, Status: biz.PolicyStatus(record.Status), CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Candidates: make([]biz.PolicyCandidate, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		var constraints map[string]any
		if err := json.Unmarshal(candidate.Constraints, &constraints); err != nil {
			return biz.ModelPolicy{}, err
		}
		result.Candidates = append(result.Candidates, biz.PolicyCandidate{ModelID: candidate.ModelID, Priority: candidate.Priority, Constraints: constraints})
	}
	return result, nil
}

// nonNilMap 保证 JSON 列写入对象而非 null，满足数据库 CHECK 约束。
func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

var _ biz.ProviderRepository = (*CatalogRepository)(nil)
var _ biz.ModelRepository = (*CatalogRepository)(nil)
var _ biz.PolicyRepository = (*CatalogRepository)(nil)
