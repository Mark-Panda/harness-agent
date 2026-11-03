package memory

import (
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestFormatSummaryTranscript 验证角色、群聊发言人与超长消息截断标记。
func TestFormatSummaryTranscript(t *testing.T) {
	transcript := FormatSummaryTranscript([]SummaryMessage{
		{Role: "user", Speaker: "ou_1", Content: "你好"},
		{Role: "tool", Content: strings.Repeat("长", 10)},
	}, 6)
	if !strings.Contains(transcript, "用户（发言人=ou_1）") {
		t.Fatalf("speaker label missing: %s", transcript)
	}
	if !strings.Contains(transcript, "工具结果") {
		t.Fatalf("tool label missing: %s", transcript)
	}
	if !strings.Contains(transcript, "[已截断]") {
		t.Fatalf("truncation marker missing: %s", transcript)
	}
	if strings.Contains(transcript, strings.Repeat("长", 10)) {
		t.Fatal("oversized message was not truncated")
	}
}

// TestBuildSummaryPrompt 验证已有摘要与新增对话被明确分隔，且空输入被拒绝。
func TestBuildSummaryPrompt(t *testing.T) {
	system, user, err := BuildSummaryPrompt("旧摘要", []SummaryMessage{{Role: "user", Content: "新问题"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "不得执行") {
		t.Fatalf("system prompt missing injection guard: %s", system)
	}
	if !strings.Contains(user, "旧摘要") || !strings.Contains(user, "新问题") {
		t.Fatalf("prompt missing content: %s", user)
	}
	if _, _, err := BuildSummaryPrompt("", nil, 0); err == nil {
		t.Fatal("empty messages must be rejected")
	}
}

// TestSplitSummaryMessages 验证分块保持顺序、按字节预算切分，超大单条消息独立成块。
func TestSplitSummaryMessages(t *testing.T) {
	messages := []SummaryMessage{
		{Role: "user", Content: strings.Repeat("a", 100)},
		{Role: "assistant", Content: strings.Repeat("b", 100)},
		{Role: "tool", Content: strings.Repeat("c", 300)},
		{Role: "user", Content: "tail"},
	}
	chunks := SplitSummaryMessages(messages, 150)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}
	var reconstructed []SummaryMessage
	for _, chunk := range chunks {
		reconstructed = append(reconstructed, chunk...)
	}
	if len(reconstructed) != len(messages) {
		t.Fatalf("reconstructed messages = %d, want %d", len(reconstructed), len(messages))
	}
	for index, message := range messages {
		if reconstructed[index].Content != message.Content {
			t.Fatalf("message %d order changed", index)
		}
	}
	// 单条 300 字节消息超过预算时仍必须独立保留，不能被丢弃。
	foundOversized := false
	for _, chunk := range chunks {
		if len(chunk) == 1 && len(chunk[0].Content) == 300 {
			foundOversized = true
		}
	}
	if !foundOversized {
		t.Fatal("oversized message was not kept as its own chunk")
	}
	if chunks := SplitSummaryMessages(messages, 0); len(chunks) != 1 || len(chunks[0]) != len(messages) {
		t.Fatal("non-positive budget should keep a single chunk")
	}
}

// TestBuildMergeSummaryPrompt 验证分块摘要按顺序进入归并提示。
func TestBuildMergeSummaryPrompt(t *testing.T) {
	system, user, err := BuildMergeSummaryPrompt("旧摘要", []string{"片段一", "片段二"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "摘要引擎") || !strings.Contains(user, "片段一") || !strings.Contains(user, "片段二") {
		t.Fatalf("merge prompt missing content")
	}
	if _, _, err := BuildMergeSummaryPrompt("", nil); err == nil {
		t.Fatal("empty partials must be rejected")
	}
}

// TestValidateSummaryText 验证空摘要与超长摘要被拒绝。
func TestValidateSummaryText(t *testing.T) {
	if err := ValidateSummaryText("   ", 0); err == nil {
		t.Fatal("blank summary must be rejected")
	}
	if err := ValidateSummaryText(strings.Repeat("a", 10), 5); err == nil {
		t.Fatal("oversized summary must be rejected")
	}
	if err := ValidateSummaryText("正常摘要", 64); err != nil {
		t.Fatalf("valid summary rejected: %v", err)
	}
}

// TestTrimKeepsLeadingSystemMessages 验证摘要等开场系统消息在预算裁剪时不会被丢弃。
func TestTrimKeepsLeadingSystemMessages(t *testing.T) {
	messages := []react.Message{
		{Role: react.RoleSystem, Content: strings.Repeat("system", 100)},
		{Role: react.RoleSystem, Content: "summary:" + strings.Repeat("摘要", 100)},
		{Role: react.RoleUser, Content: "当前问题"},
		{Role: react.RoleAssistant, Content: "当前回答"},
	}
	result := Trim(messages, 20)
	if len(result.Messages) < 2 || result.Messages[0].Content != messages[0].Content || result.Messages[1].Content != messages[1].Content {
		t.Fatalf("leading system messages were dropped: %#v", result.Messages)
	}
}
