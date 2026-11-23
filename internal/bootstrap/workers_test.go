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
