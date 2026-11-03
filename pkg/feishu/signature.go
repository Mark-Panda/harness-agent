package feishu

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrSignatureInvalid 表示签名缺失或不匹配。
var ErrSignatureInvalid = errors.New("feishu signature invalid")

// ErrTimestampOutOfWindow 表示请求时间戳超出允许的重放窗口。
var ErrTimestampOutOfWindow = errors.New("feishu timestamp out of replay window")

// EncryptionKey 是飞书事件加密密钥，ID 用于标记当前版本，Secret 为控制台配置的原始密钥。
type EncryptionKey struct {
	// ID 密钥版本标识，仅用于日志与轮换排查，不参与验签计算。
	ID string
	// Secret 飞书事件订阅配置的 Encrypt Key 原文。
	Secret string
}

// Request 是飞书回调请求中与验签相关的原始字段。
type Request struct {
	// Timestamp X-Lark-Request-Timestamp 请求头。
	Timestamp string
	// Nonce X-Lark-Request-Nonce 请求头。
	Nonce string
	// Signature X-Lark-Signature 请求头。
	Signature string
	// Body 未经修改的原始请求体字节。
	Body []byte
}

// VerifierConfig 是验签与解密的配置。
type VerifierConfig struct {
	// VerificationToken 飞书事件订阅的 Verification Token，用于明文 URL 校验。
	VerificationToken string
	// EncryptKeys 加密密钥列表；第一个为当前密钥，其余为轮换期历史密钥。
	EncryptKeys []EncryptionKey
	// Window 允许的时间戳偏差，零值使用默认 5 分钟。
	Window time.Duration
	// Now 当前时间函数，测试可注入固定时钟。
	Now func() time.Time
}

// Verifier 负责飞书回调的验签、时间戳窗口校验与事件解密。
type Verifier struct {
	token  string
	keys   []EncryptionKey
	window time.Duration
	now    func() time.Time
}

// NewVerifier 构造验签器；配置加密密钥时要求密钥非空且不重复。
func NewVerifier(config VerifierConfig) (*Verifier, error) {
	if config.Window <= 0 {
		config.Window = 5 * time.Minute
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	seen := make(map[string]struct{}, len(config.EncryptKeys))
	for index, key := range config.EncryptKeys {
		if strings.TrimSpace(key.Secret) == "" {
			return nil, fmt.Errorf("feishu encrypt key %d is empty", index)
		}
		if _, exists := seen[key.Secret]; exists {
			return nil, fmt.Errorf("feishu encrypt key %d is duplicated", index)
		}
		seen[key.Secret] = struct{}{}
	}
	return &Verifier{token: config.VerificationToken, keys: config.EncryptKeys, window: config.Window, now: config.Now}, nil
}

// Verify 校验请求时间戳窗口与签名。
// 关键步骤：签名使用 sha256(timestamp + nonce + encryptKey + rawBody)，并使用常量时间比较防止时序侧信道。
func (v *Verifier) Verify(request Request) error {
	if len(v.keys) == 0 {
		// 未配置加密密钥时飞书不发送签名头，此时仅依赖 HTTPS 与后续 Verification Token 校验。
		return nil
	}
	// 飞书时间戳头是 Unix 秒级整数，必须按事件发生时间判断重放窗口。
	timestamp, err := strconv.ParseInt(strings.TrimSpace(request.Timestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed timestamp", ErrSignatureInvalid)
	}
	occurredAt := time.Unix(timestamp, 0).UTC()
	now := v.now().UTC()
	if occurredAt.Before(now.Add(-v.window)) || occurredAt.After(now.Add(v.window)) {
		return ErrTimestampOutOfWindow
	}
	if request.Nonce == "" || request.Signature == "" {
		return fmt.Errorf("%w: missing nonce or signature", ErrSignatureInvalid)
	}
	for _, key := range v.keys {
		expected := Signature(request.Timestamp, request.Nonce, key.Secret, request.Body)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(request.Signature))) == 1 {
			return nil
		}
	}
	return ErrSignatureInvalid
}

// Signature 计算飞书回调签名，返回小写十六进制摘要。
func Signature(timestamp, nonce, encryptKey string, body []byte) string {
	digest := sha256.New()
	digest.Write([]byte(timestamp))
	digest.Write([]byte(nonce))
	digest.Write([]byte(encryptKey))
	digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil))
}

// VerifyToken 校验明文事件的 Verification Token，防止伪造 URL 验证请求。
// 未配置 Token 时返回 nil，便于本地未启用集成的测试环境。
func (v *Verifier) VerifyToken(token string) error {
	if v.token == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(v.token), []byte(token)) != 1 {
		return fmt.Errorf("%w: verification token mismatch", ErrSignatureInvalid)
	}
	return nil
}

// VerifyAndDecrypt 完成验签并返回明文事件 JSON。
// 未配置加密密钥时原样返回请求体；配置后要求请求体是 base64 密文信封。
func (v *Verifier) VerifyAndDecrypt(request Request) ([]byte, error) {
	if err := v.Verify(request); err != nil {
		return nil, err
	}
	if len(v.keys) == 0 {
		return request.Body, nil
	}
	envelope, err := DecodeEnvelope(request.Body)
	if err != nil {
		return nil, err
	}
	if envelope.Encrypt == "" {
		// 兼容飞书在密钥轮换期间发送的明文信封，但仍要求签名通过。
		return request.Body, nil
	}
	plaintext, _, err := v.Decrypt(envelope.Encrypt)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// Decrypt 依次使用当前密钥与历史密钥解密，支持控制台密钥轮换期间的平滑过渡。
func (v *Verifier) Decrypt(ciphertext string) ([]byte, string, error) {
	if len(v.keys) == 0 {
		return nil, "", errors.New("feishu decrypt requires at least one encrypt key")
	}
	var lastErr error
	for _, key := range v.keys {
		plaintext, err := DecryptPayload(key.Secret, ciphertext)
		if err == nil {
			return plaintext, key.ID, nil
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("decrypt feishu payload: %w", lastErr)
}
