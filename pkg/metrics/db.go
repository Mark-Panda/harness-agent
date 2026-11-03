package metrics

import (
	"database/sql"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// 数据库连接池指标：用于识别连接池耗尽、慢查询堆积与故障转移后的恢复情况。
// 约束：pool 标签只允许 app/auth 等稳定枚举，禁止拼接 DSN 或租户信息。
var (
	dbPoolsMu sync.RWMutex
	// dbPools 保存已注册的连接池；同名重复注册直接忽略，避免测试或热重载触发重复采集。
	dbPools = make(map[string]*sql.DB)
	// dbPoolRegistered 表示全局采集器是否已注册；所有连接池共用一个 Collector。
	dbPoolRegistered bool
)

// RegisterDBPool 注册一个数据库连接池并暴露连接数与等待统计。
// 关键步骤：同一 pool 名称只登记一次，指标采集时实时读取 sql.DBStats，避免缓存陈旧值。
func RegisterDBPool(name string, db *sql.DB) {
	if name == "" || db == nil {
		return
	}
	dbPoolsMu.Lock()
	defer dbPoolsMu.Unlock()
	if _, exists := dbPools[name]; exists {
		return
	}
	dbPools[name] = db
	if !dbPoolRegistered {
		registry.MustRegister(dbPoolCollector{})
		dbPoolRegistered = true
	}
}

// dbPoolCollector 汇总所有已注册连接池的指标，避免每个连接池单独注册造成重复描述符。
type dbPoolCollector struct{}

// 连接池指标描述符；所有指标都携带低基数 pool 标签。
var (
	dbPoolMaxOpenDesc = newDBPoolDesc("max_open_connections", "Maximum number of open connections to the database.")
	dbPoolOpenDesc    = newDBPoolDesc("open_connections", "Number of established connections to the database.")
	dbPoolInUseDesc   = newDBPoolDesc("in_use_connections", "Number of connections currently in use.")
	dbPoolIdleDesc    = newDBPoolDesc("idle_connections", "Number of idle connections.")
	dbPoolWaitDesc    = newDBPoolDesc("wait_total", "Total number of waits for a connection.")
	dbPoolWaitSeconds = newDBPoolDesc("wait_seconds_total", "Total time spent waiting for a connection in seconds.")
	dbPoolIdleClosed  = newDBPoolDesc("max_idle_closed_total", "Total connections closed due to SetConnMaxIdleTime.")
	dbPoolLifeClosed  = newDBPoolDesc("max_lifetime_closed_total", "Total connections closed due to SetConnMaxLifetime.")
)

// newDBPoolDesc 构造连接池指标描述符；pool 是唯一可变标签。
func newDBPoolDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc("harness_db_pool_"+name, help, []string{"pool"}, nil)
}

// Describe 向 Prometheus 描述全部连接池指标。
func (dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		dbPoolMaxOpenDesc, dbPoolOpenDesc, dbPoolInUseDesc, dbPoolIdleDesc,
		dbPoolWaitDesc, dbPoolWaitSeconds, dbPoolIdleClosed, dbPoolLifeClosed,
	} {
		ch <- desc
	}
}

// Collect 输出所有连接池的实时统计；计数类指标使用 CounterValue 保持累计语义。
func (dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	dbPoolsMu.RLock()
	defer dbPoolsMu.RUnlock()
	for name, db := range dbPools {
		stats := db.Stats()
		ch <- prometheus.MustNewConstMetric(dbPoolMaxOpenDesc, prometheus.GaugeValue, float64(stats.MaxOpenConnections), name)
		ch <- prometheus.MustNewConstMetric(dbPoolOpenDesc, prometheus.GaugeValue, float64(stats.OpenConnections), name)
		ch <- prometheus.MustNewConstMetric(dbPoolInUseDesc, prometheus.GaugeValue, float64(stats.InUse), name)
		ch <- prometheus.MustNewConstMetric(dbPoolIdleDesc, prometheus.GaugeValue, float64(stats.Idle), name)
		ch <- prometheus.MustNewConstMetric(dbPoolWaitDesc, prometheus.CounterValue, float64(stats.WaitCount), name)
		ch <- prometheus.MustNewConstMetric(dbPoolWaitSeconds, prometheus.CounterValue, stats.WaitDuration.Seconds(), name)
		ch <- prometheus.MustNewConstMetric(dbPoolIdleClosed, prometheus.CounterValue, float64(stats.MaxIdleClosed), name)
		ch <- prometheus.MustNewConstMetric(dbPoolLifeClosed, prometheus.CounterValue, float64(stats.MaxLifetimeClosed), name)
	}
}
