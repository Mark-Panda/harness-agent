package data

import (
	"context"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/audit"
	"gorm.io/gorm"
)

// RetentionCounts 是一次保留期清理的统计结果。
type RetentionCounts struct {
	// ExecutionPayloads 过期执行载荷数量。
	ExecutionPayloads int64
	// AuditLogs 过期审计日志数量。
	AuditLogs int64
	// AuditCheckpoints 过期审计检查点数量。
	AuditCheckpoints int64
	// FeishuEvents 过期飞书事件回执数量。
	FeishuEvents int64
	// IdempotencyKeys 过期幂等键数量。
	IdempotencyKeys int64
}

// retentionBatchSize 是单批删除的最大行数。
// 大表一次性删除会长时间持锁并阻塞写入，分批且每批独立事务可显著缩短锁持有时间。
const retentionBatchSize = 5000

// purgeExpiredInBatches 按 id 子查询分批删除过期行，每批使用独立事务提交。
// 表名与时间列只接受本文件内的常量，避免动态 SQL 注入。
func purgeExpiredInBatches(ctx context.Context, db *gorm.DB, table, timeColumn string, cutoff time.Time) (int64, error) {
	statement := fmt.Sprintf(
		"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? ORDER BY %s LIMIT ?)",
		table, table, timeColumn, timeColumn,
	)
	var deleted int64
	for {
		var affected int64
		if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
			result := tx.Exec(statement, cutoff, retentionBatchSize)
			if result.Error != nil {
				return result.Error
			}
			affected = result.RowsAffected
			return nil
		}); err != nil {
			return deleted, err
		}
		deleted += affected
		if affected < retentionBatchSize {
			return deleted, nil
		}
	}
}

// PurgeExpired 清理超过保留期的脱敏执行载荷、审计数据与飞书事件回执。
// 关键约束：apply=false 时只统计不删除；审计删除必须在同一事务内开启 audit.retention 特权开关。
// auditKey 用于为部分过期的审计分区写入签名边界检查点，保证清理后剩余链仍可验证。
// 只删除执行载荷、审计链路与短期重放回执，不删除 Run、步骤元数据、会话消息与用户业务事实。
func PurgeExpired(
	ctx context.Context, db *gorm.DB, auditKey []byte,
	executionCutoff, auditCutoff, feishuCutoff time.Time, apply bool,
) (RetentionCounts, error) {
	if db == nil {
		return RetentionCounts{}, fmt.Errorf("retention database is required")
	}
	if executionCutoff.IsZero() || auditCutoff.IsZero() || feishuCutoff.IsZero() {
		return RetentionCounts{}, fmt.Errorf("retention cutoffs are required")
	}
	recorder, err := audit.NewRecorder(db, auditKey, nil)
	if err != nil {
		return RetentionCounts{}, fmt.Errorf("purge audit data: %w", err)
	}
	var counts RetentionCounts
	if apply {
		deleted, err := purgeExpiredInBatches(ctx, db, "execution_payloads", "created_at", executionCutoff)
		if err != nil {
			return RetentionCounts{}, fmt.Errorf("purge execution payloads: %w", err)
		}
		counts.ExecutionPayloads = deleted
	} else if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("execution_payloads").Where("created_at < ?", executionCutoff).
			Count(&counts.ExecutionPayloads).Error
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge execution payloads: %w", err)
	}

	// 飞书事件回执保存原始重放载荷；超过短期保留期后整行删除，避免正文长期驻留。
	if apply {
		deleted, err := purgeExpiredInBatches(ctx, db, "feishu_event_receipts", "received_at", feishuCutoff)
		if err != nil {
			return RetentionCounts{}, fmt.Errorf("purge feishu event receipts: %w", err)
		}
		counts.FeishuEvents = deleted
	} else if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("feishu_event_receipts").Where("received_at < ?", feishuCutoff).
			Count(&counts.FeishuEvents).Error
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge feishu event receipts: %w", err)
	}

	// 幂等键保存已完成的响应信封；过期后必须清理，否则旧响应会被无限回放且表持续膨胀。
	now := time.Now().UTC()
	if apply {
		deleted, err := purgeExpiredInBatches(ctx, db, "idempotency_keys", "expires_at", now)
		if err != nil {
			return RetentionCounts{}, fmt.Errorf("purge idempotency keys: %w", err)
		}
		counts.IdempotencyKeys = deleted
	} else if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("idempotency_keys").Where("expires_at < ?", now).Count(&counts.IdempotencyKeys).Error
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge idempotency keys: %w", err)
	}

	// 审计表只授予管理员删除权限，并要求同一事务内显式开启保留期开关。
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT set_config('audit.retention', 'on', true)").Error; err != nil {
			return fmt.Errorf("enable audit retention scope: %w", err)
		}
		if !apply {
			if err := tx.Table("audit_logs").Where("occurred_at < ?", auditCutoff).Count(&counts.AuditLogs).Error; err != nil {
				return err
			}
			return tx.Table("audit_checkpoints").Where("created_at < ?", auditCutoff).Count(&counts.AuditCheckpoints).Error
		}
		logs, checkpoints, err := recorder.PurgeBefore(ctx, tx, auditCutoff)
		if err != nil {
			return err
		}
		counts.AuditLogs, counts.AuditCheckpoints = logs, checkpoints
		return nil
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge audit data: %w", err)
	}
	return counts, nil
}
