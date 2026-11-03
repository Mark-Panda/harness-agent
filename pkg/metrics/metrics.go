// Package metrics 暴露低基数 Prometheus 指标：API RED、资源 USE、Token/费用、Run、工具、队列、Outbox、SSE 与记忆。
// 约束：标签只允许稳定的枚举、路由模板、工具名与模型信息，禁止 user_id、session_id、run_id 等无界标识。
package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// inflightJobs 是无锁在途任务计数，供优雅停机判断是否可以安全退出。
var inflightJobs atomic.Int64

// InflightJobs 返回当前在途后台任务数，用于 readiness 摘流后的优雅等待。
func InflightJobs() int64 {
	return inflightJobs.Load()
}

// registry 是独立注册表，避免默认注册表被其他库注入不可控指标。
var registry = prometheus.NewRegistry()

var (
	// apiRequestsTotal 是 API RED 的请求计数，route 使用 ServeMux 模板而不是原始路径。
	apiRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "api", Name: "requests_total",
		Help: "Total inbound HTTP requests by method, route template and status code.",
	}, []string{"method", "route", "status"})
	// apiRequestDuration 是 API RED 的延迟直方图。
	apiRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "harness", Subsystem: "api", Name: "request_duration_seconds",
		Help: "Inbound HTTP request latency by method and route template.", Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	// runsTotal 统计 Run 创建与终态数量，mode 为 react/plan/supervisor。
	runsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "runs_total",
		Help: "Agent runs by orchestration mode and terminal status.",
	}, []string{"mode", "status"})
	// runDuration 统计 Run 执行耗时。
	runDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "harness", Subsystem: "agent", Name: "run_duration_seconds",
		Help: "Agent run execution duration by orchestration mode.", Buckets: prometheus.DefBuckets,
	}, []string{"mode"})
	// llmTokensTotal 统计输入/输出 Token，按模式区分。
	llmTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "llm_tokens_total",
		Help: "LLM tokens by orchestration mode and kind (input/output).",
	}, []string{"mode", "kind"})
	// llmCostTotal 统计估算费用。
	llmCostTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "llm_cost_total",
		Help: "Estimated LLM cost by orchestration mode.",
	}, []string{"mode"})
	// toolCallsTotal 统计工具调用结果。
	toolCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "tool_calls_total",
		Help: "Tool invocations by tool name and status.",
	}, []string{"tool", "status"})
	// toolCallDuration 统计工具执行耗时。
	toolCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "harness", Subsystem: "agent", Name: "tool_call_duration_seconds",
		Help: "Tool invocation duration by tool name.", Buckets: prometheus.DefBuckets,
	}, []string{"tool"})
	// jobsTotal 统计后台任务结果，type 为 agent.run/summary/feishu.event 等。
	jobsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "jobs_total",
		Help: "Background jobs by type and outcome.",
	}, []string{"type", "outcome"})
	// jobDuration 统计后台任务耗时。
	jobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "harness", Subsystem: "agent", Name: "job_duration_seconds",
		Help: "Background job duration by type.", Buckets: prometheus.DefBuckets,
	}, []string{"type"})
	// jobsInflight 统计当前处理中的任务数。
	jobsInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "harness", Subsystem: "agent", Name: "jobs_inflight",
		Help: "Background jobs currently being processed by type.",
	}, []string{"type"})
	// outboxEventsTotal 统计 Outbox 派发结果。
	outboxEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "outbox_events_total",
		Help: "Outbox events dispatched by event type and outcome.",
	}, []string{"event_type", "outcome"})
	// sseConnections 统计当前 SSE 连接数。
	sseConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "harness", Subsystem: "agent", Name: "sse_connections",
		Help: "Active Server-Sent Events connections.",
	})
	// sseEventsTotal 统计 SSE 输出事件数量。
	sseEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "sse_events_total",
		Help: "Server-Sent Events emitted by event type.",
	}, []string{"event_type"})
	// memoryOperationsTotal 统计长期记忆、摘要与事实的操作结果。
	memoryOperationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "harness", Subsystem: "agent", Name: "memory_operations_total",
		Help: "Memory operations by operation and outcome.",
	}, []string{"operation", "outcome"})
)

func init() {
	registry.MustRegister(
		apiRequestsTotal, apiRequestDuration,
		runsTotal, runDuration, llmTokensTotal, llmCostTotal, toolCallsTotal, toolCallDuration,
		jobsTotal, jobDuration, jobsInflight, outboxEventsTotal, sseConnections, sseEventsTotal, memoryOperationsTotal,
	)
	// USE 指标：Go 运行时（堆、GC、协程）与进程资源（CPU、RSS、FD）。
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Handler 返回 /metrics 的 Prometheus 采集处理器。
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// safeLabel 把外部来源的标签值转为合法 UTF-8 并限制长度，避免 Prometheus panic 与无界基数。
// 约束：只用于低基数标签；调用方仍需保证业务枚举稳定。
func safeLabel(value string) string {
	value = strings.ToValidUTF8(value, "?")
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

// ObserveHTTP 记录一次 API 请求；route 必须是路由模板而非含 ID 的原始路径。
func ObserveHTTP(method, route string, status int, elapsed time.Duration) {
	apiRequestsTotal.WithLabelValues(safeLabel(method), safeLabel(route), statusCode(status)).Inc()
	apiRequestDuration.WithLabelValues(safeLabel(method), safeLabel(route)).Observe(elapsed.Seconds())
}

// IncRunCreated 记录一次 Run 创建。
func IncRunCreated(mode string) {
	runsTotal.WithLabelValues(safeLabel(mode), "queued").Inc()
}

// IncRunStatus 记录 Run 终态数量；耗时未知时只计数，避免向直方图写入 0 值。
func IncRunStatus(mode, status string) {
	runsTotal.WithLabelValues(safeLabel(mode), safeLabel(status)).Inc()
}

// ObserveRunFinished 记录 Run 终态与耗时。
func ObserveRunFinished(mode, status string, elapsed time.Duration) {
	runsTotal.WithLabelValues(safeLabel(mode), safeLabel(status)).Inc()
	runDuration.WithLabelValues(safeLabel(mode)).Observe(elapsed.Seconds())
}

// ObserveLLM 记录一次模型尝试的 Token 与费用。
func ObserveLLM(mode string, inputTokens, outputTokens int64, cost float64) {
	llmTokensTotal.WithLabelValues(safeLabel(mode), "input").Add(float64(inputTokens))
	llmTokensTotal.WithLabelValues(safeLabel(mode), "output").Add(float64(outputTokens))
	if cost > 0 {
		llmCostTotal.WithLabelValues(safeLabel(mode)).Add(cost)
	}
}

// ObserveTool 记录一次工具调用结果与耗时。
func ObserveTool(tool, status string, elapsed time.Duration) {
	toolCallsTotal.WithLabelValues(safeLabel(tool), safeLabel(status)).Inc()
	toolCallDuration.WithLabelValues(safeLabel(tool)).Observe(elapsed.Seconds())
}

// StartJob 标记任务开始并返回结束回调；回调记录耗时、结果并减少在途计数。
func StartJob(jobType string) func(outcome string) {
	jobType = safeLabel(jobType)
	jobsInflight.WithLabelValues(jobType).Inc()
	inflightJobs.Add(1)
	started := time.Now()
	return func(outcome string) {
		jobsInflight.WithLabelValues(jobType).Dec()
		inflightJobs.Add(-1)
		jobsTotal.WithLabelValues(jobType, safeLabel(outcome)).Inc()
		jobDuration.WithLabelValues(jobType).Observe(time.Since(started).Seconds())
	}
}

// IncOutbox 记录一次 Outbox 派发结果。
func IncOutbox(eventType, outcome string) {
	outboxEventsTotal.WithLabelValues(safeLabel(eventType), safeLabel(outcome)).Inc()
}

// AddSSEConnection 调整当前 SSE 连接数。
func AddSSEConnection(delta float64) {
	sseConnections.Add(delta)
}

// IncSSEEvent 记录一条 SSE 事件。
func IncSSEEvent(eventType string) {
	sseEventsTotal.WithLabelValues(safeLabel(eventType)).Inc()
}

// ObserveMemory 记录一次记忆操作结果。
func ObserveMemory(operation, outcome string) {
	memoryOperationsTotal.WithLabelValues(safeLabel(operation), safeLabel(outcome)).Inc()
}

// statusCode 把状态码转为字符串标签，避免整数标签缺少稳定格式。
func statusCode(status int) string {
	if status <= 0 {
		status = http.StatusOK
	}
	return strconv.Itoa(status)
}
