// Package sandbox 调用独立受限 Sandbox Runner 执行高风险工具。
// 高风险工具禁止在应用进程内直接执行；客户端负责认证、超时、响应大小限制与错误分类。
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// 错误分类，供上层决定失败回填或人工对账。
var (
	// ErrInvalidConfig 配置缺失或不安全。
	ErrInvalidConfig = fmt.Errorf("sandbox configuration is invalid")
	// ErrSandboxUnavailable Sandbox Runner 不可达或返回非法响应。
	ErrSandboxUnavailable = fmt.Errorf("sandbox runner is unavailable")
	// ErrResponseTooLarge Sandbox 响应超过允许大小。
	ErrResponseTooLarge = fmt.Errorf("sandbox response exceeds the size limit")
	// ErrToolOutcomeUnknown 表示 Runner 可能已经执行工具，但客户端没有拿到结果。
	ErrToolOutcomeUnknown = fmt.Errorf("sandbox tool outcome unknown")
)

// MCPTarget identifies a user-scoped MCP tool independently of its display alias.
type MCPTarget struct {
	UserID              uuid.UUID `json:"user_id"`
	ServerID            uuid.UUID `json:"server_id"`
	OriginalName        string    `json:"original_name"`
	EndpointFingerprint string    `json:"endpoint_fingerprint"`
}

// Request 是一次 Sandbox 工具执行请求，通过工具名或 MCP 目标定位。
type Request struct {
	// Tool 工具名称。
	Tool string `json:"tool,omitempty"`
	// MCP requests use the identity-aware endpoint.
	MCP *MCPTarget `json:"mcp,omitempty"`
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
	// 复用默认 Transport 的连接池参数，但显式禁用环境代理与重定向：
	// 环境代理会把携带 Bearer 的内部请求转发到不可信出口，重定向则可能把令牌带到其他主机。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// 拨号期解析并校验目标地址，防止域名被解析到云元数据或链路本地地址后泄露令牌。
	transport.DialContext = safeDialContext
	return &Client{
		endpoint: endpoint, token: config.Token, timeout: config.Timeout,
		maxResponseBytes: config.MaxResponseBytes,
		httpClient: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
			// 不跟随 3xx：返回原始响应，由状态码校验拒绝。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// safeDialContext 解析目标主机并拒绝元数据、链路本地等不应访问的地址。
// 内网 Runner 与回环地址仍允许，保持现有私有网络部署可用。
func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, resolved := range addresses {
		if blockedSandboxAddress(resolved.IP) {
			return nil, fmt.Errorf("%w: sandbox endpoint resolves to disallowed address %s", ErrInvalidConfig, resolved.IP)
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: sandbox endpoint resolved no addresses", ErrInvalidConfig)
	}
	return nil, lastErr
}

// blockedSandboxAddress 判断地址是否属于云元数据、链路本地、组播或未指定地址。
// 私网与回环地址允许，以支持内网/本地部署的 Sandbox Runner。
func blockedSandboxAddress(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// 常见云厂商元数据地址（阿里云 100.100.100.200 不在链路本地网段内）。
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	return ip.Equal(net.IPv4bcast)
}

// Execute 调用 Sandbox Runner 并解析结果；响应体超过上限时拒绝解析。
func (c *Client) Execute(ctx context.Context, request Request) (Response, error) {
	if c == nil {
		return Response{}, fmt.Errorf("%w: client is nil", ErrInvalidConfig)
	}
	if !json.Valid(request.Arguments) {
		return Response{}, fmt.Errorf("%w: valid arguments are required", ErrInvalidConfig)
	}
	path := "/execute"
	if target := request.MCP; target != nil {
		fingerprint, err := hex.DecodeString(target.EndpointFingerprint)
		if request.Tool != "" || target.UserID == uuid.Nil || target.ServerID == uuid.Nil ||
			strings.TrimSpace(target.OriginalName) == "" || err != nil || len(fingerprint) != sha256.Size {
			return Response{}, fmt.Errorf("%w: a complete MCP target is required", ErrInvalidConfig)
		}
		path = "/execute/mcp"
	} else if strings.TrimSpace(request.Tool) == "" {
		return Response{}, fmt.Errorf("%w: tool is required", ErrInvalidConfig)
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
	var connected atomic.Bool
	callContext = httptrace.WithClientTrace(callContext, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	payload, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request", ErrInvalidConfig)
	}
	httpRequest, err := http.NewRequestWithContext(callContext, http.MethodPost, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("%w: build request", ErrInvalidConfig)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		if connected.Load() {
			return Response{}, fmt.Errorf("%w: %w: %v", ErrToolOutcomeUnknown, ErrSandboxUnavailable, err)
		}
		return Response{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
			return Response{}, fmt.Errorf("%w: %w: status %d", ErrToolOutcomeUnknown, ErrSandboxUnavailable, response.StatusCode)
		}
		return Response{}, fmt.Errorf("%w: status %d", ErrSandboxUnavailable, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w: read response", ErrToolOutcomeUnknown, ErrSandboxUnavailable)
	}
	if int64(len(body)) > c.maxResponseBytes {
		return Response{}, fmt.Errorf("%w: %w", ErrToolOutcomeUnknown, ErrResponseTooLarge)
	}
	var decoded Response
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: %w: decode response", ErrToolOutcomeUnknown, ErrSandboxUnavailable)
	}
	if decoded.ErrorCode == "" && !json.Valid(decoded.Output) {
		return Response{}, fmt.Errorf("%w: %w: invalid output json", ErrToolOutcomeUnknown, ErrSandboxUnavailable)
	}
	return decoded, nil
}
