// Command retention 清理超过保留期的脱敏执行载荷与审计数据。
// 默认 dry-run，仅统计；必须显式传入 -apply 才会删除，且应由具备数据库保留期权限的受控任务执行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/config"
)

func main() {
	apply := flag.Bool("apply", false, "actually delete expired rows; default is dry-run")
	flag.Parse()
	if err := run(*apply); err != nil {
		slog.Error("retention failed", "error", err)
		os.Exit(1)
	}
}

// run 加载配置并执行一次保留期清理。
func run(apply bool) error {
	cfg, err := config.LoadEnvironment(os.Getenv("APP_ENV"))
	if err != nil {
		return err
	}
	if cfg.Data.Postgres.DSN == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	db, err := data.OpenPostgres(cfg.Data.Postgres.DSN, 2)
	if err != nil {
		return err
	}
	defer func() {
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	}()
	now := time.Now().UTC()
	executionCutoff := now.Add(-cfg.Retention.ExecutionRecords.Duration)
	auditCutoff := now.Add(-cfg.Retention.AuditLogs.Duration)
	feishuRetention := cfg.Retention.FeishuEvents.Duration
	if feishuRetention <= 0 {
		// 配置缺省时使用 7 天，限制飞书原始事件载荷的驻留时间。
		feishuRetention = 7 * 24 * time.Hour
	}
	feishuCutoff := now.Add(-feishuRetention)
	counts, err := data.PurgeExpired(context.Background(), db, executionCutoff, auditCutoff, feishuCutoff, apply)
	if err != nil {
		return err
	}
	slog.Info("retention completed",
		"apply", apply,
		"execution_cutoff", executionCutoff,
		"audit_cutoff", auditCutoff,
		"feishu_cutoff", feishuCutoff,
		"execution_payloads", counts.ExecutionPayloads,
		"audit_logs", counts.AuditLogs,
		"audit_checkpoints", counts.AuditCheckpoints,
		"feishu_events", counts.FeishuEvents,
	)
	return nil
}
