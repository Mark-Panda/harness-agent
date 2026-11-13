package einoext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/cloudwego/eino/schema"
)

// newChatModelTestRouter 构造指向固定响应服务器的模型路由。
func newChatModelTestRouter(t *testing.T, handler http.HandlerFunc) *model.Router {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	router, err := model.NewRouter([]model.Candidate{{
		Ref: model.ModelRef{
			ProviderID: "provider", ModelID: "model", RemoteModelName: "test-model",
			ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Tools: true,
		},
		BaseURL: server.URL, APIKey: "test-key",
	}}, nil, model.RouterConfig{
		MinContextTokens: 1_000_000, MaxAttemptsPerModel: 1, MaxAttemptsPerRun: 8,
		MaxSwitchesPerRun: 1, RequestTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

// TestChatModelGenerateAndLastContent 覆盖非流式模型调用、用量与最近正文。
func TestChatModelGenerateAndLastContent(t *testing.T) {
	router := newChatModelTestRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	m := NewChatModel(router, nil, 4, 0, 0)
	message, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil || message.Content != "answer" || m.LastContent() != "answer" {
		t.Fatalf("Generate() = %#v, %v, last=%q", message, err, m.LastContent())
	}
	calls, input, output, _ := m.Usage()
	if calls != 1 || input != 3 || output != 2 {
		t.Fatalf("usage = %d/%d/%d", calls, input, output)
	}
}

// TestChatModelSummarizeAfterStepLimit 覆盖步数耗尽后的禁止工具收尾调用。
func TestChatModelSummarizeAfterStepLimit(t *testing.T) {
	router := newChatModelTestRouter(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), "已达到工具调用步数上限") {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"summary"},"finish_reason":"stop"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"first"},"finish_reason":"stop"}]}`)
	})
	m := NewChatModel(router, nil, 1, 0, 0)
	input := []*schema.Message{schema.UserMessage("hi")}
	if _, err := m.Generate(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	message, err := m.Generate(context.Background(), input)
	if err != nil || message.Content != "summary" {
		t.Fatalf("summarize = %#v, %v", message, err)
	}
}

// TestChatModelContinuesTruncatedCompletion 覆盖非流式截断续写。
func TestChatModelContinuesTruncatedCompletion(t *testing.T) {
	calls := 0
	router := newChatModelTestRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"half"},"finish_reason":"length"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"rest"},"finish_reason":"stop"}]}`)
	})
	m := NewChatModel(router, nil, 4, 0, 0)
	message, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil || message.Content != "halfrest" {
		t.Fatalf("continuation = %#v, %v", message, err)
	}
	usedCalls, _, _, _ := m.Usage()
	if usedCalls != 2 {
		t.Fatalf("usage calls = %d", usedCalls)
	}
}

// TestToolBusinessErrorBecomesObservation 验证业务错误转换为可纠正观察结果。
func TestToolBusinessErrorBecomesObservation(t *testing.T) {
	registry := testRegistry{tools: map[string]react.Tool{
		"lookup": testTool{spec: react.ToolSpec{Name: "lookup"}, err: errors.New("boom")},
	}}
	emitter := &testEmitter{}
	adapted := WrapRegistry(registry, nil)[0].(*Tool)
	output, err := adapted.InvokableRun(WithEmitter(context.Background(), emitter), `{}`)
	if err != nil || !strings.Contains(output, `"error":"failed"`) || !strings.Contains(output, "boom") {
		t.Fatalf("business error output = %q, %v", output, err)
	}
	if len(emitter.messages) != 1 || emitter.messages[0]["role"] != "tool" ||
		!strings.Contains(emitter.messages[0]["content"].(string), "boom") {
		t.Fatalf("tool messages = %#v", emitter.messages)
	}
}

// TestLoopGuardRepeatedAndValidation 覆盖重复判定与工具定义校验。
func TestLoopGuardRepeatedAndValidation(t *testing.T) {
	guard := NewLoopGuard(2)
	if guard.Repeated("lookup", `{}`) {
		t.Fatal("first call must not be treated as repeated")
	}
	if !guard.Repeated("lookup", `{}`) {
		t.Fatal("second call must be treated as repeated")
	}
	if err := validateToolSpecs([]react.ToolSpec{{Name: "lookup"}}); err != nil {
		t.Fatalf("valid specs: %v", err)
	}
	if err := validateToolSpecs([]react.ToolSpec{{Name: " "}}); err == nil {
		t.Fatal("empty tool name must be rejected")
	}
}

// TestWrapRegistryAndAgentValidation 覆盖空注册表与非法构建参数。
func TestWrapRegistryAndAgentValidation(t *testing.T) {
	if tools := WrapRegistry(nil, nil); tools != nil {
		t.Fatalf("nil registry wrapped to %d tools", len(tools))
	}
	if _, err := NewReactAgent(context.Background(), ReactConfig{}); err == nil {
		t.Fatal("agent without router must be rejected")
	}
}

// TestNilChatModelAccessors 覆盖空模型的安全访问。
func TestNilChatModelAccessors(t *testing.T) {
	var m *ChatModel
	if calls, input, output, cost := m.Usage(); calls != 0 || input != 0 || output != 0 || cost != 0 {
		t.Fatalf("nil usage = %d/%d/%d/%f", calls, input, output, cost)
	}
	if content := m.LastContent(); content != "" {
		t.Fatalf("nil last content = %q", content)
	}
}

// TestToolInfoRejectsEmptySchema 覆盖空工具定义转换。
func TestToolInfoRejectsEmptySchema(t *testing.T) {
	if _, err := ToolInfoToSpec(nil); err == nil {
		t.Fatal("nil tool info must be rejected")
	}
	if spec, err := ToolInfoToSpec(&schema.ToolInfo{Name: "lookup"}); err != nil || spec.Name != "lookup" {
		t.Fatalf("minimal tool info = %#v, %v", spec, err)
	}
}

// TestToolInfoRoundTripWithSchema 覆盖带 Schema 的工具定义转换。
func TestToolInfoRoundTripWithSchema(t *testing.T) {
	spec := react.ToolSpec{
		Name: "lookup", Description: "lookup",
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
	info, err := SpecToToolInfo(spec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "lookup" || info.ParamsOneOf == nil {
		t.Fatalf("tool info = %#v", info)
	}
}
