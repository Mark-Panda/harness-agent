// Package einoext 将现有模型路由、工具注册表与执行事件适配到 Eino 编排层。
// 业务数据、审批、预算与持久化仍由 internal/biz 负责，本包只做协议与图编排转换。
package einoext

import (
	"encoding/json"
	"fmt"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// ToReactMessages 把 Eino 消息转换为执行层协议消息。
func ToReactMessages(messages []*schema.Message) []react.Message {
	result := make([]react.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		converted := react.Message{
			Role:       react.Role(message.Role),
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			Name:       message.ToolName,
		}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, react.ToolCall{
				ID: call.ID, Name: call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments),
			})
		}
		result = append(result, converted)
	}
	return result
}

// ToSchemaMessages 把执行层消息转换为 Eino 消息。
func ToSchemaMessages(messages []react.Message) []*schema.Message {
	result := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		result = append(result, ToSchemaMessage(message))
	}
	return result
}

// ToSchemaMessage 转换单条执行层消息。
func ToSchemaMessage(message react.Message) *schema.Message {
	converted := &schema.Message{
		Role:       schema.RoleType(message.Role),
		Content:    message.Content,
		ToolCallID: message.ToolCallID,
		ToolName:   message.Name,
	}
	for index, call := range message.ToolCalls {
		position := index
		converted.ToolCalls = append(converted.ToolCalls, schema.ToolCall{
			Index: &position, ID: call.ID, Type: "function",
			Function: schema.FunctionCall{Name: call.Name, Arguments: string(call.Arguments)},
		})
	}
	return converted
}

// ToolInfoToSpec 把 Eino 工具定义转换为执行层工具定义。
func ToolInfoToSpec(info *schema.ToolInfo) (react.ToolSpec, error) {
	if info == nil || info.Name == "" {
		return react.ToolSpec{}, fmt.Errorf("tool info is empty")
	}
	spec := react.ToolSpec{Name: info.Name, Description: info.Desc}
	jsonSchema, err := info.ToJSONSchema()
	if err != nil {
		return react.ToolSpec{}, fmt.Errorf("convert tool schema %s: %w", info.Name, err)
	}
	if jsonSchema != nil {
		encoded, err := json.Marshal(jsonSchema)
		if err != nil {
			return react.ToolSpec{}, fmt.Errorf("encode tool schema %s: %w", info.Name, err)
		}
		spec.Parameters = encoded
	}
	return spec, nil
}

// SpecToToolInfo 把执行层工具定义转换为 Eino 工具定义。
func SpecToToolInfo(spec react.ToolSpec) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{Name: spec.Name, Desc: spec.Description}
	if len(spec.Parameters) == 0 {
		return info, nil
	}
	var document jsonschema.Schema
	if err := json.Unmarshal(spec.Parameters, &document); err != nil {
		return nil, fmt.Errorf("decode tool schema %s: %w", spec.Name, err)
	}
	info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&document)
	return info, nil
}
