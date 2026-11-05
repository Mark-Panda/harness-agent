package biz

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateFeishuTextKeepsValidUTF8 覆盖按 UTF-8 边界截断，避免中文或 emoji 被截成半个字符。
func TestTruncateFeishuTextKeepsValidUTF8(t *testing.T) {
	text := "飞书卡片回复需要保持完整字符"
	// 截断点落在多字节字符中间时，必须回退到上一个完整字符边界。
	truncated := truncateFeishuText(text, 7)
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated text is invalid UTF-8: %q", truncated)
	}
	if len(truncated) > 7 || !strings.HasPrefix(text, truncated) {
		t.Fatalf("truncated text = %q", truncated)
	}
	// 未超过上限时原文返回，不产生额外分配。
	if got := truncateFeishuText(text, len(text)); got != text {
		t.Fatalf("unexpected unchanged text = %q", got)
	}
	// 非正数上限视为不限制，避免生成空卡片。
	if got := truncateFeishuText(text, 0); got != text {
		t.Fatalf("zero limit text = %q", got)
	}
	// emoji 属于四字节字符，按字节截断同样必须保持合法。
	emoji := "结果🎉完成"
	clipped := truncateFeishuText(emoji, 7)
	if !utf8.ValidString(clipped) || len(clipped) > 7 {
		t.Fatalf("emoji truncated = %q", clipped)
	}
}

// TestCardFingerprintIgnoresContentBeyondLimit 覆盖指纹基于截断后内容，超限文本不产生无意义更新。
func TestCardFingerprintIgnoresContentBeyondLimit(t *testing.T) {
	base := strings.Repeat("a", 100)
	long := base + strings.Repeat("b", 100)
	if cardFingerprint("标题", base, 100) != cardFingerprint("标题", long, 100) {
		t.Fatal("fingerprint should ignore content beyond the limit")
	}
	if cardFingerprint("标题一", base, 100) == cardFingerprint("标题二", base, 100) {
		t.Fatal("fingerprint should include the title")
	}
}

// TestKeyedLimiterEnforcesPerKeyLimit 覆盖按 key 的并发名额占用与释放。
func TestKeyedLimiterEnforcesPerKeyLimit(t *testing.T) {
	limiter := newKeyedLimiter()
	if !limiter.acquire("chat", 1) {
		t.Fatal("first acquire should succeed")
	}
	if limiter.acquire("chat", 1) {
		t.Fatal("second acquire on the same key should be rejected")
	}
	// 其他 key 不受影响，避免单会话阻塞全局外发。
	if !limiter.acquire("other", 1) {
		t.Fatal("different key should succeed")
	}
	limiter.release("chat")
	limiter.release("other")
	if !limiter.acquire("chat", 1) {
		t.Fatal("acquire after release should succeed")
	}
	// limit <= 0 或空键视为不限制，调用方无需额外分支。
	if !limiter.acquire("", 0) || !limiter.acquire("chat", 0) {
		t.Fatal("unlimited acquire should always succeed")
	}
	limiter.release("")
}

// TestBuildFeishuCardUsesMarkdownV2 验证回复卡片使用 JSON 2.0 markdown 组件，能渲染标题、表格与代码块。
func TestBuildFeishuCardUsesMarkdownV2(t *testing.T) {
	text := "# 结论\n\n| 项目 | 结果 |\n| --- | --- |\n| 登录 | 正常 |\n\n```go\nfmt.Println(\"ok\")\n```"
	card := buildFeishuCard("执行完成", text, 0)
	if card["schema"] != "2.0" {
		t.Fatalf("card schema = %v", card["schema"])
	}
	config, ok := card["config"].(map[string]any)
	if !ok || config["width_mode"] != "fill" || config["update_multi"] != true {
		t.Fatalf("card config = %+v", card["config"])
	}
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatalf("card body = %+v", card["body"])
	}
	elements, ok := body["elements"].([]map[string]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("card elements = %+v", body["elements"])
	}
	if elements[0]["tag"] != "markdown" || elements[0]["content"] != text {
		t.Fatalf("markdown element = %+v", elements[0])
	}
}
