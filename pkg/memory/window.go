// Package memory 实现上下文 Token 预算与会话窗口裁剪，供执行层组装模型上下文。
package memory

import (
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// messageOverheadTokens 是单条消息的角色、分隔符与协议字段的保守开销。
const messageOverheadTokens = 4

// EstimateTokens 估算文本 Token 数：ASCII 约 4 字符/Token，非 ASCII（中文等）约 1 字符/Token。
// 该估算故意偏保守，用于预算裁剪而不是精确计费。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii, other := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	tokens := int(math.Ceil(float64(ascii)/4.0)) + other
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// EstimateMessagesTokens 估算消息序列的 Token 总量，包含协议开销与工具调用参数。
func EstimateMessagesTokens(messages []react.Message) int {
	total := 0
	for _, message := range messages {
		total += messageOverheadTokens + EstimateTokens(message.Content)
		for _, call := range message.ToolCalls {
			total += messageOverheadTokens + EstimateTokens(call.Name) + EstimateTokens(string(call.Arguments))
		}
	}
	return total
}

// TrimReason 是裁剪决策的结构化原因，供日志、事件与评测复现。
type TrimReason struct {
	// Code 稳定的原因码。
	Code string
	// Detail 可读明细，不包含消息正文。
	Detail string
}

// TrimResult 是窗口裁剪结果与可观测元数据。
type TrimResult struct {
	// Messages 裁剪后的消息序列，保持原始顺序。
	Messages []react.Message
	// EstimatedTokens 裁剪后估算 Token。
	EstimatedTokens int
	// Budget 本次裁剪的 Token 预算。
	Budget int
	// DroppedMessages 被丢弃的消息数。
	DroppedMessages int
	// DroppedTurns 被丢弃的对话轮次数。
	DroppedTurns int
	// Reasons 裁剪决策原因，按发生顺序记录。
	Reasons []TrimReason
}

// Trim 按确定性规则裁剪上下文：始终保留系统提示与最后一轮对话，再从新到旧保留历史轮次。
// 裁剪顺序固定为：保留全部开场 system 消息 → 保留当前轮次 → 从新到旧追加历史轮次 → 丢弃剩余最旧轮次。
// 以用户消息作为轮次边界，避免把工具调用与其结果拆散导致模型协议非法。
func Trim(messages []react.Message, budget int) TrimResult {
	result := TrimResult{Messages: messages, EstimatedTokens: EstimateMessagesTokens(messages), Budget: budget}
	if budget <= 0 || len(messages) == 0 || result.EstimatedTokens <= budget {
		return result
	}
	// 开场连续的 system 消息（主系统提示、长期记忆、摘要）都是上下文约束，任何预算下都必须保留。
	system := make([]react.Message, 0, len(messages))
	rest := messages
	for len(rest) > 0 && rest[0].Role == react.RoleSystem {
		system = append(system, rest[0])
		rest = rest[1:]
	}
	if len(system) > 0 {
		result.Reasons = append(result.Reasons, TrimReason{
			Code: "system_messages_kept", Detail: fmt.Sprintf("%d leading system messages are mandatory", len(system)),
		})
	}
	if len(rest) == 0 {
		return result
	}
	turns := splitTurns(rest)
	if len(turns) == 0 {
		return result
	}
	// 最后一轮（当前用户输入及其工具/助手尾部）始终保留，即使超出预算也不截断当前意图。
	kept := [][]react.Message{turns[len(turns)-1]}
	tokens := EstimateMessagesTokens(system) + EstimateMessagesTokens(turns[len(turns)-1])
	result.Reasons = append(result.Reasons, TrimReason{Code: "current_turn_kept", Detail: "last user turn is always preserved"})
	if tokens > budget {
		result.Reasons = append(result.Reasons, TrimReason{
			Code:   "fixed_context_exceeds_budget",
			Detail: fmt.Sprintf("system and current turn require %d tokens over budget %d", tokens, budget),
		})
	}
	for index := len(turns) - 2; index >= 0; index-- {
		turnTokens := EstimateMessagesTokens(turns[index])
		if tokens+turnTokens > budget {
			result.DroppedTurns = index + 1
			break
		}
		kept = append([][]react.Message{turns[index]}, kept...)
		tokens += turnTokens
	}
	trimmed := make([]react.Message, 0, len(system)+len(rest))
	trimmed = append(trimmed, system...)
	for _, turn := range kept {
		trimmed = append(trimmed, turn...)
	}
	result.Messages = trimmed
	result.EstimatedTokens = tokens
	result.DroppedMessages = len(messages) - len(trimmed)
	if result.DroppedTurns > 0 {
		result.Reasons = append(result.Reasons, TrimReason{
			Code: "oldest_turns_dropped",
			Detail: fmt.Sprintf("dropped %d oldest turns (%d messages) to fit budget %d",
				result.DroppedTurns, result.DroppedMessages, budget),
		})
	}
	return result
}

// splitTurns 以用户消息为边界切分对话；没有用户消息时所有消息作为单轮处理。
func splitTurns(messages []react.Message) [][]react.Message {
	turns := make([][]react.Message, 0, 4)
	current := make([]react.Message, 0, 4)
	for _, message := range messages {
		if message.Role == react.RoleUser && len(current) > 0 {
			turns = append(turns, current)
			current = make([]react.Message, 0, 4)
		}
		current = append(current, message)
	}
	if len(current) > 0 {
		turns = append(turns, current)
	}
	return turns
}
