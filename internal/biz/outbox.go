package biz

import "github.com/google/uuid"

// OutboxEvent 是业务事务内写入的审计事件，由 Outbox Dispatcher 异步发布到审计链。
type OutboxEvent struct {
	// UserID 事件归属用户，系统事件使用操作者 ID。
	UserID uuid.UUID
	// EventType 事件类型，同时也是审计 action。
	EventType string
	// AggregateType 聚合类型，如 llm_provider、agent、session。
	AggregateType string
	// AggregateID 聚合标识，用于按资源追溯。
	AggregateID string
	// DedupeKey 去重键，保证同一业务事件只发布一次。
	DedupeKey string
	// Payload 审计元数据白名单载荷，不得包含明文密钥或正文。
	Payload map[string]any
}
