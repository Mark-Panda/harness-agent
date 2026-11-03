package logging

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// TestSetupRejectsInvalidOptions 验证非法级别与格式被拒绝。
func TestSetupRejectsInvalidOptions(t *testing.T) {
	if _, err := Setup(Options{Level: "verbose"}); err == nil {
		t.Fatal("invalid level must fail")
	}
	if _, err := Setup(Options{Format: "xml"}); err == nil {
		t.Fatal("invalid format must fail")
	}
}

// TestSetupJSONLevelAndAttributes 验证 JSON 输出、级别过滤与全局服务属性。
func TestSetupJSONLevelAndAttributes(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := Setup(Options{Level: "warn", Format: "json", Service: "harness-agent", Environment: "test", Output: &buffer})
	if err != nil {
		t.Fatalf("setup logging: %v", err)
	}
	logger.Debug("filtered")
	logger.Warn("kept", "key", "value")
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("debug leak or missing warn line: %q", buffer.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["level"] != "WARN" || record["service"] != "harness-agent" ||
		record["environment"] != "test" || record["key"] != "value" {
		t.Fatalf("log record = %+v", record)
	}
}

// TestContextHandlerInjectsCorrelationIDs 验证 request_id、trace_id、span_id 与 run_id 自动注入。
func TestContextHandlerInjectsCorrelationIDs(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := Setup(Options{Format: "json", Output: &buffer})
	if err != nil {
		t.Fatalf("setup logging: %v", err)
	}
	traceBytes, _ := hex.DecodeString("4bf92f3577b34da6a3ce929d0e0e4736")
	spanBytes, _ := hex.DecodeString("00f067aa0ba902b7")
	var traceID trace.TraceID
	copy(traceID[:], traceBytes)
	var spanID trace.SpanID
	copy(spanID[:], spanBytes)
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	ctx = telemetry.WithRequestID(ctx, "req-1")
	ctx = telemetry.WithRunID(ctx, "run-1")
	logger.InfoContext(ctx, "correlated")
	var record map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["request_id"] != "req-1" || record["run_id"] != "run-1" ||
		record["trace_id"] != traceID.String() || record["span_id"] != spanID.String() {
		t.Fatalf("correlation ids = %+v", record)
	}
	// 日志正文只包含业务消息，不采集 Prompt/Completion 或工具原文。
	if record["msg"] != "correlated" {
		t.Fatalf("message = %v", record["msg"])
	}
}

// TestSetupTextFormat 验证 text 格式可用于本地调试。
func TestSetupTextFormat(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := Setup(Options{Format: "text", Level: "info", Output: &buffer})
	if err != nil {
		t.Fatalf("setup text logging: %v", err)
	}
	logger.Info("plain", slog.String("mode", "react"))
	if !strings.Contains(buffer.String(), "mode=react") {
		t.Fatalf("text log = %q", buffer.String())
	}
}
