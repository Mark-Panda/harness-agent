package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildFactExtractionPrompt 验证事实提取提示包含 JSON 约束与编号对话。
func TestBuildFactExtractionPrompt(t *testing.T) {
	system, user, err := BuildFactExtractionPrompt([]SummaryMessage{
		{Role: "user", Content: "我偏好中文回答"},
		{Role: "assistant", Content: "好的"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, `"facts"`) || !strings.Contains(system, "correction") {
		t.Fatalf("system prompt missing schema: %s", system)
	}
	if !strings.Contains(user, "我偏好中文回答") {
		t.Fatalf("user prompt missing transcript: %s", user)
	}
	if _, _, err := BuildFactExtractionPrompt(nil, 0); err == nil {
		t.Fatal("empty messages must be rejected")
	}
}

// TestParseFactCandidates 验证合法事实解析、去重、过滤与代码块容错。
func TestParseFactCandidates(t *testing.T) {
	payload := "```json\n" + `{"facts":[
		{"key":"preference.language","value":"中文","confidence":0.9,"correction":false,"source_index":1},
		{"key":"preference.language","value":"英文","confidence":0.5,"correction":false,"source_index":2},
		{"key":"Bad Key","value":"x","confidence":0.9},
		{"key":"preference.timezone","value":null,"confidence":0.9},
		{"key":"constraint.deploy","value":{"window":"周末"},"confidence":0.8,"correction":true,"source_index":2}
	]}` + "\n```"
	facts, err := ParseFactCandidates(payload, 20, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %#v", facts)
	}
	// 同 key 保留置信度更高的候选。
	if facts[0].Key != "constraint.deploy" || facts[1].Key != "preference.language" || string(facts[1].Value) != `"中文"` {
		t.Fatalf("facts = %#v", facts)
	}
	if !facts[0].Correction {
		t.Fatalf("correction flag lost: %#v", facts[0])
	}
}

// TestParseFactCandidatesRejectsInvalidJSON 验证非法 JSON 与超限值被拒绝。
func TestParseFactCandidatesRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseFactCandidates("not json", 20, 1024); err == nil {
		t.Fatal("invalid json must be rejected")
	}
	huge := `{"facts":[{"key":"k","value":"` + strings.Repeat("a", 200) + `","confidence":0.9}]}`
	if _, err := ParseFactCandidates(huge, 20, 100); err != nil {
		t.Fatalf("invalid oversized value should be filtered, not fail: %v", err)
	} else if facts, _ := ParseFactCandidates(huge, 20, 100); len(facts) != 0 {
		t.Fatalf("oversized value was accepted: %#v", facts)
	}
	// 空数组是合法输出，表示没有可提取事实。
	if facts, err := ParseFactCandidates(`{"facts":[]}`, 20, 1024); err != nil || len(facts) != 0 {
		t.Fatalf("empty facts = %#v err=%v", facts, err)
	}
}

// TestFactValueEncoding 验证复杂 JSON 值保持原始结构。
func TestFactValueEncoding(t *testing.T) {
	payload := `{"facts":[{"key":"profile.settings","value":{"theme":"dark","lang":"zh"},"confidence":0.85}]}`
	facts, err := ParseFactCandidates(payload, 20, 8*1024)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%#v err=%v", facts, err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(facts[0].Value, &decoded); err != nil || decoded["theme"] != "dark" {
		t.Fatalf("value = %s", facts[0].Value)
	}
}
