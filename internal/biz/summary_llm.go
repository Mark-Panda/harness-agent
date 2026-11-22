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
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
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
	usage     UsageRecorder
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

// WithUsageRecorder 注入后台摘要用量记录器；为空时只做预算校验不累加计数。
func (g *SummaryLLMGenerator) WithUsageRecorder(recorder UsageRecorder) *SummaryLLMGenerator {
	g.usage = recorder
	return g
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
	inputPrice      float64
	outputPrice     float64
}

// summaryContextSafetyMarginTokens 为 Token 估算误差与供应商协议开销预留固定安全余量。
const summaryContextSafetyMarginTokens = 256

// Summarize 解析模型候选、按 Token 分块生成并进行分层归并。
func (g *SummaryLLMGenerator) Summarize(ctx context.Context, userID, sessionID uuid.UUID, previous string, messages []Message) (SummaryGeneration, error) {
	if len(messages) == 0 {
		return SummaryGeneration{}, fmt.Errorf("%w: summary requires at least one message", ErrSummaryPermanent)
	}
	resolved, err := resolveSessionModel(ctx, g.runs, g.providers, g.opener, userID, sessionID)
	if err != nil {
		return SummaryGeneration{}, err
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
	inputBudget := int(resolved.contextWindow) - summaryOutputBudget(g.config, resolved) - summaryContextSafetyMarginTokens
	if inputBudget <= 0 {
		return SummaryGeneration{}, fmt.Errorf("%w: model context leaves no summary input budget", ErrSummaryPermanent)
	}
	byteChunks := memory.SplitSummaryMessages(toSummaryMessages(messages), g.config.MaxInputBytes)
	chunks := make([][]memory.SummaryMessage, 0, len(byteChunks))
	for _, chunk := range byteChunks {
		bounded, splitErr := memory.SplitSummaryMessagesByTokens(chunk, g.config.MaxMessageBytes, inputBudget)
		if splitErr != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, splitErr)
		}
		chunks = append(chunks, bounded...)
	}

	inputTokens, outputTokens := 0, 0
	partials := make([]string, 0, len(chunks))
	previousForMerge := previous
	chunkBackground := ""
	if len(chunks) == 1 && strings.TrimSpace(previous) != "" {
		system, user, buildErr := memory.BuildSummaryPrompt(previous, chunks[0], g.config.MaxMessageBytes)
		if buildErr != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, buildErr)
		}
		requestTokens := model.EstimateRequestTokens(chatMessagesFromReact([]react.Message{
			{Role: react.RoleSystem, Content: system}, {Role: react.RoleUser, Content: user},
		}), nil)
		if requestTokens <= int64(inputBudget) {
			chunkBackground = previous
		}
	}
	for index := 0; index < len(chunks); {
		system, user, buildErr := memory.BuildSummaryPrompt(chunkBackground, chunks[index], g.config.MaxMessageBytes)
		if buildErr != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, buildErr)
		}
		response, callErr := g.call(ctx, client, resolved, userID, system, user)
		inputTokens += response.InputTokens
		outputTokens += response.OutputTokens
		if errors.Is(callErr, model.ErrContextExceeded) {
			if chunkBackground != "" {
				chunkBackground = ""
				continue
			}
			smaller, ok := splitSummaryChunkForRetry(chunks[index])
			if !ok {
				return SummaryGeneration{}, callErr
			}
			chunks = append(chunks[:index], append(smaller, chunks[index+1:]...)...)
			continue
		}
		if callErr != nil {
			return SummaryGeneration{}, callErr
		}
		if chunkBackground != "" {
			previousForMerge = ""
		}
		partials = append(partials, response.Content)
		index++
	}

	mergeItems := partials
	// 归并必须有界：模型若始终输出接近上限的摘要，拆分与归并可能长期不收敛。
	for round := 0; strings.TrimSpace(previousForMerge) != "" || len(mergeItems) > 1; round++ {
		if round >= maxSummaryMergeRounds {
			return SummaryGeneration{}, fmt.Errorf("%w: summary merge did not converge after %d rounds", ErrSummaryPermanent, maxSummaryMergeRounds)
		}
		before := len(mergeItems)
		beforeTokens := mergeTokenEstimate(mergeItems)
		if strings.TrimSpace(previousForMerge) != "" {
			before++
			beforeTokens += memory.EstimateTokens(previousForMerge)
		}
		batches, buildErr := memory.BuildSummaryMergeBatches(previousForMerge, mergeItems, inputBudget)
		if buildErr != nil {
			return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, buildErr)
		}
		queue := make([]summaryMergeBatch, 0, len(batches))
		for _, batch := range batches {
			queue = append(queue, summaryMergeBatch{items: batch})
		}
		next := make([]string, 0, len(batches))
		retriedContext := false
		for index := 0; index < len(queue); {
			batch := queue[index]
			if len(batch.items) == 1 && !batch.forceSingle {
				next = append(next, batch.items[0])
				index++
				continue
			}
			system, user, promptErr := memory.BuildMergeSummaryPrompt("", batch.items)
			if promptErr != nil {
				return SummaryGeneration{}, fmt.Errorf("%w: %v", ErrSummaryPermanent, promptErr)
			}
			response, callErr := g.call(ctx, client, resolved, userID, system, user)
			inputTokens += response.InputTokens
			outputTokens += response.OutputTokens
			if errors.Is(callErr, model.ErrContextExceeded) {
				smaller, ok := splitSummaryMergeBatchForRetry(batch)
				if !ok {
					return SummaryGeneration{}, callErr
				}
				queue = append(queue[:index], append(smaller, queue[index+1:]...)...)
				retriedContext = true
				continue
			}
			if callErr != nil {
				return SummaryGeneration{}, callErr
			}
			next = append(next, response.Content)
			index++
		}
		// 预拆分会让片段数暂时等于原始条目数，但真实压缩已发生；
		// 只有“条目数未减少且总 Token 也未下降”才说明本轮没有取得任何进展。
		if !retriedContext && before > 1 && len(next) >= before && mergeTokenEstimate(next) >= beforeTokens {
			return SummaryGeneration{}, fmt.Errorf("%w: summary merge made no reduction", ErrSummaryPermanent)
		}
		mergeItems = next
		previousForMerge = ""
	}
	if len(mergeItems) != 1 {
		return SummaryGeneration{}, fmt.Errorf("%w: summary merge produced no final text", ErrSummaryPermanent)
	}
	providerID := resolved.providerID
	return SummaryGeneration{
		Text: strings.TrimSpace(mergeItems[0]), ProviderID: &providerID, ModelID: resolved.modelID,
		InputTokens: inputTokens, OutputTokens: outputTokens,
	}, nil
}

// maxSummaryMergeRounds 限制分层归并轮数，避免模型持续输出超长摘要时无限循环。
const maxSummaryMergeRounds = 8

// mergeTokenEstimate 汇总待归并摘要的估算 Token，用于判断本轮是否发生真实压缩。
func mergeTokenEstimate(items []string) int {
	total := 0
	for _, item := range items {
		total += memory.EstimateTokens(item)
	}
	return total
}

// summaryMergeBatch 标记归并批次；上下文超限拆出的单片段也必须经模型压缩，不能直接当作最终摘要。
type summaryMergeBatch struct {
	items       []string
	forceSingle bool
}

// splitSummaryChunkForRetry 将被拒绝的分块严格拆小，禁止相同 Payload 原地重试。
func splitSummaryChunkForRetry(chunk []memory.SummaryMessage) ([][]memory.SummaryMessage, bool) {
	if len(chunk) > 1 {
		middle := len(chunk) / 2
		return [][]memory.SummaryMessage{chunk[:middle], chunk[middle:]}, true
	}
	if len(chunk) != 1 {
		return nil, false
	}
	runes := []rune(chunk[0].Content)
	if len(runes) < 2 {
		return nil, false
	}
	middle := len(runes) / 2
	left, right := chunk[0], chunk[0]
	left.Content = string(runes[:middle])
	right.Content = string(runes[middle:])
	return [][]memory.SummaryMessage{{left}, {right}}, true
}

// splitSummaryMergeBatchForRetry 将被拒绝的归并输入严格拆小，并保持所有片段顺序。
func splitSummaryMergeBatchForRetry(batch summaryMergeBatch) ([]summaryMergeBatch, bool) {
	if len(batch.items) > 1 {
		middle := len(batch.items) / 2
		return []summaryMergeBatch{{items: batch.items[:middle], forceSingle: true}, {items: batch.items[middle:], forceSingle: true}}, true
	}
	if len(batch.items) != 1 {
		return nil, false
	}
	runes := []rune(batch.items[0])
	if len(runes) < 2 {
		return nil, false
	}
	middle := len(runes) / 2
	return []summaryMergeBatch{{items: []string{string(runes[:middle])}, forceSingle: true}, {items: []string{string(runes[middle:])}, forceSingle: true}}, true
}

// summaryOutputBudget 返回配置与模型快照共同约束后的实际输出预算。
func summaryOutputBudget(config SummaryLLMConfig, resolved resolvedSummaryModel) int {
	maxOutput := config.MaxOutputTokens
	if resolved.maxOutput > 0 && maxOutput > resolved.maxOutput {
		maxOutput = resolved.maxOutput
	}
	return maxOutput
}

// call 执行一次非流式摘要调用并分类错误；流式与工具能力不参与摘要。
func (g *SummaryLLMGenerator) call(ctx context.Context, client *model.Client, resolved resolvedSummaryModel, userID uuid.UUID, system, user string) (model.ChatResponse, error) {
	maxOutput := summaryOutputBudget(g.config, resolved)
	request := model.ChatRequest{
		Model: model.ModelRef{
			ProviderID: resolved.providerID.String(), ModelID: resolved.modelID,
			RemoteModelName: resolved.remoteModelName, ContextWindowTokens: resolved.contextWindow,
			MaxOutputTokens: resolved.maxOutput,
		},
		Temperature: g.config.Temperature, MaxOutputTokens: maxOutput,
	}
	completion, err := completeText(ctx, []react.Message{
		{Role: react.RoleSystem, Content: system}, {Role: react.RoleUser, Content: user},
	}, func(callCtx context.Context, messages []react.Message) (react.Completion, error) {
		if err := checkBackgroundBudget(callCtx, g.budgets, userID, resolved.providerID); err != nil {
			return react.Completion{}, err
		}
		request.Messages = chatMessagesFromReact(messages)
		response, chatErr := client.Chat(callCtx, request)
		if g.usage != nil && (response.InputTokens > 0 || response.OutputTokens > 0) {
			providerID := resolved.providerID
			cost := estimateBackgroundCost(resolved.inputPrice, resolved.outputPrice, response.InputTokens, response.OutputTokens)
			if recordErr := g.usage.RecordUsage(context.WithoutCancel(callCtx), userID, nil, &providerID,
				int64(response.InputTokens), int64(response.OutputTokens), cost, time.Now().UTC()); recordErr != nil {
				_ = recordErr
			}
		}
		return completionFromChatResponse(response), chatErr
	})
	response := model.ChatResponse{
		Content: completion.Content, FinishReason: completion.FinishReason,
		InputTokens: completion.Usage.InputTokens, OutputTokens: completion.Usage.OutputTokens,
	}
	if err != nil {
		if errors.Is(err, ErrBudgetExceeded) {
			return response, fmt.Errorf("%w: %w", ErrSummaryModelUnavailable, err)
		}
		// 认证失败、参数错误、上下文超限及响应守卫错误均不能作为有效摘要。
		if isCompletionGuardError(err) || errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrAuthentication) ||
			errors.Is(err, model.ErrCapability) || errors.Is(err, model.ErrContextExceeded) {
			return response, fmt.Errorf("%w: %w", ErrSummaryPermanent, err)
		}
		return response, err
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
		if candidate.ProviderBaseURL != "" && candidate.ProviderBaseURL != provider.BaseURL {
			continue
		}
		plaintext, err := opener.Open(credential.KeyVersion, credential.Encrypted)
		if err != nil || len(plaintext) == 0 {
			continue
		}
		return resolvedSummaryModel{
			providerID: providerID, modelID: candidate.ModelID, remoteModelName: candidate.RemoteModelName,
			baseURL: provider.BaseURL, apiKey: string(plaintext),
			contextWindow: candidate.ContextWindowTokens, maxOutput: candidate.MaxOutputTokens,
			inputPrice: candidate.InputPricePerMillion, outputPrice: candidate.OutputPricePerMillion,
		}, nil
	}
	return resolvedSummaryModel{}, fmt.Errorf("%w: no compatible enabled provider with decryptable credential", ErrSummaryModelUnavailable)
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
