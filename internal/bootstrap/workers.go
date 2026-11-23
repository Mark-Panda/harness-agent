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
	filter := strings.TrimSpace(cfg.PoolFilter)
	if filter == "" {
		return pools
	}
	allowed := make(map[string]struct{})
	for _, part := range strings.Split(filter, ",") {
		if name := strings.ToLower(strings.TrimSpace(part)); name != "" {
			allowed[name] = struct{}{}
		}
	}
	filtered := make([]config.WorkerPoolConfig, 0, len(pools))
	for _, pool := range pools {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(pool.Name))]; ok {
			filtered = append(filtered, pool)
		}
	}
	return filtered
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
		present := make(map[string]struct{}, len(stats))
		for _, stat := range stats {
			present[stat.Lane] = struct{}{}
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
		// 已知池即使队列为空也输出 0，保证 HPA 与看板不会因序列缺失而抖动。
		for _, lane := range []string{
			biz.JobLaneReact, biz.JobLanePlan, biz.JobLaneSupervisor, biz.JobLaneBackground, biz.JobLaneFeishu,
		} {
			if _, exists := present[lane]; exists {
				continue
			}
			labels := map[string]string{"lane": lane}
			metrics.SetGauge("harness_jobs_pending", labels, 0)
			metrics.SetGauge("harness_jobs_running", labels, 0)
			metrics.SetGauge("harness_jobs_oldest_age_seconds", labels, 0)
		}
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
