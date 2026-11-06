package model

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// ReActModel 把 OpenAI-compatible 客户端适配为 react.Model。
type ReActModel struct {
	client      *Client
	model       ModelRef
	temperature float64
}

// NewReActModel 构造 ReAct 模型适配器。
func NewReActModel(client *Client, model ModelRef, temperature float64) (*ReActModel, error) {
	if client == nil || model.RemoteModelName == "" || model.ContextWindowTokens < 1_000_000 {
		return nil, errors.New("react model requires an OpenAI client and a 1M+ model snapshot")
	}
	return &ReActModel{client: client, model: model, temperature: temperature}, nil
}

// Complete 实现 react.Model：转换会话消息与工具定义，调用 Chat Completions。
func (m *ReActModel) Complete(ctx context.Context, messages []react.Message, tools []react.ToolSpec) (react.Completion, error) {
	chatMessages := make([]ChatMessage, 0, len(messages))
	for _, message := range messages {
		converted := ChatMessage{Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			// 助手消息必须原样回放 tool_calls，否则供应商会拒绝后续工具结果。
			converted.ToolCalls = append(converted.ToolCalls, ChatToolCall{
				ID: call.ID, Type: "function", Function: ChatFunction{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		chatMessages = append(chatMessages, converted)
	}
	definitions := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		definitions = append(definitions, ToolDefinition{Name: tool.Name, Description: tool.Description, Parameters: parameters})
	}
	response, err := m.client.Chat(ctx, ChatRequest{
		Model: m.model, Messages: chatMessages, Tools: definitions,
		Temperature: m.temperature, MaxOutputTokens: m.model.MaxOutputTokens,
	})
	if err != nil {
		return react.Completion{}, err
	}
	completion := react.Completion{
		Content: response.Content, ReasoningContent: response.ReasoningContent, FinishReason: response.FinishReason,
		Usage:     react.Usage{InputTokens: response.InputTokens, OutputTokens: response.OutputTokens},
		ToolCalls: make([]react.ToolCall, 0, len(response.ToolCalls)),
	}
	for _, call := range response.ToolCalls {
		arguments := json.RawMessage(call.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		completion.ToolCalls = append(completion.ToolCalls, react.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	return completion, nil
}
