package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// FactLLMConfig 是 LLM 事实提取配置。
type FactLLMConfig struct {
	// MaxMessageBytes 单条消息进入提示的最大字节数。
	MaxMessageBytes int
	// MaxOutputTokens 事实 JSON 输出上限。
	MaxOutputTokens int
	// Temperature 采样温度，事实提取建议低温度。
	Temperature float64
	// RequestTimeout 单次调用超时。
	RequestTimeout time.Duration
}

// normalize 补齐事实提取默认配置。
func (c FactLLMConfig) normalize() FactLLMConfig {
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

// FactLLMExtractor 使用会话最近一次 Run 的模型生成结构化事实 JSON。
type FactLLMExtractor struct {
	runs      RunRepository
	providers ProviderCredentialRepository
	opener    CredentialOpener
	usage     UsageRecorder
	config    FactLLMConfig
}

// NewFactLLMExtractor 构造 LLM 事实提取器。
func NewFactLLMExtractor(runs RunRepository, providers ProviderCredentialRepository, opener CredentialOpener, config FactLLMConfig) (*FactLLMExtractor, error) {
	if runs == nil || providers == nil || opener == nil {
		return nil, invalid("fact extractor requires runs, providers and credential opener")
	}
	return &FactLLMExtractor{runs: runs, providers: providers, opener: opener, config: config.normalize()}, nil
}

// WithUsageRecorder 注入后台事实提取用量记录器。
func (e *FactLLMExtractor) WithUsageRecorder(recorder UsageRecorder) *FactLLMExtractor {
	e.usage = recorder
	return e
}

// Extract 调用模型提取事实并解析为候选；空输出表示本轮没有可提取事实。
func (e *FactLLMExtractor) Extract(ctx context.Context, userID, sessionID uuid.UUID, messages []Message) ([]FactCandidate, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	resolved, err := resolveSessionModel(ctx, e.runs, e.providers, e.opener, userID, sessionID)
	if err != nil {
		if errors.Is(err, ErrSummaryModelUnavailable) || errors.Is(err, ErrSummaryPermanent) {
			return nil, fmt.Errorf("%w: %v", ErrFactExtractionUnavailable, err)
		}
		return nil, err
	}
	client, err := model.NewClient(resolved.baseURL, resolved.apiKey, e.config.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFactExtractionUnavailable, err)
	}
	system, user, err := memory.BuildFactExtractionPrompt(toSummaryMessages(messages), e.config.MaxMessageBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFactPermanent, err)
	}
	maxOutput := e.config.MaxOutputTokens
	if resolved.maxOutput > 0 && maxOutput > resolved.maxOutput {
		maxOutput = resolved.maxOutput
	}
	response, err := client.Chat(ctx, model.ChatRequest{
		Model: model.ModelRef{
			ProviderID: resolved.providerID.String(), ModelID: resolved.modelID,
			RemoteModelName: resolved.remoteModelName, ContextWindowTokens: resolved.contextWindow,
			MaxOutputTokens: resolved.maxOutput,
		},
		Messages: []model.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: e.config.Temperature, MaxOutputTokens: maxOutput,
	})
	if err != nil {
		if errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrAuthentication) ||
			errors.Is(err, model.ErrCapability) || errors.Is(err, model.ErrContextExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrFactPermanent, err)
		}
		return nil, err
	}
	if strings.TrimSpace(response.Content) == "" {
		return nil, nil
	}
	if len(response.ToolCalls) > 0 {
		return nil, fmt.Errorf("%w: provider returned tool calls for fact extraction", ErrFactPermanent)
	}
	if e.usage != nil {
		providerID := resolved.providerID
		cost := estimateBackgroundCost(resolved.inputPrice, resolved.outputPrice, response.InputTokens, response.OutputTokens)
		if err := e.usage.RecordUsage(ctx, userID, nil, &providerID, int64(response.InputTokens), int64(response.OutputTokens), cost, time.Now().UTC()); err != nil {
			// 用量记录失败不阻断事实提取结果。
			_ = err
		}
	}
	candidates, err := ParseFactModelOutput(response.Content, messages)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFactPermanent, err)
	}
	return candidates, nil
}

// 编译期断言事实提取器实现。
var _ FactExtractor = (*FactLLMExtractor)(nil)
