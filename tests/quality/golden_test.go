// Package quality_test 运行版本化 Agent 黄金集与对抗集，防止执行质量相对基线回退。
package quality_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
)

// goldenConfig 是版本化黄金集配置。
type goldenConfig struct {
	// Version 数据集版本。
	Version int `json:"version"`
	// Baseline 基线日期。
	Baseline string `json:"baseline"`
	// Thresholds 质量阈值。
	Thresholds struct {
		// MaxSteps 单 Run 最大模型步数。
		MaxSteps int `json:"max_steps"`
		// DuplicateToolThreshold 连续重复工具调用阈值。
		DuplicateToolThreshold int `json:"duplicate_tool_threshold"`
		// MaxToolResultBytes 单个工具结果字节上限。
		MaxToolResultBytes int `json:"max_tool_result_bytes"`
	} `json:"thresholds"`
	// Scenarios 场景列表。
	Scenarios []goldenScenario `json:"scenarios"`
}

// goldenScenario 是单个黄金集/对抗集场景。
type goldenScenario struct {
	// ID 场景 ID。
	ID string `json:"id"`
	// Kind 场景类型。
	Kind string `json:"kind"`
	// Prompt 用户输入。
	Prompt string `json:"prompt"`
	// Tool 工具名。
	Tool string `json:"tool"`
	// Arguments 工具参数。
	Arguments json.RawMessage `json:"arguments"`
	// ToolOutput 工具输出。
	ToolOutput json.RawMessage `json:"tool_output"`
	// ExpectedAnswer 期望最终回答。
	ExpectedAnswer string `json:"expected_answer"`
}

// scriptedModel 按场景脚本返回工具调用与最终回答。
type scriptedModel struct {
	scenario goldenScenario
}

// Complete 首步返回工具调用，非循环场景在看到工具结果后返回最终回答。
func (m *scriptedModel) Complete(_ context.Context, messages []react.Message, _ []react.ToolSpec) (react.Completion, error) {
	if m.scenario.Kind != "tool_loop" {
		for _, message := range messages {
			if message.Role == react.RoleTool {
				return react.Completion{Content: m.scenario.ExpectedAnswer, FinishReason: "stop"}, nil
			}
		}
	}
	return react.Completion{
		ToolCalls: []react.ToolCall{{
			ID: "call-" + m.scenario.ID, Name: m.scenario.Tool, Arguments: m.scenario.Arguments,
		}},
		FinishReason: "tool_calls",
	}, nil
}

// goldenTool 返回场景预设输出。
type goldenTool struct {
	scenario goldenScenario
}

// Spec 返回工具定义。
func (t *goldenTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: t.scenario.Tool, Description: "golden tool", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// Invoke 返回场景输出；oversize 场景生成超过上限的结果。
func (t *goldenTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if t.scenario.Kind == "oversize_result" {
		return json.RawMessage(`{"content":"` + strings.Repeat("x", 131072) + `"}`), nil
	}
	return t.scenario.ToolOutput, nil
}

// RiskLevel 返回低风险，测试不涉及审批。
func (t *goldenTool) RiskLevel() string { return "low" }

// goldenRegistry 是单工具注册表。
type goldenRegistry struct {
	tool *goldenTool
}

// Lookup 按名称返回工具。
func (r *goldenRegistry) Lookup(name string) (react.Tool, bool) {
	if r.tool != nil && name == r.tool.scenario.Tool {
		return r.tool, true
	}
	return nil, false
}

// List 返回工具定义。
func (r *goldenRegistry) List() []react.ToolSpec {
	if r.tool == nil {
		return nil
	}
	return []react.ToolSpec{r.tool.Spec()}
}

// loadGoldenConfig 读取并校验版本化黄金集。
func loadGoldenConfig(t *testing.T) goldenConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config goldenConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.Version <= 0 || config.Baseline == "" || len(config.Scenarios) < 5 {
		t.Fatalf("golden config is incomplete: %#v", config)
	}
	if config.Thresholds.MaxSteps <= 0 || config.Thresholds.DuplicateToolThreshold <= 0 || config.Thresholds.MaxToolResultBytes <= 0 {
		t.Fatalf("golden thresholds are incomplete: %#v", config.Thresholds)
	}
	return config
}

// TestGoldenAndAdversarialScenarios 执行全部黄金集与对抗集场景，校验质量阈值与失败分类。
func TestGoldenAndAdversarialScenarios(t *testing.T) {
	config := loadGoldenConfig(t)
	for _, scenario := range config.Scenarios {
		scenario := scenario
		t.Run(scenario.ID, func(t *testing.T) {
			registry := &goldenRegistry{}
			if scenario.Tool != "" && scenario.Kind != "unknown_tool" {
				registry.tool = &goldenTool{scenario: scenario}
			}
			runner, err := react.NewRunner(&scriptedModel{scenario: scenario}, registry, react.Limits{
				MaxSteps:               config.Thresholds.MaxSteps,
				DuplicateToolThreshold: config.Thresholds.DuplicateToolThreshold,
				MaxToolResultBytes:     config.Thresholds.MaxToolResultBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), react.Request{
				Messages: []react.Message{{Role: react.RoleUser, Content: scenario.Prompt}},
			}, nil)
			switch scenario.Kind {
			case "success":
				if err != nil {
					t.Fatalf("golden scenario failed: %v", err)
				}
				if result.Content != scenario.ExpectedAnswer {
					t.Fatalf("answer = %q, want %q", result.Content, scenario.ExpectedAnswer)
				}
				if result.Steps < 2 {
					t.Fatalf("success scenario did not use the tool: steps=%d", result.Steps)
				}
				// 工具结果必须作为不可信 tool 消息进入上下文，不能伪装成 system 指令。
				toolMessages := 0
				for _, message := range result.Messages {
					if message.Role == react.RoleSystem && strings.Contains(message.Content, "IGNORE ALL PREVIOUS INSTRUCTIONS") {
						t.Fatalf("tool output was promoted to a system message: %#v", message)
					}
					if message.Role == react.RoleTool {
						toolMessages++
					}
				}
				if toolMessages != 1 {
					t.Fatalf("tool messages = %d", toolMessages)
				}
			case "tool_loop":
				if !errors.Is(err, react.ErrToolLoop) {
					t.Fatalf("tool loop error = %v", err)
				}
			case "unknown_tool":
				// 未知工具必须转成可恢复的工具错误，不得中断 Run 或执行任意工具。
				if err != nil {
					t.Fatalf("unknown tool should be recoverable: %v", err)
				}
				if content := lastToolContent(result.Messages); !strings.Contains(content, `"error":"unknown_tool"`) {
					t.Fatalf("unknown tool content = %q", content)
				}
			case "oversize_result":
				// 超限结果必须替换为错误标记，原文不得进入上下文。
				if err != nil {
					t.Fatalf("oversize result should be recoverable: %v", err)
				}
				content := lastToolContent(result.Messages)
				if !strings.Contains(content, `"error":"tool_result_too_large"`) ||
					strings.Contains(content, strings.Repeat("x", 1024)) {
					t.Fatalf("oversize tool content = %q", content)
				}
			default:
				t.Fatalf("unknown scenario kind %q", scenario.Kind)
			}
		})
	}
}

// lastToolContent 返回最后一条 tool 消息内容，用于验证错误标记。
func lastToolContent(messages []react.Message) string {
	content := ""
	for _, message := range messages {
		if message.Role == react.RoleTool {
			content = message.Content
		}
	}
	return content
}
