package biz

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactExportValueRecursive 验证递归脱敏保留普通字段、数字与布尔值，只替换敏感键与凭证。
func TestRedactExportValueRecursive(t *testing.T) {
	output := redactExportValue(map[string]any{
		"title":  "普通标题",
		"score":  42.5,
		"active": true,
		"nested": map[string]any{"note": "hello", "password": "secret-value"},
		"items":  []any{"ok", map[string]any{"client_secret": "hidden"}},
		"config": map[string]any{
			"endpoint": "postgres://user:pass@db:5432/app",
			"token":    "ghp_abcdefghijklmnopqrstuvwxyz0123",
		},
	}).(map[string]any)
	if output["title"] != "普通标题" || output["score"] != 42.5 || output["active"] != true {
		t.Fatalf("non-secret fields changed: %#v", output)
	}
	nested, _ := output["nested"].(map[string]any)
	if nested["note"] != "hello" || nested["password"] != exportRedactedMarker {
		t.Fatalf("nested = %#v", nested)
	}
	items, _ := output["items"].([]any)
	if items[0] != "ok" {
		t.Fatalf("items = %#v", items)
	}
	if inner, _ := items[1].(map[string]any); inner["client_secret"] != exportRedactedMarker {
		t.Fatalf("inner = %#v", inner)
	}
	config, _ := output["config"].(map[string]any)
	if strings.Contains(config["endpoint"].(string), "user:pass") ||
		strings.Contains(config["token"].(string), "ghp_") {
		t.Fatalf("config = %#v", config)
	}
}

// TestRedactExportJSONInvalidFallback 验证非法 JSON 也按文本脱敏，不会原样导出。
func TestRedactExportJSONInvalidFallback(t *testing.T) {
	raw := json.RawMessage(`{"token":"sk-abcdefghijklmnopqrstuvwx"`)
	redacted := redactExportJSON(raw)
	var text string
	if err := json.Unmarshal(redacted, &text); err != nil {
		t.Fatalf("decode fallback: %v (%s)", err, redacted)
	}
	if strings.Contains(text, "sk-abcdefghijklmnopqrstuvwx") || !strings.Contains(text, exportRedactedMarker) {
		t.Fatalf("fallback = %q", text)
	}
}

// TestRedactExportTextPatterns 验证常见凭证模式全部被替换。
func TestRedactExportTextPatterns(t *testing.T) {
	cases := []string{
		"Bearer abcdefghijklmnop",
		"sk-abcdefghijklmnopqrstuvwx",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"github_pat_abcdefghijklmnopqrst",
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturevalue",
		"postgres://user:pass@db:5432/app",
		"-----BEGIN RSA PRIVATE KEY-----\nsecret\n-----END RSA PRIVATE KEY-----",
	}
	for _, input := range cases {
		if output := RedactExportText(input); !strings.Contains(output, exportRedactedMarker) {
			t.Fatalf("pattern not redacted: %q -> %q", input, output)
		}
	}
}
