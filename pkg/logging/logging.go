// Package logging 统一结构化日志级别、输出格式与关联 ID 注入。
// 默认输出 JSON 到 stdout，自动附加 request_id、trace_id、span_id 与 run_id；
// 日志保留期由部署侧采集管道统一管理，进程不写入本地滚动文件，避免多副本写入冲突。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// Options 是日志初始化参数。
type Options struct {
	// Level 日志级别：debug/info/warn/error；空值默认 info。
	Level string
	// Format 输出格式：json/text；空值默认 json。
	Format string
	// Service 服务名，作为全局属性写入每条日志。
	Service string
	// Environment 部署环境，作为全局属性写入每条日志。
	Environment string
	// Output 输出目标；为空时写 stdout，测试可注入缓冲区。
	Output io.Writer
}

// Setup 构造并返回结构化 Logger；非法级别或格式返回错误。
func Setup(options Options) (*slog.Logger, error) {
	level := slog.LevelInfo
	if strings.TrimSpace(options.Level) != "" {
		if err := level.UnmarshalText([]byte(strings.TrimSpace(options.Level))); err != nil {
			return nil, fmt.Errorf("invalid log level %q: %w", options.Level, err)
		}
	}
	output := options.Output
	if output == nil {
		output = os.Stdout
	}
	handlerOptions := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(options.Format)) {
	case "", "json":
		handler = slog.NewJSONHandler(output, handlerOptions)
	case "text":
		handler = slog.NewTextHandler(output, handlerOptions)
	default:
		return nil, fmt.Errorf("unsupported log format %q", options.Format)
	}
	attributes := make([]slog.Attr, 0, 2)
	if options.Service != "" {
		attributes = append(attributes, slog.String("service", options.Service))
	}
	if options.Environment != "" {
		attributes = append(attributes, slog.String("environment", options.Environment))
	}
	if len(attributes) > 0 {
		handler = handler.WithAttrs(attributes)
	}
	return slog.New(contextHandler{Handler: handler}), nil
}

// contextHandler 在写入日志前从上下文提取关联 ID，避免业务代码手工重复传参。
type contextHandler struct {
	slog.Handler
}

// Handle 注入 request_id、trace_id、span_id 与 run_id 后写日志。
func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if requestID := telemetry.RequestIDFrom(ctx); requestID != "" {
		record.AddAttrs(slog.String(telemetry.AttrRequestID, requestID))
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	if runID := telemetry.RunIDFrom(ctx); runID != "" {
		record.AddAttrs(slog.String(telemetry.AttrRunID, runID))
	}
	return h.Handler.Handle(ctx, record)
}

// WithAttrs 保持上下文注入能力，返回派生的 handler。
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup 保持上下文注入能力，返回分组后的 handler。
func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
