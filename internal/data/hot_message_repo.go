package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// hotMessageKey 生成包含用户与会话维度的 Redis Key，防止跨用户复用上下文。
func hotMessageKey(userID, sessionID uuid.UUID) string {
	return fmt.Sprintf("session:%s:%s:messages", userID, sessionID)
}

// hotMessageMetadataKey 保存缓存版本与最新持久化消息 ID，独立于列表以覆盖缓存未命中期间的 Append。
func hotMessageMetadataKey(userID, sessionID uuid.UUID) string {
	return hotMessageKey(userID, sessionID) + ":metadata"
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
	// SpeakerOpenID 飞书群聊发言人 open_id，单聊为空。
	SpeakerOpenID string `json:"speaker_open_id,omitempty"`
	// RunID 产生该消息的 Run ID；用户消息为空。
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// loadHotMessageScript 原子读取列表和版本元数据，避免把不同时间点的列表与高水位拼在一起。
var loadHotMessageScript = redis.NewScript(`
local payloads = redis.call('LRANGE', KEYS[1], 0, -1)
if #payloads == 0 then
  return {}
end
local version = redis.call('HGET', KEYS[2], 'version') or ''
local newest = redis.call('HGET', KEYS[2], 'newest') or ''
return {version, newest, unpack(payloads)}
`)

// invalidateHotMessageScript 只清理仍为本次读取版本的残窗，避免误删读取后并发写入的新窗口或高水位。
var invalidateHotMessageScript = redis.NewScript(`
local version = redis.call('HGET', KEYS[2], 'version') or ''
local newest = redis.call('HGET', KEYS[2], 'newest') or ''
if version ~= ARGV[1] or newest ~= ARGV[2] then
  return 0
end
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`)

// saveHotMessageScript 只在 PG 快照覆盖当前缓存高水位时替换列表。
// 高水位由 Append 推进；未命中期间只写元数据，因此旧快照也不能抹掉并发新消息。
var saveHotMessageScript = redis.NewScript(`
local key = KEYS[1]
local metadata = KEYS[2]
local max = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local count = tonumber(ARGV[3])
local currentNewest = redis.call('HGET', metadata, 'newest')
local currentVersion = tonumber(redis.call('HGET', metadata, 'version')) or 0
if count == 0 then
  redis.call('DEL', key)
  if not currentNewest or currentNewest == '' then
    redis.call('DEL', metadata)
  end
  return 1
end
if currentNewest and currentNewest ~= '' then
  local covered = false
  for index = 1, count do
    if ARGV[3 + index] == currentNewest then
      covered = true
      break
    end
  end
  if not covered then
    return 0
  end
elseif redis.call('EXISTS', key) == 1 then
  return -1
end
local payloadStart = 4 + count
redis.call('DEL', key)
redis.call('RPUSH', key, unpack(ARGV, payloadStart, payloadStart + count - 1))
redis.call('LTRIM', key, -max, -1)
redis.call('EXPIRE', key, ttl)
if currentVersion < 0 then
  currentVersion = 0
end
redis.call('HSET', metadata, 'version', currentVersion + 1, 'newest', ARGV[3 + count])
redis.call('EXPIRE', metadata, ttl)
return 1
`)

// appendHotMessageScript 幂等追加消息并原子推进版本；缓存不存在时只推进高水位，
// 不用单条消息伪造完整窗口。缺少元数据的旧窗口拒绝追加，由 Load 清理并回源。
var appendHotMessageScript = redis.NewScript(`
local key = KEYS[1]
local metadata = KEYS[2]
local payload = ARGV[1]
local messageID = ARGV[2]
local max = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])
local version = tonumber(redis.call('HGET', metadata, 'version')) or 0
local newest = redis.call('HGET', metadata, 'newest')
if redis.call('EXISTS', key) == 0 then
  redis.call('HSET', metadata, 'version', version + 1, 'newest', messageID)
  redis.call('EXPIRE', metadata, ttl)
  return -1
end
if not newest or newest == '' or version <= 0 then
  return -2
end
if redis.call('LPOS', key, payload) then
  redis.call('EXPIRE', key, ttl)
  redis.call('EXPIRE', metadata, ttl)
  return 0
end
redis.call('RPUSH', key, payload)
redis.call('LTRIM', key, -max, -1)
redis.call('HSET', metadata, 'version', version + 1, 'newest', messageID)
redis.call('EXPIRE', key, ttl)
redis.call('EXPIRE', metadata, ttl)
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

// Load 按时间正序读取热消息；缺少版本、高水位不一致或消息残窗均回源 PostgreSQL。
func (r *HotMessageRepository) Load(ctx context.Context, userID, sessionID uuid.UUID) ([]biz.Message, bool, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return nil, false, fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	values, err := loadHotMessageScript.Run(ctx, r.client,
		[]string{hotMessageKey(userID, sessionID), hotMessageMetadataKey(userID, sessionID)}).StringSlice()
	if err != nil {
		return nil, false, fmt.Errorf("load hot messages: %w", err)
	}
	// 空列表不视为命中；独立高水位必须保留，用于拒绝尚未包含并发 Append 的旧快照。
	if len(values) == 0 {
		return nil, false, nil
	}
	loadedVersion, loadedNewest := "", ""
	if len(values) >= 2 {
		loadedVersion, loadedNewest = values[0], values[1]
	}
	invalidateLoaded := func() {
		_ = invalidateHotMessageScript.Run(ctx, r.client,
			[]string{hotMessageKey(userID, sessionID), hotMessageMetadataKey(userID, sessionID)},
			loadedVersion, loadedNewest).Err()
	}
	invalidWindow := func() ([]biz.Message, bool, error) {
		invalidateLoaded()
		return nil, false, nil
	}
	if len(values) < 3 {
		return invalidWindow()
	}
	version, versionErr := strconv.ParseInt(values[0], 10, 64)
	newest, newestErr := uuid.Parse(values[1])
	if versionErr != nil || version <= 0 || newestErr != nil || newest == uuid.Nil {
		return invalidWindow()
	}
	payloads := values[2:]
	messages := make([]biz.Message, 0, len(payloads))
	for _, payload := range payloads {
		var record hotMessage
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			invalidateLoaded()
			return nil, false, fmt.Errorf("decode hot message: %w", err)
		}
		messages = append(messages, biz.Message{
			ID: record.ID, SessionID: sessionID, UserID: userID, Role: record.Role, Content: record.Content,
			ToolCalls: record.ToolCalls, ToolCallID: record.ToolCallID, ToolName: record.ToolName,
			TokenCount: record.TokenCount, SpeakerOpenID: record.SpeakerOpenID, RunID: record.RunID,
			CreatedAt: record.CreatedAt,
		})
	}
	if err := validateHotMessageOrder(messages); err != nil || messages[len(messages)-1].ID != newest || !hasUserMessage(messages) {
		return invalidWindow()
	}
	return messages, true, nil
}

// validateHotMessageOrder 校验 PostgreSQL 使用的 created_at + id 正序，并拒绝空 ID 与重复消息。
func validateHotMessageOrder(messages []biz.Message) error {
	seen := make(map[uuid.UUID]struct{}, len(messages))
	for index, message := range messages {
		if message.ID == uuid.Nil {
			return fmt.Errorf("hot message id is missing")
		}
		if _, exists := seen[message.ID]; exists {
			return fmt.Errorf("hot message %s is duplicated", message.ID)
		}
		seen[message.ID] = struct{}{}
		if index == 0 {
			continue
		}
		previous := messages[index-1]
		if message.CreatedAt.Before(previous.CreatedAt) ||
			(message.CreatedAt.Equal(previous.CreatedAt) && message.ID.String() <= previous.ID.String()) {
			return fmt.Errorf("hot messages are out of order at %s", message.ID)
		}
	}
	return nil
}

// hasUserMessage 判断窗口内是否存在用户输入。
func hasUserMessage(messages []biz.Message) bool {
	for _, message := range messages {
		if message.Role == biz.MessageUser {
			return true
		}
	}
	return false
}

// Save 用 PG 快照重建缓存；快照未覆盖当前高水位时保留现有窗口，绝不回退并发 Append。
func (r *HotMessageRepository) Save(ctx context.Context, userID, sessionID uuid.UUID, messages []biz.Message) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	if err := validateHotMessageOrder(messages); err != nil {
		return fmt.Errorf("save hot messages: %w", err)
	}
	arguments := make([]any, 0, 3+2*len(messages))
	arguments = append(arguments, r.maxMessages, hotMessageTTLSeconds(r.ttl), len(messages))
	for _, message := range messages {
		arguments = append(arguments, message.ID.String())
	}
	for _, message := range messages {
		encoded, err := json.Marshal(messageToHot(message, sessionID, userID))
		if err != nil {
			return fmt.Errorf("encode hot message: %w", err)
		}
		arguments = append(arguments, encoded)
	}
	if err := saveHotMessageScript.Run(ctx, r.client,
		[]string{hotMessageKey(userID, sessionID), hotMessageMetadataKey(userID, sessionID)}, arguments...).Err(); err != nil {
		return fmt.Errorf("save hot messages: %w", err)
	}
	return nil
}

// Append 幂等追加单条消息、推进高水位并裁剪长度；缓存缺失时仅记录版本，等待 PG 快照重建。
func (r *HotMessageRepository) Append(ctx context.Context, userID, sessionID uuid.UUID, message biz.Message) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	if message.ID == uuid.Nil {
		return fmt.Errorf("%w: hot message id is required", biz.ErrInvalidConfig)
	}
	encoded, err := json.Marshal(messageToHot(message, sessionID, userID))
	if err != nil {
		return fmt.Errorf("encode hot message: %w", err)
	}
	_, err = appendHotMessageScript.Run(ctx, r.client,
		[]string{hotMessageKey(userID, sessionID), hotMessageMetadataKey(userID, sessionID)},
		encoded, message.ID.String(), r.maxMessages, hotMessageTTLSeconds(r.ttl)).Result()
	if err != nil {
		return fmt.Errorf("append hot message: %w", err)
	}
	return nil
}

func hotMessageTTLSeconds(ttl time.Duration) int64 {
	seconds := int64(ttl.Seconds())
	if seconds <= 0 {
		return 1
	}
	return seconds
}

// Invalidate 删除会话热缓存。
func (r *HotMessageRepository) Invalidate(ctx context.Context, userID, sessionID uuid.UUID) error {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return fmt.Errorf("%w: user and session ids are required", biz.ErrInvalidConfig)
	}
	if err := r.client.Del(ctx, hotMessageKey(userID, sessionID), hotMessageMetadataKey(userID, sessionID)).Err(); err != nil {
		return fmt.Errorf("invalidate hot messages: %w", err)
	}
	return nil
}

// messageToHot 把领域消息转换为缓存 DTO。
func messageToHot(message biz.Message, sessionID, userID uuid.UUID) hotMessage {
	return hotMessage{
		ID: message.ID, Role: message.Role, Content: message.Content, ToolCalls: message.ToolCalls,
		ToolCallID: message.ToolCallID, ToolName: message.ToolName, TokenCount: message.TokenCount,
		SpeakerOpenID: message.SpeakerOpenID, RunID: message.RunID, CreatedAt: message.CreatedAt,
	}
}

// 编译期断言接口实现。
var _ biz.HotMessageStore = (*HotMessageRepository)(nil)
