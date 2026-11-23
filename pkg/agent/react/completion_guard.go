package react

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TextContinuationInstruction 要求模型从截断处继续，且不得在文本专用调用中请求工具。
const TextContinuationInstruction = "上一条回答因输出长度限制被截断。请从中断处继续，不要重复已经输出的内容，也不要调用工具。"

// ValidateCompletion 统一校验供应商结束原因、工具调用与最终正文。
// allowTools 仅用于 ReAct 等允许工具调用的轮次；文本/JSON 专用调用必须传 false。
func ValidateCompletion(completion Completion, allowTools bool) error {
	reason := strings.ToLower(strings.TrimSpace(completion.FinishReason))
	switch reason {
	case "content_filter":
		return fmt.Errorf("%w: finish_reason=content_filter", ErrContentFiltered)
	case "length":
		if len(completion.ToolCalls) > 0 {
			return fmt.Errorf("%w: tool calls truncated", ErrIncompleteToolCall)
		}
	}
	if !allowTools && len(completion.ToolCalls) > 0 {
		return fmt.Errorf("%w: text completion returned tool calls", ErrIncompleteToolCall)
	}
	// 部分供应商（如 DeepSeek V4 系列）在未下发工具时会用原生标记把工具调用写进正文；
	// 这类标记必须终止执行，而不是当作模型答案持久化并展示给用户。
	if len(completion.ToolCalls) == 0 && containsToolCallMarkup(completion.Content) {
		return fmt.Errorf("%w: provider returned tool-call text instead of structured tool_calls", ErrUnparsedToolCall)
	}
	if strings.TrimSpace(completion.Content) != "" || (allowTools && len(completion.ToolCalls) > 0) {
		return nil
	}
	if strings.TrimSpace(completion.ReasoningContent) != "" {
		return fmt.Errorf("%w", ErrReasoningOnly)
	}
	return fmt.Errorf("%w", ErrEmptyResponse)
}

// IsLengthFinish 判断供应商是否因输出长度限制截断响应。
func IsLengthFinish(completion Completion) bool {
	return strings.EqualFold(strings.TrimSpace(completion.FinishReason), "length")
}

// toolCallMarkupMarkers 是已知会被错误写进正文的供应商原生工具调用标记。
// 只用完整标记匹配，避免模型正常讨论相关格式时误判。
var toolCallMarkupMarkers = []string{
	"<｜｜dsml｜｜",
	"<||dsml||",
	"<|dsml|",
	"<｜tool▁calls▁begin｜>",
	"<|tool▁calls▁begin|",
	"<tool_call>",
	"<function_call>",
}

// containsToolCallMarkup 判断正文是否包含未解析的原生工具调用标记。
func containsToolCallMarkup(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, marker := range toolCallMarkupMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ContinuationMessages 在原上下文后追加不落库的半截回答与续写指令。
func ContinuationMessages(messages []Message, partial Completion) []Message {
	result := make([]Message, 0, len(messages)+2)
	result = append(result, messages...)
	return append(result,
		Message{Role: RoleAssistant, Content: partial.Content, Ephemeral: true},
		Message{Role: RoleUser, Content: TextContinuationInstruction, Ephemeral: true},
	)
}

// maxContinuationOverlapRunes 限制重叠检测的最大字符数，避免超长回答出现二次方扫描。
const maxContinuationOverlapRunes = 240

// mergeContinuationText 合并续写文本，去除模型在续写开头重复输出的已有后缀。
// 仅处理“已有文本后缀 == 续写前缀”的真实重叠，不会删除正常重复的短词句。
func mergeContinuationText(prefix, continuation string) string {
	if continuation == "" {
		return prefix
	}
	if prefix == "" {
		return continuation
	}
	prefixRunes := []rune(prefix)
	continuationRunes := []rune(continuation)
	maxOverlap := len(prefixRunes)
	if len(continuationRunes) < maxOverlap {
		maxOverlap = len(continuationRunes)
	}
	if maxOverlap > maxContinuationOverlapRunes {
		maxOverlap = maxContinuationOverlapRunes
	}
	for overlap := maxOverlap; overlap > 0; overlap-- {
		if string(prefixRunes[len(prefixRunes)-overlap:]) == string(continuationRunes[:overlap]) {
			return prefix + string(continuationRunes[overlap:])
		}
	}
	return prefix + continuation
}

// ContinuationDeltaFilter 在线去除续写开头与已有后缀重叠的增量。
// 未确定重叠前最多暂存 240 个续写 rune，确定后直接透传后续内容。
type ContinuationDeltaFilter struct {
	prefix   string
	limit    int
	pending  []rune
	resolved bool
}

// NewContinuationDeltaFilter 创建续写增量过滤器，仅保留参与重叠匹配的已有文本后缀。
func NewContinuationDeltaFilter(prefix string) *ContinuationDeltaFilter {
	suffixStart := len(prefix)
	limit := 0
	for suffixStart > 0 && limit < maxContinuationOverlapRunes {
		_, size := utf8.DecodeLastRuneInString(prefix[:suffixStart])
		suffixStart -= size
		limit++
	}
	return &ContinuationDeltaFilter{
		prefix: prefix[suffixStart:], limit: limit,
		pending: make([]rune, 0, limit), resolved: limit == 0,
	}
}

// Push 输入一个续写增量，返回已经可以发送的去重后文本。
func (f *ContinuationDeltaFilter) Push(delta string) string {
	if delta == "" {
		return ""
	}
	if f.resolved {
		return delta
	}
	for index := 0; index < len(delta); {
		r, size := utf8.DecodeRuneInString(delta[index:])
		f.pending = append(f.pending, r)
		index += size
		if len(f.pending) == f.limit {
			return f.resolve() + delta[index:]
		}
	}
	return ""
}

// Flush 处理未达到重叠检测上限的短续写；重复调用不会重复输出。
func (f *ContinuationDeltaFilter) Flush() string {
	if f.resolved {
		return ""
	}
	return f.resolve()
}

func (f *ContinuationDeltaFilter) resolve() string {
	merged := mergeContinuationText(f.prefix, string(f.pending))
	filtered := merged[len(f.prefix):]
	f.pending = nil
	f.resolved = true
	return filtered
}

// MergeContinuation 校验并合并一次文本续写，同时累计两次真实调用的 Token 与费用。
func MergeContinuation(partial, continued Completion) (Completion, error) {
	if err := ValidateCompletion(continued, false); err != nil {
		return partial, err
	}
	partial.Content = mergeContinuationText(partial.Content, continued.Content)
	partial.ReasoningContent = ""
	partial.ToolCalls = nil
	partial.Usage.InputTokens += continued.Usage.InputTokens
	partial.Usage.OutputTokens += continued.Usage.OutputTokens
	partial.EstimatedCost += continued.EstimatedCost
	partial.FinishReason = continued.FinishReason
	if IsLengthFinish(continued) {
		return partial, fmt.Errorf("%w: continuation truncated", ErrStepLimit)
	}
	return partial, nil
}
