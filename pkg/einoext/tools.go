package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// Tool 把执行层工具适配为 Eino InvokableTool，并复用现有审批/步骤包装。
type Tool struct {
	inner   react.Tool
	emitter Emitter
}

// WrapRegistry 把执行层工具注册表转换为 Eino 工具列表。
func WrapRegistry(registry react.Registry, emitter Emitter) []tool.BaseTool {
	if registry == nil {
		return nil
	}
	specs := registry.List()
	result := make([]tool.BaseTool, 0, len(specs))
	for _, spec := range specs {
		inner, ok := registry.Lookup(spec.Name)
		if !ok {
			continue
		}
		result = append(result, &Tool{inner: inner, emitter: emitter})
	}
	return result
}

// Info 返回 Eino 工具定义。
func (t *Tool) Info(context.Context) (*schemaToolInfo, error) {
	return SpecToToolInfo(t.inner.Spec())
}

// InvokableRun 执行工具并把业务错误转换为模型可纠正的观察结果。
// 审批门禁与基础设施故障必须原样上抛：前者由 Worker 暂停 Run 等待人工决策，
// 后者决定重试或人工对账，转成观察结果会让模型继续执行并丢失故障语义。
func (t *Tool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	spec := t.inner.Spec()
	callID := compose.GetToolCallID(ctx)
	// 执行层工具包装依赖上下文中的调用元数据创建审批记录与幂等键。
	ctx = react.WithToolCall(ctx, callID, spec.Name)
	if t.emitter != nil {
		t.emitter.Emit("tool.started", map[string]any{
			"tool": spec.Name, "tool_call_id": callID, "risk_level": t.inner.RiskLevel(),
		})
	}
	if guard := LoopGuardFromContext(ctx); guard != nil {
		content, loopErr := guard.checkToolCall(ctx, spec.Name, argumentsInJSON)
		if loopErr != nil {
			return "", loopErr
		}
		if content != "" {
			t.finish(ctx, spec.Name, callID, "repeated", content, "")
			return content, nil
		}
	}
	output, err := t.inner.Invoke(ctx, json.RawMessage(argumentsInJSON))
	if err != nil {
		// 审批门禁与基础设施故障不属于模型可纠正的业务错误。
		if errors.Is(err, react.ErrApprovalRequired) || errors.Is(err, react.ErrToolInfrastructure) {
			return "", err
		}
		// 父上下文已取消/超时：终止 Run，而不是让模型基于残缺结果继续推理。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		status := "failed"
		var invalidArguments *react.InvalidArgumentsError
		switch {
		case errors.As(err, &invalidArguments):
			status = "invalid_arguments"
		case errors.Is(err, context.DeadlineExceeded):
			status = "timeout"
		}
		content := toolErrorContent(status, err, output)
		t.finish(ctx, spec.Name, callID, status, content, errorText(err))
		return content, nil
	}
	content := truncateToolOutput(string(output))
	t.finish(ctx, spec.Name, callID, "completed", content, "")
	return content, nil
}

func (t *Tool) finish(ctx context.Context, name, callID, status, content, message string) {
	if t.emitter != nil {
		payload := map[string]any{"tool": name, "tool_call_id": callID, "status": status}
		if message != "" {
			payload["error"] = message
		}
		t.emitter.Emit("tool.completed", payload)
	}
	if emitter, ok := EmitterFromContext(ctx); ok && emitter != nil {
		EmitMessage(ctx, emitter.NextMessageIndex(), "tool", content, callID, name, nil)
	}
}

func toolErrorContent(status string, err error, output []byte) string {
	payload := map[string]any{"error": status}
	if err != nil {
		payload["detail"] = errorText(err)
	}
	if len(output) > 0 {
		payload["output"] = truncateToolOutput(string(output))
	}
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return `{"error":"tool_execution_failed"}`
	}
	return string(encoded)
}

// maximumToolOutputBytes 是单个工具结果进入模型上下文的字节上限。
const maximumToolOutputBytes = 64 * 1024

// truncateToolOutput 超限时保留首尾片段并说明原始大小。
// 只保留头部会丢掉 JSON 尾部字段、日志结论和最终状态，因此必须首尾同时保留；
// 完整结果已由执行步骤载荷归档，可在运行详情中回查。
func truncateToolOutput(output string) string {
	if len(output) <= maximumToolOutputBytes {
		return output
	}
	note := fmt.Sprintf("\n[... tool result truncated: original %d bytes; showing head and tail, full result is archived in execution details ...]\n", len(output))
	budget := maximumToolOutputBytes - len(note)
	if budget < 2*1024 {
		budget = 2 * 1024
	}
	headBytes := budget * 2 / 3
	tailBytes := budget - headBytes
	return truncateUTF8(output, headBytes) + note + truncateUTF8FromEnd(output, tailBytes)
}

// truncateUTF8FromEnd 按字节上限保留尾部且不切断多字节字符。
func truncateUTF8FromEnd(text string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(text) <= maximum {
		return text
	}
	start := len(text) - maximum
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

// truncateUTF8 按字节上限截断且不切断多字节字符，避免 PostgreSQL 拒绝非法 UTF-8。
func truncateUTF8(text string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(text) <= maximum {
		return text
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// schemaToolInfo 是 Eino ToolInfo 的别名，避免调用方直接依赖 schema 包。
type schemaToolInfo = schema.ToolInfo

// 编译期断言工具实现 Eino 接口。
var _ tool.InvokableTool = (*Tool)(nil)

// 确保导入的 compose 包在测试替身未启用 Eino 工具节点时仍被使用。
var _ = compose.GetToolCallID
