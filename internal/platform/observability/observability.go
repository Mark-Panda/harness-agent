// Package observability 提供进程内 Prometheus 指标与 W3C Trace Context 追踪。
// 指标直接以 Prometheus 文本格式暴露在 /metrics，无需额外依赖；
// 追踪使用 OpenTelemetry API 建立服务端 Span，未注册 TracerProvider 时退化为
// 可传播的 TraceID，保证日志、响应头与外部链路仍可关联。
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// metricHelp 是已知指标的 HELP 文本；未登记的指标省略 HELP。
var metricHelp = map[string]string{
	"agent_llm_tokens_total":             "Total LLM tokens consumed by agent mode and direction.",
	"agent_llm_request_duration_seconds": "LLM request duration in seconds by agent mode and status.",
	"agent_tool_call_duration_seconds":   "Tool call duration in seconds by agent mode, tool and status.",
	"agent_event_append_failures_total":  "Total Run event append failures by event type.",
	"harness_jobs_pending":               "Pending persistent jobs by resource pool lane.",
	"harness_jobs_running":               "Running persistent jobs by resource pool lane.",
	"harness_jobs_oldest_age_seconds":    "Age in seconds of the oldest pending job by resource pool lane.",
}

// durationBuckets 是耗时秒指标的默认直方图分桶。
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// histogram 保存单个标签组合的直方图累计值。
type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

// Metrics 是最小化的 Prometheus 兼容指标注册表。
type Metrics struct {
	mu         sync.Mutex
	counters   map[string]map[string]float64
	gauges     map[string]map[string]float64
	histograms map[string]map[string]*histogram
}

// NewMetrics 构造空指标注册表。
func NewMetrics() *Metrics {
	return &Metrics{
		counters:   make(map[string]map[string]float64),
		gauges:     make(map[string]map[string]float64),
		histograms: make(map[string]map[string]*histogram),
	}
}

var defaultMetrics = NewMetrics()

// Default 返回进程级默认指标注册表。
func Default() *Metrics { return defaultMetrics }

// IncCounter 累加计数器；delta 为负值时忽略，避免指标回退。
func (m *Metrics) IncCounter(name string, labels map[string]string, delta float64) {
	if delta < 0 {
		return
	}
	key := labelsKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.counters[name]
	if !ok {
		series = make(map[string]float64)
		m.counters[name] = series
	}
	series[key] += delta
}

// SetGauge 设置瞬时值；负值按 0 处理，避免队列深度出现负指标。
func (m *Metrics) SetGauge(name string, labels map[string]string, value float64) {
	if value < 0 {
		value = 0
	}
	key := labelsKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.gauges[name]
	if !ok {
		series = make(map[string]float64)
		m.gauges[name] = series
	}
	series[key] = value
}

// ObserveDuration 记录耗时（秒）到默认分桶的直方图。
func (m *Metrics) ObserveDuration(name string, labels map[string]string, seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	key := labelsKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.histograms[name]
	if !ok {
		series = make(map[string]*histogram)
		m.histograms[name] = series
	}
	value, ok := series[key]
	if !ok {
		value = &histogram{counts: make([]uint64, len(durationBuckets))}
		series[key] = value
	}
	for index, bound := range durationBuckets {
		if seconds <= bound {
			value.counts[index]++
		}
	}
	value.sum += seconds
	value.count++
}

// MetricsHandler 以 Prometheus 文本格式输出全部指标。
func (m *Metrics) MetricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(m.Render()))
}

// Render 返回 Prometheus 文本格式快照，便于测试与调试。
func (m *Metrics) Render() string {
	m.mu.Lock()
	counters := make(map[string]map[string]float64, len(m.counters))
	for name, series := range m.counters {
		copied := make(map[string]float64, len(series))
		for key, value := range series {
			copied[key] = value
		}
		counters[name] = copied
	}
	gauges := make(map[string]map[string]float64, len(m.gauges))
	for name, series := range m.gauges {
		copied := make(map[string]float64, len(series))
		for key, value := range series {
			copied[key] = value
		}
		gauges[name] = copied
	}
	histograms := make(map[string]map[string]histogram, len(m.histograms))
	for name, series := range m.histograms {
		copied := make(map[string]histogram, len(series))
		for key, value := range series {
			copied[key] = histogram{counts: append([]uint64(nil), value.counts...), sum: value.sum, count: value.count}
		}
		histograms[name] = copied
	}
	m.mu.Unlock()

	var builder strings.Builder
	for _, name := range sortedKeys(counters) {
		writeMeta(&builder, name, "counter")
		series := counters[name]
		for _, key := range sortedKeys(series) {
			fmt.Fprintf(&builder, "%s%s %s\n", name, key, formatFloat(series[key]))
		}
	}
	for _, name := range sortedKeys(gauges) {
		writeMeta(&builder, name, "gauge")
		series := gauges[name]
		for _, key := range sortedKeys(series) {
			fmt.Fprintf(&builder, "%s%s %s\n", name, key, formatFloat(series[key]))
		}
	}
	for _, name := range sortedKeys(histograms) {
		writeMeta(&builder, name, "histogram")
		series := histograms[name]
		for _, key := range sortedKeys(series) {
			value := series[key]
			for index, bound := range durationBuckets {
				fmt.Fprintf(&builder, "%s_bucket%s %d\n", name, mergeLabel(key, "le", strconv.FormatFloat(bound, 'g', -1, 64)), value.counts[index])
			}
			fmt.Fprintf(&builder, "%s_bucket%s %d\n", name, mergeLabel(key, "le", "+Inf"), value.count)
			fmt.Fprintf(&builder, "%s_sum%s %s\n", name, key, formatFloat(value.sum))
			fmt.Fprintf(&builder, "%s_count%s %d\n", name, key, value.count)
		}
	}
	return builder.String()
}

// writeMeta 输出 HELP 与 TYPE 行。
func writeMeta(builder *strings.Builder, name, kind string) {
	if help, ok := metricHelp[name]; ok {
		fmt.Fprintf(builder, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(builder, "# TYPE %s %s\n", name, kind)
}

// labelsKey 生成稳定的标签串；无标签时返回空串。
func labelsKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(key)
		builder.WriteString(`="`)
		builder.WriteString(escapeLabel(labels[key]))
		builder.WriteByte('"')
	}
	builder.WriteByte('}')
	return builder.String()
}

// mergeLabel 把额外标签并入既有标签串，用于直方图 le 标签。
func mergeLabel(key, extraKey, extraValue string) string {
	if key == "" {
		return fmt.Sprintf(`{%s="%s"}`, extraKey, escapeLabel(extraValue))
	}
	return key[:len(key)-1] + fmt.Sprintf(`,%s="%s"}`, extraKey, escapeLabel(extraValue))
}

// escapeLabel 转义 Prometheus 标签值中的反斜杠、引号与换行。
func escapeLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// formatFloat 输出不带多余尾零的浮点数。
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// sortedKeys 返回有序键，保证指标输出稳定可比对。
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// traceIDKey 是 TraceID 的上下文键。
type traceIDKey struct{}

// WithTraceID 把 TraceID 写入上下文，供日志与业务事件读取。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFrom 读取上下文中的 TraceID；不存在时返回空串。
func TraceIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey{}).(string)
	return value
}

// SetupTracing 安装 W3C Trace Context/Baggage 传播器；provider 为空时仅做 TraceID 传播。
func SetupTracing(provider trace.TracerProvider) {
	if provider != nil {
		otel.SetTracerProvider(provider)
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
}

// TracingMiddleware 为每个请求建立服务端 Span，并回写 X-Trace-Id。
// 未配置 TracerProvider 时仍会复用入站 traceparent 或生成新的 TraceID，
// 保证日志、执行事件与外部调用链路可以按同一 TraceID 聚合。
func TracingMiddleware(next http.Handler) http.Handler {
	SetupTracing(nil)
	tracer := otel.Tracer("harness-agent/http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		if !trace.SpanContextFromContext(ctx).IsValid() {
			ctx = trace.ContextWithSpanContext(ctx, newSpanContext())
		}
		ctx, span := tracer.Start(ctx, spanName(r), trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		if spanContext := span.SpanContext(); spanContext.IsValid() {
			traceID := spanContext.TraceID().String()
			ctx = WithTraceID(ctx, traceID)
			w.Header().Set("X-Trace-Id", traceID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// maxSpanNameLength 限制 Span 名称长度，防止异常超长路径进入追踪后端。
const maxSpanNameLength = 128

// spanName 生成低基数 Span 名称。
// ServeMux 的路由模式要进入 handler 后才写入请求，中间件内读不到，
// 因此按路径形态归一：纯数字、UUID 与长十六进制段统一替换为 {id}，
// 避免每个资源 ID 生成独立 Span 名称导致追踪索引基数爆炸。
func spanName(r *http.Request) string {
	return truncateSpanName(r.Method + " " + normalizeRoute(r.URL.Path))
}

// normalizeRoute 把路径中的资源标识段替换为 {id} 占位符。
func normalizeRoute(urlPath string) string {
	if urlPath == "" {
		return "/"
	}
	segments := strings.Split(urlPath, "/")
	for index, segment := range segments {
		if highCardinalitySegment(segment) {
			segments[index] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}

// highCardinalitySegment 判断路径段是否承载资源标识。
func highCardinalitySegment(segment string) bool {
	if segment == "" {
		return false
	}
	if _, err := strconv.ParseUint(segment, 10, 64); err == nil {
		return true
	}
	if len(segment) == 36 && segment[8] == '-' && segment[13] == '-' && segment[18] == '-' && segment[23] == '-' {
		return isHexString(strings.ReplaceAll(segment, "-", ""))
	}
	return len(segment) >= 16 && isHexString(segment)
}

// isHexString 判断字符串是否全部由十六进制字符组成。
func isHexString(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

// truncateSpanName 按字符截断 Span 名称，避免切断多字节字符。
func truncateSpanName(name string) string {
	if len(name) <= maxSpanNameLength {
		return name
	}
	runes := []rune(name)
	if len(runes) <= maxSpanNameLength {
		return name
	}
	return string(runes[:maxSpanNameLength])
}

// newSpanContext 生成合法的 TraceID/SpanID，用于未启用 OTel SDK 的场景。
func newSpanContext() trace.SpanContext {
	var traceBytes [16]byte
	if _, err := rand.Read(traceBytes[:]); err != nil {
		return trace.SpanContext{}
	}
	var spanBytes [8]byte
	if _, err := rand.Read(spanBytes[:]); err != nil {
		return trace.SpanContext{}
	}
	traceID, err := trace.TraceIDFromHex(hex.EncodeToString(traceBytes[:]))
	if err != nil {
		return trace.SpanContext{}
	}
	spanID, err := trace.SpanIDFromHex(hex.EncodeToString(spanBytes[:]))
	if err != nil {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
}
