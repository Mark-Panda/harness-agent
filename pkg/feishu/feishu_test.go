package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedNow 是测试使用的固定时钟，保证时间戳窗口判断确定。
var fixedNow = time.Unix(1_700_000_000, 0).UTC()

// signRequest 构造带签名回调请求，避免测试重复计算。
func signRequest(secret string, body []byte, at time.Time) Request {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	nonce := "nonce-1"
	return Request{Timestamp: timestamp, Nonce: nonce, Signature: Signature(timestamp, nonce, secret, body), Body: body}
}

// TestVerifierSignatureReplayWindowAndKeyRotation 覆盖验签、重放窗口与密钥轮换。
func TestVerifierSignatureReplayWindowAndKeyRotation(t *testing.T) {
	verifier, err := NewVerifier(VerifierConfig{
		EncryptKeys: []EncryptionKey{{ID: "current", Secret: "secret-current"}, {ID: "previous", Secret: "secret-previous"}},
		Window:      5 * time.Minute,
		Now:         func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	body := []byte(`{"schema":"2.0"}`)
	if err := verifier.Verify(signRequest("secret-current", body, fixedNow)); err != nil {
		t.Fatalf("current key verify: %v", err)
	}
	// 轮换期历史密钥仍可验签，避免开关切换瞬间丢事件。
	if err := verifier.Verify(signRequest("secret-previous", body, fixedNow)); err != nil {
		t.Fatalf("previous key verify: %v", err)
	}
	tampered := signRequest("secret-current", body, fixedNow)
	tampered.Body = []byte(`{"schema":"2.1"}`)
	if err := verifier.Verify(tampered); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("tampered body returned %v, want signature invalid", err)
	}
	if err := verifier.Verify(signRequest("secret-current", body, fixedNow.Add(-10*time.Minute))); !errors.Is(err, ErrTimestampOutOfWindow) {
		t.Fatalf("stale timestamp returned %v, want replay window error", err)
	}
	if err := verifier.Verify(signRequest("secret-current", body, fixedNow.Add(10*time.Minute))); !errors.Is(err, ErrTimestampOutOfWindow) {
		t.Fatalf("future timestamp returned %v, want replay window error", err)
	}
}

// TestVerifierDecryptAndToken 覆盖密文解密、密钥轮换解密与 URL 验证 Token。
func TestVerifierDecryptAndToken(t *testing.T) {
	verifier, err := NewVerifier(VerifierConfig{
		VerificationToken: "verify-token",
		EncryptKeys:       []EncryptionKey{{ID: "current", Secret: "secret-current"}, {ID: "previous", Secret: "secret-previous"}},
		Now:               func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	plaintext := []byte(`{"schema":"2.0","header":{"event_id":"evt-1"}}`)
	ciphertext, err := encryptPayloadForTest("secret-previous", string(plaintext))
	if err != nil {
		t.Fatalf("encrypt payload: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"encrypt": ciphertext})
	request := signRequest("secret-previous", body, fixedNow)
	decoded, err := verifier.VerifyAndDecrypt(request)
	if err != nil {
		t.Fatalf("verify and decrypt: %v", err)
	}
	if string(decoded) != string(plaintext) {
		t.Fatalf("decrypted payload = %s", decoded)
	}
	if err := verifier.VerifyToken("verify-token"); err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if err := verifier.VerifyToken("forged"); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("forged token returned %v", err)
	}
}

// TestDecodeEnvelopeAndParseMessageEvent 覆盖事件信封、文本提取、@ 触发与话题键。
func TestDecodeEnvelopeAndParseMessageEvent(t *testing.T) {
	raw := []byte(`{
		"schema":"2.0",
		"header":{"event_id":"evt-1","event_type":"im.message.receive_v1","tenant_key":"tenant","app_id":"app"},
		"event":{
			"sender":{"sender_id":{"open_id":"ou_user"},"sender_type":"user"},
			"message":{
				"message_id":"om_1","chat_id":"oc_1","chat_type":"group","message_type":"text",
				"content":"{\"text\":\"@_user_1 帮我总结\"}",
				"thread_id":"omt_1","create_time":"1700000000123",
				"mentions":[{"key":"@_user_1","open_id":"ou_bot","name":"机器人"}]
			}
		}
	}`)
	envelope, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Header.EventID != "evt-1" || envelope.Header.EventType != EventTypeMessageReceive || envelope.IsURLVerification() {
		t.Fatalf("unexpected envelope %+v", envelope.Header)
	}
	event, err := ParseMessageEvent(envelope.Event)
	if err != nil {
		t.Fatalf("parse message event: %v", err)
	}
	if event.Text() != "帮我总结" {
		t.Fatalf("text = %q", event.Text())
	}
	if !event.Triggered("ou_bot", "") || event.Triggered("ou_other", "") {
		t.Fatalf("triggered judgement is wrong")
	}
	if event.ThreadKey() != "omt_1" {
		t.Fatalf("thread key = %q", event.ThreadKey())
	}
	// 乱序保护：消息发生时间必须取飞书 create_time（毫秒时间戳），而非本地处理时间。
	fallback := time.Unix(1_800_000_000, 0).UTC()
	if occurredAt := event.CreatedAt(fallback); !occurredAt.Equal(time.UnixMilli(1_700_000_000_123).UTC()) {
		t.Fatalf("occurred at = %v", occurredAt)
	}
	// 缺失或非法 create_time 时回退到 fallback，秒级时间戳同样兼容。
	if occurredAt := (&MessageEvent{}).CreatedAt(fallback); !occurredAt.Equal(fallback) {
		t.Fatalf("missing create_time = %v", occurredAt)
	}
	if occurredAt := (&MessageEvent{CreateTime: "1700000000"}).CreatedAt(fallback); !occurredAt.Equal(time.Unix(1_700_000_000, 0).UTC()) {
		t.Fatalf("seconds create_time = %v", occurredAt)
	}
	if occurredAt := (&MessageEvent{CreateTime: "not-a-time"}).CreatedAt(fallback); !occurredAt.Equal(fallback) {
		t.Fatalf("invalid create_time = %v", occurredAt)
	}
	// 富文本必须拼接文本节点并忽略图片节点。
	post := MessageEvent{MessageType: "post", Content: `{"title":"标题","content":[[{"tag":"text","text":"正文"},{"tag":"img","image_key":"k"}]]}`}
	if text := post.Text(); !strings.Contains(text, "标题") || !strings.Contains(text, "正文") {
		t.Fatalf("post text = %q", text)
	}
	// URL 验证请求必须被识别，且缺少消息 ID 的事件应被拒绝。
	verification, _ := DecodeEnvelope([]byte(`{"type":"url_verification","challenge":"abc","token":"t"}`))
	if !verification.IsURLVerification() || verification.Challenge != "abc" {
		t.Fatalf("url verification not detected")
	}
	if _, err := ParseMessageEvent(json.RawMessage(`{"message":{"chat_id":"oc_1"}}`)); err == nil {
		t.Fatalf("missing message id should fail")
	}
}

// TestClientTokenCacheReplyAndInvalidation 覆盖 Token 缓存、回复卡片、文本降级与失效重取。
func TestClientTokenCacheReplyAndInvalidation(t *testing.T) {
	var mu sync.Mutex
	tokenCalls := 0
	failNextReply := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			tokenCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "token-" + strconv.Itoa(tokenCalls), "expire": 7200})
		case strings.HasSuffix(r.URL.Path, "/reply"):
			if failNextReply {
				failNextReply = false
				_, _ = w.Write([]byte(`{"code":99991663,"msg":"token invalid"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_reply"}}`))
		case r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, AppID: "app-id", AppSecret: "app-secret",
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	messageID, err := client.ReplyCard(context.Background(), "om_1", map[string]any{"header": "card"})
	if err != nil || messageID != "om_reply" {
		t.Fatalf("reply card = %q err=%v", messageID, err)
	}
	if _, err := client.ReplyText(context.Background(), "om_2", "plain"); err != nil {
		t.Fatalf("reply text: %v", err)
	}
	if err := client.UpdateCard(context.Background(), "om_reply", map[string]any{"header": "updated"}); err != nil {
		t.Fatalf("update card: %v", err)
	}
	// 两次回复复用同一个 Token，只有一次获取请求。
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls)
	}
	// Token 失效后清空缓存并重取，飞书请求自动重试一次。
	mu.Lock()
	failNextReply = true
	mu.Unlock()
	if _, err := client.ReplyCard(context.Background(), "om_3", map[string]any{"header": "card"}); err != nil {
		t.Fatalf("reply after token invalidation: %v", err)
	}
	if tokenCalls != 2 {
		t.Fatalf("token calls after invalidation = %d, want 2", tokenCalls)
	}
}

// TestClientReplyInThreadPayload 验证话题回复显式传递 reply_in_thread=true 并解析 thread_id。
func TestClientReplyInThreadPayload(t *testing.T) {
	var payloads []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal") {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"token","expire":7200}`))
			return
		}
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payloads = append(payloads, payload)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_reply","thread_id":"omt_1"}}`))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, AppID: "app-id", AppSecret: "app-secret"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	// 话题内回复必须显式传字符串 "true"，否则飞书只做普通回复而不会创建话题。
	threaded, err := client.ReplyCardInThread(context.Background(), "om_root", map[string]any{"header": "card"})
	if err != nil {
		t.Fatalf("reply card in thread: %v", err)
	}
	if threaded.MessageID != "om_reply" || threaded.ThreadID != "omt_1" {
		t.Fatalf("threaded reply = %#v", threaded)
	}
	if len(payloads) != 1 || payloads[0]["reply_in_thread"] != "true" {
		t.Fatalf("thread reply payload = %#v", payloads)
	}
	// 普通回复不得携带该字段，避免在单聊或主时间线误建话题。
	if _, err := client.ReplyCard(context.Background(), "om_plain", map[string]any{"header": "card"}); err != nil {
		t.Fatalf("plain reply: %v", err)
	}
	if len(payloads) != 2 {
		t.Fatalf("reply payloads = %#v", payloads)
	}
	if _, ok := payloads[1]["reply_in_thread"]; ok {
		t.Fatalf("plain reply carried reply_in_thread: %#v", payloads[1])
	}
}

// TestClientRateLimitClassification 覆盖限流响应到 APIError 的映射。
func TestClientRateLimitClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal") {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"token","expire":7200}`))
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, AppID: "app-id", AppSecret: "app-secret"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.ReplyCard(context.Background(), "om_1", map[string]any{})
	if err == nil || !IsRateLimited(err) {
		t.Fatalf("rate limit error = %v", err)
	}
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.RetryAfter != 2*time.Second {
		t.Fatalf("retry after not parsed: %+v", err)
	}
}
