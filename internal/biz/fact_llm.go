package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
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
	budgets   BudgetChecker
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

// WithBudgetChecker 注入后台事实提取预算门禁。
func (e *FactLLMExtractor) WithBudgetChecker(checker BudgetChecker) *FactLLMExtractor {
	e.budgets = checker
	return e
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
	request := model.ChatRequest{
		Model: model.ModelRef{
			ProviderID: resolved.providerID.String(), ModelID: resolved.modelID,
			RemoteModelName: resolved.remoteModelName, ContextWindowTokens: resolved.contextWindow,
			MaxOutputTokens: resolved.maxOutput,
		},
		Temperature: e.config.Temperature, MaxOutputTokens: maxOutput,
	}
	completion, err := completeText(ctx, []react.Message{
		{Role: react.RoleSystem, Content: system}, {Role: react.RoleUser, Content: user},
	}, func(callCtx context.Context, messages []react.Message) (react.Completion, error) {
		if err := checkBackgroundBudget(callCtx, e.budgets, userID, resolved.providerID); err != nil {
			return react.Completion{}, err
		}
		request.Messages = chatMessagesFromReact(messages)
		response, chatErr := client.Chat(callCtx, request)
		// 每次真实请求（包括续写和携带 usage 的错误响应）都立即计量。
		if e.usage != nil && (response.InputTokens > 0 || response.OutputTokens > 0) {
			providerID := resolved.providerID
			cost := estimateBackgroundCost(resolved.inputPrice, resolved.outputPrice, response.InputTokens, response.OutputTokens)
			if recordErr := e.usage.RecordUsage(context.WithoutCancel(callCtx), userID, nil, &providerID,
				int64(response.InputTokens), int64(response.OutputTokens), cost, time.Now().UTC()); recordErr != nil {
				_ = recordErr
			}
		}
		return completionFromChatResponse(response), chatErr
	})
	if err != nil {
		if errors.Is(err, ErrBudgetExceeded) {
			return nil, err
		}
		if isCompletionGuardError(err) || errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrAuthentication) ||
			errors.Is(err, model.ErrCapability) || errors.Is(err, model.ErrContextExceeded) {
			return nil, fmt.Errorf("%w: %w", ErrFactPermanent, err)
		}
		return nil, err
	}
	candidates, err := ParseFactModelOutput(completion.Content, messages)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFactPermanent, err)
	}
	return candidates, nil
}

// 编译期断言事实提取器实现。
var _ FactExtractor = (*FactLLMExtractor)(nil)
