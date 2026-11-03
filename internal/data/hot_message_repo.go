package data

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// hotMessageKey 生成包含用户与会话维度的 Redis Key，防止跨用户复用上下文。
func hotMessageKey(userID, sessionID uuid.UUID) string {
	return fmt.Sprintf("session:%s:%s:messages", userID, sessionID)
}

// hotMessage 是热缓存中的消息 DTO；只保存上下文回放所需字段。
type hotMessage struct {
	// ID 消息 ID。
	ID uuid.UUID `json:"id"`
	// Role 消息角色。
	Role biz.MessageRole `json:"role"`
	// Content 消息正文。
	Content string `json:"content"`
	// ToolCalls 工具调用元数据。
	ToolCalls []map[string]any `json:"tool_calls,omitempty"`
	// ToolCallID 工具结果对应的调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName 工具名称。
	ToolName string `json:"tool_name,omitempty"`
	// TokenCount Token 估算。
	TokenCount int `json:"token_count,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// appendHotMessageScript 幂等追加消息：已存在相同载荷时跳过，随后裁剪长度并刷新 TTL。
var appendHotMessageScript = redis.NewScript(`
local key = KEYS[1]
local payload = ARGV[1]
local max = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
if redis.call('LPOS', key, payload) then
  return 0
end
redis.call('RPUSH', key, payload)
redis.call('LTRIM', key, -max, -1)
redis.call('EXPIRE', key, ttl)
return 1
`)

// HotMessageRepository 是会话热消息的 Redis 实现，使用列表保存时间正序消息。
type HotMessageRepository struct {
	client *redis.Client
	// ttl 缓存过期时间，避免长期占用内存。
	ttl time.Duration
	// maxMessages 单个会话保留的最大消息数。
	maxMessages int
}

// NewHotMessageStore 构造热消息缓存；maxMessages 与 ttl 非正时使用安全默认值。
func NewHotMessageStore(client *redis.Client, maxMessages int, ttl time.Duration) (*HotMessageRepository, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: redis client is required", biz.ErrInvalidConfig)
	}
	if maxMessages <= 0 {
		maxMessages = 100
	}
	if maxMessages > 500 {
		maxMessages = 500
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &HotMessageRepository{client: client, ttl: ttl, maxMessages: maxMessages}, nil
}

// Load 按时间正序读取热消息；未命中返回 false，损坏数据会清理缓存并返回错误。
func (r *HotMessageRepository) Load(ctx context.Context, userID, sessionID uuid.UUID) ([]biz.Message, bool, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return nil, false, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	key := hotMessageKey(userID, sessionID)
	payloads, err := r.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, false, fmt.Errorf("load hot messages: %w", err)
	}
	// 空列表不视为命中，避免把缓存缺失伪装成“会话没有消息”。
	if len(payloads) == 0 {
		return nil, false, nil
	}
	messages := make([]biz.Message, 0, len(payloads))
	for _, payload := range payloads {
		var record hotMessage
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			_ = r.Invalidate(ctx, userID, sessionID)
			return nil, false, fmt.Errorf("decode hot message: %w", err)
		}
		messages = append(messages, biz.Message{
			ID: record.ID, SessionID: sessionID, UserID: userID, Role: record.Role, Content: record.Content,
			ToolCalls: record.ToolCalls, ToolCallID: record.ToolCallID, ToolName: record.ToolName,
			TokenCount: record.TokenCount, CreatedAt: record.CreatedAt,
		})
	}
	return messages, true, nil
}

// Save 用完整消息列表重建缓存；空列表直接清理缓存，禁止缓存空值。
func (r *HotMessageRepository) Save(ctx context.Context, userID, sessionID uuid.UUID, messages []biz.Message) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	key := hotMessageKey(userID, sessionID)
	if len(messages) == 0 {
		return r.Invalidate(ctx, userID, sessionID)
	}
	payloads := make([]any, 0, len(messages))
	for _, message := range messages {
		encoded, err := json.Marshal(messageToHot(message, sessionID, userID))
		if err != nil {
			return fmt.Errorf("encode hot message: %w", err)
		}
		payloads = append(payloads, encoded)
	}
	// 使用事务管线保证重建过程不会暴露半成品缓存。
	pipe := r.client.TxPipeline()
	pipe.Del(ctx, key)
	pipe.RPush(ctx, key, payloads...)
	pipe.LTrim(ctx, key, int64(-r.maxMessages), -1)
	pipe.Expire(ctx, key, r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("save hot messages: %w", err)
	}
	return nil
}

// Append 幂等追加单条消息并裁剪长度、刷新 TTL。
func (r *HotMessageRepository) Append(ctx context.Context, userID, sessionID uuid.UUID, message biz.Message) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	encoded, err := json.Marshal(messageToHot(message, sessionID, userID))
	if err != nil {
		return fmt.Errorf("encode hot message: %w", err)
	}
	_, err = appendHotMessageScript.Run(ctx, r.client,
		[]string{hotMessageKey(userID, sessionID)}, encoded, r.maxMessages, int64(r.ttl.Seconds())).Result()
	if err != nil {
		return fmt.Errorf("append hot message: %w", err)
	}
	return nil
}

// Invalidate 删除会话热缓存。
func (r *HotMessageRepository) Invalidate(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	if err := r.client.Del(ctx, hotMessageKey(userID, sessionID)).Err(); err != nil {
		return fmt.Errorf("invalidate hot messages: %w", err)
	}
	return nil
}

// messageToHot 把领域消息转换为缓存 DTO。
func messageToHot(message biz.Message, sessionID, userID uuid.UUID) hotMessage {
	return hotMessage{
		ID: message.ID, Role: message.Role, Content: message.Content, ToolCalls: message.ToolCalls,
		ToolCallID: message.ToolCallID, ToolName: message.ToolName, TokenCount: message.TokenCount,
		CreatedAt: message.CreatedAt,
	}
}

// 编译期断言接口实现。
var _ biz.HotMessageStore = (*HotMessageRepository)(nil)
