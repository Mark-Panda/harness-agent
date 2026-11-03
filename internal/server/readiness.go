package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// readyState 表示实例是否接收流量；停机前先置 false 实现 readiness 摘流。
var readyState atomic.Bool

// dependencyProbeTimeout 是单次依赖探测上限，避免探针在依赖故障时挂起。
const dependencyProbeTimeout = 2 * time.Second

// DependencyProbe 描述一个外部依赖的健康检查。
// Critical 为 true 时故障会使实例不可就绪（如 PostgreSQL）；false 时只标记降级（如 Redis 缓存）。
type DependencyProbe struct {
	// Name 依赖名称，作为响应中的稳定键，禁止包含连接串等敏感信息。
	Name string
	// Critical 是否关键依赖；关键依赖不可用时实例必须退出流量。
	Critical bool
	// Check 执行探测；返回错误表示依赖不可用，错误内容不会暴露给调用方。
	Check func(context.Context) error
}

// dependencyProbeSet 是探针集合的不可变快照，供并发请求安全读取。
type dependencyProbeSet struct {
	probes []DependencyProbe
}

// dependencyProbes 保存当前探针集合；原子替换避免探针注册与探针请求竞争。
var dependencyProbes atomic.Pointer[dependencyProbeSet]

func init() {
	readyState.Store(true)
}

// SetReady 设置实例就绪状态；false 时 readiness 返回 503，流量应被负载均衡摘除。
func SetReady(ready bool) {
	readyState.Store(ready)
}

// Ready 返回当前实例是否就绪。
func Ready() bool {
	return readyState.Load()
}

// SetDependencyProbes 注册或清空依赖探针；空参数用于测试或运行期重置。
// 关键步骤：复制入参切片，避免调用方后续修改与探针读取发生数据竞争。
func SetDependencyProbes(probes ...DependencyProbe) {
	copied := make([]DependencyProbe, len(probes))
	copy(copied, probes)
	dependencyProbes.Store(&dependencyProbeSet{probes: copied})
}

// readinessHandler 返回就绪探针。
// 摘流期间返回 503；关键依赖故障返回 503；非关键依赖故障返回 200 + degraded，
// 避免 Redis 缓存故障直接摘除仍可服务核心链路的实例。
func readinessHandler(w http.ResponseWriter, r *http.Request) {
	if !Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	set := dependencyProbes.Load()
	statuses := make(map[string]string)
	overall := "ready"
	code := http.StatusOK
	if set != nil {
		for _, probe := range set.probes {
			if probe.Name == "" || probe.Check == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), dependencyProbeTimeout)
			err := probe.Check(ctx)
			cancel()
			if err != nil {
				statuses[probe.Name] = "unavailable"
				if probe.Critical {
					overall = "unavailable"
					code = http.StatusServiceUnavailable
					continue
				}
				if overall == "ready" {
					overall = "degraded"
				}
				continue
			}
			statuses[probe.Name] = "ok"
		}
	}
	body := map[string]any{"status": overall}
	if len(statuses) > 0 {
		body["dependencies"] = statuses
	}
	writeJSON(w, code, body)
}
