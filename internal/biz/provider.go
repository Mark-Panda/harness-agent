package biz

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProviderStatus 表示 LLM 供应商连接的启停状态。
type ProviderStatus string

const (
	// ProviderEnabled 表示供应商连接可用，允许其下模型参与路由。
	ProviderEnabled ProviderStatus = "enabled"
	// ProviderDisabled 表示供应商连接被管理员停用，不参与新的模型调用。
	ProviderDisabled ProviderStatus = "disabled"
)

// Provider 是 LLM 供应商连接的领域实体，仅保存连接与凭证元数据，绝不承载明文密钥。
type Provider struct {
	// ID 供应商唯一标识。
	ID uuid.UUID
	// Name 供应商展示名称，全局唯一。
	Name string
	// ProviderType 供应商标识类型，当前固定为 openai_compatible。
	ProviderType string
	// BaseURL OpenAI-compatible 服务的基础地址，必须为 HTTPS。
	BaseURL string
	// KeyVersion 当前 API Key 密文所使用的密钥版本，用于轮换解密。
	KeyVersion string
	// Status 供应商启停状态。
	Status ProviderStatus
	// DataRegion 数据所在区域，用于合规路由过滤。
	DataRegion string
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// ProviderInput 是创建或更新供应商的入参，APIKey 仅用于写入，任何查询接口都不返回。
type ProviderInput struct {
	// Name 供应商名称。
	Name string
	// BaseURL 服务基础地址。
	BaseURL string
	// APIKey 明文密钥，仅在写入路径短暂存在，落库前必须加密。
	APIKey string
	// Status 目标启停状态，留空时默认启用。
	Status ProviderStatus
	// DataRegion 数据区域。
	DataRegion string
}

// ProviderCredential 是加密后的供应商凭证，仅允许 Data 层持久化。
type ProviderCredential struct {
	// Encrypted AES-256-GCM 密文，包含随机 nonce。
	Encrypted []byte
	// KeyVersion 加密所用密钥版本，解密时必须显式指定。
	KeyVersion string
}

// CredentialCipher 抽象凭证加密能力，便于替换密钥轮换实现与测试注入。
type CredentialCipher interface {
	// Seal 使用当前主密钥加密明文，返回密文与密钥版本。
	Seal(plaintext []byte) (ciphertext []byte, keyVersion string, err error)
}

// ProviderRepository 定义供应商持久化契约，所有写操作必须与审计 Outbox 同事务提交。
type ProviderRepository interface {
	// CreateProvider 创建供应商并写入审计事件。
	CreateProvider(ctx context.Context, actorID uuid.UUID, provider Provider, credential ProviderCredential, event OutboxEvent) (Provider, error)
	// UpdateProvider 更新供应商；credential 为 nil 表示保留原密钥。
	UpdateProvider(ctx context.Context, actorID uuid.UUID, provider Provider, credential *ProviderCredential, event OutboxEvent) (Provider, error)
	// DeleteProvider 删除供应商并写入审计事件。
	DeleteProvider(ctx context.Context, actorID, providerID uuid.UUID, event OutboxEvent) error
	// GetProvider 按 ID 读取供应商（不含明文密钥）。
	GetProvider(ctx context.Context, providerID uuid.UUID) (Provider, error)
	// ListProviders 列出全部供应商，供管理员管理台使用。
	ListProviders(ctx context.Context) ([]Provider, error)
}

// ProviderCredentialRepository 在执行层读取供应商密文凭证，仅允许 Worker 内部使用。
// 读取结果不得进入事件、日志、HTTP/gRPC 响应或审计载荷。
type ProviderCredentialRepository interface {
	// GetProviderCredential 返回供应商元数据与加密凭证；供应商不存在时返回未找到。
	GetProviderCredential(ctx context.Context, providerID uuid.UUID) (Provider, ProviderCredential, error)
}

// CredentialOpener 抽象按密钥版本解密凭证的能力，生产实现为 AES-256-GCM 密钥环。
type CredentialOpener interface {
	// Open 使用指定版本的密钥解密；版本未知或密文被篡改时返回错误。
	Open(keyVersion string, ciphertext []byte) ([]byte, error)
}

// ProviderService 实现供应商管理用例：仅管理员可写，凭证只写不读。
type ProviderService struct {
	repo   ProviderRepository
	cipher CredentialCipher
}

// NewProviderService 构造供应商服务，依赖仓库与凭证加密器。
func NewProviderService(repo ProviderRepository, cipher CredentialCipher) (*ProviderService, error) {
	if repo == nil || cipher == nil {
		return nil, invalid("provider repository and credential cipher are required")
	}
	return &ProviderService{repo: repo, cipher: cipher}, nil
}

// Create 创建供应商：校验管理员身份、校验入参并加密 API Key 后再落库。
func (s *ProviderService) Create(ctx context.Context, principal Principal, input ProviderInput) (Provider, error) {
	if err := RequireAdmin(principal); err != nil {
		return Provider{}, err
	}
	provider, err := validateProviderInput(uuid.New(), input)
	if err != nil {
		return Provider{}, err
	}
	credential, err := s.seal(input.APIKey)
	if err != nil {
		return Provider{}, err
	}
	return s.repo.CreateProvider(ctx, principal.UserID, provider, credential, mutationEvent(principal.UserID, "provider.created", "llm_provider", provider.ID))
}

// Update 更新供应商；APIKey 为空时保留原密文，实现密钥无损更新。
func (s *ProviderService) Update(ctx context.Context, principal Principal, providerID uuid.UUID, input ProviderInput) (Provider, error) {
	if err := RequireAdmin(principal); err != nil {
		return Provider{}, err
	}
	provider, err := validateProviderInput(providerID, input)
	if err != nil {
		return Provider{}, err
	}
	var credential *ProviderCredential
	if input.APIKey != "" {
		sealed, err := s.seal(input.APIKey)
		if err != nil {
			return Provider{}, err
		}
		credential = &sealed
		provider.KeyVersion = sealed.KeyVersion
	}
	return s.repo.UpdateProvider(ctx, principal.UserID, provider, credential, mutationEvent(principal.UserID, "provider.updated", "llm_provider", provider.ID))
}

// Delete 删除供应商，要求管理员身份并写入审计事件。
func (s *ProviderService) Delete(ctx context.Context, principal Principal, providerID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	if providerID == uuid.Nil {
		return invalid("provider id is required")
	}
	return s.repo.DeleteProvider(ctx, principal.UserID, providerID, mutationEvent(principal.UserID, "provider.deleted", "llm_provider", providerID))
}

// Get 读取供应商元数据，管理员专用。
func (s *ProviderService) Get(ctx context.Context, principal Principal, providerID uuid.UUID) (Provider, error) {
	if err := RequireAdmin(principal); err != nil {
		return Provider{}, err
	}
	return s.repo.GetProvider(ctx, providerID)
}

// List 列出供应商元数据，管理员专用。
func (s *ProviderService) List(ctx context.Context, principal Principal) ([]Provider, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListProviders(ctx)
}

// seal 校验密钥长度并调用密钥环加密，返回带版本的密文。
func (s *ProviderService) seal(secret string) (ProviderCredential, error) {
	if secret == "" || len(secret) > 4096 {
		return ProviderCredential{}, invalid("provider API key must contain between 1 and 4096 bytes")
	}
	ciphertext, version, err := s.cipher.Seal([]byte(secret))
	if err != nil {
		return ProviderCredential{}, err
	}
	return ProviderCredential{Encrypted: ciphertext, KeyVersion: version}, nil
}

// validateProviderInput 校验并规范化供应商入参：名称长度、HTTPS 地址、状态与区域。
func validateProviderInput(id uuid.UUID, input ProviderInput) (Provider, error) {
	name := strings.TrimSpace(input.Name)
	if id == uuid.Nil || len(name) < 1 || len(name) > 128 {
		return Provider{}, invalid("provider id and a name of at most 128 characters are required")
	}
	// 只允许无凭据、无查询串、无片段的 HTTPS 基址，避免密钥出现在 URL 或被重定向泄露。
	baseURL, err := url.Parse(strings.TrimSpace(input.BaseURL))
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return Provider{}, invalid("provider base URL must be an HTTPS endpoint without credentials, query or fragment")
	}
	status := input.Status
	if status == "" {
		status = ProviderEnabled
	}
	if status != ProviderEnabled && status != ProviderDisabled {
		return Provider{}, invalid("unsupported provider status")
	}
	if len(input.DataRegion) > 64 {
		return Provider{}, invalid("provider data region is too long")
	}
	return Provider{ID: id, Name: name, ProviderType: "openai_compatible", BaseURL: strings.TrimRight(baseURL.String(), "/"), Status: status, DataRegion: strings.TrimSpace(input.DataRegion)}, nil
}
