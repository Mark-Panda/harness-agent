package einoext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// ReactConfig 是 Eino ReAct Agent 的构建参数。
type ReactConfig struct {
	// Router 现有模型路由，负责候选切换、重试、熔断与预算。
	Router *model.Router
	// Registry 执行层工具注册表，已包含观测、审批与沙箱包装。
	Registry react.Registry
	// Emitter 执行事件发射器。
	Emitter Emitter
	// MaxSteps 最大模型调用步数。
	MaxSteps int
	// MaxTotalTokens 输入加输出 Token 上限，零表示不限制。
	MaxTotalTokens int
	// MaxCost 估算费用上限，零表示不限制。
	MaxCost float64
	// LoopThreshold 连续重复工具调用阈值。
	LoopThreshold int
	// ContextBudget 每次模型调用前的 Prompt Token 预算。
	ContextBudget int
	// Trim 上下文裁剪函数，保持 system 与当前轮次。
	Trim func([]react.Message, int) []react.Message
}

// ReactAgent 是 Eino ReAct 执行器。
type ReactAgent struct {
	agent         *einoagent.Agent
	model         *ChatModel
	emitter       Emitter
	maxSteps      int
	loopThreshold int
}

// NewReactAgent 构建 Eino ReAct Agent。
func NewReactAgent(ctx context.Context, config ReactConfig) (*ReactAgent, error) {
	if config.Router == nil || config.Registry == nil {
		return nil, errors.New("eino react requires router and tool registry")
	}
	maxSteps := config.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	chatModel := NewChatModel(config.Router, config.Emitter, maxSteps, config.MaxTotalTokens, config.MaxCost)
	tools := WrapRegistry(config.Registry, config.Emitter)
	agent, err := einoagent.NewAgent(ctx, &einoagent.AgentConfig{
		ToolCallingModel: chatModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
			// 串行执行保证审批门禁、事件顺序与工具副作用顺序稳定。
			ExecuteSequentially: true,
			UnknownToolsHandler: func(ctx context.Context, name, arguments string) (string, error) {
				content := `{"error":"unknown_tool","hint":"该工具不存在，请使用已注册工具或直接回答。"}`
				// 未知工具同样要落库为 tool 消息，否则历史中会留下没有结果的 tool_calls。
				if emitter, ok := EmitterFromContext(ctx); ok && emitter != nil {
					EmitMessage(ctx, emitter.NextMessageIndex(), string(react.RoleTool), content, compose.GetToolCallID(ctx), name, nil)
				}
				return content, nil
			},
		},
		// Eino 的 MaxStep 统计图节点步数，ReAct 一轮通常包含模型与工具两个节点。
		MaxStep:               maxSteps*2 + 4,
		StreamToolCallChecker: toolCallsInStream,
		MessageRewriter: func(ctx context.Context, input []*schema.Message) []*schema.Message {
			if config.ContextBudget <= 0 || config.Trim == nil || len(input) == 0 {
				return input
			}
			return ToSchemaMessages(config.Trim(ToReactMessages(input), config.ContextBudget))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build eino react agent: %w", err)
	}
	return &ReactAgent{agent: agent, model: chatModel, emitter: config.Emitter, maxSteps: maxSteps, loopThreshold: config.LoopThreshold}, nil
}

// Run 执行 ReAct 并返回统一结果。
func (a *ReactAgent) Run(ctx context.Context, messages []react.Message) (react.Result, error) {
	if a == nil || a.agent == nil {
		return react.Result{}, errors.New("eino react agent is not initialized")
	}
	// 事件发射器通过 Context 传入模型/工具适配器。
	if a.emitter != nil {
		ctx = WithEmitter(ctx, a.emitter)
	}
	ctx = WithLoopGuard(ctx, NewLoopGuard(a.loopThreshold))
	stream, err := a.agent.Stream(ctx, ToSchemaMessages(messages))
	if err != nil {
		return a.result(ctx, messages, "", err)
	}
	defer stream.Close()
	var content strings.Builder
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return a.result(ctx, messages, content.String(), recvErr)
		}
		if chunk == nil || chunk.Content == "" {
			continue
		}
		content.WriteString(chunk.Content)
	}
	finalContent := content.String()
	if strings.TrimSpace(finalContent) == "" {
		// 部分供应商或非流式模型不会产生增量帧，使用最近一次完整响应兜底。
		finalContent = a.model.LastContent()
	}
	return a.result(ctx, messages, finalContent, nil)
}

func (a *ReactAgent) result(_ context.Context, messages []react.Message, content string, runErr error) (react.Result, error) {
	calls, inputTokens, outputTokens, cost := a.model.Usage()
	result := react.Result{
		Content: content, Steps: calls,
		TotalTokens: inputTokens + outputTokens, InputTokens: inputTokens, OutputTokens: outputTokens,
		EstimatedCost: cost, Messages: messages,
	}
	if provider, ok := a.emitter.(interface{ LastMessageID() string }); ok {
		result.FinalMessageID = provider.LastMessageID()
	}
	if content != "" {
		result.Messages = append(result.Messages, react.Message{Role: react.RoleAssistant, Content: content})
	}
	return result, runErr
}

// toolCallsInStream 完整扫描流式响应，兼容“先输出文本、后输出工具调用”的供应商。
func toolCallsInStream(_ context.Context, stream *schema.StreamReader[*schema.Message]) (bool, error) {
	defer stream.Close()
	found := false
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return false, err
		}
		if message != nil && len(message.ToolCalls) > 0 {
			found = true
		}
	}
}
