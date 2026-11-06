package biz

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// 领域通用错误，Service 层据此映射稳定业务错误码。
var (
	// ErrForbidden 身份或角色不足。
	ErrForbidden = errors.New("forbidden")
	// ErrResourceNotFound 资源不存在或不属于当前用户。
	ErrResourceNotFound = errors.New("resource not found")
	// ErrResourceConflict 状态冲突、版本冲突或并发写冲突。
	ErrResourceConflict = errors.New("resource conflict")
	// ErrInvalidConfig 入参或配置未通过领域校验。
	ErrInvalidConfig = errors.New("invalid configuration")
	// ErrSessionRunActive 同一会话已有 Run 正在执行或等待审批，本次执行需继续排队。
	ErrSessionRunActive = errors.New("session has an active run")
)

// RequireAdmin 要求调用者为已认证管理员，平台级配置仅管理员可写。
func RequireAdmin(principal Principal) error {
	if principal.UserID == uuid.Nil || principal.Role != RoleAdmin {
		return ErrForbidden
	}
	return nil
}

// mutationEvent 构造管理变更的审计 Outbox 事件，必须与业务写入同事务提交。
func mutationEvent(actorID uuid.UUID, action, aggregateType string, aggregateID uuid.UUID) OutboxEvent {
	return OutboxEvent{
		UserID: actorID, EventType: action, AggregateType: aggregateType, AggregateID: aggregateID.String(),
		DedupeKey: fmt.Sprintf("%s:%s:%s", action, aggregateID, uuid.NewString()),
		Payload:   map[string]any{"outcome": "success"},
	}
}

// invalid 包装领域校验失败，统一返回 ErrInvalidConfig 以便 HTTP 映射 400。
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}
