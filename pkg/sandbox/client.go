// Package sandbox 调用独立受限 Sandbox Runner 执行高风险工具。
// 高风险工具禁止在应用进程内直接执行；客户端负责认证、超时、响应大小限制与错误分类。
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 错误分类，供上层决定失败回填或人工对账。
var (
	// ErrInvalidConfig 配置缺失或不安全。
	ErrInvalidConfig = fmt.Errorf("sandbox configuration is invalid")
	// ErrSandboxUnavailable Sandbox Runner 不可达或返回非法响应。
	ErrSandboxUnavailable = fmt.Errorf("sandbox runner is unavailable")
	// ErrResponseTooLarge Sandbox 响应超过允许大小。
	ErrResponseTooLarge = fmt.Errorf("sandbox response exceeds the size limit")
)

// Request 是一次 Sandbox 工具执行请求，只包含工具名、参数、风险等级与超时。
type Request struct {
	// Tool 工具名称。
	Tool string `json:"tool"`
	// Arguments 工具参数 JSON。
	Arguments json.RawMessage `json:"arguments"`
	// RiskLevel 工具风险等级。
	RiskLevel string `json:"risk_level"`
	// TimeoutMS 单次执行超时毫秒。
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// Response 是 Sandbox Runner 的执行结果。
type Response struct {
	// Output 工具输出 JSON。
	Output json.RawMessage `json:"output"`
	// ErrorCode 业务错误码；为空表示成功。
	ErrorCode string `json:"error_code,omitempty"`
	// Message 脱敏错误摘要。
	Message string `json:"message,omitempty"`
}

// Runner 是高风险工具执行端口。
type Runner interface {
	// Execute 在独立 Sandbox 中执行一次工具调用。
	Execute(ctx context.Context, request Request) (Response, error)
}

// Config 是 Sandbox 客户端配置。
type Config struct {
	// Endpoint Sandbox Runner HTTP 端点。
	Endpoint string
	// Token 出口认证令牌，必须以 Bearer 方式发送。
	Token string
	// AllowInsecure 是否允许明文 HTTP，仅限内网调试。
	AllowInsecure bool
	// Timeout 单次调用超时。
	Timeout time.Duration
	// MaxResponseBytes 响应最大字节数。
	MaxResponseBytes int64
}

// Client 是 Sandbox Runner 的 HTTP 客户端。
type Client struct {
	endpoint         string
	token            string
	timeout          time.Duration
	maxResponseBytes int64
	httpClient       *http.Client
}

// NewClient 构造 Sandbox 客户端并校验端点安全性与认证配置。
func NewClient(config Config) (*Client, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(config.Endpoint), "/")
	if endpoint == "" || strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("%w: endpoint and token are required", ErrInvalidConfig)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint must be a valid http(s) URL", ErrInvalidConfig)
	}
	if parsed.Scheme != "https" && !config.AllowInsecure {
		return nil, fmt.Errorf("%w: https is required unless allow_insecure is enabled", ErrInvalidConfig)
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.MaxResponseBytes <= 0 {
		config.MaxResponseBytes = 1024 * 1024
	}
	return &Client{
		endpoint: endpoint, token: config.Token, timeout: config.Timeout,
		maxResponseBytes: config.MaxResponseBytes,
		httpClient:       &http.Client{Timeout: config.Timeout},
	}, nil
}

// Execute 调用 Sandbox Runner 并解析结果；响应体超过上限时拒绝解析。
func (c *Client) Execute(ctx context.Context, request Request) (Response, error) {
	if c == nil {
		return Response{}, fmt.Errorf("%w: client is nil", ErrInvalidConfig)
	}
	if strings.TrimSpace(request.Tool) == "" || !json.Valid(request.Arguments) {
		return Response{}, fmt.Errorf("%w: tool and valid arguments are required", ErrInvalidConfig)
	}
	timeout := c.timeout
	if request.TimeoutMS > 0 {
		requested := time.Duration(request.TimeoutMS) * time.Millisecond
		if requested > 0 && requested < timeout {
			timeout = requested
		}
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request", ErrInvalidConfig)
	}
	httpRequest, err := http.NewRequestWithContext(callContext, http.MethodPost, c.endpoint+"/execute", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("%w: build request", ErrInvalidConfig)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("%w: read response", ErrSandboxUnavailable)
	}
	if int64(len(body)) > c.maxResponseBytes {
		return Response{}, ErrResponseTooLarge
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, fmt.Errorf("%w: status %d", ErrSandboxUnavailable, response.StatusCode)
	}
	var decoded Response
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: decode response", ErrSandboxUnavailable)
	}
	if decoded.ErrorCode == "" && !json.Valid(decoded.Output) {
		return Response{}, fmt.Errorf("%w: invalid output json", ErrSandboxUnavailable)
	}
	return decoded, nil
}
