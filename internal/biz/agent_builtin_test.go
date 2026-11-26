package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestAgentMutationEventRecordsBuiltinTools 验证 Agent 审计事件携带内置工具绑定明细。
func TestAgentMutationEventRecordsBuiltinTools(t *testing.T) {
	userID := uuid.New()
	agent := Agent{ID: uuid.New(), UserID: userID, BuiltinTools: []string{builtinToolShell}}
	event := agentMutationEvent(userID, "agent.updated", agent)
	if event.EventType != "agent.updated" || event.Payload["outcome"] != "success" {
		t.Fatalf("event = %#v", event)
	}
	tools, ok := event.Payload["builtin_tools"].([]string)
	if !ok || len(tools) != 1 || tools[0] != builtinToolShell {
		t.Fatalf("builtin tools = %#v", event.Payload["builtin_tools"])
	}

	// 空切片表示显式禁用全部，必须与 nil（继承平台配置）区分。
	empty := agentMutationEvent(userID, "agent.updated", Agent{ID: uuid.New(), BuiltinTools: []string{}})
	emptyTools, ok := empty.Payload["builtin_tools"].([]string)
	if !ok || emptyTools == nil || len(emptyTools) != 0 {
		t.Fatalf("empty builtin tools = %#v", empty.Payload["builtin_tools"])
	}
	inherit := agentMutationEvent(userID, "agent.updated", Agent{ID: uuid.New()})
	if value, exists := inherit.Payload["builtin_tools"]; !exists || value != nil {
		t.Fatalf("inherit builtin tools = %#v (exists=%v)", value, exists)
	}
}

func TestAgentValidateBuiltinTools(t *testing.T) {
	service := &AgentService{}
	base := Agent{ID: uuid.New(), UserID: uuid.New(), Name: "assistant", Mode: AgentModeReact, Version: 1}
	valid := base
	valid.BuiltinTools = []string{builtinToolShell, builtinToolReadFile}
	if err := service.validate(context.Background(), &valid, false); err != nil {
		t.Fatalf("valid builtin tools rejected: %v", err)
	}

	unknown := base
	unknown.BuiltinTools = []string{"rm_rf"}
	if err := service.validate(context.Background(), &unknown, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown tool error = %v", err)
	}

	duplicate := base
	duplicate.BuiltinTools = []string{builtinToolShell, builtinToolShell}
	if err := service.validate(context.Background(), &duplicate, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate tool error = %v", err)
	}

	empty := base
	empty.BuiltinTools = []string{}
	if err := service.validate(context.Background(), &empty, false); err != nil {
		t.Fatalf("explicit empty binding rejected: %v", err)
	}
}
