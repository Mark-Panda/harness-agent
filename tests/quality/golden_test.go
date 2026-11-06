// Package quality_test 运行版本化 Agent 黄金集与对抗集，防止执行质量相对基线回退。
package quality_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/einoext"
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

// requestMessage 是模型请求中的最小消息结构。
type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// scriptedState 记录脚本模型收到过的请求，供不可信内容断言使用。
type scriptedState struct {
	mu       sync.Mutex
	requests [][]requestMessage
}

// lastRequest 返回最近一次模型请求的消息。
func (s *scriptedState) lastRequest() []requestMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

// record 保存请求消息。
func (s *scriptedState) record(messages []requestMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, messages)
}

// newScriptedServer 按场景脚本返回工具调用或最终回答。
// 工具循环场景在收到步数上限收尾指令后返回最终结论，验证 Eino 收尾节点生效。
func newScriptedServer(t *testing.T, scenario goldenScenario, state *scriptedState) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []requestMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		state.record(body.Messages)

		hasToolResult := false
		stepLimit := false
		for _, message := range body.Messages {
			if message.Role == "tool" {
				hasToolResult = true
			}
			if strings.Contains(message.Content, "已达到工具调用步数上限") {
				stepLimit = true
			}
		}

		content := ""
		var toolCalls []any
		switch scenario.Kind {
		case "success", "oversize_result":
			if hasToolResult {
				content = scenario.ExpectedAnswer
			} else {
				toolCalls = scriptedToolCalls(scenario)
			}
		case "unknown_tool":
			if hasToolResult {
				content = scenario.ExpectedAnswer
			} else {
				toolCalls = scriptedToolCalls(scenario)
			}
		case "tool_loop":
			if stepLimit {
				content = "工具调用达到上限，已停止重复调用。"
			} else {
				toolCalls = scriptedToolCalls(scenario)
			}
		default:
			t.Fatalf("unknown scenario kind %q", scenario.Kind)
		}

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "req-" + scenario.ID,
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": content, "tool_calls": toolCalls},
				"finish_reason": finishReason,
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// scriptedToolCalls 构造场景工具调用。
func scriptedToolCalls(scenario goldenScenario) []any {
	arguments := string(scenario.Arguments)
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	return []any{map[string]any{
		"id": "call-" + scenario.ID,
		"function": map[string]any{
			"name": scenario.Tool, "arguments": arguments,
		},
	}}
}

// qualityEmitter 记录 Eino 输出的消息事件，供质量断言检查工具观察结果。
type qualityEmitter struct {
	mu       sync.Mutex
	next     int
	step     int
	messages []map[string]any
}

// Emit 记录消息创建事件。
func (e *qualityEmitter) Emit(eventType string, payload map[string]any) {
	if eventType != "message.created" {
		return
	}
	copied := make(map[string]any, len(payload))
	for key, value := range payload {
		copied[key] = value
	}
	e.mu.Lock()
	e.messages = append(e.messages, copied)
	e.mu.Unlock()
}

// NextMessageIndex 分配消息序号。
func (e *qualityEmitter) NextMessageIndex() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	return e.next
}

// NextStep 分配模型步序号。
func (e *qualityEmitter) NextStep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.step++
	return e.step
}

// toolMessages 返回全部工具观察结果文本。
func (e *qualityEmitter) toolMessages() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]string, 0)
	for _, message := range e.messages {
		if message["role"] != "tool" {
			continue
		}
		if content, ok := message["content"].(string); ok {
			result = append(result, content)
		}
	}
	return result
}

// goldenTool 返回场景预设输出。
type goldenTool struct {
	scenario goldenScenario
	// calls 记录真实执行次数，用于验证重复工具调用被拦截。
	calls int
}

// Spec 返回工具定义。
func (t *goldenTool) Spec() react.ToolSpec {
	return react.ToolSpec{Name: t.scenario.Tool, Description: "golden tool", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// Invoke 返回场景输出；oversize 场景生成超过上限的结果。
func (t *goldenTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	t.calls++
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

// TestGoldenAndAdversarialScenarios 执行全部黄金集与对抗集场景，校验 Eino 执行质量。
func TestGoldenAndAdversarialScenarios(t *testing.T) {
	config := loadGoldenConfig(t)
	for _, scenario := range config.Scenarios {
		scenario := scenario
		t.Run(scenario.ID, func(t *testing.T) {
			state := &scriptedState{}
			server := newScriptedServer(t, scenario, state)
			registry := &goldenRegistry{}
			if scenario.Tool != "" && scenario.Kind != "unknown_tool" {
				registry.tool = &goldenTool{scenario: scenario}
			}
			router, err := model.NewRouter([]model.Candidate{{
				Priority: 1,
				Ref: model.ModelRef{
					ProviderID: "quality", ModelID: scenario.ID, RemoteModelName: "quality-model",
					ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Tools: true,
				},
				BaseURL: server.URL, APIKey: "test-key",
			}}, nil, model.RouterConfig{
				MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 64,
				MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
				BackoffBase: time.Millisecond, MaxBackoff: time.Millisecond,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			emitter := &qualityEmitter{}
			agent, err := einoext.NewReactAgent(context.Background(), einoext.ReactConfig{
				Router: router, Registry: registry, Emitter: emitter,
				MaxSteps: config.Thresholds.MaxSteps, MaxTotalTokens: 2_000_000,
				LoopThreshold: config.Thresholds.DuplicateToolThreshold,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := agent.Run(context.Background(), []react.Message{{Role: react.RoleUser, Content: scenario.Prompt}})
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
				tools := emitter.toolMessages()
				if len(tools) != 1 {
					t.Fatalf("tool messages = %d", len(tools))
				}
				// 工具结果必须作为不可信 tool 消息进入上下文，不能伪装成 system 指令。
				for _, message := range state.lastRequest() {
					if message.Role == "system" && strings.Contains(message.Content, "IGNORE ALL PREVIOUS INSTRUCTIONS") {
						t.Fatalf("tool output was promoted to a system message: %#v", message)
					}
				}
			case "tool_loop":
				// 重复工具调用返回可观测错误且不重复执行；步数耗尽后由收尾节点给出结论。
				if err != nil {
					t.Fatalf("tool loop should finish with summary: %v", err)
				}
				if registry.tool == nil {
					t.Fatal("tool loop scenario registered no tool")
				}
				if want := config.Thresholds.DuplicateToolThreshold - 1; registry.tool.calls != want {
					t.Fatalf("repeated tool call was executed %d times, want %d", registry.tool.calls, want)
				}
				tools := emitter.toolMessages()
				if len(tools) == 0 || !strings.Contains(tools[len(tools)-1], `"error":"repeated_tool_call"`) {
					t.Fatalf("repeated tool content = %#v", tools)
				}
				if strings.TrimSpace(result.Content) == "" {
					t.Fatal("step-limit summary is empty")
				}
			case "unknown_tool":
				// 未知工具必须转成可恢复的工具错误，不得中断 Run 或执行任意工具。
				if err != nil {
					t.Fatalf("unknown tool should be recoverable: %v", err)
				}
				tools := emitter.toolMessages()
				if len(tools) == 0 || !strings.Contains(tools[len(tools)-1], `"error":"unknown_tool"`) {
					t.Fatalf("unknown tool content = %#v", tools)
				}
				if result.Content != scenario.ExpectedAnswer {
					t.Fatalf("answer = %q, want %q", result.Content, scenario.ExpectedAnswer)
				}
			case "oversize_result":
				// 超限结果必须裁剪并标注，原文不得整体进入上下文。
				if err != nil {
					t.Fatalf("oversize result should be recoverable: %v", err)
				}
				tools := emitter.toolMessages()
				if len(tools) == 0 {
					t.Fatal("oversize scenario produced no tool message")
				}
				content := tools[len(tools)-1]
				if !strings.Contains(content, "tool result truncated") ||
					len(content) > config.Thresholds.MaxToolResultBytes+512 {
					t.Fatalf("oversize tool content length = %d", len(content))
				}
				if result.Content != scenario.ExpectedAnswer {
					t.Fatalf("answer = %q, want %q", result.Content, scenario.ExpectedAnswer)
				}
			default:
				t.Fatalf("unknown scenario kind %q", scenario.Kind)
			}
		})
	}
}
