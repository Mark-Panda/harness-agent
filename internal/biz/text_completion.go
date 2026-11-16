package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// textCompletionFunc 执行一次真实模型调用；预算检查与用量记录由调用方在每次调用内完成。
type textCompletionFunc func(context.Context, []react.Message) (react.Completion, error)

// completeText 为文本/JSON 专用模型调用提供统一响应守卫与最多一次长度续写。
func completeText(ctx context.Context, messages []react.Message, complete textCompletionFunc) (react.Completion, error) {
	completion, err := complete(ctx, messages)
	if err != nil {
		return completion, err
	}
	if err := react.ValidateCompletion(completion, false); err != nil {
		return completion, err
	}
	if !react.IsLengthFinish(completion) {
		return completion, nil
	}
	continued, err := complete(ctx, react.ContinuationMessages(messages, completion))
	if err != nil {
		completion.Usage.InputTokens += continued.Usage.InputTokens
		completion.Usage.OutputTokens += continued.Usage.OutputTokens
		completion.EstimatedCost += continued.EstimatedCost
		return completion, err
	}
	merged, err := react.MergeContinuation(completion, continued)
	if err != nil && merged.Usage == completion.Usage {
		// 校验失败时也要把第二次真实请求的用量返回给上层计量。
		merged.Usage.InputTokens += continued.Usage.InputTokens
		merged.Usage.OutputTokens += continued.Usage.OutputTokens
		merged.EstimatedCost += continued.EstimatedCost
	}
	return merged, err
}

// completionFromChatResponse 把直接 Chat 响应映射为共享执行类型。
func completionFromChatResponse(response model.ChatResponse) react.Completion {
	completion := react.Completion{
		Content: response.Content, ReasoningContent: response.ReasoningContent, FinishReason: response.FinishReason,
		Usage: react.Usage{InputTokens: response.InputTokens, OutputTokens: response.OutputTokens},
	}
	for _, call := range response.ToolCalls {
		completion.ToolCalls = append(completion.ToolCalls, react.ToolCall{
			ID: call.ID, Name: call.Name, Arguments: []byte(call.Arguments),
		})
	}
	return completion
}

// chatMessagesFromReact 将文本专用上下文转换为 OpenAI-compatible 消息。
func chatMessagesFromReact(messages []react.Message) []model.ChatMessage {
	result := make([]model.ChatMessage, 0, len(messages))
	for _, message := range messages {
		converted := model.ChatMessage{Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, model.ChatToolCall{
				ID: call.ID, Type: "function", Function: model.ChatFunction{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		result = append(result, converted)
	}
	return result
}

// aggregatePromptEntry 是必须保留骨架的一条汇总输入；Body 可按剩余 Token 预算压缩。
type aggregatePromptEntry struct {
	Header string
	Body   string
}

// boundedAggregateUserPrompt 先保留所有条目骨架，再公平分配正文预算。
// 预算连骨架都容纳不下时显式失败，绝不通过删除尾部条目制造“成功”请求。
func boundedAggregateUserPrompt(system, prefix string, entries []aggregatePromptEntry, budget int) (string, error) {
	if budget <= 0 {
		return "", fmt.Errorf("aggregation prompt budget must be positive")
	}
	render := func(bodies []string) string {
		var builder strings.Builder
		builder.WriteString(prefix)
		for index, entry := range entries {
			builder.WriteString(entry.Header)
			builder.WriteString("\nresult=")
			if index < len(bodies) {
				builder.WriteString(bodies[index])
			}
			builder.WriteString("\n\n")
		}
		return strings.TrimSpace(builder.String())
	}
	requestTokens := func(user string) int {
		return int(model.EstimateRequestTokens(chatMessagesFromReact([]react.Message{
			{Role: react.RoleSystem, Content: system},
			{Role: react.RoleUser, Content: user},
		}), nil))
	}
	base := render(nil)
	remaining := budget - requestTokens(base)
	if remaining < 0 {
		return "", fmt.Errorf("aggregation prompt budget %d cannot retain all %d entry skeletons", budget, len(entries))
	}

	quotas := make([]int, len(entries))
	active := make([]int, 0, len(entries))
	for index, entry := range entries {
		if entry.Body != "" {
			active = append(active, index)
		}
	}
	for remaining > 0 && len(active) > 0 {
		share := remaining / len(active)
		if share == 0 {
			break
		}
		next := make([]int, 0, len(active))
		spent := 0
		for _, index := range active {
			need := react.EstimateTextTokens(entries[index].Body) - quotas[index]
			if need <= share {
				quotas[index] += need
				spent += need
				continue
			}
			quotas[index] += share
			spent += share
			next = append(next, index)
		}
		remaining -= spent
		if len(next) == len(active) {
			break
		}
		active = next
	}
	bodies := make([]string, len(entries))
	for index, entry := range entries {
		if quotas[index] > 0 {
			bodies[index] = fitTextHeadTailTokens(entry.Body, quotas[index])
		}
	}
	prompt := render(bodies)
	if tokens := requestTokens(prompt); tokens > budget {
		return "", fmt.Errorf("aggregation prompt tokens %d exceed budget %d", tokens, budget)
	}
	return prompt, nil
}

// fitTextHeadTailTokens 按 Token 保留正文首尾；压缩处使用明确省略标记。
func fitTextHeadTailTokens(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if react.EstimateTextTokens(text) <= budget {
		return text
	}
	const marker = "\n……中间内容已省略……\n"
	markerTokens := react.EstimateTextTokens(marker)
	if budget <= markerTokens {
		return prefixWithinTokenBudget(marker, budget)
	}
	remaining := budget - markerTokens
	headBudget := (remaining + 1) / 2
	tailBudget := remaining - headBudget
	return prefixWithinTokenBudget(text, headBudget) + marker + suffixWithinTokenBudget(text, tailBudget)
}

func prefixWithinTokenBudget(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if react.EstimateTextTokens(string(runes[:middle])) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[:low])
}

func suffixWithinTokenBudget(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if react.EstimateTextTokens(string(runes[len(runes)-middle:])) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[len(runes)-low:])
}

// boundedTextAggregateBudget 给汇总调用设置确定上界；未配置全局预算时使用默认值，
// 同时仍按模型窗口扣除输出与安全余量。
func boundedTextAggregateBudget(configured int, minWindow int64, reserveOutput, safetyMargin int) int {
	if configured > 0 {
		return configured
	}
	budget := textAggregateDefaultPromptTokens
	if minWindow <= 0 {
		return budget
	}
	available := int(minWindow) - reserveOutput - safetyMargin
	if available <= 0 {
		return 0
	}
	if available < budget {
		return available
	}
	return budget
}

// aggregateHeaderText 将标题等骨架字段压缩为单行，避免数据伪造新的结果条目。
func aggregateHeaderText(text string, maxTokens int) string {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(text))
	return fitTextHeadTailTokens(text, maxTokens)
}

// isCompletionGuardError 识别不应被业务层降级为空成功的模型响应错误。
func isCompletionGuardError(err error) bool {
	return errors.Is(err, react.ErrContentFiltered) || errors.Is(err, react.ErrReasoningOnly) ||
		errors.Is(err, react.ErrEmptyResponse) || errors.Is(err, react.ErrIncompleteToolCall) ||
		errors.Is(err, react.ErrStepLimit)
}
