//go:build integration

package data

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestConnectionPoolExhaustionTimeoutAndRecovery 验证连接池耗尽时请求按上下文超时失败并可自动恢复。
// 关键约束：池耗尽不得无限阻塞请求；连接释放后同一实例无需重启即可继续服务。
func TestConnectionPoolExhaustionTimeoutAndRecovery(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("integration database is not required")
	}
	db := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// 占用唯一连接，模拟慢查询/事务泄漏导致连接池耗尽。
	held, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var value int
	exhaustedContext, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err = db.WithContext(exhaustedContext).Raw("SELECT 1").Scan(&value).Error
	cancel()
	if err == nil || !errors.Is(MapError(err), ErrTimeout) {
		t.Fatalf("pool exhaustion must fail with timeout, got %v", err)
	}
	stats := sqlDB.Stats()
	if stats.WaitCount == 0 || stats.WaitDuration == 0 {
		t.Fatalf("pool wait stats not recorded: %+v", stats)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	// 释放连接后无需重建连接池即可恢复；超时应来自请求上下文而不是永久故障。
	recoveryContext, cancelRecovery := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRecovery()
	if err := db.WithContext(recoveryContext).Raw("SELECT 1").Scan(&value).Error; err != nil {
		t.Fatalf("pool did not recover after connection release: %v", err)
	}
}
