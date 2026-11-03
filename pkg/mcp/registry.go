package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// ErrEmptyRegistry 表示没有可执行的低风险 MCP 工具。
var ErrEmptyRegistry = errors.New("no executable MCP tools")

// ErrToolBusiness MCP 工具返回 isError=true，结果仍作为不可信数据回填模型。
var ErrToolBusiness = errors.New("MCP tool returned business error")

// ErrToolOutputTooLarge 工具返回超过执行层允许的字节上限。
var ErrToolOutputTooLarge = errors.New("MCP tool output too large")

// MaxToolOutputBytes 是单个工具结果允许进入上下文的字节上限。
const MaxToolOutputBytes = 64 * 1024

// Server 是注册到执行层的单个 MCP 服务；风险等级决定是否需要人工审批。
type Server struct {
	// Name 服务展示名称，用于生成稳定工具前缀。
	Name string
	// RiskLevel 风险等级；low 可自动执行，medium/high 必须审批。
	RiskLevel string
	// Client 已构造的 MCP 客户端。
	Client *Client
}

// binding 是规范工具名到原始服务与工具的映射。
type binding struct {
	// server 所属服务。
	server Server
	// tool 原始工具定义。
	tool Tool
	// spec 暴露给模型的工具定义。
	spec react.ToolSpec
}

// Registry 汇总多个 MCP 服务的低风险工具，实现 react.Registry。
type Registry struct {
	servers []Server
	// bindings 规范工具名到工具绑定。
	bindings map[string]binding
	// specs 暴露给模型的工具定义，顺序稳定。
	specs []react.ToolSpec
	// prepared 是否已完成工具发现。
	prepared bool
	// preparedServers 成功返回工具列表的服务数，用于区分全部故障与无工具。
	preparedServers int
}

// NewRegistry 构造注册表；保留全部合法风险等级，是否执行由上层审批门禁决定。
// 注册表本身不再静默丢弃高风险工具，否则模型无法发起需要人工确认的操作。
func NewRegistry(servers []Server) *Registry {
	allowed := make([]Server, 0, len(servers))
	for _, server := range servers {
		if server.Client == nil {
			continue
		}
		if server.RiskLevel == "" {
			server.RiskLevel = "low"
		}
		if server.RiskLevel != "low" && server.RiskLevel != "medium" && server.RiskLevel != "high" {
			continue
		}
		allowed = append(allowed, server)
	}
	return &Registry{servers: allowed, bindings: map[string]binding{}}
}

// idempotencyKeyContextKey 是工具幂等键在 Context 中的键类型。
type idempotencyKeyContextKey struct{}

// WithIdempotencyKey 把审批执行幂等键注入工具调用 Context，通过 MCP _meta 透传给远端服务。
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyContextKey{}, key)
}

// IdempotencyKeyFromContext 读取工具调用幂等键；不存在时返回空串。
func IdempotencyKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyContextKey{}).(string)
	return key
}

// Prepare 发现并缓存全部工具定义；单个服务失败时跳过，不影响其他服务。
func (r *Registry) Prepare(ctx context.Context) error {
	if r.prepared {
		return nil
	}
	seen := map[string]struct{}{}
	for _, server := range r.servers {
		tools, err := server.Client.ListTools(ctx)
		if err != nil {
			continue
		}
		r.preparedServers++
		for _, tool := range tools {
			canonical := canonicalToolName(server.Name, tool.Name)
			if _, exists := seen[canonical]; exists {
				// 同名工具来自不同服务时追加短哈希，保证模型侧名称唯一。
				digest := sha256.Sum256([]byte(server.Name + "\x00" + tool.Name))
				canonical = truncateName(canonical, 52) + "_" + hex.EncodeToString(digest[:4])
			}
			if _, exists := seen[canonical]; exists {
				continue
			}
			seen[canonical] = struct{}{}
			spec := react.ToolSpec{Name: canonical, Description: tool.Description}
			if len(tool.InputSchema) > 0 {
				spec.Parameters = tool.InputSchema
			}
			r.bindings[canonical] = binding{server: server, tool: tool, spec: spec}
			r.specs = append(r.specs, spec)
		}
	}
	r.prepared = true
	if len(r.specs) == 0 {
		if r.preparedServers == 0 && len(r.servers) > 0 {
			return fmt.Errorf("%w: all MCP servers failed tool discovery", ErrTransport)
		}
		return ErrEmptyRegistry
	}
	return nil
}

// Lookup 按规范工具名查找工具。
func (r *Registry) Lookup(name string) (react.Tool, bool) {
	bound, exists := r.bindings[name]
	if !exists {
		return nil, false
	}
	return bound, true
}

// List 返回全部工具定义，顺序在 Prepare 后稳定。
func (r *Registry) List() []react.ToolSpec {
	return append([]react.ToolSpec(nil), r.specs...)
}

// Spec 返回工具定义。
func (b binding) Spec() react.ToolSpec { return b.spec }

// RiskLevel 返回服务风险等级，供上层审批门禁判断是否需要人工确认。
func (b binding) RiskLevel() string { return b.server.RiskLevel }

// Invoke 调用 MCP 工具并把返回内容编码为不可信 JSON 数据。
// 审批恢复路径会在 Context 中注入幂等键，这里通过 _meta 透传，供远端服务去重副作用。
func (b binding) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	meta := map[string]any{}
	if key := IdempotencyKeyFromContext(ctx); key != "" {
		meta["idempotency_key"] = key
	}
	result, err := b.server.Client.CallToolWithMeta(ctx, b.tool.Name, arguments, meta)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"content": result.Content, "is_error": result.IsError}
	if result.StructuredContent != nil {
		payload["structured_content"] = result.StructuredContent
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool result: %w", err)
	}
	// 工具返回超过 64KB 时明确失败，避免截断后的 JSON 被模型当作完整数据。
	if len(encoded) > MaxToolOutputBytes {
		return nil, ErrToolOutputTooLarge
	}
	if result.IsError {
		return encoded, ErrToolBusiness
	}
	return encoded, nil
}

// canonicalToolName 生成符合模型函数名约束的稳定工具名。
func canonicalToolName(server, tool string) string {
	name := "mcp_" + sanitizeName(server) + "_" + sanitizeName(tool)
	if len(name) > 60 {
		digest := sha256.Sum256([]byte(server + "\x00" + tool))
		name = truncateName(name, 51) + "_" + hex.EncodeToString(digest[:4])
	}
	if name == "mcp__" {
		return "mcp_tool"
	}
	return name
}

// sanitizeName 仅保留字母、数字、下划线与短横线，其余字符替换为下划线。
func sanitizeName(value string) string {
	mapped := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return unicode.ToLower(r)
		}
		return '_'
	}, strings.TrimSpace(value))
	return strings.Trim(mapped, "_")
}

// truncateName 截断名称到指定长度，空结果回退为 tool。
func truncateName(value string, maximum int) string {
	if value == "" {
		return "tool"
	}
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
