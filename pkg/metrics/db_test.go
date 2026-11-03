package metrics_test

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/metrics"
)

// stubDriver 是测试用空驱动，仅用于构造 sql.DB 统计对象，不建立真实连接。
type stubDriver struct{}

// Open 返回一个不做任何事情的连接。
func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

// stubConn 是空连接实现。
type stubConn struct{}

// Prepare 不支持预处理语句。
func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }

// Close 关闭连接。
func (stubConn) Close() error { return nil }

// Begin 不支持事务。
func (stubConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }

var registerStubOnce sync.Once

// openStubDB 构造使用测试驱动的连接池。
func openStubDB(t *testing.T) *sql.DB {
	t.Helper()
	registerStubOnce.Do(func() { sql.Register("metrics-stub", stubDriver{}) })
	db, err := sql.Open("metrics-stub", "")
	if err != nil {
		t.Fatalf("open stub db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRegisterDBPoolExposesStatsAndIgnoresDuplicates 验证连接池指标暴露、重复注册被忽略且空参数安全。
func TestRegisterDBPoolExposesStatsAndIgnoresDuplicates(t *testing.T) {
	metrics.RegisterDBPool("", nil)
	db := openStubDB(t)
	db.SetMaxOpenConns(7)
	metrics.RegisterDBPool("app_test_pool", db)
	// 同名重复注册必须忽略，避免 Prometheus 重复描述符 panic 与错误覆盖。
	duplicate := openStubDB(t)
	duplicate.SetMaxOpenConns(3)
	metrics.RegisterDBPool("app_test_pool", duplicate)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, name := range []string{
		"harness_db_pool_max_open_connections", "harness_db_pool_open_connections",
		"harness_db_pool_in_use_connections", "harness_db_pool_idle_connections",
		"harness_db_pool_wait_total", "harness_db_pool_wait_seconds_total",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("metrics output missing %s", name)
		}
	}
	if !strings.Contains(body, `pool="app_test_pool"`) {
		t.Fatalf("metrics output missing pool label: %s", body)
	}
	// 第一个注册的连接池上限为 7，重复注册的 3 不得覆盖。
	if !strings.Contains(body, "harness_db_pool_max_open_connections{pool=\"app_test_pool\"} 7") {
		t.Fatalf("duplicate registration overrode pool stats: %s", body)
	}
}
