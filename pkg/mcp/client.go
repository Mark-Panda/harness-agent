// Package mcp 实现最小可用的 MCP Streamable HTTP 客户端，供 ReAct 工具执行层调用。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 客户端错误分类。
var (
	// ErrInvalidEndpoint 端点不是安全的 HTTPS 地址。
	ErrInvalidEndpoint = errors.New("invalid MCP endpoint")
	// ErrRPC MCP 服务返回 JSON-RPC 错误。
	ErrRPC = errors.New("MCP JSON-RPC error")
	// ErrTransport 网络或响应解析失败。
	ErrTransport = errors.New("MCP transport error")
)

// maxResponseBytes 限制单次 MCP 响应大小，避免恶意服务耗尽内存。
const maxResponseBytes = 8 << 20

// AuthHeader 根据鉴权类型与凭证构造请求头，凭证永不写入日志。
func AuthHeader(authType, credential string) (string, string) {
	switch authType {
	case "bearer", "oauth":
		if credential == "" {
			return "", ""
		}
		return "Authorization", "Bearer " + credential
	case "header":
		parts := strings.SplitN(credential, ":", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		}
		if credential != "" {
			return "X-MCP-Key", credential
		}
	}
	return "", ""
}

// Client 调用 MCP Streamable HTTP 端点，支持 JSON 与单事件 SSE 响应。
type Client struct {
	// endpoint 已校验的 HTTPS 端点。
	endpoint string
	// authHeader 鉴权头名称。
	authHeader string
	// authValue 鉴权头值，仅在内存中存在。
	authValue string
	// http 禁用重定向的 HTTP 客户端。
	http *http.Client
	// mu 串行化初始化与请求编号，避免会话状态竞争。
	mu sync.Mutex
	// initialized 是否已完成 initialize 握手。
	initialized bool
	// sessionID 服务端返回的 MCP 会话 ID。
	sessionID string
	// nextID JSON-RPC 请求编号。
	nextID int64
}

// Option 用于注入测试 HTTP 客户端。
type Option func(*Client)

// WithHTTPClient 覆写 HTTP 客户端，测试可注入信任自签证书的 Transport。
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// NewClient 构造 MCP 客户端；端点必须是 HTTPS 且不包含用户信息。
func NewClient(endpoint, authHeader, authValue string, timeout time.Duration, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrInvalidEndpoint
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// MCP 不允许通过代理访问，且拨号阶段再次校验解析后的 IP，防御 DNS 重绑定与内网探测。
	transport.Proxy = nil
	transport.DialContext = safeDialContext(timeout)
	client := &Client{
		endpoint: parsed.String(), authHeader: authHeader, authValue: authValue,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// 禁止重定向，避免凭证被转发到未准入的地址。
				return http.ErrUseLastResponse
			},
		},
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// safeDialContext 在拨号前解析并校验目标 IP，拒绝回环、私网、链路本地与元数据地址。
// 通过直接拨号已校验的 IP，避免 DNS 重绑定把连接导向内网。
func safeDialContext(timeout time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid address", ErrInvalidEndpoint)
		}
		if ip := net.ParseIP(host); ip != nil {
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("%w: private or metadata address", ErrInvalidEndpoint)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("%w: resolve host", ErrTransport)
		}
		for _, resolved := range ips {
			if !isPublicIP(resolved.IP) {
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("%w: no public address available", ErrInvalidEndpoint)
	}
}

// isPublicIP 判断地址是否为公网可路由地址。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return true
}

// rpcError 是 JSON-RPC 错误对象。
type rpcError struct {
	// Code 错误码。
	Code int `json:"code"`
	// Message 错误信息，可能包含服务端细节。
	Message string `json:"message"`
}

// rpcResponse 是 JSON-RPC 响应。
type rpcResponse struct {
	// JSONRPC 协议版本。
	JSONRPC string `json:"jsonrpc"`
	// ID 请求编号。
	ID json.RawMessage `json:"id"`
	// Result 成功结果。
	Result json.RawMessage `json:"result"`
	// Error 失败信息。
	Error *rpcError `json:"error"`
}

// Tool 是 MCP 服务暴露的工具定义。
type Tool struct {
	// Name 原始工具名。
	Name string `json:"name"`
	// Description 工具用途。
	Description string `json:"description"`
	// InputSchema JSON Schema 参数定义。
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Content 是工具返回的单个内容块。
type Content struct {
	// Type 内容类型，如 text。
	Type string `json:"type"`
	// Text 文本内容。
	Text string `json:"text,omitempty"`
	// Data 二进制内容 Base64，未使用时为空。
	Data string `json:"data,omitempty"`
	// MimeType 内容类型。
	MimeType string `json:"mimeType,omitempty"`
}

// CallResult 是 tools/call 的返回结果。
type CallResult struct {
	// Content 内容块列表。
	Content []Content `json:"content"`
	// IsError 工具是否返回业务错误。
	IsError bool `json:"isError"`
	// StructuredContent 结构化输出。
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
}

// Initialize 执行 initialize 握手并发送 initialized 通知；重复调用幂等。
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	params := map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "harness-agent", "version": "1.0"},
	}
	response, err := c.callLocked(ctx, "initialize", params, true)
	if err != nil {
		return err
	}
	var result struct {
		// ProtocolVersion 服务端协议版本。
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return fmt.Errorf("%w: decode initialize result", ErrTransport)
	}
	// initialized 通知失败不影响后续工具调用，部分服务允许省略。
	_ = c.notifyLocked(ctx, "notifications/initialized", map[string]any{})
	c.initialized = true
	return nil
}

// ListTools 读取全部工具定义，按 cursor 分页最多 10 页。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	var tools []Tool
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		response, err := c.call(ctx, "tools/list", params, false)
		if err != nil {
			return nil, err
		}
		var result struct {
			// Tools 工具列表。
			Tools []Tool `json:"tools"`
			// NextCursor 下一页游标。
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return nil, fmt.Errorf("%w: decode tools/list result", ErrTransport)
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return tools, nil
}

// CallTool 调用指定工具；结果中的内容由执行层视为不可信数据。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	return c.CallToolWithMeta(ctx, name, arguments, nil)
}

// CallToolWithMeta 调用指定工具并通过 MCP _meta 传递幂等键等调用元数据。
// meta 只允许非敏感元数据，凭证仍只放在请求头中。
func (c *Client) CallToolWithMeta(ctx context.Context, name string, arguments json.RawMessage, meta map[string]any) (CallResult, error) {
	if err := c.Initialize(ctx); err != nil {
		return CallResult{}, err
	}
	params := map[string]any{"name": name, "arguments": json.RawMessage(`{}`)}
	if len(arguments) > 0 {
		params["arguments"] = arguments
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	response, err := c.call(ctx, "tools/call", params, false)
	if err != nil {
		return CallResult{}, err
	}
	var result CallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return CallResult{}, fmt.Errorf("%w: decode tools/call result", ErrTransport)
	}
	return result, nil
}

// notify 发送无编号 JSON-RPC 通知。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifyLocked(ctx, method, params)
}

// notifyLocked 在已持有锁时发送通知。
func (c *Client) notifyLocked(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	c.applyAuth(request)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: transport failure", ErrTransport)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("%w: notification status %d", ErrTransport, response.StatusCode)
	}
	return nil
}

// call 执行一次 JSON-RPC 请求并解析 JSON 或 SSE 响应；requireSession 为真时记录会话 ID。
func (c *Client) call(ctx context.Context, method string, params any, requireSession bool) (rpcResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callLocked(ctx, method, params, requireSession)
}

// callLocked 在已持有锁时执行 JSON-RPC 请求。
func (c *Client) callLocked(ctx context.Context, method string, params any, requireSession bool) (rpcResponse, error) {
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return rpcResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	c.applyAuth(request)
	if c.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("%w: transport failure", ErrTransport)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted && method == "notifications/initialized" {
		return rpcResponse{}, nil
	}
	if response.StatusCode != http.StatusOK {
		return rpcResponse{}, fmt.Errorf("%w: status %d", ErrTransport, response.StatusCode)
	}
	if requireSession {
		if session := response.Header.Get("Mcp-Session-Id"); session != "" && len(session) <= 256 {
			c.sessionID = session
		}
	}
	payload, err := readPayload(response)
	if err != nil {
		return rpcResponse{}, err
	}
	var decoded rpcResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return rpcResponse{}, fmt.Errorf("%w: decode response", ErrTransport)
	}
	if decoded.Error != nil {
		return rpcResponse{}, fmt.Errorf("%w: code %d: %s", ErrRPC, decoded.Error.Code, truncate(decoded.Error.Message, 256))
	}
	if len(decoded.Result) == 0 {
		return rpcResponse{}, fmt.Errorf("%w: empty result", ErrTransport)
	}
	return decoded, nil
}

// applyAuth 设置鉴权头；头名称与值均来自内存中的已解密凭证。
func (c *Client) applyAuth(request *http.Request) {
	if c.authHeader != "" && c.authValue != "" {
		request.Header.Set(c.authHeader, c.authValue)
	}
}

// readPayload 读取 JSON 或 SSE 单事件响应体，限制总大小。
func readPayload(response *http.Response) ([]byte, error) {
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 1024), maxResponseBytes)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			return []byte(payload), nil
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("%w: read SSE response", ErrTransport)
		}
		return nil, fmt.Errorf("%w: empty SSE response", ErrTransport)
	}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrTransport)
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response too large", ErrTransport)
	}
	return payload, nil
}

// truncate 截断错误信息，避免服务端超长内容进入日志。
func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

// ParseRetryAfter 解析 Retry-After 头，供上层统一退避；保留与模型适配器一致的语义。
func ParseRetryAfter(header string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 || seconds > 300 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
