// Package model 提供 OpenAI-compatible 模型适配器与路由所需的类型定义。
package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 模型调用错误分类，供上层决定重试或故障切换。
var (
	// ErrContextExceeded 请求超出模型上下文窗口，禁止重试。
	ErrContextExceeded = errors.New("model context window exceeded")
	// ErrCapability 模型缺少所需能力（流式、工具或结构化输出）。
	ErrCapability = errors.New("model capability mismatch")
	// ErrRateLimited 供应商限流或配额耗尽，可按 Retry-After 等待。
	ErrRateLimited = errors.New("model rate limited")
	// ErrRetryable 连接错误、超时或可重试 5xx。
	ErrRetryable = errors.New("model call retryable")
	// ErrAuthentication 供应商返回 401/403，当前 Endpoint 凭证失效。
	ErrAuthentication = errors.New("model authentication failed")
	// ErrPermanent 400、内容审核拒绝等不可重试错误。
	ErrPermanent = errors.New("model call failed permanently")
)

// ResponseError 是供应商非 2xx 响应的结构化错误，保留状态码与 Retry-After 供路由层决策。
// 通过 Unwrap 暴露底层分类，调用方仍可使用 errors.Is 判断错误类别。
type ResponseError struct {
	// StatusCode 供应商 HTTP 状态码。
	StatusCode int
	// RetryAfter 供应商建议的等待时长，仅 429 等场景携带。
	RetryAfter time.Duration
	// Class 错误分类，指向 ErrRateLimited、ErrRetryable、ErrAuthentication 或 ErrPermanent。
	Class error
	// Message 已脱敏的响应摘要，禁止包含凭证或用户正文。
	Message string
}

// Error 返回稳定且可读的错误文本；429 时附带 Retry-After 秒数便于排障。
func (e *ResponseError) Error() string {
	suffix := ""
	if e.RetryAfter > 0 {
		suffix = fmt.Sprintf(" (retry after %d seconds)", int(e.RetryAfter.Seconds()))
	}
	if e.Message == "" {
		return fmt.Sprintf("provider status %d%s", e.StatusCode, suffix)
	}
	return fmt.Sprintf("%s: provider status %d: %s%s", e.Class.Error(), e.StatusCode, e.Message, suffix)
}

// Unwrap 暴露底层分类，使 errors.Is 能识别重试与切换规则。
func (e *ResponseError) Unwrap() error { return e.Class }

// ModelRef 是调用所需的模型元数据快照，Run 创建时固化，运行中不受目录变更影响。
type ModelRef struct {
	// ProviderID 供应商 ID，用于熔断与配额隔离。
	ProviderID string
	// ModelID 模型 ID。
	ModelID string
	// RemoteModelName 供应商侧模型名。
	RemoteModelName string
	// ContextWindowTokens 上下文窗口；零表示策略未声明，此时不做事前硬性拒绝。
	ContextWindowTokens int64
	// MaxOutputTokens 最大输出 Token 数。
	MaxOutputTokens int
	// Stream 是否要求流式能力。
	Stream bool
	// Tools 是否要求工具调用能力。
	Tools bool
	// StructuredOutput 是否要求结构化输出能力。
	StructuredOutput bool
}

// ChatToolCall 是协议层的工具调用表示。
type ChatToolCall struct {
	// ID 调用 ID。
	ID string `json:"id,omitempty"`
	// Type 调用类型，固定 function。
	Type string `json:"type,omitempty"`
	// Function 函数名与参数。
	Function ChatFunction `json:"function"`
}

// ChatFunction 是工具调用的函数名与 JSON 参数。
type ChatFunction struct {
	// Name 函数名。
	Name string `json:"name"`
	// Arguments JSON 参数字符串。
	Arguments string `json:"arguments"`
}

// ChatMessage 是发往模型的单条消息，仅包含协议字段。
type ChatMessage struct {
	// Role 角色：system/user/assistant/tool。
	Role string `json:"role"`
	// Content 文本内容。
	Content string `json:"content,omitempty"`
	// ToolCallID 工具消息对应的调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls 助手消息携带的工具调用。
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`
}

// ToolDefinition 是暴露给模型的 JSON Schema 工具定义。
type ToolDefinition struct {
	// Name 工具名。
	Name string
	// Description 工具用途说明。
	Description string
	// Parameters JSON Schema。
	Parameters json.RawMessage
}

// ChatRequest 是一次模型调用的入参。
type ChatRequest struct {
	// Model 模型元数据快照。
	Model ModelRef
	// Messages 规范上下文消息。
	Messages []ChatMessage
	// Tools 暴露给模型的工具定义。
	Tools []ToolDefinition
	// Temperature 采样温度。
	Temperature float64
	// MaxOutputTokens 本请求的输出上限，受模型上限约束。
	MaxOutputTokens int
}

// ToolCall 是模型返回的工具调用。
type ToolCall struct {
	// ID 工具调用 ID。
	ID string `json:"id"`
	// Name 工具名。
	Name string `json:"name"`
	// Arguments JSON 参数原文，由执行层严格校验。
	Arguments string `json:"arguments"`
}

// ChatResponse 是模型调用结果，刻意不包含隐藏思维链。
type ChatResponse struct {
	// Content 最终文本。
	Content string
	// ReasoningContent 供应商返回的推理内容，仅用于判断空回答，不写入上下文也不持久化。
	ReasoningContent string
	// ToolCalls 工具调用列表。
	ToolCalls []ToolCall
	// FinishReason 结束原因。
	FinishReason string
	// InputTokens 输入 Token 数。
	InputTokens int
	// OutputTokens 输出 Token 数。
	OutputTokens int
	// ProviderRequestID 供应商请求 ID，用于排障与对账。
	ProviderRequestID string
}

// Client 调用 OpenAI-compatible Chat Completions 接口。
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient 构造适配器；HTTP 客户端禁用重定向且设置总超时，避免凭证被转发到其他主机。
func NewClient(baseURL, apiKey string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || apiKey == "" {
		return nil, errors.New("model base URL and API key are required")
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// RemoteModel 是供应商 /models 接口返回的可用模型条目。
type RemoteModel struct {
	// ID 供应商侧模型名，登记模型时必须完全一致。
	ID string `json:"id"`
	// OwnedBy 供应商返回的归属方，仅用于展示。
	OwnedBy string `json:"owned_by"`
}

// Models 查询 OpenAI-compatible /models 接口，只读取模型标识与归属方。
func (c *Client) Models(ctx context.Context) ([]RemoteModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create models request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: list provider models: %v", ErrRetryable, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read provider models: %v", ErrRetryable, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		class := ErrPermanent
		switch {
		case response.StatusCode == http.StatusTooManyRequests:
			class = ErrRateLimited
		case response.StatusCode >= 500:
			class = ErrRetryable
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			class = ErrAuthentication
		}
		return nil, &ResponseError{
			StatusCode: response.StatusCode,
			RetryAfter: RetryAfter(response.Header.Get("Retry-After"), 0),
			Class:      class,
			Message:    "provider model list failed",
		}
	}
	var payload struct {
		Data []RemoteModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode provider models response", ErrPermanent)
	}
	models := make([]RemoteModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		models = append(models, RemoteModel{ID: strings.TrimSpace(item.ID), OwnedBy: strings.TrimSpace(item.OwnedBy)})
	}
	return models, nil
}

// EstimateTextTokens 以保守口径估算文本 Token：ASCII 字符约 4 字符/Token，
// 非 ASCII（如中文）按 1 字符/Token 计算，避免按字节估算时低估中文输入。
func EstimateTextTokens(text string) int64 {
	var ascii, nonASCII int64
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}

// EstimateTokens 估算消息 Token（不含工具定义）；usage 缺失时的回退口径。
func EstimateTokens(messages []ChatMessage) int64 {
	return EstimateRequestTokens(messages, nil)
}

// EstimateRequestTokens 估算完整请求的输入 Token，包含消息、工具调用参数与工具定义，
// 用于调用前的上下文窗口校验与候选模型筛选。
func EstimateRequestTokens(messages []ChatMessage, tools []ToolDefinition) int64 {
	total := int64(len(messages) * 4)
	for _, message := range messages {
		total += EstimateTextTokens(message.Content) + EstimateTextTokens(message.ToolCallID) + EstimateTextTokens(message.Role)
		for _, call := range message.ToolCalls {
			total += EstimateTextTokens(call.Function.Name) + EstimateTextTokens(call.Function.Arguments)
		}
	}
	for _, tool := range tools {
		parameters := string(tool.Parameters)
		total += 4 + EstimateTextTokens(tool.Name) + EstimateTextTokens(tool.Description) + EstimateTextTokens(parameters)
	}
	return total
}

// Chat 执行一次非流式调用；调用前校验上下文与能力，错误按可重试性分类且不泄露凭证。
func (c *Client) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	return c.doChat(ctx, request, nil)
}

// ChatStream 执行一次 SSE 流式调用，并在每个文本增量到达时回调 onDelta。
// 回调为空时退化为非流式调用。
func (c *Client) ChatStream(ctx context.Context, request ChatRequest, onDelta func(string)) (ChatResponse, error) {
	if onDelta == nil {
		return c.doChat(ctx, request, nil)
	}
	return c.doChat(ctx, request, onDelta)
}

// doChat 是流式与非流式调用的共享实现；onDelta 非空时启用 SSE。
func (c *Client) doChat(ctx context.Context, request ChatRequest, onDelta func(string)) (ChatResponse, error) {
	if len(request.Tools) > 0 && !request.Model.Tools {
		// 工具调用能力是硬准入条件，缺少能力时必须在路由阶段切换到其他候选。
		return ChatResponse{}, fmt.Errorf("%w: model does not support tool calls", ErrCapability)
	}
	inputTokens := EstimateRequestTokens(request.Messages, request.Tools)
	outputBudget := request.MaxOutputTokens
	if outputBudget <= 0 {
		outputBudget = request.Model.MaxOutputTokens
	}
	if outputBudget > request.Model.MaxOutputTokens {
		outputBudget = request.Model.MaxOutputTokens
	}
	// 上下文窗口为零表示策略未声明窗口，此时不做硬性拒绝；非零时仍按保守估算校验。
	if window := request.Model.ContextWindowTokens; window > 0 && inputTokens+int64(outputBudget) > window {
		return ChatResponse{}, fmt.Errorf("%w: estimated %d input + %d output exceeds %d", ErrContextExceeded, inputTokens, outputBudget, window)
	}

	body := map[string]any{
		"model":       request.Model.RemoteModelName,
		"messages":    request.Messages,
		"max_tokens":  outputBudget,
		"temperature": request.Temperature,
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			parameters := tool.Parameters
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": parameters},
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if onDelta != nil {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: encode request: %v", ErrPermanent, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("%w: build request: %v", ErrPermanent, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	if onDelta != nil {
		httpRequest.Header.Set("Accept", "text/event-stream")
	}

	response, err := c.http.Do(httpRequest)
	if err != nil {
		// 用户取消与 Run Deadline 属于控制流错误，必须原样上抛，不能误判为可重试故障。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatResponse{}, ctxErr
		}
		return ChatResponse{}, fmt.Errorf("%w: transport error: %w", ErrRetryable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ChatResponse{}, c.statusError(response)
	}
	if onDelta != nil {
		// 供应商可能忽略 stream 参数并直接返回 JSON；此时回退为非流式解析，
		// 仍把完整文本作为一次增量回调，避免静默丢失回答。
		if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
			return c.decodeStream(response, onDelta, EstimateRequestTokens(request.Messages, request.Tools))
		}
		result, err := c.decodeResponse(response, request)
		if err == nil && result.Content != "" {
			onDelta(result.Content)
		}
		return result, err
	}
	return c.decodeResponse(response, request)
}

// statusError 把供应商非 2xx 响应转换为可路由的结构化错误。
func (c *Client) statusError(response *http.Response) error {
	if response.StatusCode == http.StatusTooManyRequests {
		// 保留 Retry-After 供路由层判断等待是否在可接受预算内。
		return &ResponseError{
			StatusCode: response.StatusCode,
			RetryAfter: RetryAfter(response.Header.Get("Retry-After"), 0),
			Class:      ErrRateLimited,
			Message:    "rate limited",
		}
	}
	if response.StatusCode >= 500 {
		return &ResponseError{StatusCode: response.StatusCode, Class: ErrRetryable, Message: "provider server error"}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// 鉴权失败表示当前 Endpoint 凭证不可用，路由层应熔断该 Endpoint 并切换其他合规供应商。
		return &ResponseError{
			StatusCode: response.StatusCode, Class: ErrAuthentication, Message: c.redactedError(response.Body),
		}
	}
	return &ResponseError{
		StatusCode: response.StatusCode, Class: ErrPermanent, Message: c.redactedError(response.Body),
	}
}

// decodeResponse 解析非流式响应体。
func (c *Client) decodeResponse(response *http.Response, request ChatRequest) (ChatResponse, error) {
	var payload struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return ChatResponse{}, fmt.Errorf("%w: decode response: %v", ErrRetryable, err)
	}
	if len(payload.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("%w: provider returned no choices", ErrRetryable)
	}
	choice := payload.Choices[0]
	result := ChatResponse{
		Content: choice.Message.Content, ReasoningContent: choice.Message.ReasoningContent,
		FinishReason: choice.FinishReason,
		InputTokens:  payload.Usage.PromptTokens, OutputTokens: payload.Usage.CompletionTokens,
		ProviderRequestID: payload.ID, ToolCalls: make([]ToolCall, 0, len(choice.Message.ToolCalls)),
	}
	for _, call := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if result.InputTokens == 0 {
		// 非流式供应商也可能省略 usage，不能因此让预算与成本统计恒为零。
		result.InputTokens = int(EstimateRequestTokens(request.Messages, request.Tools))
	}
	if result.OutputTokens == 0 {
		result.OutputTokens = estimateChatResponseOutput(result)
	}
	return result, nil
}

// estimateChatResponseOutput 在供应商未返回 usage 时估算输出，包含正文与工具调用参数。
func estimateChatResponseOutput(response ChatResponse) int {
	if response.Content == "" && len(response.ToolCalls) == 0 {
		return 0
	}
	message := ChatMessage{Role: "assistant", Content: response.Content}
	for _, call := range response.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, ChatToolCall{
			ID: call.ID, Type: "function", Function: ChatFunction{Name: call.Name, Arguments: call.Arguments},
		})
	}
	return int(EstimateTokens([]ChatMessage{message}))
}

// streamToolCall 累积流式响应中逐步下发的工具调用片段。
type streamToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// decodeStream 解析 OpenAI SSE 流，累计文本、工具调用与用量。
// 读取中断时返回已收到的部分结果与错误，调用方据此避免把半截回答当作成功。
func (c *Client) decodeStream(response *http.Response, onDelta func(string), fallbackInputTokens int64) (ChatResponse, error) {
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var content strings.Builder
	var reasoning strings.Builder
	calls := map[int]*streamToolCall{}
	result := ChatResponse{}
	var readErr error
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 损坏帧可能已经丢失正文或工具参数片段，必须显式失败；
			// 静默继续会把不完整回答或工具调用当作成功结果执行。
			return ChatResponse{}, fmt.Errorf("%w: decode stream event: %v", ErrRetryable, err)
		}
		if chunk.ID != "" {
			result.ProviderRequestID = chunk.ID
		}
		if chunk.Usage != nil {
			result.InputTokens = chunk.Usage.PromptTokens
			result.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.ReasoningContent != "" {
			// 推理内容只用于判断“只有推理没有回答”，不回调给前端，也不进入上下文。
			reasoning.WriteString(choice.Delta.ReasoningContent)
		}
		if choice.Delta.Content != "" {
			content.WriteString(choice.Delta.Content)
			onDelta(choice.Delta.Content)
		}
		for _, call := range choice.Delta.ToolCalls {
			item, exists := calls[call.Index]
			if !exists {
				item = &streamToolCall{}
				calls[call.Index] = item
			}
			if call.ID != "" {
				item.ID = call.ID
			}
			if call.Function.Name != "" {
				item.Name = call.Function.Name
			}
			if call.Function.Arguments != "" {
				item.Arguments.WriteString(call.Function.Arguments)
			}
		}
		if choice.FinishReason != "" {
			result.FinishReason = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		readErr = fmt.Errorf("%w: stream read failed: %v", ErrRetryable, err)
	}
	result.Content = content.String()
	result.ReasoningContent = reasoning.String()
	if result.OutputTokens == 0 {
		// 供应商未返回 usage 时使用保守估算，正文与工具调用参数都计入输出。
		result.OutputTokens = estimateChatResponseOutput(result)
	}
	if result.InputTokens == 0 && fallbackInputTokens > 0 {
		// 流式响应常省略 usage：用请求侧估算回填输入 Token，避免预算与成本统计恒为零。
		result.InputTokens = int(fallbackInputTokens)
	}
	if len(calls) > 0 {
		indexes := make([]int, 0, len(calls))
		for index := range calls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		result.ToolCalls = make([]ToolCall, 0, len(calls))
		for _, index := range indexes {
			item := calls[index]
			result.ToolCalls = append(result.ToolCalls, ToolCall{ID: item.ID, Name: item.Name, Arguments: item.Arguments.String()})
		}
	}
	return result, readErr
}

// redactedError 读取并截断供应商错误体，防止超长内容或回显敏感信息进入日志。
func (c *Client) redactedError(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "unreadable error body"
	}
	message := strings.TrimSpace(string(raw))
	if len(message) > 512 {
		message = strings.ToValidUTF8(message[:512], "")
	}
	message = strings.ReplaceAll(message, c.apiKey, "[REDACTED]")
	return message
}

// RetryAfter 解析 Retry-After 响应头，非法或缺失时返回默认退避时间。
func RetryAfter(header string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 || seconds > 300 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
