package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIError 是飞书开放平台返回的业务错误，携带可重试与 Token 失效语义。
type APIError struct {
	// StatusCode HTTP 状态码。
	StatusCode int
	// Code 飞书业务错误码。
	Code int
	// Message 脱敏后的错误消息。
	Message string
	// RetryAfter 飞书建议的重试等待时间。
	RetryAfter time.Duration
}

// Error 实现 error 接口，不包含请求体或凭证。
func (e *APIError) Error() string {
	return fmt.Sprintf("feishu api error: status=%d code=%d message=%s", e.StatusCode, e.Code, e.Message)
}

// IsTokenInvalid 判断错误是否属于 Tenant Access Token 失效，需要清空缓存并重新获取。
func IsTokenInvalid(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.Code {
	case 99991663, 99991661, 99991668, 99991664, 99991665:
		return true
	}
	return apiError.StatusCode == http.StatusUnauthorized
}

// IsRateLimited 判断错误是否属于飞书限流，调用方应遵守 RetryAfter 退避。
func IsRateLimited(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return false
	}
	return apiError.Code == 99991400 || apiError.Code == 99991401 || apiError.StatusCode == http.StatusTooManyRequests
}

// ClientConfig 是飞书 HTTP 客户端配置。
type ClientConfig struct {
	// BaseURL 开放平台地址，默认 https://open.feishu.cn。
	BaseURL string
	// AppID 应用 App ID。
	AppID string
	// AppSecret 应用 App Secret，仅保存在内存中。
	AppSecret string
	// HTTPClient 可选的 HTTP 客户端，测试可注入 fake Transport。
	HTTPClient *http.Client
	// Now 当前时间函数，测试可注入固定时钟。
	Now func() time.Time
}

// Client 是飞书开放平台最小权限客户端，只封装获取 Token、回复消息与更新卡片。
type Client struct {
	baseURL    string
	appID      string
	appSecret  string
	httpClient *http.Client
	now        func() time.Time
	mu         sync.Mutex
	token      string
	expiresAt  time.Time
}

// NewClient 构造飞书客户端；App ID 与 Secret 缺失时返回错误。
func NewClient(config ClientConfig) (*Client, error) {
	if strings.TrimSpace(config.AppID) == "" || strings.TrimSpace(config.AppSecret) == "" {
		return nil, errors.New("feishu app id and app secret are required")
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://open.feishu.cn"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"), appID: config.AppID, appSecret: config.AppSecret,
		httpClient: config.HTTPClient, now: config.Now,
	}, nil
}

// TenantAccessToken 获取并缓存 Tenant Access Token，提前 60 秒刷新以避免边界失效。
// 获取过程持锁串行化，避免多 Worker 并发刷新触发飞书限流。
func (c *Client) TenantAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiresAt) {
		return c.token, nil
	}
	payload := map[string]string{"app_id": c.appID, "app_secret": c.appSecret}
	var response struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := c.do(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", payload, &response, false); err != nil {
		return "", err
	}
	if response.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu token response is empty: code=%d", response.Code)
	}
	expire := time.Duration(response.Expire) * time.Second
	if expire <= 0 {
		expire = 2 * time.Hour
	}
	c.token = response.TenantAccessToken
	c.expiresAt = c.now().Add(expire - 60*time.Second)
	return c.token, nil
}

// InvalidateToken 清空本地 Token 缓存，用于飞书返回 Token 失效错误后强制重取。
func (c *Client) InvalidateToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.expiresAt = time.Time{}
}

// ThreadReply 是话题回复结果，包含消息 ID 与飞书实际返回的话题 ID。
type ThreadReply struct {
	// MessageID 机器人回复的消息 ID，用于后续卡片更新。
	MessageID string
	// ThreadID 飞书话题 ID；回复主时间线消息且创建话题后由飞书返回。
	ThreadID string
}

// ReplyCard 以可更新卡片回复指定消息，返回飞书消息 ID。
func (c *Client) ReplyCard(ctx context.Context, replyTo string, card any) (string, error) {
	result, err := c.reply(ctx, replyTo, "interactive", card, false)
	return result.MessageID, err
}

// ReplyCardInThread 在指定消息下创建或复用话题并回复卡片。
// 关键步骤：reply_in_thread=true 让飞书为群聊主时间线的根消息建立话题，
// 后续同话题追问与卡片更新都落在该话题内，避免回退为群级共享会话。
func (c *Client) ReplyCardInThread(ctx context.Context, replyTo string, card any) (ThreadReply, error) {
	return c.reply(ctx, replyTo, "interactive", card, true)
}

// ReplyText 以纯文本回复指定消息，作为卡片不可用时的降级方案。
func (c *Client) ReplyText(ctx context.Context, replyTo, text string) (string, error) {
	result, err := c.reply(ctx, replyTo, "text", map[string]string{"text": text}, false)
	return result.MessageID, err
}

// ReplyTextInThread 在话题内回复纯文本，用于卡片不可用且需要保持话题上下文时的降级。
func (c *Client) ReplyTextInThread(ctx context.Context, replyTo, text string) (ThreadReply, error) {
	return c.reply(ctx, replyTo, "text", map[string]string{"text": text}, true)
}

// UpdateCard 增量更新已发送的卡片内容。
func (c *Client) UpdateCard(ctx context.Context, messageID string, card any) error {
	content, err := encodeMessageContent(card)
	if err != nil {
		return err
	}
	var response apiResponse
	err = c.callWithTokenRetry(ctx, func() error {
		response = apiResponse{}
		if err := c.authenticated(ctx, http.MethodPatch, "/open-apis/im/v1/messages/"+url.PathEscape(messageID), map[string]string{"content": content}, &response); err != nil {
			return err
		}
		return checkResponse(response)
	})
	return err
}

// reply 调用飞书回复接口，保持消息所在话题上下文；inThread 为 true 时创建或复用话题。
func (c *Client) reply(ctx context.Context, replyTo, messageType string, content any, inThread bool) (ThreadReply, error) {
	encoded, err := encodeMessageContent(content)
	if err != nil {
		return ThreadReply{}, err
	}
	payload := map[string]string{"msg_type": messageType, "content": encoded}
	if inThread {
		// 飞书要求字符串形式的布尔值，显式传 "true" 才能让根消息成为话题起点。
		payload["reply_in_thread"] = "true"
	}
	var response struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
			ThreadID  string `json:"thread_id"`
		} `json:"data"`
	}
	err = c.callWithTokenRetry(ctx, func() error {
		response.Code, response.Msg = 0, ""
		response.Data.MessageID, response.Data.ThreadID = "", ""
		if err := c.authenticated(ctx, http.MethodPost, "/open-apis/im/v1/messages/"+url.PathEscape(replyTo)+"/reply", payload, &response); err != nil {
			return err
		}
		if response.Code != 0 {
			return &APIError{Code: response.Code, Message: safeMessage(response.Msg)}
		}
		return nil
	})
	if err != nil {
		return ThreadReply{}, err
	}
	if response.Data.MessageID == "" {
		return ThreadReply{}, errors.New("feishu reply response is missing message id")
	}
	return ThreadReply{MessageID: response.Data.MessageID, ThreadID: response.Data.ThreadID}, nil
}

// apiResponse 是飞书通用响应信封。
type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// checkResponse 把非零业务码转换为 APIError。
func checkResponse(response apiResponse) error {
	if response.Code != 0 {
		return &APIError{Code: response.Code, Message: safeMessage(response.Msg)}
	}
	return nil
}

// authenticated 发送带 Token 的请求，Token 获取失败时直接返回错误。
func (c *Client) authenticated(ctx context.Context, method, path string, payload, out any) error {
	token, err := c.TenantAccessToken(ctx)
	if err != nil {
		return err
	}
	return c.doWithToken(ctx, method, path, payload, out, token)
}

// callWithTokenRetry 在 HTTP 或业务码判定 Token 失效时清空缓存并重试一次。
// 关键步骤：飞书 Token 过期通常以 HTTP 200 + 业务码返回，因此业务码也必须触发重取。
func (c *Client) callWithTokenRetry(ctx context.Context, call func() error) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := call()
		if err == nil {
			return nil
		}
		if IsTokenInvalid(err) && attempt == 0 {
			c.InvalidateToken()
			continue
		}
		return err
	}
	return errors.New("feishu request failed after token refresh")
}

// do 发送未认证请求。
func (c *Client) do(ctx context.Context, method, path string, payload, out any, authenticated bool) error {
	if authenticated {
		return errors.New("use authenticated for token-authenticated requests")
	}
	return c.doWithToken(ctx, method, path, payload, out, "")
}

// doWithToken 执行 HTTP 请求并解析统一响应，错误中不保留上游原始响应体。
func (c *Client) doWithToken(ctx context.Context, method, path string, payload, out any, token string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode feishu request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create feishu request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call feishu api: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read feishu response: %w", err)
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return &APIError{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"))}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode feishu response: status=%d", response.StatusCode)
	}
	return nil
}

// parseRetryAfter 解析 Retry-After 头，无法解析时返回默认 1 秒。
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return time.Second
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil {
		if wait := time.Until(parsed); wait > 0 {
			return wait
		}
	}
	return time.Second
}

// encodeMessageContent 把卡片对象或文本编码为飞书要求的 JSON 字符串。
func encodeMessageContent(content any) (string, error) {
	if text, ok := content.(string); ok {
		return text, nil
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode feishu message content: %w", err)
	}
	return string(raw), nil
}

// safeMessage 截断上游错误消息，避免把可能包含敏感回显的原文写入日志或错误链。
func safeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}
