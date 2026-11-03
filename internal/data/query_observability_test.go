package data

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestWithPostgresTimeouts 验证 URL 补充超时且保留已有值，非 URL DSN 保持不变。
func TestWithPostgresTimeouts(t *testing.T) {
	dsn := withPostgresTimeouts("postgres://user:pass@localhost:5432/app?sslmode=disable")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("statement_timeout") != "30s" || query.Get("lock_timeout") != "10s" ||
		query.Get("idle_in_transaction_session_timeout") != "60s" {
		t.Fatalf("timeouts = %v", query)
	}
	custom := withPostgresTimeouts("postgres://localhost/app?statement_timeout=5s")
	parsed, _ = url.Parse(custom)
	if parsed.Query().Get("statement_timeout") != "5s" {
		t.Fatalf("custom timeout was overwritten: %s", custom)
	}
	if keyword := "host=localhost user=app"; withPostgresTimeouts(keyword) != keyword {
		t.Fatal("keyword DSN was modified")
	}
}

// TestQueryLoggerRedactsSQLAndReportsSlowQueries 验证慢查询日志只记录耗时与行数。
func TestQueryLoggerRedactsSQLAndReportsSlowQueries(t *testing.T) {
	var buffer bytes.Buffer
	logger := queryLogger{logger: slog.New(slog.NewJSONHandler(&buffer, nil)), slow: time.Millisecond}
	logger.Trace(context.Background(), time.Now().Add(-10*time.Millisecond), func() (string, int64) {
		return "SELECT secret FROM users", 3
	}, nil)
	if content := buffer.String(); !strings.Contains(content, "slow database query") || strings.Contains(content, "secret") {
		t.Fatalf("slow log = %q", content)
	}
	buffer.Reset()
	logger.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, gorm.ErrRecordNotFound)
	if buffer.Len() != 0 {
		t.Fatalf("record not found was logged: %q", buffer.String())
	}
	buffer.Reset()
	logger.Trace(context.Background(), time.Now(), func() (string, int64) { return "UPDATE secret", 0 }, errors.New("db down"))
	if content := buffer.String(); strings.Contains(content, "secret") || strings.Contains(content, "db down") {
		t.Fatalf("failed log leaked details: %q", content)
	}
}
