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
	"github.com/Mark-Panda/harness-agent/pkg/memory"
	"github.com/google/uuid"
)

// SummaryLLMConfig 是 LLM 摘要生成器配置。
type SummaryLLMConfig struct {
	// MaxInputBytes 单次摘要调用的输入分块上限，超过后自动分块归并。
	MaxInputBytes int
	// MaxMessageBytes 单条消息纳入摘要的最大字节数，超长内容截断并标记。
	MaxMessageBytes int
	// MaxOutputTokens 摘要输出上限。
	MaxOutputTokens int
	// Temperature 摘要采样温度，建议低温度以保证忠实。
	Temperature float64
	// RequestTimeout 单次摘要模型调用超时。
	RequestTimeout time.Duration
}

// normalize 补齐 LLM 摘要生成器默认配置。
func (c SummaryLLMConfig) normalize() SummaryLLMConfig {
	if c.MaxInputBytes <= 0 {
		c.MaxInputBytes = 128 * 1024
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 8 * 1024
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = 1024
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	return c
}

// SummaryLLMGenerator 使用会话最近一次 Run 固化的模型候选生成摘要。
// 摘要调用不携带工具、不持久化隐藏思维链，只保存公开文本与 Token 元数据。
type SummaryLLMGenerator struct {
	runs      RunRepository
	providers ProviderCredentialRepository
	opener    CredentialOpener
	budgets   BudgetChecker
	limiter   model.ConcurrencyLimiter
	config    SummaryLLMConfig
}

// NewSummaryLLMGenerator 构造 LLM 摘要生成器。
func NewSummaryLLMGenerator(runs RunRepository, providers ProviderCredentialRepository, opener CredentialOpener,
	budgets BudgetChecker, limiter model.ConcurrencyLimiter, config SummaryLLMConfig) (*SummaryLLMGenerator, error) {
	if runs == nil || providers == nil || opener == nil {
		return nil, invalid("summary generator requires runs, providers and credential opener")
	}
	return &SummaryLLMGenerator{
		runs: runs, providers: providers, opener: opener, budgets: budgets, limiter: limiter, config: config.normalize(),
	}, nil
}

// resolvedSummaryModel 是解密后可用于摘要调用的模型与凭证。
type resolvedSummaryModel struct {
	providerID      uuid.UUID
	modelID         string
	remoteModelName string
	baseURL         string
	apiKey          string
	contextWindow   int64
	maxOutput       int
}

// Summarize 解析模型候选、分块生成并归并摘要。
func (g *SummaryLLMGenerator) Summarize(ctx context.Context, userID, sessionID uuid.UUID, previous string, messages []Message) (SummaryGeneration, error) {
	// 空来源无法生成有意义摘要，直接永久拒绝，避免后续分块索引越界。
	if len(messages) == 0 {
		return SummaryGeneration{}, fmt.Errorf("%w: summary requires at least one message", ErrSummaryPermanent)
	}
	resolved, err := resolveSessionModel(ctx, g.runs, g.providers, g.opener, userID, sessionID)
	if err != nil {
		return SummaryGeneration{}, err
	}
	if g.budgets != nil {
		providerID := resolved.providerID
		if _, err := g.budgets.Check(ctx, userID, nil, &providerID); err != nil {
			if errors.Is(err, ErrBudgetExceeded) {
				return SummaryGeneration{}, fmt.Errorf("%w: daily budget exhausted", ErrSummaryModelUnavailable)
			}
			// 预算存储故障时降级放行，避免摘要功能被缓存故障阻断。
		}
	}
	if g.limiter != nil {
		release, acquired, err := g.limiter.TryAcquire(ctx, "provider:"+resolved.providerID.String())
		if err != nil {
			// 舱壁存储故障时降级放行，与执行 Worker 的失败开放策略一致。
		} else if !acquired {
			return SummaryGeneration{}, ErrSummaryProviderBusy
		} else if release != nil {
			defer release()
		}
	}
	client, err := model.NewClient(resolved.baseURL, resolved.apiKey, g.config.RequestTimeout)
	if err != nil {
		return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryModelUnavailable, err)
	}
	chunks := memory.SplitSummaryMessages(toSummaryMessages(messages), g.config.MaxInputBytes)
	partials := make([]string, 0, len(chunks))
	inputTokens, outputTokens := 0, 0
	carried := previous
	for _, chunk := range chunks {
		system, user, err := memory.BuildSummaryPrompt(carried, chunk, g.config.MaxMessageBytes)
		if err != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, err)
		}
		response, err := g.call(ctx, client, resolved, system, user)
		if err != nil {
			return SummaryGeneration{}, err
		}
		partials = append(partials, response.Content)
		inputTokens += response.InputTokens
		outputTokens += response.OutputTokens
		// 后续分块携带上一片段摘要，保持跨分块的指代与事实连续。
		carried = response.Content
	}
	finalText := partials[0]
	if len(partials) > 1 {
		system, user, err := memory.BuildMergeSummaryPrompt(previous, partials)
		if err != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, err)
		}
		response, err := g.call(ctx, client, resolved, system, user)
		if err != nil {
			return SummaryGeneration{}, err
		}
		finalText = response.Content
		inputTokens += response.InputTokens
		outputTokens += response.OutputTokens
	}
	providerID := resolved.providerID
	return SummaryGeneration{
		Text: strings.TrimSpace(finalText), ProviderID: &providerID, ModelID: resolved.modelID,
		InputTokens: inputTokens, OutputTokens: outputTokens,
	}, nil
}

// call 执行一次非流式摘要调用并分类错误；流式与工具能力不参与摘要。
func (g *SummaryLLMGenerator) call(ctx context.Context, client *model.Client, resolved resolvedSummaryModel, system, user string) (model.ChatResponse, error) {
	maxOutput := g.config.MaxOutputTokens
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
		Temperature: g.config.Temperature, MaxOutputTokens: maxOutput,
	})
	if err != nil {
		// 认证失败、参数错误与上下文超限重试无意义；其余错误交给任务退避重试。
		if errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrAuthentication) || errors.Is(err, model.ErrCapability) || errors.Is(err, model.ErrContextExceeded) {
			return model.ChatResponse{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, err)
		}
		return model.ChatResponse{}, err
	}
	if strings.TrimSpace(response.Content) == "" || len(response.ToolCalls) > 0 {
		return model.ChatResponse{}, fmt.Errorf("%w: provider returned an empty or unexpected summary", ErrSummaryPermanent)
	}
	return response, nil
}

// resolveSessionModel 读取会话最近一次 Run 的模型快照并解密首个可用候选凭证。
// 摘要与事实提取共用同一解析逻辑，保证异步记忆任务与执行路由使用一致的模型来源。
func resolveSessionModel(ctx context.Context, runs RunRepository, providers ProviderCredentialRepository,
	opener CredentialOpener, userID, sessionID uuid.UUID) (resolvedSummaryModel, error) {
	page, err := runs.ListRuns(ctx, userID, sessionID, 1, "")
	if err != nil {
		return resolvedSummaryModel{}, err
	}
	if len(page.Items) == 0 {
		return resolvedSummaryModel{}, fmt.Errorf("%w: session has no run to inherit a model policy", ErrSummaryModelUnavailable)
	}
	encoded, err := json.Marshal(page.Items[0].ModelPolicySnapshot)
	if err != nil {
		return resolvedSummaryModel{}, fmt.Errorf("%w: encode policy snapshot", ErrSummaryPermanent)
	}
	var document policySnapshotDocument
	if err := json.Unmarshal(encoded, &document); err != nil || len(document.Candidates) == 0 {
		return resolvedSummaryModel{}, fmt.Errorf("%w: policy snapshot has no candidates", ErrSummaryModelUnavailable)
	}
	// 与执行路由保持相同优先级顺序，摘要优先使用主候选模型。
	sort.SliceStable(document.Candidates, func(i, j int) bool {
		return document.Candidates[i].Priority < document.Candidates[j].Priority
	})
	for _, candidate := range document.Candidates {
		providerID, err := uuid.Parse(candidate.ProviderID)
		if err != nil {
			continue
		}
		if _, err := uuid.Parse(candidate.ModelID); err != nil {
			continue
		}
		if candidate.ContextWindowTokens < MinGenerationContextTokens || candidate.MaxOutputTokens <= 0 {
			continue
		}
		provider, credential, err := providers.GetProviderCredential(ctx, providerID)
		if err != nil || provider.Status != ProviderEnabled {
			continue
		}
		plaintext, err := opener.Open(credential.KeyVersion, credential.Encrypted)
		if err != nil || len(plaintext) == 0 {
			continue
		}
		baseURL := candidate.ProviderBaseURL
		if baseURL == "" {
			baseURL = provider.BaseURL
		}
		return resolvedSummaryModel{
			providerID: providerID, modelID: candidate.ModelID, remoteModelName: candidate.RemoteModelName,
			baseURL: baseURL, apiKey: string(plaintext),
			contextWindow: candidate.ContextWindowTokens, maxOutput: candidate.MaxOutputTokens,
		}, nil
	}
	return resolvedSummaryModel{}, fmt.Errorf("%w: no enabled provider with decryptable credential", ErrSummaryModelUnavailable)
}

// toSummaryMessages 把领域消息转换为摘要输入，保留群聊发言人归属。
func toSummaryMessages(messages []Message) []memory.SummaryMessage {
	result := make([]memory.SummaryMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, memory.SummaryMessage{
			Role: string(message.Role), Speaker: message.SpeakerOpenID, Content: message.Content,
		})
	}
	return result
}

// 编译期断言摘要生成器实现。
var _ SummaryGenerator = (*SummaryLLMGenerator)(nil)
