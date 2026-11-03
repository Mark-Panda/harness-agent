package data

import (
	"context"
	"fmt"
	"time"

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
}

// PurgeExpired 清理超过保留期的脱敏执行载荷、审计数据与飞书事件回执。
// 关键约束：apply=false 时只统计不删除；审计删除必须在同一事务内开启 audit.retention 特权开关。
// 只删除执行载荷、审计链路与短期重放回执，不删除 Run、步骤元数据、会话消息与用户业务事实。
func PurgeExpired(
	ctx context.Context, db *gorm.DB, executionCutoff, auditCutoff, feishuCutoff time.Time, apply bool,
) (RetentionCounts, error) {
	if db == nil {
		return RetentionCounts{}, fmt.Errorf("retention database is required")
	}
	if executionCutoff.IsZero() || auditCutoff.IsZero() || feishuCutoff.IsZero() {
		return RetentionCounts{}, fmt.Errorf("retention cutoffs are required")
	}
	var counts RetentionCounts
	if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
		if apply {
			result := tx.Exec("DELETE FROM execution_payloads WHERE created_at < ?", executionCutoff)
			if result.Error != nil {
				return result.Error
			}
			counts.ExecutionPayloads = result.RowsAffected
			return nil
		}
		return tx.Table("execution_payloads").Where("created_at < ?", executionCutoff).
			Count(&counts.ExecutionPayloads).Error
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge execution payloads: %w", err)
	}

	// 飞书事件回执保存原始重放载荷；超过短期保留期后整行删除，避免正文长期驻留。
	if err := WithScope(ctx, db, Scope{System: true}, func(tx *gorm.DB) error {
		if apply {
			result := tx.Exec("DELETE FROM feishu_event_receipts WHERE received_at < ?", feishuCutoff)
			if result.Error != nil {
				return result.Error
			}
			counts.FeishuEvents = result.RowsAffected
			return nil
		}
		return tx.Table("feishu_event_receipts").Where("received_at < ?", feishuCutoff).
			Count(&counts.FeishuEvents).Error
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge feishu event receipts: %w", err)
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
		result := tx.Exec("DELETE FROM audit_logs WHERE occurred_at < ?", auditCutoff)
		if result.Error != nil {
			return result.Error
		}
		counts.AuditLogs = result.RowsAffected
		result = tx.Exec("DELETE FROM audit_checkpoints WHERE created_at < ?", auditCutoff)
		if result.Error != nil {
			return result.Error
		}
		counts.AuditCheckpoints = result.RowsAffected
		return nil
	}); err != nil {
		return RetentionCounts{}, fmt.Errorf("purge audit data: %w", err)
	}
	return counts, nil
}
