package einoext

import (
	"context"
	"encoding/json"
	"errors"

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

// InvokableRun 执行工具并把错误转换为模型可纠正的观察结果。
// 审批门禁错误必须原样上抛，由 Worker 暂停 Run 等待人工决策。
func (t *Tool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	spec := t.inner.Spec()
	callID := compose.GetToolCallID(ctx)
	if t.emitter != nil {
		t.emitter.Emit("tool.started", map[string]any{
			"tool": spec.Name, "tool_call_id": callID, "risk_level": t.inner.RiskLevel(),
		})
	}
	if guard := LoopGuardFromContext(ctx); guard != nil && guard.Repeated(spec.Name, argumentsInJSON) {
		content := `{"error":"repeated_tool_call","hint":"与紧邻的上一次调用参数完全相同，工具未重复执行；请总结已有结果、补充缺失信息或改用其他参数或工具。"}`
		t.finish(ctx, spec.Name, callID, "repeated", content, "")
		return content, nil
	}
	output, err := t.inner.Invoke(ctx, json.RawMessage(argumentsInJSON))
	if err != nil {
		if errors.Is(err, react.ErrApprovalRequired) {
			return "", err
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

func truncateToolOutput(output string) string {
	const maximum = 64 * 1024
	if len(output) <= maximum {
		return output
	}
	return output[:maximum] + "\n[... tool result truncated ...]"
}

// schemaToolInfo 是 Eino ToolInfo 的别名，避免调用方直接依赖 schema 包。
type schemaToolInfo = schema.ToolInfo

// 编译期断言工具实现 Eino 接口。
var _ tool.InvokableTool = (*Tool)(nil)

// 确保导入的 compose 包在测试替身未启用 Eino 工具节点时仍被使用。
var _ = compose.GetToolCallID
