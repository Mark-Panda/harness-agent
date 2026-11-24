package biz

import (
	"context"

	"github.com/google/uuid"
)

// stubRunRepository 是 Run 仓储的最小测试替身，仅实现事件与 Attempt 写入。
type stubRunRepository struct {
	RunRepository
}

// AppendEventFenced 返回成功事件。
func (stubRunRepository) AppendEventFenced(_ context.Context, event RunEvent, _ RunFence) (RunEvent, error) {
	return event, nil
}

// SetRunAttempt 更新尝试序号；测试中无需持久化。
func (stubRunRepository) SetRunAttempt(_ context.Context, _, _ uuid.UUID, _ int, _ RunFence) error {
	return nil
}

// ListEvents 默认没有历史事件；需要事件的场景由其他替身覆盖。
func (stubRunRepository) ListEvents(_ context.Context, _, _ uuid.UUID, _ int64, _ int) ([]RunEvent, error) {
	return nil, nil
}

// stubSessionRepository 是会话消息幂等写入的最小测试替身。
type stubSessionRepository struct {
	SessionRepository
}

// AppendMessageIdempotent 返回输入消息。
func (stubSessionRepository) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent, _ RunFence) (Message, error) {
	return message, nil
}

// statefulSessionRepository 模拟数据库按消息 ID 幂等返回原记录，用于完成恢复测试。
type statefulSessionRepository struct {
	stubSessionRepository
	messages map[uuid.UUID]Message
}

// AppendMessageIdempotent 已存在时返回首次保存的内容，模拟 ON CONFLICT DO NOTHING。
func (r *statefulSessionRepository) AppendMessageIdempotent(_ context.Context, message Message, _ OutboxEvent, _ RunFence) (Message, error) {
	if r.messages == nil {
		r.messages = map[uuid.UUID]Message{}
	}
	if existing, ok := r.messages[message.ID]; ok {
		return existing, nil
	}
	r.messages[message.ID] = message
	return message, nil
}
