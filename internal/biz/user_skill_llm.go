package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// UserSkillLLMConfig 是 LLM 技能提炼配置。
type UserSkillLLMConfig struct {
	// DefaultModelPolicyID 平台默认策略；仅用户未配置策略时使用。
	DefaultModelPolicyID *uuid.UUID
	// DefaultModelPolicyVersion 平台默认策略版本。
	DefaultModelPolicyVersion int
	// MaxMessageBytes 单条消息进入提示的最大字节数。
	MaxMessageBytes int
	// MaxOutputTokens 技能 JSON 输出上限。
	MaxOutputTokens int
	// Temperature 采样温度，技能提炼建议低温度。
	Temperature float64
	// RequestTimeout 单次调用超时。
	RequestTimeout time.Duration
}

// normalize 补齐技能提炼默认配置。
func (c UserSkillLLMConfig) normalize() UserSkillLLMConfig {
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 8 * 1024
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = 2048
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	return c
}

// SkillLLMExtractor 使用用户配置或平台默认的已发布策略提炼可复用技能。
type SkillLLMExtractor struct {
	settings  UserSkillSynthesisSettingsReader
	policies  PolicyRepository
	providers ProviderCredentialRepository
	opener    CredentialOpener
	usage     UsageRecorder
	config    UserSkillLLMConfig
}

// NewSkillLLMExtractor 构造 LLM 技能提炼器。
func NewSkillLLMExtractor(settings UserSkillSynthesisSettingsReader, policies PolicyRepository, providers ProviderCredentialRepository, opener CredentialOpener, config UserSkillLLMConfig) (*SkillLLMExtractor, error) {
	if settings == nil || policies == nil || providers == nil || opener == nil {
		return nil, invalid("skill extractor requires settings, policies, providers and credential opener")
	}
	if (config.DefaultModelPolicyID == nil) != (config.DefaultModelPolicyVersion == 0) {
		return nil, invalid("default skill synthesis policy id and version must be configured together")
	}
	if config.DefaultModelPolicyID != nil && (*config.DefaultModelPolicyID == uuid.Nil || config.DefaultModelPolicyVersion <= 0) {
		return nil, invalid("default skill synthesis policy requires a valid id and positive version")
	}
	return &SkillLLMExtractor{settings: settings, policies: policies, providers: providers, opener: opener, config: config.normalize()}, nil
}

// WithUsageRecorder 注入后台技能提炼用量记录器。
func (e *SkillLLMExtractor) WithUsageRecorder(recorder UsageRecorder) *SkillLLMExtractor {
	e.usage = recorder
	return e
}

// Extract 按“用户策略、平台默认策略”的顺序解析配置，并依候选优先级调用模型。
// 不再继承最近会话 Run；没有显式可用策略时返回永久不可用错误。
func (e *SkillLLMExtractor) Extract(ctx context.Context, userID uuid.UUID, sessions []Session, messagesBySession map[uuid.UUID][]Message) (SkillCandidate, error) {
	if len(sessions) == 0 || len(messagesBySession) == 0 {
		return SkillCandidate{ShouldCreate: false}, nil
	}
	settings, err := e.settings.GetUserSkillSynthesisSettings(ctx, userID)
	if err != nil {
		return SkillCandidate{}, err
	}
	if !settings.Enabled {
		return SkillCandidate{}, fmt.Errorf("%w: user disabled skill synthesis", ErrUserSkillSynthesisUnavailable)
	}
	policyID, policyVersion, err := e.selectPolicy(settings)
	if err != nil {
		return SkillCandidate{}, err
	}
	policy, err := e.policies.GetPolicy(ctx, policyID)
	if err != nil {
		if errors.Is(err, ErrResourceNotFound) {
			return SkillCandidate{}, fmt.Errorf("%w: selected policy does not exist", ErrUserSkillSynthesisUnavailable)
		}
		return SkillCandidate{}, err
	}
	if policy.Version != policyVersion || policy.Status != PolicyPublished {
		return SkillCandidate{}, fmt.Errorf("%w: selected policy version is not published", ErrUserSkillSynthesisUnavailable)
	}
	system, user, err := buildSkillExtractionPrompt(sessions, messagesBySession, e.config.MaxMessageBytes)
	if err != nil {
		return SkillCandidate{}, fmt.Errorf("%w: %v", ErrUserSkillSynthesisUnavailable, err)
	}
	candidates := append([]PolicyCandidate(nil), policy.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Priority < candidates[j].Priority })
	if len(candidates) == 0 {
		return SkillCandidate{}, fmt.Errorf("%w: selected policy has no candidates", ErrUserSkillSynthesisUnavailable)
	}
	modelIDs := make([]uuid.UUID, 0, len(candidates))
	for _, candidate := range candidates {
		modelIDs = append(modelIDs, candidate.ModelID)
	}
	catalogModels, err := e.policies.ListModelsByIDs(ctx, modelIDs)
	if err != nil {
		return SkillCandidate{}, err
	}
	modelsByID := make(map[uuid.UUID]Model, len(catalogModels))
	for _, catalogModel := range catalogModels {
		modelsByID[catalogModel.ID] = catalogModel
	}

	var lastTemporary error
	var lastPermanent error
	for _, candidate := range candidates {
		catalogModel, ok := modelsByID[candidate.ModelID]
		if !ok {
			lastPermanent = fmt.Errorf("candidate model %s does not exist", candidate.ModelID)
			continue
		}
		if err := validateSkillSynthesisCandidate(policy, candidate, catalogModel); err != nil {
			lastPermanent = err
			continue
		}
		provider, credential, err := e.providers.GetProviderCredential(ctx, catalogModel.ProviderID)
		if err != nil {
			if errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrInvalidConfig) {
				lastPermanent = err
			} else {
				lastTemporary = err
			}
			continue
		}
		if provider.Status != ProviderEnabled {
			lastPermanent = fmt.Errorf("provider %s is disabled", provider.ID)
			continue
		}
		secret, err := e.opener.Open(credential.KeyVersion, credential.Encrypted)
		if err != nil || len(secret) == 0 {
			lastPermanent = fmt.Errorf("provider %s credential cannot be decrypted", provider.ID)
			continue
		}
		apiKey := string(secret)
		for index := range secret {
			secret[index] = 0
		}
		client, err := model.NewClient(provider.BaseURL, apiKey, e.config.RequestTimeout)
		if err != nil {
			lastPermanent = err
			continue
		}
		maxOutput := e.config.MaxOutputTokens
		if catalogModel.MaxOutputTokens > 0 && maxOutput > catalogModel.MaxOutputTokens {
			maxOutput = catalogModel.MaxOutputTokens
		}
		response, chatErr := client.Chat(ctx, model.ChatRequest{
			Model: model.ModelRef{
				ProviderID: provider.ID.String(), ModelID: catalogModel.ID.String(),
				RemoteModelName: catalogModel.RemoteModelName, ContextWindowTokens: catalogModel.ContextWindowTokens,
				MaxOutputTokens: catalogModel.MaxOutputTokens,
			},
			Messages:    []model.ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
			Temperature: e.config.Temperature, MaxOutputTokens: maxOutput,
		})
		e.recordUsage(ctx, userID, provider.ID, catalogModel, response)
		if chatErr != nil {
			if ctx.Err() != nil {
				return SkillCandidate{}, ctx.Err()
			}
			if errors.Is(chatErr, model.ErrPermanent) || errors.Is(chatErr, model.ErrAuthentication) ||
				errors.Is(chatErr, model.ErrCapability) || errors.Is(chatErr, model.ErrContextExceeded) {
				lastPermanent = chatErr
			} else {
				lastTemporary = chatErr
			}
			continue
		}
		if strings.TrimSpace(response.Content) == "" || len(response.ToolCalls) > 0 {
			return SkillCandidate{ShouldCreate: false}, nil
		}
		result, parseErr := parseSkillCandidate(response.Content)
		if parseErr != nil {
			// 模型输出无法解析时不重试，视为不值得生成。
			return SkillCandidate{ShouldCreate: false}, nil
		}
		return result, nil
	}
	if lastTemporary != nil {
		return SkillCandidate{}, lastTemporary
	}
	if lastPermanent != nil {
		return SkillCandidate{}, fmt.Errorf("%w: %v", ErrUserSkillSynthesisUnavailable, lastPermanent)
	}
	return SkillCandidate{}, fmt.Errorf("%w: selected policy has no routable candidates", ErrUserSkillSynthesisUnavailable)
}

// selectPolicy 选择用户显式策略；未配置时才使用平台默认策略。
func (e *SkillLLMExtractor) selectPolicy(settings UserSkillSynthesisSettings) (uuid.UUID, int, error) {
	if settings.ModelPolicyID != nil || settings.ModelPolicyVersion != nil {
		if settings.ModelPolicyID == nil || settings.ModelPolicyVersion == nil ||
			*settings.ModelPolicyID == uuid.Nil || *settings.ModelPolicyVersion <= 0 {
			return uuid.Nil, 0, fmt.Errorf("%w: user policy id and version are invalid", ErrUserSkillSynthesisUnavailable)
		}
		return *settings.ModelPolicyID, *settings.ModelPolicyVersion, nil
	}
	if e.config.DefaultModelPolicyID == nil {
		return uuid.Nil, 0, fmt.Errorf("%w: neither user nor platform default policy is configured", ErrUserSkillSynthesisUnavailable)
	}
	return *e.config.DefaultModelPolicyID, e.config.DefaultModelPolicyVersion, nil
}

// validateSkillSynthesisCandidate 在调用前重新执行模型能力、上下文与区域硬准入。
func validateSkillSynthesisCandidate(policy ModelPolicy, candidate PolicyCandidate, catalogModel Model) error {
	if catalogModel.Kind != ModelKindChat || catalogModel.Status != ModelEnabled ||
		catalogModel.VerifiedAt == nil || strings.TrimSpace(catalogModel.VerificationSource) == "" ||
		strings.TrimSpace(catalogModel.RemoteModelName) == "" || catalogModel.MaxOutputTokens <= 0 {
		return fmt.Errorf("candidate model %s is not an enabled and verified chat model", catalogModel.ID)
	}
	if catalogModel.ContextWindowTokens < policy.MinContextTokens {
		return fmt.Errorf("candidate model %s does not satisfy the policy context minimum", catalogModel.ID)
	}
	region, err := candidateDataRegion(candidate.Constraints)
	if err != nil {
		return err
	}
	if region != "" && catalogModel.DataRegion != region {
		return fmt.Errorf("candidate model %s does not satisfy the data region constraint", catalogModel.ID)
	}
	return nil
}

// recordUsage 按实际供应商记录单次候选调用用量；记录失败不改变提炼结果。
func (e *SkillLLMExtractor) recordUsage(ctx context.Context, userID, providerID uuid.UUID, catalogModel Model, response model.ChatResponse) {
	if e.usage == nil || (response.InputTokens <= 0 && response.OutputTokens <= 0) {
		return
	}
	cost := estimateBackgroundCost(catalogModel.InputPricePerMillion, catalogModel.OutputPricePerMillion, response.InputTokens, response.OutputTokens)
	if err := e.usage.RecordUsage(context.WithoutCancel(ctx), userID, nil, &providerID,
		int64(response.InputTokens), int64(response.OutputTokens), cost, time.Now().UTC()); err != nil {
		_ = err
	}
}

// skillExtractionOutput 是 LLM 输出的 JSON 结构。
type skillExtractionOutput struct {
	ShouldCreate   bool           `json:"should_create"`
	Confidence     float64        `json:"confidence"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	PromptTemplate string         `json:"prompt_template"`
	Parameters     map[string]any `json:"parameters"`
}

// parseSkillCandidate 解析模型输出为技能候选。
func parseSkillCandidate(text string) (SkillCandidate, error) {
	// 兼容模型在 JSON 外包裹 markdown 代码块的情况。
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	var output skillExtractionOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return SkillCandidate{}, err
	}
	if !output.ShouldCreate {
		return SkillCandidate{ShouldCreate: false}, nil
	}
	candidate := SkillCandidate{
		ShouldCreate:   true,
		Confidence:     output.Confidence,
		Name:           strings.TrimSpace(output.Name),
		Description:    strings.TrimSpace(output.Description),
		PromptTemplate: strings.TrimSpace(output.PromptTemplate),
		Parameters:     output.Parameters,
	}
	if candidate.Name == "" || candidate.PromptTemplate == "" {
		return SkillCandidate{}, fmt.Errorf("skill name and prompt template are required when should_create is true")
	}
	if candidate.Parameters == nil {
		candidate.Parameters = map[string]any{}
	}
	return candidate, nil
}

// buildSkillExtractionPrompt 构建技能提炼的 system 与 user 提示。
func buildSkillExtractionPrompt(sessions []Session, messagesBySession map[uuid.UUID][]Message, maxMessageBytes int) (string, string, error) {
	system := strings.TrimSpace(`你是一个技能提炼助手。用户会提供过去一天的多轮对话记录。
请判断这些对话中是否蕴含一个可复用的"技能"（Skill）——即一个可泛化、可重复使用的提示词模板，
用于解决某类常见任务或遵循某种稳定的工作流程。

判断标准（满足才输出 should_create=true）：
1. 对话中存在重复出现的任务模式、工作流或结构化处理方式；
2. 该模式可被抽象为一个通用提示词模板，脱离具体对话上下文仍有价值；
3. 模板不包含用户个人隐私数据（姓名、账号、密钥等），需泛化处理。

输出严格 JSON，不要输出任何额外文本或代码块标记，结构如下：
{
  "should_create": <boolean>,
  "confidence": <0 到 1 之间的浮点数>,
  "name": "<技能名称，英文 snake_case，不超过 64 字符>",
  "description": "<技能用途描述，不超过 500 字>",
  "prompt_template": "<提示词模板正文，使用 {{参数名}} 占位>",
  "parameters": {<JSON Schema 风格的参数定义对象>}
}

若不值得生成技能，只输出 {"should_create": false}。`)

	var builder strings.Builder
	written := 0
	builder.WriteString("以下是过去一天的对话记录（按会话分组）：\n\n")
	for _, session := range sessions {
		messages, ok := messagesBySession[session.ID]
		if !ok || len(messages) == 0 {
			continue
		}
		title := session.Title
		if strings.TrimSpace(title) == "" {
			title = "(无标题)"
		}
		builder.WriteString(fmt.Sprintf("=== 会话: %s ===\n", title))
		for _, message := range messages {
			content := message.Content
			if len(content) > maxMessageBytes {
				content = content[:maxMessageBytes] + "\n...[已截断]"
			}
			role := "用户"
			if message.Role == MessageAssistant {
				role = "助手"
			}
			builder.WriteString(fmt.Sprintf("[%s] %s\n", role, content))
			written++
		}
		builder.WriteString("\n")
	}
	if written == 0 {
		return "", "", fmt.Errorf("no conversation content provided")
	}
	return system, builder.String(), nil
}
