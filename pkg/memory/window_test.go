package memory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestEstimateTokens 验证 ASCII 与中文的保守 Token 估算。
func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Fatal("empty text must estimate to zero")
	}
	if tokens := EstimateTokens("hello world"); tokens < 2 || tokens > 4 {
		t.Fatalf("ascii tokens = %d", tokens)
	}
	if tokens := EstimateTokens("你好世界"); tokens != 4 {
		t.Fatalf("cjk tokens = %d", tokens)
	}
	messages := []react.Message{
		{Role: react.RoleSystem, Content: "system"},
		{Role: react.RoleAssistant, ToolCalls: []react.ToolCall{{ID: "1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}}},
	}
	if EstimateMessagesTokens(messages) <= EstimateTokens("system") {
		t.Fatal("message overhead and tool calls were not counted")
	}
}

// TestTrimKeepsSystemAndLatestTurn 验证预算不足时从最旧轮次开始裁剪，保留系统提示与当前轮次。
func TestTrimKeepsSystemAndLatestTurn(t *testing.T) {
	oldTurn := []react.Message{
		{Role: react.RoleUser, Content: strings.Repeat("旧", 20)},
		{Role: react.RoleAssistant, Content: strings.Repeat("答", 20)},
	}
	currentTurn := []react.Message{
		{Role: react.RoleUser, Content: "当前问题"},
		{Role: react.RoleAssistant, Content: "当前回答"},
	}
	messages := append([]react.Message{{Role: react.RoleSystem, Content: "system"}}, oldTurn...)
	messages = append(messages, currentTurn...)

	trimmed := Trim(messages, 30)
	if len(trimmed.Messages) != 3 || trimmed.Messages[0].Role != react.RoleSystem ||
		trimmed.Messages[1].Content != "当前问题" || trimmed.Messages[2].Content != "当前回答" {
		t.Fatalf("trimmed messages = %#v", trimmed.Messages)
	}
	if trimmed.DroppedTurns != 1 || trimmed.DroppedMessages != 2 {
		t.Fatalf("trim metadata = %#v", trimmed)
	}
	if !hasTrimReason(trimmed, "current_turn_kept") || !hasTrimReason(trimmed, "oldest_turns_dropped") {
		t.Fatalf("trim reasons = %#v", trimmed.Reasons)
	}

	// 预算充足时不裁剪。
	if trimmed := Trim(messages, 10000); len(trimmed.Messages) != len(messages) || trimmed.DroppedMessages != 0 {
		t.Fatalf("under budget trim = %#v", trimmed)
	}
	// 零预算表示不限制。
	if trimmed := Trim(messages, 0); len(trimmed.Messages) != len(messages) || trimmed.DroppedTurns != 0 {
		t.Fatalf("zero budget trim = %#v", trimmed)
	}
}

// hasTrimReason 判断裁剪原因中是否包含指定原因码。
func hasTrimReason(result TrimResult, code string) bool {
	for _, reason := range result.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// TestTrimRecordsFixedContextOverBudget 验证系统提示与当前轮次超出预算时记录明确原因。
func TestTrimRecordsFixedContextOverBudget(t *testing.T) {
	messages := []react.Message{
		{Role: react.RoleSystem, Content: strings.Repeat("系", 100)},
		{Role: react.RoleUser, Content: strings.Repeat("问", 100)},
	}
	trimmed := Trim(messages, 20)
	if len(trimmed.Messages) != 2 || trimmed.DroppedMessages != 0 {
		t.Fatalf("fixed context trim = %#v", trimmed)
	}
	if !hasTrimReason(trimmed, "fixed_context_exceeds_budget") {
		t.Fatalf("reasons = %#v", trimmed.Reasons)
	}
}

// TestTrimKeepsToolPairsTogether 验证轮次整组裁剪不会产生孤立的工具结果。
func TestTrimKeepsToolPairsTogether(t *testing.T) {
	messages := []react.Message{
		{Role: react.RoleSystem, Content: "system"},
		{Role: react.RoleUser, Content: strings.Repeat("旧", 40)},
		{Role: react.RoleAssistant, ToolCalls: []react.ToolCall{{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{}`)}}},
		{Role: react.RoleTool, Content: strings.Repeat("结果", 40), ToolCallID: "call-1"},
		{Role: react.RoleUser, Content: "新问题"},
	}
	trimmed := Trim(messages, 20)
	if len(trimmed.Messages) != 2 {
		t.Fatalf("trimmed messages = %#v", trimmed.Messages)
	}
	for _, message := range trimmed.Messages {
		if message.Role == react.RoleTool && message.ToolCallID == "call-1" {
			t.Fatal("orphan tool result survived trimming")
		}
	}
	// 没有系统提示与空消息时保持稳定。
	if trimmed := Trim(nil, 10); len(trimmed.Messages) != 0 {
		t.Fatalf("empty trim = %#v", trimmed)
	}
}
