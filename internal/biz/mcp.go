package biz

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// MCPStatus MCP 接入状态。
type MCPStatus string

const (
	// MCPEnabled MCP 可用，可被 Agent 绑定。
	MCPEnabled MCPStatus = "enabled"
	// MCPDisabled MCP 已停用。
	MCPDisabled MCPStatus = "disabled"
)

// MCPInput 是 MCP 创建/更新入参，Credentials 仅在写入路径短暂存在。
type MCPInput struct {
	// Name MCP 名称。
	Name string
	// Endpoint 远端地址。
	Endpoint string
	// AuthType 鉴权类型。
	AuthType string
	// Credentials 鉴权凭证明文，落库前必须加密。
	Credentials string
	// RiskLevel 风险等级。
	RiskLevel string
	// Status 启停状态。
	Status MCPStatus
}

// MCPRepository 定义 MCP 持久化契约，凭证以密文保存。
type MCPRepository interface {
	// CreateMCP 创建 MCP 并写入审计事件。
	CreateMCP(ctx context.Context, actorID uuid.UUID, mcp MCP, credential *ProviderCredential, event OutboxEvent) (MCP, error)
	// UpdateMCP 更新 MCP；credential 为 nil 时保留原密文。
	UpdateMCP(ctx context.Context, actorID uuid.UUID, mcp MCP, credential *ProviderCredential, event OutboxEvent) (MCP, error)
	// DeleteMCP 删除 MCP。
	DeleteMCP(ctx context.Context, actorID, mcpID uuid.UUID, event OutboxEvent) error
	// GetMCP 按 ID 读取 MCP 元数据。
	GetMCP(ctx context.Context, mcpID uuid.UUID) (MCP, error)
	// ListMCPs 列出全部 MCP。
	ListMCPs(ctx context.Context) ([]MCP, error)
}

// MCPCredentialRepository 在执行层读取 MCP 鉴权凭证，仅允许 Worker 内部使用。
// 读取结果不得进入事件、日志、HTTP/gRPC 响应或审计载荷。
type MCPCredentialRepository interface {
	// GetMCPCredential 返回 MCP 元数据与加密凭证；none 鉴权时密文为空。
	GetMCPCredential(ctx context.Context, mcpID uuid.UUID) (MCP, ProviderCredential, error)
}

// MCPService 实现 MCP 管理，强制 HTTPS、阻止内网与元数据地址，并加密凭证。
type MCPService struct {
	repo   MCPRepository
	cipher CredentialCipher
}

// NewMCPService 构造 MCP 服务。
func NewMCPService(repo MCPRepository, cipher CredentialCipher) (*MCPService, error) {
	if repo == nil || cipher == nil {
		return nil, invalid("MCP repository and credential cipher are required")
	}
	return &MCPService{repo: repo, cipher: cipher}, nil
}

// Create 创建 MCP，端点通过 SSRF 准入且凭证加密后落库。
func (s *MCPService) Create(ctx context.Context, principal Principal, input MCPInput) (MCP, error) {
	if err := RequireAdmin(principal); err != nil {
		return MCP{}, err
	}
	mcp, err := validateMCP(uuid.New(), input)
	if err != nil {
		return MCP{}, err
	}
	credential, err := s.sealCredential(input.AuthType, input.Credentials)
	if err != nil {
		return MCP{}, err
	}
	return s.repo.CreateMCP(ctx, principal.UserID, mcp, credential, mutationEvent(principal.UserID, "mcp.created", "mcp", mcp.ID))
}

// Update 更新 MCP；凭证为空时保留原密文。
func (s *MCPService) Update(ctx context.Context, principal Principal, mcpID uuid.UUID, input MCPInput) (MCP, error) {
	if err := RequireAdmin(principal); err != nil {
		return MCP{}, err
	}
	mcp, err := validateMCP(mcpID, input)
	if err != nil {
		return MCP{}, err
	}
	current, err := s.repo.GetMCP(ctx, mcpID)
	if err != nil {
		return MCP{}, err
	}
	mcp.KeyVersion = current.KeyVersion
	var credential *ProviderCredential
	if input.Credentials != "" {
		sealed, err := s.sealCredential(input.AuthType, input.Credentials)
		if err != nil {
			return MCP{}, err
		}
		credential = sealed
		mcp.KeyVersion = sealed.KeyVersion
	}
	return s.repo.UpdateMCP(ctx, principal.UserID, mcp, credential, mutationEvent(principal.UserID, "mcp.updated", "mcp", mcp.ID))
}

// Delete 删除 MCP。
func (s *MCPService) Delete(ctx context.Context, principal Principal, mcpID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	return s.repo.DeleteMCP(ctx, principal.UserID, mcpID, mutationEvent(principal.UserID, "mcp.deleted", "mcp", mcpID))
}

// Get 读取 MCP 元数据，不返回凭证。
func (s *MCPService) Get(ctx context.Context, principal Principal, mcpID uuid.UUID) (MCP, error) {
	if err := RequireAdmin(principal); err != nil {
		return MCP{}, err
	}
	return s.repo.GetMCP(ctx, mcpID)
}

// List 列出全部 MCP。
func (s *MCPService) List(ctx context.Context, principal Principal) ([]MCP, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListMCPs(ctx)
}

// sealCredential 仅在需要鉴权时加密凭证，none 类型必须留空。
func (s *MCPService) sealCredential(authType, secret string) (*ProviderCredential, error) {
	if authType == "none" {
		if secret != "" {
			return nil, invalid("MCP with auth type none must not carry credentials")
		}
		return nil, nil
	}
	if secret == "" || len(secret) > 4096 {
		return nil, invalid("MCP credentials must contain between 1 and 4096 bytes")
	}
	ciphertext, version, err := s.cipher.Seal([]byte(secret))
	if err != nil {
		return nil, err
	}
	return &ProviderCredential{Encrypted: ciphertext, KeyVersion: version}, nil
}

// validateMCP 校验名称、鉴权类型、风险等级与端点 SSRF 安全性。
func validateMCP(id uuid.UUID, input MCPInput) (MCP, error) {
	name := strings.TrimSpace(input.Name)
	if id == uuid.Nil || name == "" || len(name) > 128 {
		return MCP{}, invalid("MCP id and a name of at most 128 characters are required")
	}
	if input.AuthType != "none" && input.AuthType != "bearer" && input.AuthType != "oauth" && input.AuthType != "header" {
		return MCP{}, invalid("unsupported MCP auth type")
	}
	if input.RiskLevel != "low" && input.RiskLevel != "medium" && input.RiskLevel != "high" {
		return MCP{}, invalid("unsupported MCP risk level")
	}
	status := input.Status
	if status == "" {
		status = MCPEnabled
	}
	if status != MCPEnabled && status != MCPDisabled {
		return MCP{}, invalid("unsupported MCP status")
	}
	normalizedEndpoint, err := validateMCPSsrf(input.Endpoint)
	if err != nil {
		return MCP{}, err
	}
	return MCP{ID: id, Name: name, Endpoint: normalizedEndpoint, AuthType: input.AuthType, RiskLevel: input.RiskLevel, Status: string(status)}, nil
}

// validateMCPSsrf 只允许 HTTPS 公网端点，拒绝内网、回环、链路本地与元数据地址。
func validateMCPSsrf(raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return "", invalid("MCP endpoint must be an HTTPS URL without credentials or fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	if host == "" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", invalid("MCP endpoint host is not a public domain")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return "", invalid("MCP endpoint must not target private or metadata addresses")
		}
		// 云元数据地址 169.254.169.254 属于链路本地网段，已被上面的检查覆盖。
	}
	return endpoint.String(), nil
}
