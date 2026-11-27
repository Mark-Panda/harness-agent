package data

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// slowQueryThreshold 是慢查询日志阈值。
const slowQueryThreshold = 200 * time.Millisecond

// idempotentJobConflictConstraint 是任务幂等入队依赖的去重唯一索引。
const idempotentJobConflictConstraint = "jobs_dedupe_key"

// isIdempotentJobConflict 判断是否为幂等任务入队的去重键冲突。
// 各 Enqueue* 方法把该冲突视为幂等成功或按需重新激活，属于预期控制流，不应作为查询失败告警。
func isIdempotentJobConflict(err error) bool {
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return false
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.ConstraintName == idempotentJobConflictConstraint
}

// withPostgresTimeouts 为 PostgreSQL URL 补充语句、锁与空闲事务超时，已有值保持不变。
func withPostgresTimeouts(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return dsn
	}
	query := parsed.Query()
	if query.Get("statement_timeout") == "" {
		query.Set("statement_timeout", "30s")
	}
	if query.Get("lock_timeout") == "" {
		query.Set("lock_timeout", "10s")
	}
	if query.Get("idle_in_transaction_session_timeout") == "" {
		query.Set("idle_in_transaction_session_timeout", "60s")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// queryLogger 只记录慢查询与失败查询的耗时和行数，不记录 SQL 文本或参数，避免泄露敏感内容。
type queryLogger struct {
	logger *slog.Logger
	slow   time.Duration
}

// newQueryLogger 构造脱敏查询日志器。
func newQueryLogger() queryLogger {
	return queryLogger{logger: slog.Default(), slow: slowQueryThreshold}
}

// LogMode 返回自身；日志级别由查询是否慢/失败决定。
func (l queryLogger) LogMode(logger.LogLevel) logger.Interface { return l }

// Info 忽略常规信息日志。
func (queryLogger) Info(context.Context, string, ...interface{}) {}

// Warn 忽略框架警告，避免输出 SQL 原文。
func (queryLogger) Warn(context.Context, string, ...interface{}) {}

// Error 忽略框架错误文本，避免错误信息携带参数。
func (queryLogger) Error(context.Context, string, ...interface{}) {}

// Trace 记录慢查询与失败查询的耗时、行数与错误分类，不包含 SQL 与参数。
func (l queryLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)
	_, rows := fc()
	attributes := []any{"elapsed_ms", float64(elapsed.Microseconds()) / 1000, "rows", rows}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		if isIdempotentJobConflict(err) {
			// 幂等入队的去重冲突是预期控制流，降级为 Debug 避免周期性失败告警噪音。
			l.logger.DebugContext(ctx, "database query dedupe conflict", attributes...)
			return
		}
		l.logger.WarnContext(ctx, "database query failed", append(attributes, "error", fmt.Sprintf("%T", err))...)
		return
	}
	if elapsed >= l.slow {
		l.logger.WarnContext(ctx, "slow database query", attributes...)
	}
}
