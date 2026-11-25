package bootstrap

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/Mark-Panda/harness-agent/internal/platform/observability"
)

// jobQueueMetricsInterval 是队列深度指标的刷新间隔；HPA 读取该指标扩缩容。
const jobQueueMetricsInterval = 15 * time.Second

// workerComponentEnabled 判断当前进程是否启动指定 Worker 组件；为空表示全部启动。
func workerComponentEnabled(cfg config.WorkerConfig, component string) bool {
	raw := strings.TrimSpace(cfg.Components)
	if raw == "" {
		return true
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(part), component) {
			return true
		}
	}
	return false
}

// effectiveExecutionPools 返回实际启动的执行资源池；未配置时保持单池兼容模式。
func effectiveExecutionPools(cfg config.WorkerConfig) []config.WorkerPoolConfig {
	pools := cfg.Pools
	if len(pools) == 0 {
		pools = []config.WorkerPoolConfig{{
			Name:              "default",
			Concurrency:       cfg.Concurrency,
			GlobalConcurrency: cfg.GlobalConcurrency,
			Lease:             cfg.Lease,
			Heartbeat:         cfg.Heartbeat,
		}}
	}
	// lane 统一小写，保证与入队时写入的 lane 值一致；避免 "REACT" 这类配置静默认领不到任务。
	normalized := make([]config.WorkerPoolConfig, len(pools))
	for index, pool := range pools {
		pool.Lanes = normalizePoolLanes(pool.Lanes)
		normalized[index] = pool
	}
	filters := poolFilterValues(cfg.PoolFilter)
	if len(filters) == 0 {
		return normalized
	}
	allowed := make(map[string]struct{}, len(filters))
	for _, name := range filters {
		// 与配置校验保持一致：只要出现 * 就表示全部池，避免 "*,plan" 这类写法在运行时过滤为空。
		if name == "*" {
			return normalized
		}
		allowed[name] = struct{}{}
	}
	filtered := make([]config.WorkerPoolConfig, 0, len(normalized))
	for _, pool := range normalized {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(pool.Name))]; ok {
			filtered = append(filtered, pool)
		}
	}
	return filtered
}

// poolFilterValues 按配置校验相同的规则拆分 pool_filter：逗号分隔、去空白、统一小写、去重。
func poolFilterValues(raw string) []string {
	seen := make(map[string]struct{}, 4)
	values := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(part))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

// normalizePoolLanes 去除空白并统一小写；空 lane 由配置校验拒绝，这里只做防御性跳过。
func normalizePoolLanes(lanes []string) []string {
	if len(lanes) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		if value := strings.ToLower(strings.TrimSpace(lane)); value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

// knownJobLanes 是即使队列为空也必须输出 0 的内置 lane，避免 HPA 因序列缺失而抖动。
var knownJobLanes = []string{
	biz.JobLaneReact, biz.JobLanePlan, biz.JobLaneSupervisor, biz.JobLaneBackground, biz.JobLaneFeishu,
}

// collectJobQueueMetrics 周期性把各 lane 的队列深度写入 Prometheus 指标。
// 查询失败只记录日志，不阻断 API 或 Worker 主流程。
func collectJobQueueMetrics(ctx context.Context, jobs *data.JobRepository, interval time.Duration) {
	if jobs == nil {
		return
	}
	if interval <= 0 {
		interval = jobQueueMetricsInterval
	}
	// previous 记录上一轮出现过的 lane：自定义 lane 队列清空后没有聚合行，
	// 必须显式归零，否则 gauge 会保留最后一次非零值导致 HPA 无法缩容。
	previous := make(map[string]struct{})
	collect := func() {
		queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
		stats, err := jobs.StatsByLane(queryContext)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("collect job queue metrics failed", "error", err)
			}
			return
		}
		metrics := observability.Default()
		now := time.Now().UTC()
		zero := func(lane string) {
			labels := map[string]string{"lane": lane}
			metrics.SetGauge("harness_jobs_pending", labels, 0)
			metrics.SetGauge("harness_jobs_running", labels, 0)
			metrics.SetGauge("harness_jobs_oldest_age_seconds", labels, 0)
		}
		next := make(map[string]struct{}, len(stats))
		for _, stat := range stats {
			next[stat.Lane] = struct{}{}
			labels := map[string]string{"lane": stat.Lane}
			metrics.SetGauge("harness_jobs_pending", labels, float64(stat.Pending))
			metrics.SetGauge("harness_jobs_running", labels, float64(stat.Running))
			age := 0.0
			if stat.OldestAvailableAt != nil && stat.Pending > 0 {
				if elapsed := now.Sub(*stat.OldestAvailableAt).Seconds(); elapsed > 0 {
					age = elapsed
				}
			}
			metrics.SetGauge("harness_jobs_oldest_age_seconds", labels, age)
		}
		for lane := range previous {
			if _, exists := next[lane]; !exists {
				zero(lane)
			}
		}
		for _, lane := range knownJobLanes {
			if _, exists := next[lane]; !exists {
				zero(lane)
			}
		}
		previous = next
	}
	collect()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

// workerDrainTimeout 返回停机排空上限；未配置时回退到 HTTP 停机超时。
// 应与 Pod 的 terminationGracePeriodSeconds 对齐，否则长任务会被提前取消。
func workerDrainTimeout(cfg *config.Config) time.Duration {
	if timeout := cfg.Agent.Worker.DrainTimeout.Duration; timeout > 0 {
		return timeout
	}
	if timeout := cfg.Server.ShutdownTimeout.Duration; timeout > 0 {
		return timeout
	}
	return 30 * time.Second
}

// drainWorkers 停止认领新任务并等待在途任务收敛；超时后返回，由调用方取消上下文。
func drainWorkers(timeout time.Duration) {
	biz.BeginWorkerDrain()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for biz.InflightJobs() > 0 {
		select {
		case <-deadline.C:
			slog.Warn("worker drain timed out", "inflight", biz.InflightJobs(), "timeout", timeout)
			return
		case <-ticker.C:
		}
	}
}
