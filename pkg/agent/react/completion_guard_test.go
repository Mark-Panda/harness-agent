package react

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateCompletionResponseGuards(t *testing.T) {
	toolCall := ToolCall{ID: "call-1", Name: "unexpected", Arguments: []byte(`{}`)}
	for _, tc := range []struct {
		name       string
		completion Completion
		allowTools bool
		want       error
	}{
		{name: "text", completion: Completion{Content: "ok"}},
		{name: "tool call without content", completion: Completion{ToolCalls: []ToolCall{toolCall}}, allowTools: true},
		{name: "empty", completion: Completion{}, want: ErrEmptyResponse},
		{name: "reasoning only", completion: Completion{ReasoningContent: "hidden"}, want: ErrReasoningOnly},
		{name: "content filtered", completion: Completion{Content: "partial", FinishReason: "content_filter"}, want: ErrContentFiltered},
		{name: "truncated tool call", completion: Completion{FinishReason: "length", ToolCalls: []ToolCall{toolCall}}, allowTools: true, want: ErrIncompleteToolCall},
		{name: "text response rejects tool call", completion: Completion{Content: "{}", ToolCalls: []ToolCall{toolCall}}, want: ErrIncompleteToolCall},
		{name: "plain text may be continued", completion: Completion{Content: "partial", FinishReason: "length"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCompletion(tc.completion, tc.allowTools)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateCompletion() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateCompletion() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestContinuationMessagesPreserveContextAndPartial(t *testing.T) {
	messages := []Message{{Role: RoleSystem, Content: "system"}, {Role: RoleUser, Content: "question"}}
	got := ContinuationMessages(messages, Completion{Content: "partial"})
	if len(got) != 4 || got[0].Role != messages[0].Role || got[0].Content != messages[0].Content ||
		got[1].Role != messages[1].Role || got[1].Content != messages[1].Content {
		t.Fatalf("continuation messages = %#v", got)
	}
	if got[2].Role != RoleAssistant || got[2].Content != "partial" || !got[2].Ephemeral {
		t.Fatalf("partial message = %#v", got[2])
	}
	if got[3].Role != RoleUser || !strings.Contains(got[3].Content, "不要调用工具") || !got[3].Ephemeral {
		t.Fatalf("continuation instruction = %#v", got[3])
	}
}

func TestMergeContinuationValidatesAndAccumulatesUsage(t *testing.T) {
	partial := Completion{Content: "first", FinishReason: "length", Usage: Usage{InputTokens: 3, OutputTokens: 2}, EstimatedCost: 0.25}
	continued := Completion{Content: " second", FinishReason: "stop", Usage: Usage{InputTokens: 5, OutputTokens: 4}, EstimatedCost: 0.5}
	merged, err := MergeContinuation(partial, continued)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Content != "first second" || merged.FinishReason != "stop" ||
		merged.Usage.InputTokens != 8 || merged.Usage.OutputTokens != 6 || merged.EstimatedCost != 0.75 {
		t.Fatalf("merged completion = %#v", merged)
	}

	for _, tc := range []struct {
		name      string
		continued Completion
		want      error
	}{
		{name: "still truncated", continued: Completion{Content: "tail", FinishReason: "length"}, want: ErrStepLimit},
		{name: "unexpected tools", continued: Completion{Content: "tail", ToolCalls: []ToolCall{{ID: "call"}}}, want: ErrIncompleteToolCall},
		{name: "filtered", continued: Completion{FinishReason: "content_filter"}, want: ErrContentFiltered},
		{name: "empty", continued: Completion{}, want: ErrEmptyResponse},
		{name: "reasoning only", continued: Completion{ReasoningContent: "hidden"}, want: ErrReasoningOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MergeContinuation(partial, tc.continued); !errors.Is(err, tc.want) {
				t.Fatalf("MergeContinuation() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestMergeContinuationDeduplicatesRepeatedSuffix 验证续写开头重复已输出后缀时不会重复拼接。
func TestMergeContinuationDeduplicatesRepeatedSuffix(t *testing.T) {
	merged, err := MergeContinuation(Completion{Content: "第一段内容，接下来是第二段"}, Completion{Content: "第二段内容继续"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Content != "第一段内容，接下来是第二段内容继续" {
		t.Fatalf("merged content = %q", merged.Content)
	}

	// 无重叠时必须原样拼接，不能误删正文。
	merged, err = MergeContinuation(Completion{Content: "alpha"}, Completion{Content: " beta"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Content != "alpha beta" {
		t.Fatalf("non-overlap merged content = %q", merged.Content)
	}

	if got := mergeContinuationText("", "续写"); got != "续写" {
		t.Fatalf("empty prefix = %q", got)
	}
	if got := mergeContinuationText("已有内容", ""); got != "已有内容" {
		t.Fatalf("empty continuation = %q", got)
	}
}

func assertContinuationDeltaFilter(t *testing.T, prefix, continuation string, deltas []string) {
	t.Helper()
	filter := NewContinuationDeltaFilter(prefix)
	var streamed strings.Builder
	for _, delta := range deltas {
		streamed.WriteString(filter.Push(delta))
	}
	streamed.WriteString(filter.Flush())
	if got, want := prefix+streamed.String(), mergeContinuationText(prefix, continuation); got != want {
		t.Fatalf("filtered stream = %q, want %q (prefix=%q continuation=%q deltas=%q)", got, want, prefix, continuation, deltas)
	}
	if got := filter.Flush(); got != "" {
		t.Fatalf("second Flush() = %q, want empty", got)
	}
}

func TestContinuationDeltaFilterMatchesMerge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prefix       string
		continuation string
		deltas       []string
	}{
		{name: "no overlap then passthrough", prefix: "abc", continuation: "XYZtail", deltas: []string{"XYZ", "tail"}},
		{name: "partial overlap in single large delta", prefix: "abcde", continuation: "cdefgh", deltas: []string{"cdefgh"}},
		{name: "full overlap", prefix: "abc", continuation: "abc", deltas: []string{"a", "b", "c"}},
		{name: "overlap across multiple deltas", prefix: "abc", continuation: "abcXYZ", deltas: []string{"a", "bc", "X", "Y", "Z"}},
		{name: "short partial overlap flushed", prefix: "prefix abc", continuation: "abc tail", deltas: []string{"ab", "c ta", "il"}},
		{name: "short full overlap flushed", prefix: "prefix abc", continuation: "abc", deltas: []string{"a", "bc"}},
		{name: "Chinese and emoji", prefix: "已输出：你好🙂", continuation: "你好🙂，世界", deltas: []string{"你", "好🙂", "，世", "界"}},
		{name: "empty prefix", prefix: "", continuation: "续写🙂", deltas: []string{"", "续写🙂", ""}},
		{name: "empty continuation", prefix: "已有内容", continuation: "", deltas: []string{"", ""}},
		{
			name: "exact 240 rune overlap", prefix: "头" + strings.Repeat("界", 240),
			continuation: strings.Repeat("界", 240) + "尾",
			deltas:       []string{strings.Repeat("界", 120), strings.Repeat("界", 120), "尾"},
		},
		{
			name: "overlap beyond 240 rune limit", prefix: "头" + strings.Repeat("🙂", 241),
			continuation: strings.Repeat("🙂", 241) + "尾",
			deltas:       []string{strings.Repeat("🙂", 120), strings.Repeat("🙂", 121), "尾"},
		},
		{name: "single large delta", prefix: strings.Repeat("长", 500), continuation: strings.Repeat("abc", 300), deltas: []string{strings.Repeat("abc", 300)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertContinuationDeltaFilter(t, tc.prefix, tc.continuation, tc.deltas)
		})
	}
}

func TestContinuationDeltaFilterArbitrarySplitsMatchMerge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prefix       string
		continuation string
	}{
		{name: "partial unicode overlap", prefix: "开头：甲乙🙂", continuation: "甲乙🙂丙丁🌏"},
		{name: "no overlap", prefix: "abcde", continuation: "XYZ123"},
		{name: "full overlap", prefix: "前缀abc", continuation: "abc"},
		{name: "empty continuation", prefix: "abc", continuation: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runes := []rune(tc.continuation)
			for split := 0; split <= len(runes); split++ {
				assertContinuationDeltaFilter(t, tc.prefix, tc.continuation, []string{string(runes[:split]), string(runes[split:])})
			}
			deltas := make([]string, 0, len(runes))
			for _, r := range runes {
				deltas = append(deltas, string(r))
			}
			assertContinuationDeltaFilter(t, tc.prefix, tc.continuation, deltas)
		})
	}
}

func TestContinuationDeltaFilterBoundsPendingRunes(t *testing.T) {
	filter := NewContinuationDeltaFilter(strings.Repeat("已", 1000))
	if got := filter.Push(strings.Repeat("新", 239)); got != "" {
		t.Fatalf("Push() before overlap resolution = %q, want empty", got)
	}
	if got := len(filter.pending); got != 239 {
		t.Fatalf("pending runes = %d, want 239", got)
	}
	if got, want := filter.Push(strings.Repeat("新", 500)), strings.Repeat("新", 739); got != want {
		t.Fatalf("resolved Push() returned %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
	if got := len(filter.pending); got != 0 {
		t.Fatalf("pending runes after resolution = %d, want 0", got)
	}
	if got := filter.Flush(); got != "" {
		t.Fatalf("Flush() after resolution = %q, want empty", got)
	}
}
