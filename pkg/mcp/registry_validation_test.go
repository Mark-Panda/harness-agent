package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// TestCompileToolSchemaValidatesArguments 验证本地 Schema 能拦截缺字段、类型错误和额外字段。
func TestCompileToolSchemaValidatesArguments(t *testing.T) {
	schema, err := compileToolSchema(json.RawMessage(`{
		"type":"object",
		"properties":{"q":{"type":"string"}},
		"required":["q"],
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateToolArguments(schema, json.RawMessage(`{"q":"ok"}`)); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	for _, arguments := range []string{`{}`, `{"q":1}`, `{"q":"ok","extra":true}`} {
		err := validateToolArguments(schema, json.RawMessage(arguments))
		var invalid *react.InvalidArgumentsError
		if !errors.As(err, &invalid) {
			t.Fatalf("arguments %s error = %v, want InvalidArgumentsError", arguments, err)
		}
		if invalid.Detail == "" {
			t.Fatalf("arguments %s missing validation detail", arguments)
		}
	}
}

// TestBindingInvokeKeepsLargeOutput 验证 MCP 大结果会交给执行层截断，而不是在适配层直接失败。
func TestBindingInvokeKeepsLargeOutput(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": strings.Repeat("x", 70*1024)}}, "isError": false}, true
		default:
			t.Fatalf("unexpected method %s", method)
			return nil, false
		}
	})
	bound := binding{server: Server{Name: "docs", RiskLevel: "low", Client: newTestClient(t, server, "none", "")}, tool: Tool{Name: "search"}}
	output, err := bound.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(output) <= 64*1024 {
		t.Fatalf("large output was unexpectedly discarded: %d bytes", len(output))
	}
}

// TestCompileToolSchemaRejectsMalformedSchema 验证非法 Schema JSON 不会编译成功。
func TestCompileToolSchemaRejectsMalformedSchema(t *testing.T) {
	if _, err := compileToolSchema(json.RawMessage(`{"type":`)); err == nil {
		t.Fatal("malformed schema compiled successfully")
	}
	if schema, err := compileToolSchema(nil); err != nil || schema != nil {
		t.Fatalf("empty schema = %v, %v", schema, err)
	}
	// 外部 $ref 必须在编译阶段被拒绝，避免 Schema 校验引发 SSRF。
	if _, err := compileToolSchema(json.RawMessage(`{"$ref":"https://example.com/external.json"}`)); err == nil {
		t.Fatal("external schema reference compiled successfully")
	}
}
