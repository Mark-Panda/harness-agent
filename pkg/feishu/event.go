package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 事件类型常量，仅处理消息接收、撤回与编辑等与对话相关的事件。
const (
	// EventTypeURLVerification URL 验证事件。
	EventTypeURLVerification = "url_verification"
	// EventTypeMessageReceive 接收消息事件。
	EventTypeMessageReceive = "im.message.receive_v1"
	// EventTypeMessageRecalled 消息撤回事件。
	EventTypeMessageRecalled = "im.message.recalled_v1"
	// EventTypeMessageUpdated 消息编辑事件。
	EventTypeMessageUpdated = "im.message.updated_v1"
)

// 会话类型常量。
const (
	// ChatTypeP2P 单聊。
	ChatTypeP2P = "p2p"
	// ChatTypeGroup 群聊。
	ChatTypeGroup = "group"
)

// Header 是飞书事件信封头，包含幂等键、类型与租户信息。
type Header struct {
	// EventID 事件唯一 ID，用于幂等入库。
	EventID string `json:"event_id"`
	// EventType 事件类型。
	EventType string `json:"event_type"`
	// CreateTime 事件创建时间字符串。
	CreateTime string `json:"create_time"`
	// Token 明文 URL 验证 Token。
	Token string `json:"token"`
	// AppID 事件所属应用。
	AppID string `json:"app_id"`
	// TenantKey 事件所属租户。
	TenantKey string `json:"tenant_key"`
}

// Envelope 是飞书回调的信信封，可能是明文、加密或 URL 验证请求。
type Envelope struct {
	// Encrypt 加密密文，明文事件为空。
	Encrypt string `json:"encrypt"`
	// Schema 事件协议版本。
	Schema string `json:"schema"`
	// Challenge URL 验证挑战串。
	Challenge string `json:"challenge"`
	// Type 明文 URL 验证类型。
	Type string `json:"type"`
	// Token 明文 URL 验证 Token。
	Token string `json:"token"`
	// Header 事件头。
	Header Header `json:"header"`
	// Event 原始事件载荷，保留 JSON 以便按类型解析。
	Event json.RawMessage `json:"event"`
}

// DecodeEnvelope 解析飞书回调信封；空请求体或非法 JSON 返回错误。
func DecodeEnvelope(body []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode feishu envelope: %w", err)
	}
	return envelope, nil
}

// IsURLVerification 判断是否为明文 URL 验证请求。
func (e Envelope) IsURLVerification() bool {
	return strings.TrimSpace(e.Challenge) != "" || e.Type == EventTypeURLVerification
}

// MessageEvent 是 im.message.receive_v1 的标准化领域事件。
type MessageEvent struct {
	// SenderOpenID 发送者 open_id，用于身份映射。
	SenderOpenID string
	// SenderUserID 发送者租户内 user_id，仅作辅助，不得作为唯一身份依据。
	SenderUserID string
	// SenderType 发送者类型：user/app。
	SenderType string
	// MessageID 消息 ID，用于幂等与回复定位。
	MessageID string
	// RootID 话题根消息 ID，无话题时为空。
	RootID string
	// ParentID 父消息 ID，回复消息时有值。
	ParentID string
	// ThreadID 话题 ID，新版群聊话题消息有值。
	ThreadID string
	// ChatID 会话 ID，单聊与群聊均存在。
	ChatID string
	// ChatType 会话类型：p2p/group。
	ChatType string
	// MessageType 消息类型：text/post/image 等。
	MessageType string
	// Content 原始消息内容 JSON 字符串。
	Content string
	// Mentions 被 @ 的用户列表。
	Mentions []Mention
	// CreateTime 消息创建时间字符串。
	CreateTime string
}

// Mention 是被 @ 的用户信息。
type Mention struct {
	// Key 消息正文中的占位符，如 @_user_1。
	Key string `json:"key"`
	// OpenID 被 @ 用户 open_id。
	OpenID string `json:"open_id"`
	// UserID 被 @ 用户租户内 user_id。
	UserID string `json:"user_id"`
	// Name 被 @ 用户显示名。
	Name string `json:"name"`
}

// messageEventPayload 是飞书消息事件原始结构。
type messageEventPayload struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
			UserID string `json:"user_id"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string    `json:"message_id"`
		RootID      string    `json:"root_id"`
		ParentID    string    `json:"parent_id"`
		ThreadID    string    `json:"thread_id"`
		ChatID      string    `json:"chat_id"`
		ChatType    string    `json:"chat_type"`
		MessageType string    `json:"message_type"`
		Content     string    `json:"content"`
		CreateTime  string    `json:"create_time"`
		Mentions    []Mention `json:"mentions"`
	} `json:"message"`
}

// ParseMessageEvent 解析消息事件原始载荷。
func ParseMessageEvent(raw json.RawMessage) (MessageEvent, error) {
	var payload messageEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return MessageEvent{}, fmt.Errorf("decode feishu message event: %w", err)
	}
	message := payload.Message
	if message.MessageID == "" || message.ChatID == "" {
		return MessageEvent{}, fmt.Errorf("feishu message event is missing message or chat id")
	}
	return MessageEvent{
		SenderOpenID: payload.Sender.SenderID.OpenID, SenderUserID: payload.Sender.SenderID.UserID,
		SenderType: payload.Sender.SenderType, MessageID: message.MessageID, RootID: message.RootID,
		ParentID: message.ParentID, ThreadID: message.ThreadID, ChatID: message.ChatID,
		ChatType: message.ChatType, MessageType: message.MessageType, Content: message.Content,
		Mentions: message.Mentions, CreateTime: message.CreateTime,
	}, nil
}

// mentionPlaceholder 匹配飞书文本中的 @ 占位符。
var mentionPlaceholder = regexp.MustCompile(`@_user_\d+`)

// Text 提取可用于 Agent 上下文的纯文本，并清理 @ 占位符。
// 图片、文件等非文本消息返回空串，由调用方决定是否忽略或提示。
func (e MessageEvent) Text() string {
	switch e.MessageType {
	case "text":
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(e.Content), &content); err == nil {
			return normalizeText(content.Text)
		}
	case "post":
		return extractPostText(e.Content)
	}
	return ""
}

// extractPostText 从富文本 post 中拼接文本节点，跳过图片与 @ 节点。
func extractPostText(content string) string {
	var post struct {
		Title   string `json:"title"`
		Content [][]struct {
			Tag  string `json:"tag"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &post); err != nil {
		return ""
	}
	var builder strings.Builder
	if strings.TrimSpace(post.Title) != "" {
		builder.WriteString(post.Title)
		builder.WriteString("\n")
	}
	for _, line := range post.Content {
		for _, node := range line {
			if node.Tag == "text" && node.Text != "" {
				builder.WriteString(node.Text)
			}
		}
		builder.WriteString("\n")
	}
	return normalizeText(builder.String())
}

// normalizeText 清理 @ 占位符并压缩首尾空白。
func normalizeText(text string) string {
	cleaned := mentionPlaceholder.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned)
}

// Triggered 判断消息是否 @ 了机器人；配置为空时按“存在任意 mention”判断。
func (e MessageEvent) Triggered(botOpenID, botName string) bool {
	if len(e.Mentions) == 0 {
		return false
	}
	for _, mention := range e.Mentions {
		if botOpenID != "" && mention.OpenID == botOpenID {
			return true
		}
		if botOpenID == "" && botName != "" && mention.Name == botName {
			return true
		}
		if botOpenID == "" && botName == "" {
			return true
		}
	}
	return false
}

// ThreadKey 返回话题维度键：优先 thread_id，其次 root_id，再 parent_id；都没有时返回空串。
// 调用方必须为群聊主时间线的根消息生成稳定别名，禁止回退为群级共享会话。
func (e MessageEvent) ThreadKey() string {
	for _, candidate := range []string{e.ThreadID, e.RootID, e.ParentID} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

// CreatedAt 解析飞书消息创建时间，作为消息的权威发生时间。
// 事件可能乱序到达，调用方必须使用该时间而不是本地接收时间，避免上下文与摘要顺序被投递顺序污染。
// 关键步骤：飞书 create_time 为 Unix 时间戳字符串，毫秒（13 位及以上）与秒（10 位）均需兼容；
// 缺失、非法或非正值时回退到 fallback（通常是服务端当前时间）。
func (e MessageEvent) CreatedAt(fallback time.Time) time.Time {
	raw := strings.TrimSpace(e.CreateTime)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if len(raw) >= 13 {
		return time.UnixMilli(parsed).UTC()
	}
	return time.Unix(parsed, 0).UTC()
}
