package bootstrap

import (
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
)

// TestEffectiveExecutionPools 校验默认单池与按名称过滤。
func TestEffectiveExecutionPools(t *testing.T) {
	defaultPools := effectiveExecutionPools(config.WorkerConfig{Concurrency: 2})
	if len(defaultPools) != 1 || defaultPools[0].Name != "default" {
		t.Fatalf("unexpected default pools: %#v", defaultPools)
	}
	cfg := config.WorkerConfig{
		PoolFilter: "react",
		Pools: []config.WorkerPoolConfig{
			{Name: "react", Lanes: []string{"react"}},
			{Name: "plan", Lanes: []string{"plan"}},
		},
	}
	pools := effectiveExecutionPools(cfg)
	if len(pools) != 1 || pools[0].Name != "react" {
		t.Fatalf("unexpected filtered pools: %#v", pools)
	}
}

// TestEffectiveExecutionPoolsNormalizeLanes 校验 lane 大小写与空白会被归一化。
func TestEffectiveExecutionPoolsNormalizeLanes(t *testing.T) {
	cfg := config.WorkerConfig{
		Pools: []config.WorkerPoolConfig{{Name: "react", Lanes: []string{" REACT "}}},
	}
	pools := effectiveExecutionPools(cfg)
	if len(pools) != 1 || len(pools[0].Lanes) != 1 || pools[0].Lanes[0] != "react" {
		t.Fatalf("unexpected normalized lanes: %#v", pools)
	}
}

// TestEffectiveExecutionPoolsStar 校验 * 显式启动全部池。
func TestEffectiveExecutionPoolsStar(t *testing.T) {
	cfg := config.WorkerConfig{
		PoolFilter: "*",
		Pools: []config.WorkerPoolConfig{
			{Name: "react", Lanes: []string{"react"}},
			{Name: "plan", Lanes: []string{"plan"}},
		},
	}
	pools := effectiveExecutionPools(cfg)
	if len(pools) != 2 {
		t.Fatalf("unexpected pools: %#v", pools)
	}

	// 与配置校验相同：逗号/空白变体也识别为全部池，不能静默过滤成 0 个。
	cfg.PoolFilter = " *, "
	if pools := effectiveExecutionPools(cfg); len(pools) != 2 {
		t.Fatalf("unexpected pools for normalized star: %#v", pools)
	}
}

// TestWorkerComponentEnabled 校验组件过滤为空时启动全部。
func TestWorkerComponentEnabled(t *testing.T) {
	if !workerComponentEnabled(config.WorkerConfig{}, "execution") {
		t.Fatal("empty components should enable all")
	}
	cfg := config.WorkerConfig{Components: "execution,background"}
	if !workerComponentEnabled(cfg, "execution") || !workerComponentEnabled(cfg, "background") {
		t.Fatal("configured components should be enabled")
	}
	if workerComponentEnabled(cfg, "feishu") {
		t.Fatal("unconfigured component should be disabled")
	}
}
