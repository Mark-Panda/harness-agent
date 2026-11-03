// Package telemetry 提供 OpenTelemetry 初始化、上下文传播与最小化埋点辅助。
// 设计约束：默认不采集 Prompt、Completion、工具入参与结果原文，只记录低基数元数据与脱敏错误分类。
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// instrumentationName 是 tracer 名称，出现在导出数据的 scope 中。
	instrumentationName = "harness-agent"

	// AttrRequestID 请求关联 ID，贯通 HTTP/gRPC 与后台任务日志。
	AttrRequestID = "request_id"
	// AttrRunID 一次 Agent Run 的 ID；禁止作为指标标签，仅用于 Trace。
	AttrRunID = "run_id"
	// AttrStepID 执行步骤 ID；与 run_id 一起定位一次模型或工具步骤。
	AttrStepID = "step_id"
	// AttrAgentMode 编排模式：react/plan/supervisor。
	AttrAgentMode = "agent.mode"
	// AttrSessionID 会话 ID；与 run_id 一起定位多轮上下文，禁止作为指标标签。
	AttrSessionID = "session_id"
	// AttrJobID 后台任务 ID。
	AttrJobID = "job.id"
	// AttrJobType 后台任务类型，低基数。
	AttrJobType = "job.type"
	// AttrToolName 工具名称，低基数。
	AttrToolName = "tool.name"
	// AttrToolRiskLevel 工具风险等级。
	AttrToolRiskLevel = "tool.risk_level"
	// AttrToolStatus 工具执行结果状态。
	AttrToolStatus = "tool.status"
	// AttrProviderID 模型供应商标识。
	AttrProviderID = "llm.provider_id"
	// AttrModelID 模型标识。
	AttrModelID = "llm.model_id"
	// AttrRemoteModelName 供应商侧模型名。
	AttrRemoteModelName = "llm.remote_model_name"
	// AttrAttemptNo 模型尝试序号。
	AttrAttemptNo = "llm.attempt_no"
	// AttrInputTokens 输入 Token 数。
	AttrInputTokens = "llm.input_tokens" // #nosec G101 -- 指标属性键，不是凭证
	// AttrOutputTokens 输出 Token 数。
	AttrOutputTokens = "llm.output_tokens" // #nosec G101 -- 指标属性键，不是凭证
	// AttrFinishReason 模型结束原因，低频值。
	AttrFinishReason = "llm.finish_reason"
	// AttrErrorClass 脱敏错误分类；禁止写入错误原文。
	AttrErrorClass = "error.class"
	// AttrHTTPMethod HTTP 方法。
	AttrHTTPMethod = "http.request.method"
	// AttrHTTPPath 仅记录路径，不记录查询串，避免凭证或用户正文进入 Trace。
	AttrHTTPPath = "url.path"
	// AttrHTTPStatus HTTP 响应状态码。
	AttrHTTPStatus = "http.response.status_code"
	// AttrRPCSystem RPC 系统，固定为 grpc。
	AttrRPCSystem = "rpc.system"
	// AttrRPCService gRPC 服务名。
	AttrRPCService = "rpc.service"
	// AttrRPCMethod gRPC 方法名。
	AttrRPCMethod = "rpc.method"
	// AttrGRPCStatusCode gRPC 状态码。
	AttrGRPCStatusCode = "rpc.grpc.status_code"
	// AttrFeishuEventType 飞书事件类型，低基数。
	AttrFeishuEventType = "feishu.event_type"
	// AttrFeishuChatID 飞书会话 ID，用于定位群聊链路。
	AttrFeishuChatID = "feishu.chat_id"
	// AttrFeishuThreadID 飞书话题 ID，用于定位一话题一会话链路。
	AttrFeishuThreadID = "feishu.thread_id"
)

// Options 是 OpenTelemetry 初始化参数；零值表示禁用追踪。
type Options struct {
	// Enabled 是否启用 Trace 导出。
	Enabled bool
	// ServiceName 服务名，写入 service.name 资源属性。
	ServiceName string
	// Environment 部署环境，写入 deployment.environment。
	Environment string
	// Endpoint OTLP HTTP 端点，支持 host:port 或完整 URL。
	Endpoint string
	// Insecure host:port 形式下是否使用明文 HTTP。
	Insecure bool
	// Headers 导出请求头，用于出口认证；空值会被忽略。
	Headers map[string]string
	// SampleRatio 采样比例，必须位于 (0, 1]；父级采样决策优先。
	SampleRatio float64
	// BatchTimeout 批量导出间隔。
	BatchTimeout time.Duration
	// ExportTimeout 单次导出超时。
	ExportTimeout time.Duration
	// ResourceAttributes 附加资源属性，禁止写入用户或会话标识。
	ResourceAttributes map[string]string
}

// normalize 校验并补齐默认值；启用追踪时端点与采样比例必须有效。
func (o Options) normalize() (Options, error) {
	if !o.Enabled {
		return o, nil
	}
	if strings.TrimSpace(o.ServiceName) == "" {
		o.ServiceName = instrumentationName
	}
	if strings.TrimSpace(o.Endpoint) == "" {
		return Options{}, fmt.Errorf("telemetry endpoint is required when tracing is enabled")
	}
	if strings.Contains(o.Endpoint, "://") {
		parsed, err := url.Parse(o.Endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Options{}, fmt.Errorf("telemetry endpoint must be a valid http(s) URL")
		}
	}
	if o.SampleRatio <= 0 || o.SampleRatio > 1 {
		return Options{}, fmt.Errorf("telemetry sample ratio must be within (0, 1]")
	}
	if o.BatchTimeout <= 0 {
		o.BatchTimeout = 5 * time.Second
	}
	if o.ExportTimeout <= 0 {
		o.ExportTimeout = 10 * time.Second
	}
	return o, nil
}

// Provider 持有 TracerProvider 的关闭句柄；禁用追踪时关闭为空操作。
type Provider struct {
	// shutdown 刷新并关闭导出管线。
	shutdown func(context.Context) error
}

// Shutdown 刷新未导出的 Span 并关闭 Provider。
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Setup 初始化全局 TracerProvider 与上下文传播器；禁用时安装 noop Provider。
// 关键步骤：始终注册 TraceContext + Baggage 传播器，保证禁用导出后仍能解析 traceparent 关联外部链路。
func Setup(ctx context.Context, options Options) (*Provider, error) {
	normalized, err := options.normalize()
	if err != nil {
		return nil, err
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if !normalized.Enabled {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return &Provider{shutdown: func(context.Context) error { return nil }}, nil
	}
	exporterOptions := []otlptracehttp.Option{otlptracehttp.WithTimeout(normalized.ExportTimeout)}
	if strings.Contains(normalized.Endpoint, "://") {
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(normalized.Endpoint))
	} else {
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpoint(normalized.Endpoint))
		if normalized.Insecure {
			exporterOptions = append(exporterOptions, otlptracehttp.WithInsecure())
		}
	}
	headers := make(map[string]string, len(normalized.Headers))
	for key, value := range normalized.Headers {
		// 空值不发送，避免把未注入的环境变量变成空凭证头。
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			headers[key] = value
		}
	}
	if len(headers) > 0 {
		exporterOptions = append(exporterOptions, otlptracehttp.WithHeaders(headers))
	}
	exporter, err := otlptracehttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}
	attributes := []attribute.KeyValue{attribute.String("service.name", normalized.ServiceName)}
	if normalized.Environment != "" {
		attributes = append(attributes, attribute.String("deployment.environment", normalized.Environment))
	}
	for key, value := range normalized.ResourceAttributes {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			attributes = append(attributes, attribute.String(key, value))
		}
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(normalized.BatchTimeout),
			sdktrace.WithExportTimeout(normalized.ExportTimeout),
		),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(normalized.SampleRatio))),
		sdktrace.WithResource(resource.NewSchemaless(attributes...)),
	)
	otel.SetTracerProvider(provider)
	return &Provider{shutdown: provider.Shutdown}, nil
}

// Tracer 返回当前全局 Provider 的 tracer；Provider 未初始化时自动降级为 noop。
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// Start 在上下文中开启内部 Span；调用方必须 defer span.End()。
// 关键约束：attrs 只能是低基数元数据，禁止传入消息正文、Prompt、Completion 或工具原文。
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...), trace.WithSpanKind(trace.SpanKindInternal))
}

// StartServer 开启服务端 Span，用于 HTTP/gRPC 入站请求。
func StartServer(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...), trace.WithSpanKind(trace.SpanKindServer))
}

// Fail 以脱敏错误分类标记 Span 失败；禁止把错误原文写入描述或属性。
func Fail(span trace.Span, errorClass string) {
	if span == nil {
		return
	}
	if strings.TrimSpace(errorClass) == "" {
		errorClass = "error"
	}
	span.SetAttributes(attribute.String(AttrErrorClass, errorClass))
	span.SetStatus(codes.Error, errorClass)
}

// Annotate 向 Span 追加低基数元数据，空值自动忽略。
func Annotate(span trace.Span, attrs ...attribute.KeyValue) {
	if span == nil {
		return
	}
	span.SetAttributes(attrs...)
}
