// Package einoext 提供 Harness 执行层与 Eino 组件之间的适配：
// 把 Runner、模型与工具转换为 Eino 的 ChatModel 和 InvokableTool，供后续 Graph 编排复用。
package einoext

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
)

// MessagesFromEino 把 Eino 消息转换为执行器内部消息。
// 只保留文本、工具调用与工具结果，不传播隐藏推理内容或多模态数据。
func MessagesFromEino(input []*schema.Message) []react.Message {
	messages := make([]react.Message, 0, len(input))
	for _, message := range input {
		if message == nil {
			continue
		}
		converted := react.Message{
			Role: roleFromEino(message.Role), Content: message.Content,
			ToolCallID: message.ToolCallID, Name: message.ToolName,
		}
		for _, call := range message.ToolCalls {
			arguments := json.RawMessage(call.Function.Arguments)
			if len(arguments) == 0 || !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			converted.ToolCalls = append(converted.ToolCalls, react.ToolCall{
				ID: call.ID, Name: call.Function.Name, Arguments: arguments,
			})
		}
		messages = append(messages, converted)
	}
	return messages
}

// MessagesToEino 把执行器内部消息转换为 Eino 消息。
func MessagesToEino(input []react.Message) []*schema.Message {
	messages := make([]*schema.Message, 0, len(input))
	for _, message := range input {
		converted := &schema.Message{
			Role: roleToEino(message.Role), Content: message.Content,
			ToolCallID: message.ToolCallID, ToolName: message.Name,
		}
		for _, call := range message.ToolCalls {
			arguments := string(call.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			converted.ToolCalls = append(converted.ToolCalls, schema.ToolCall{
				ID: call.ID, Type: "function",
				Function: schema.FunctionCall{Name: call.Name, Arguments: arguments},
			})
		}
		messages = append(messages, converted)
	}
	return messages
}

// roleFromEino 把 Eino 角色映射为执行器角色；未知角色按用户输入处理。
func roleFromEino(role schema.RoleType) react.Role {
	switch role {
	case schema.System:
		return react.RoleSystem
	case schema.Assistant:
		return react.RoleAssistant
	case schema.Tool:
		return react.RoleTool
	default:
		return react.RoleUser
	}
}

// roleToEino 把执行器角色映射为 Eino 角色。
func roleToEino(role react.Role) schema.RoleType {
	switch role {
	case react.RoleSystem:
		return schema.System
	case react.RoleAssistant:
		return schema.Assistant
	case react.RoleTool:
		return schema.Tool
	default:
		return schema.User
	}
}

// ToolInfoFromSpec 把执行器工具定义转换为 Eino ToolInfo；参数 Schema 必须是合法 JSON。
func ToolInfoFromSpec(spec react.ToolSpec) (*schema.ToolInfo, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("tool name is required")
	}
	info := &schema.ToolInfo{Name: spec.Name, Desc: spec.Description}
	if len(spec.Parameters) > 0 {
		var parameters jsonschema.Schema
		if err := json.Unmarshal(spec.Parameters, &parameters); err != nil {
			return nil, fmt.Errorf("decode tool parameters for %s: %w", spec.Name, err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&parameters)
	}
	return info, nil
}

// SpecFromToolInfo 把 Eino ToolInfo 还原为执行器工具定义。
func SpecFromToolInfo(info *schema.ToolInfo) (react.ToolSpec, error) {
	if info == nil || info.Name == "" {
		return react.ToolSpec{}, fmt.Errorf("tool info name is required")
	}
	spec := react.ToolSpec{Name: info.Name, Description: info.Desc}
	if info.ParamsOneOf != nil {
		parameters, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			return react.ToolSpec{}, fmt.Errorf("encode tool parameters for %s: %w", info.Name, err)
		}
		raw, err := json.Marshal(parameters)
		if err != nil {
			return react.ToolSpec{}, err
		}
		spec.Parameters = raw
	}
	return spec, nil
}

// Tool 把执行器工具包装为 Eino 可调用工具；审批与观测包装仍在执行器侧生效。
type Tool struct {
	inner react.Tool
}

// NewTool 构造 Eino 工具适配器。
func NewTool(inner react.Tool) (tool.InvokableTool, error) {
	if inner == nil {
		return nil, fmt.Errorf("react tool is required")
	}
	return &Tool{inner: inner}, nil
}

// Info 返回工具元数据。
func (t *Tool) Info(context.Context) (*schema.ToolInfo, error) {
	return ToolInfoFromSpec(t.inner.Spec())
}

// InvokableRun 校验参数 JSON 后调用执行器工具，并返回其 JSON 文本结果。
func (t *Tool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	// Eino 工具 Span 只记录工具名与结果状态，参数与结果原文禁止进入 Trace。
	ctx, span := telemetry.Start(ctx, "eino.tool.invoke", attribute.String(telemetry.AttrToolName, t.inner.Spec().Name))
	defer span.End()
	if argumentsInJSON == "" {
		argumentsInJSON = "{}"
	}
	if !json.Valid([]byte(argumentsInJSON)) {
		telemetry.Fail(span, "invalid_arguments")
		return "", fmt.Errorf("tool arguments for %s are not valid JSON", t.inner.Spec().Name)
	}
	output, err := t.inner.Invoke(ctx, json.RawMessage(argumentsInJSON))
	if err != nil {
		telemetry.Fail(span, "tool_failed")
		return "", err
	}
	telemetry.Annotate(span, attribute.String(telemetry.AttrToolStatus, "completed"))
	return string(output), nil
}

// ChatModel 把执行器模型包装为 Eino ToolCallingChatModel。
// 上游执行器是非流式模型；Stream 返回单块消息，流式增量由 Run 事件层负责，
// 绝不在流式已输出后静默拼接另一个模型的结果。
type ChatModel struct {
	inner react.Model
	specs []react.ToolSpec
}

// NewChatModel 构造 Eino 模型适配器。
func NewChatModel(inner react.Model) (model.ToolCallingChatModel, error) {
	if inner == nil {
		return nil, fmt.Errorf("react model is required")
	}
	return &ChatModel{inner: inner}, nil
}

// WithTools 返回绑定工具定义的新实例，不修改原实例，保证并发安全。
func (m *ChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	specs := make([]react.ToolSpec, 0, len(tools))
	for _, info := range tools {
		spec, err := SpecFromToolInfo(info)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return &ChatModel{inner: m.inner, specs: specs}, nil
}

// Generate 执行一次非流式模型调用并转换为 Eino 消息。
func (m *ChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	// Eino 模型 Span 只记录消息数与工具数，Prompt 与 Completion 原文禁止进入 Trace。
	ctx, span := telemetry.Start(ctx, "eino.model.generate",
		attribute.Int("llm.message_count", len(input)),
		attribute.Int("llm.tool_count", len(m.specs)),
	)
	defer span.End()
	completion, err := m.inner.Complete(ctx, MessagesFromEino(input), append([]react.ToolSpec(nil), m.specs...))
	if err != nil {
		telemetry.Fail(span, "model_call_failed")
		return nil, err
	}
	message := &schema.Message{Role: schema.Assistant, Content: completion.Content}
	for _, call := range completion.ToolCalls {
		arguments := string(call.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
			ID: call.ID, Type: "function",
			Function: schema.FunctionCall{Name: call.Name, Arguments: arguments},
		})
	}
	return message, nil
}

// Stream 以单块消息模拟流式输出，保持 Eino 接口兼容且不伪造增量。
func (m *ChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

// 编译期断言 Eino 接口实现。
var (
	_ tool.InvokableTool         = (*Tool)(nil)
	_ model.ToolCallingChatModel = (*ChatModel)(nil)
)
