package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FeishuBotStatus 飞书机器人启停状态。
type FeishuBotStatus string

const (
	// FeishuBotEnabled 机器人可用，可被助手绑定并接收事件。
	FeishuBotEnabled FeishuBotStatus = "enabled"
	// FeishuBotDisabled 机器人已停用，不再路由事件。
	FeishuBotDisabled FeishuBotStatus = "disabled"
)

// FeishuBotSecret 是机器人应用凭据明文，只在写入与验签路径短暂存在，绝不落库或返回响应。
type FeishuBotSecret struct {
	// AppSecret 飞书应用 App Secret。
	AppSecret string `json:"app_secret"`
	// VerificationToken 事件订阅 Verification Token，可为空。
	VerificationToken string `json:"verification_token"`
	// EncryptKey 事件加密密钥，可为空表示飞书不加密事件体。
	EncryptKey string `json:"encrypt_key"`
}

// FeishuBot 是管理员登记的飞书机器人配置，仅保存凭据密文与元数据。
type FeishuBot struct {
	// ID 机器人唯一标识。
	ID uuid.UUID
	// Name 机器人展示名称。
	Name string
	// AppID 飞书应用 App ID，事件回执按此路由。
	AppID string
	// BotOpenID 机器人 open_id，用于群聊 @ 判断。
	BotOpenID string
	// KeyVersion 凭据密文对应的密钥版本。
	KeyVersion string
	// Status 启停状态。
	Status FeishuBotStatus
	// BoundUserID 绑定助手的所属用户；未绑定时为空。
	BoundUserID *uuid.UUID
	// BoundUsername 绑定助手的所属用户名。
	BoundUsername string
	// BoundAgentID 绑定助手版本 ID；未绑定时为空。
	BoundAgentID *uuid.UUID
	// BoundAgentName 绑定助手名称。
	BoundAgentName string
	// BoundAgentVersion 绑定助手版本号。
	BoundAgentVersion *int
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// FeishuBotInput 是机器人创建/更新入参；密钥字段留空表示更新时保留原值。
type FeishuBotInput struct {
	// Name 机器人展示名称。
	Name string
	// AppID 飞书应用 App ID。
	AppID string
	// AppSecret 应用密钥，创建时必填。
	AppSecret string
	// VerificationToken 事件订阅 Verification Token。
	VerificationToken string
	// EncryptKey 事件加密密钥。
	EncryptKey string
	// BotOpenID 机器人 open_id。
	BotOpenID string
	// Status 启停状态。
	Status FeishuBotStatus
}

// FeishuBotRepository 定义机器人配置持久化契约，写操作与审计事件同事务提交。
type FeishuBotRepository interface {
	// CreateFeishuBot 创建机器人并写入加密凭据与审计事件。
	CreateFeishuBot(ctx context.Context, actorID uuid.UUID, bot FeishuBot, credential ProviderCredential, event OutboxEvent) (FeishuBot, error)
	// UpdateFeishuBot 更新机器人；credential 为 nil 时保留原密文。
	UpdateFeishuBot(ctx context.Context, actorID uuid.UUID, bot FeishuBot, credential *ProviderCredential, event OutboxEvent) (FeishuBot, error)
	// DeleteFeishuBot 删除机器人并解除其绑定。
	DeleteFeishuBot(ctx context.Context, actorID, botID uuid.UUID, event OutboxEvent) error
	// GetFeishuBot 按 ID 读取机器人元数据，不返回凭据。
	GetFeishuBot(ctx context.Context, botID uuid.UUID) (FeishuBot, error)
	// ListFeishuBots 列出全部机器人（管理员视图）。
	ListFeishuBots(ctx context.Context) ([]FeishuBot, error)
	// GetFeishuBotCredential 读取机器人元数据与加密凭据，仅供验签与服务端客户端构造使用。
	GetFeishuBotCredential(ctx context.Context, botID uuid.UUID) (FeishuBot, ProviderCredential, error)
}

// FeishuBotTester 抽象机器人应用凭据连通性测试，生产实现调用飞书开放平台换取 Token。
type FeishuBotTester interface {
	// TestBot 使用应用凭据获取 Tenant Access Token，验证 App ID/Secret 是否有效。
	TestBot(ctx context.Context, appID, appSecret string) error
}

// FeishuBotTestResult 是连通性测试结果，Message 已脱敏且可直接展示。
type FeishuBotTestResult struct {
	// OK 是否连通成功。
	OK bool
	// Message 成功或失败的简短说明。
	Message string
}

// FeishuBotService 实现机器人注册管理，仅管理员可写。
type FeishuBotService struct {
	repo   FeishuBotRepository
	cipher CredentialCipher
	opener CredentialOpener
	tester FeishuBotTester
}

// NewFeishuBotService 构造机器人管理服务。
func NewFeishuBotService(repo FeishuBotRepository, cipher CredentialCipher, opener CredentialOpener) (*FeishuBotService, error) {
	if repo == nil || cipher == nil || opener == nil {
		return nil, invalid("feishu bot repository, credential cipher and opener are required")
	}
	return &FeishuBotService{repo: repo, cipher: cipher, opener: opener}, nil
}

// WithTester 启用机器人连通性测试能力。
func (s *FeishuBotService) WithTester(tester FeishuBotTester) *FeishuBotService {
	s.tester = tester
	return s
}

// Create 创建机器人；应用密钥必填，Verification Token 与 Encrypt Key 可选。
func (s *FeishuBotService) Create(ctx context.Context, principal Principal, input FeishuBotInput) (FeishuBot, error) {
	if err := RequireAdmin(principal); err != nil {
		return FeishuBot{}, err
	}
	bot, err := validateFeishuBot(uuid.New(), input)
	if err != nil {
		return FeishuBot{}, err
	}
	if strings.TrimSpace(input.AppSecret) == "" {
		return FeishuBot{}, invalid("feishu bot app secret is required")
	}
	secret := FeishuBotSecret{
		AppSecret: strings.TrimSpace(input.AppSecret), VerificationToken: strings.TrimSpace(input.VerificationToken),
		EncryptKey: strings.TrimSpace(input.EncryptKey),
	}
	credential, err := s.sealSecret(secret)
	if err != nil {
		return FeishuBot{}, err
	}
	bot.KeyVersion = credential.KeyVersion
	return s.repo.CreateFeishuBot(ctx, principal.UserID, bot, credential,
		mutationEvent(principal.UserID, "feishu_bot.created", "feishu_bot", bot.ID))
}

// Update 更新机器人元数据；凭据字段留空时保留原密文，填写时整体重建加密凭据。
func (s *FeishuBotService) Update(ctx context.Context, principal Principal, botID uuid.UUID, input FeishuBotInput) (FeishuBot, error) {
	if err := RequireAdmin(principal); err != nil {
		return FeishuBot{}, err
	}
	current, credential, err := s.repo.GetFeishuBotCredential(ctx, botID)
	if err != nil {
		return FeishuBot{}, err
	}
	bot, err := validateFeishuBot(botID, input)
	if err != nil {
		return FeishuBot{}, err
	}
	bot.KeyVersion = current.KeyVersion
	var updatedCredential *ProviderCredential
	if strings.TrimSpace(input.AppSecret) != "" || strings.TrimSpace(input.VerificationToken) != "" || strings.TrimSpace(input.EncryptKey) != "" {
		plaintext, err := s.opener.Open(credential.KeyVersion, credential.Encrypted)
		if err != nil {
			return FeishuBot{}, err
		}
		var secret FeishuBotSecret
		if err := json.Unmarshal(plaintext, &secret); err != nil {
			return FeishuBot{}, fmt.Errorf("%w: feishu bot credentials are invalid", ErrInvalidConfig)
		}
		if value := strings.TrimSpace(input.AppSecret); value != "" {
			secret.AppSecret = value
		}
		if value := strings.TrimSpace(input.VerificationToken); value != "" {
			secret.VerificationToken = value
		}
		if value := strings.TrimSpace(input.EncryptKey); value != "" {
			secret.EncryptKey = value
		}
		if strings.TrimSpace(secret.AppSecret) == "" {
			return FeishuBot{}, invalid("feishu bot app secret is required")
		}
		sealed, err := s.sealSecret(secret)
		if err != nil {
			return FeishuBot{}, err
		}
		updatedCredential = &sealed
		bot.KeyVersion = sealed.KeyVersion
	}
	return s.repo.UpdateFeishuBot(ctx, principal.UserID, bot, updatedCredential,
		mutationEvent(principal.UserID, "feishu_bot.updated", "feishu_bot", botID))
}

// Delete 删除机器人；绑定关系由外键级联解除。
func (s *FeishuBotService) Delete(ctx context.Context, principal Principal, botID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	return s.repo.DeleteFeishuBot(ctx, principal.UserID, botID, mutationEvent(principal.UserID, "feishu_bot.deleted", "feishu_bot", botID))
}

// List 列出全部机器人及绑定信息，仅管理员可用。
func (s *FeishuBotService) List(ctx context.Context, principal Principal) ([]FeishuBot, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListFeishuBots(ctx)
}

// Get 读取单个机器人元数据，仅管理员可用。
func (s *FeishuBotService) Get(ctx context.Context, principal Principal, botID uuid.UUID) (FeishuBot, error) {
	if err := RequireAdmin(principal); err != nil {
		return FeishuBot{}, err
	}
	return s.repo.GetFeishuBot(ctx, botID)
}

// Test 使用机器人应用凭据请求飞书 Tenant Access Token，验证凭据与网络连通性。
func (s *FeishuBotService) Test(ctx context.Context, principal Principal, botID uuid.UUID) (FeishuBotTestResult, error) {
	if err := RequireAdmin(principal); err != nil {
		return FeishuBotTestResult{}, err
	}
	if s.tester == nil {
		return FeishuBotTestResult{}, fmt.Errorf("%w: feishu bot tester is not configured", ErrInvalidConfig)
	}
	bot, credential, err := s.repo.GetFeishuBotCredential(ctx, botID)
	if err != nil {
		return FeishuBotTestResult{}, err
	}
	plaintext, err := s.opener.Open(credential.KeyVersion, credential.Encrypted)
	if err != nil {
		return FeishuBotTestResult{OK: false, Message: "应用凭据无法解密，请重新填写密钥。"}, nil
	}
	var secret FeishuBotSecret
	if err := json.Unmarshal(plaintext, &secret); err != nil {
		return FeishuBotTestResult{OK: false, Message: "应用凭据格式无效，请重新填写密钥。"}, nil
	}
	if strings.TrimSpace(secret.AppSecret) == "" {
		return FeishuBotTestResult{OK: false, Message: "应用密钥为空，请填写 App Secret。"}, nil
	}
	if err := s.tester.TestBot(ctx, strings.TrimSpace(bot.AppID), strings.TrimSpace(secret.AppSecret)); err != nil {
		// 只返回截断后的错误摘要，避免把上游响应中的长文本或凭据写入页面。
		return FeishuBotTestResult{OK: false, Message: truncateError("连通失败："+err.Error(), 512)}, nil
	}
	return FeishuBotTestResult{OK: true, Message: "已成功获取飞书访问令牌，应用凭据有效。"}, nil
}

// sealSecret 序列化并加密机器人凭据。
func (s *FeishuBotService) sealSecret(secret FeishuBotSecret) (ProviderCredential, error) {
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return ProviderCredential{}, err
	}
	ciphertext, version, err := s.cipher.Seal(plaintext)
	if err != nil {
		return ProviderCredential{}, err
	}
	return ProviderCredential{Encrypted: ciphertext, KeyVersion: version}, nil
}

// validateFeishuBot 校验机器人名称、App ID、open_id 与状态。
func validateFeishuBot(id uuid.UUID, input FeishuBotInput) (FeishuBot, error) {
	name := strings.TrimSpace(input.Name)
	appID := strings.TrimSpace(input.AppID)
	openID := strings.TrimSpace(input.BotOpenID)
	if id == uuid.Nil || name == "" || len(name) > 128 {
		return FeishuBot{}, invalid("feishu bot id and a name of at most 128 characters are required")
	}
	if appID == "" || len(appID) > 128 || len(openID) > 128 {
		return FeishuBot{}, invalid("feishu bot app id is required and identifiers must not exceed 128 characters")
	}
	status := input.Status
	if status == "" {
		status = FeishuBotEnabled
	}
	if status != FeishuBotEnabled && status != FeishuBotDisabled {
		return FeishuBot{}, invalid("unsupported feishu bot status")
	}
	if len(strings.TrimSpace(input.VerificationToken)) > 512 || len(strings.TrimSpace(input.EncryptKey)) > 512 {
		return FeishuBot{}, invalid("feishu bot verification token and encrypt key must not exceed 512 characters")
	}
	return FeishuBot{ID: id, Name: name, AppID: appID, BotOpenID: openID, Status: status}, nil
}

// FeishuBotBinding 是机器人到助手版本的绑定，Worker 据此选择运行助手。
type FeishuBotBinding struct {
	// BotID 机器人 ID。
	BotID uuid.UUID
	// BotName 机器人名称。
	BotName string
	// BotOpenID 机器人 open_id。
	BotOpenID string
	// UserID 绑定助手的所属用户，也是唯一允许使用该机器人的飞书用户。
	UserID uuid.UUID
	// AgentID 绑定的助手版本 ID。
	AgentID uuid.UUID
	// AgentName 助手名称。
	AgentName string
	// AgentVersion 助手版本号。
	AgentVersion int
	// AgentStatus 助手生命周期状态。
	AgentStatus AgentStatus
	// AgentMode 助手编排模式。
	AgentMode AgentMode
}

// FeishuBotOption 是普通用户可见的机器人摘要，其他用户的绑定信息会被脱敏。
type FeishuBotOption struct {
	// ID 机器人 ID。
	ID uuid.UUID
	// Name 机器人名称。
	Name string
	// BotOpenID 机器人 open_id。
	BotOpenID string
	// Status 启停状态。
	Status FeishuBotStatus
	// BoundAgentID 当前用户已绑定的助手；未绑定时为空。
	BoundAgentID *uuid.UUID
	// BoundAgentName 当前用户已绑定的助手名称。
	BoundAgentName string
	// Available 是否可绑定：机器人启用且未被任何助手占用。
	Available bool
}

// FeishuBotBindingRepository 定义绑定持久化契约。
type FeishuBotBindingRepository interface {
	// ListFeishuBotsForUser 返回用户可见的启用机器人及脱敏绑定状态。
	ListFeishuBotsForUser(ctx context.Context, userID uuid.UUID) ([]FeishuBot, error)
	// BindFeishuBot 创建绑定；机器人已被占用时返回资源冲突。
	BindFeishuBot(ctx context.Context, binding FeishuBotBinding, event OutboxEvent) error
	// UnbindFeishuBot 解除助手的机器人绑定，返回受影响行数。
	UnbindFeishuBot(ctx context.Context, userID, agentID uuid.UUID, event OutboxEvent) (int, error)
}

// FeishuBotAgentLookup 是绑定服务校验助手归属与状态的窄接口。
type FeishuBotAgentLookup interface {
	// GetAgent 按用户与 ID 读取助手版本。
	GetAgent(ctx context.Context, userID, agentID uuid.UUID) (Agent, error)
}

// FeishuBotBindingService 提供用户侧机器人查询、绑定与解绑用例。
type FeishuBotBindingService struct {
	bots     FeishuBotRepository
	bindings FeishuBotBindingRepository
	agents   FeishuBotAgentLookup
}

// NewFeishuBotBindingService 构造机器人绑定服务。
func NewFeishuBotBindingService(bots FeishuBotRepository, bindings FeishuBotBindingRepository, agents FeishuBotAgentLookup) (*FeishuBotBindingService, error) {
	if bots == nil || bindings == nil || agents == nil {
		return nil, invalid("feishu bot binding service requires bots, bindings and agents")
	}
	return &FeishuBotBindingService{bots: bots, bindings: bindings, agents: agents}, nil
}

// List 返回当前用户可见的启用机器人；其他用户的绑定只显示“已占用”。
func (s *FeishuBotBindingService) List(ctx context.Context, principal Principal) ([]FeishuBotOption, error) {
	if principal.UserID == uuid.Nil {
		return nil, ErrForbidden
	}
	bots, err := s.bindings.ListFeishuBotsForUser(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	options := make([]FeishuBotOption, 0, len(bots))
	for _, bot := range bots {
		occupied := bot.BoundUserID != nil
		option := FeishuBotOption{ID: bot.ID, Name: bot.Name, BotOpenID: bot.BotOpenID, Status: bot.Status, Available: bot.Status == FeishuBotEnabled && !occupied}
		if occupied && *bot.BoundUserID == principal.UserID {
			option.BoundAgentID = bot.BoundAgentID
			option.BoundAgentName = bot.BoundAgentName
		}
		options = append(options, option)
	}
	return options, nil
}

// Bind 把已发布助手绑定到启用且未占用的机器人；一个机器人只能绑定一个助手。
func (s *FeishuBotBindingService) Bind(ctx context.Context, principal Principal, agentID, botID uuid.UUID) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	if agentID == uuid.Nil || botID == uuid.Nil {
		return invalid("agent id and bot id are required")
	}
	agent, err := s.agents.GetAgent(ctx, principal.UserID, agentID)
	if err != nil {
		return err
	}
	if agent.Status != AgentPublished {
		return fmt.Errorf("%w: only published agents can bind a feishu bot", ErrResourceConflict)
	}
	bot, err := s.bots.GetFeishuBot(ctx, botID)
	if err != nil {
		return err
	}
	if bot.Status != FeishuBotEnabled {
		return fmt.Errorf("%w: the feishu bot is disabled", ErrResourceConflict)
	}
	binding := FeishuBotBinding{
		BotID: botID, BotName: bot.Name, BotOpenID: bot.BotOpenID, UserID: principal.UserID,
		AgentID: agent.ID, AgentName: agent.Name, AgentVersion: agent.Version,
		AgentStatus: agent.Status, AgentMode: agent.Mode,
	}
	return s.bindings.BindFeishuBot(ctx, binding,
		mutationEvent(principal.UserID, "feishu_bot.bound", "agent", agentID))
}

// Unbind 解除助手当前绑定的机器人；未绑定时幂等返回 nil。
func (s *FeishuBotBindingService) Unbind(ctx context.Context, principal Principal, agentID uuid.UUID) error {
	if principal.UserID == uuid.Nil {
		return ErrForbidden
	}
	if agentID == uuid.Nil {
		return invalid("agent id is required")
	}
	if _, err := s.agents.GetAgent(ctx, principal.UserID, agentID); err != nil {
		return err
	}
	_, err := s.bindings.UnbindFeishuBot(ctx, principal.UserID, agentID,
		mutationEvent(principal.UserID, "feishu_bot.unbound", "agent", agentID))
	return err
}

// FeishuBotRouter 是事件 Worker 解析机器人与其绑定的端口。
type FeishuBotRouter interface {
	// LookupFeishuBot 按 App ID 读取机器人配置；未登记时 found 为 false。
	LookupFeishuBot(ctx context.Context, appID string) (FeishuBot, bool, error)
	// FindFeishuBotBinding 按机器人 ID 查找绑定助手。
	FindFeishuBotBinding(ctx context.Context, botID uuid.UUID) (FeishuBotBinding, bool, error)
}

// FeishuBotCredentialReader 是事件接入按机器人 ID 读取验签凭据的端口。
type FeishuBotCredentialReader interface {
	// GetFeishuBotCredential 返回机器人元数据与加密凭据。
	GetFeishuBotCredential(ctx context.Context, botID uuid.UUID) (FeishuBot, ProviderCredential, error)
}

// FeishuReplySenderResolver 按 App ID 解析外发回复使用的飞书客户端。
type FeishuReplySenderResolver interface {
	// ResolveSender 返回指定应用对应的回复发送端口；未登记时返回默认客户端。
	ResolveSender(ctx context.Context, appID string) (FeishuReplySender, error)
}
