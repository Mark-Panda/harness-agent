package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// TestBuiltinToolRegistryDisabled 验证未启用时不注册任何内置工具。
func TestBuiltinToolRegistryDisabled(t *testing.T) {
	if registry := newBuiltinToolRegistry(BuiltinToolConfig{}, nil); registry != nil {
		t.Fatalf("disabled registry = %#v", registry)
	}
}

// TestBuiltinToolIdentityRoundTrip 验证审批目标可稳定还原为 Sandbox 工具名。
func TestBuiltinToolIdentityRoundTrip(t *testing.T) {
	target := builtinToolIdentity(builtinToolShell)
	name, ok := builtinToolName(target)
	if !ok || name != builtinToolShell {
		t.Fatalf("builtinToolName() = %q, %v", name, ok)
	}
	// MCP 审批目标不能被误判为内置工具。
	mcpTarget := target
	mcpTarget.ServerID = uuid.New()
	if _, ok := builtinToolName(mcpTarget); ok {
		t.Fatal("MCP identity was treated as a builtin tool")
	}
}

// TestBuiltinRegistryExposesTools 验证启用后全部工具都进入注册表且风险等级正确。
func TestBuiltinRegistryExposesTools(t *testing.T) {
	registry := newBuiltinToolRegistry(BuiltinToolConfig{
		Shell: BuiltinShellConfig{Enabled: true, AllowLocalExecution: true},
		HTTP:  BuiltinHTTPConfig{Enabled: true, AllowLocalExecution: true},
		Files: BuiltinFilesConfig{Enabled: true, AllowLocalExecution: true},
	}, nil)
	if registry == nil || len(registry.List()) != 8 {
		t.Fatalf("registry = %#v", registry)
	}
	shell, ok := registry.Lookup(builtinToolShell)
	if !ok || shell.RiskLevel() != "high" {
		t.Fatalf("shell tool = %#v", shell)
	}
	httpTool, ok := registry.Lookup(builtinToolHTTP)
	if !ok || httpTool.RiskLevel() != "high" {
		t.Fatalf("http tool = %#v", httpTool)
	}
	readTool, ok := registry.Lookup(builtinToolReadFile)
	if !ok || readTool.RiskLevel() != "low" {
		t.Fatalf("read tool = %#v", readTool)
	}
	writeTool, ok := registry.Lookup(builtinToolWriteFile)
	if !ok || writeTool.RiskLevel() != "medium" {
		t.Fatalf("write tool = %#v", writeTool)
	}
}

// TestShellExecToolRunsCommand 验证本地执行返回退出码与合并输出。
func TestShellExecToolRunsCommand(t *testing.T) {
	tool := &shellExecTool{
		config: BuiltinShellConfig{AllowLocalExecution: true, AllowUnsafeLocalExecution: true, Timeout: 5 * time.Second, MaxOutputBytes: 4096, WorkDir: t.TempDir()},
		shell:  resolveShellBinary(""),
	}
	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":"printf 'hello' && echo world"}`))
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	var result struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Status != "ok" || result.ExitCode != 0 || result.Output != "helloworld\n" {
		t.Fatalf("result = %+v", result)
	}
}

// TestShellExecToolRequiresLocalExecution 验证未开启本机执行时失败关闭。
func TestShellExecToolRequiresLocalExecution(t *testing.T) {
	tool := &shellExecTool{config: BuiltinShellConfig{}, shell: resolveShellBinary("")}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":"echo hi"}`)); !errors.Is(err, ErrBuiltinToolUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

// TestShellExecToolTimeoutIsOutcomeUnknown 验证命令启动后超时必须标记副作用未知。
func TestShellExecToolTimeoutIsOutcomeUnknown(t *testing.T) {
	tool := &shellExecTool{
		config: BuiltinShellConfig{AllowLocalExecution: true, AllowUnsafeLocalExecution: true, Timeout: 100 * time.Millisecond, MaxOutputBytes: 4096, WorkDir: t.TempDir()},
		shell:  resolveShellBinary(""),
	}
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":"sleep 5"}`))
	if !errors.Is(err, ErrToolResultPersistence) {
		t.Fatalf("error = %v", err)
	}
}

// TestHTTPRequestToolRunsRequest 验证本地 HTTP 工具透传方法、请求头与正文。
func TestHTTPRequestToolRunsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", r.Header.Get("X-Test"))
		_, _ = w.Write([]byte(r.Method + ":" + string(body)))
	}))
	defer server.Close()
	tool := &httpRequestTool{config: BuiltinHTTPConfig{AllowLocalExecution: true, Timeout: 5 * time.Second, MaxResponseBytes: 4096}}
	payload, err := json.Marshal(map[string]any{
		"method": "POST", "url": server.URL, "headers": map[string]string{"X-Test": "echo"}, "body": "payload",
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Invoke(context.Background(), payload)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	var result struct {
		StatusCode int                 `json:"status_code"`
		Headers    map[string][]string `json:"headers"`
		Body       string              `json:"body"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.StatusCode != http.StatusOK || result.Body != "POST:payload" || len(result.Headers["X-Echo"]) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

// TestHTTPRequestToolRejectsUnsupportedURL 验证仅允许 http/https。
func TestHTTPRequestToolRejectsUnsupportedURL(t *testing.T) {
	tool := &httpRequestTool{config: BuiltinHTTPConfig{AllowLocalExecution: true, Timeout: time.Second, MaxResponseBytes: 1024}}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`)); err == nil {
		t.Fatal("file URL was accepted")
	}
}

// TestHTTPRequestToolMarksOutcomeUnknownAfterDispatch 验证请求写出后连接中断按“结果未知”处理。
func TestHTTPRequestToolMarksOutcomeUnknownAfterDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijacker is unavailable")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	tool := &httpRequestTool{config: BuiltinHTTPConfig{
		AllowLocalExecution: true, Timeout: 5 * time.Second, MaxResponseBytes: 1024,
	}.normalize()}
	payload := json.RawMessage(fmt.Sprintf(`{"method":"POST","url":%q,"body":"payload"}`, server.URL))
	if _, err := tool.Invoke(context.Background(), payload); !errors.Is(err, ErrToolResultPersistence) {
		t.Fatalf("error = %v", err)
	}
}

// TestHTTPRequestToolConnectionFailureIsNotOutcomeUnknown 验证未写出请求的连接失败可按普通失败回填。
func TestHTTPRequestToolConnectionFailureIsNotOutcomeUnknown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	tool := &httpRequestTool{config: BuiltinHTTPConfig{
		AllowLocalExecution: true, Timeout: time.Second, MaxResponseBytes: 1024,
	}.normalize()}
	payload := json.RawMessage(fmt.Sprintf(`{"url":"http://%s/"}`, address))
	_, err = tool.Invoke(context.Background(), payload)
	if err == nil || errors.Is(err, ErrToolResultPersistence) {
		t.Fatalf("error = %v", err)
	}
}

// TestExecuteApprovedToolRoutesBuiltinToSandbox 验证内置工具审批恢复时按工具名转发 Runner，而不是走 MCP 接口。
func TestExecuteApprovedToolRoutesBuiltinToSandbox(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		toolName string
		risk     string
		args     string
	}{
		{name: "high shell", toolName: builtinToolShell, risk: "high", args: `{"command":"echo hi"}`},
		{name: "medium file edit", toolName: builtinToolEditFile, risk: "medium", args: `{"path":"a.txt","old_string":"a","new_string":"b"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New()}
			stepID := uuid.New()
			target := builtinToolIdentity(testCase.toolName)
			approval := ToolApproval{
				ID: uuid.New(), UserID: run.UserID, RunID: run.ID, StepID: &stepID,
				ToolName: testCase.toolName, ToolCallID: "call_tool", Status: ApprovalApproved,
				Arguments: json.RawMessage(testCase.args), MCPBinding: target, RiskLevel: testCase.risk,
			}
			base := &fakeApprovalRepository{approvals: []ToolApproval{approval}}
			worker := newObservedWorker(&fakeStepRepository{})
			worker.sessions = fakeSessionRepository{}
			worker.runs = stubRunRepository{}
			worker.approvals = base
			worker.approvalDelivery = testAtomicApprovalDelivery{
				sessions: worker.sessions, runs: worker.runs, steps: worker.steps, approvals: worker.approvals,
			}
			runner := &fakeSandboxRunner{output: json.RawMessage(`{"status":"ok"}`)}
			worker.sandbox = runner
			worker.config.RequireSandboxForHighRisk = true
			worker.config.MCPTimeout = 5 * time.Second

			inner := &fakeRuntimeTool{name: testCase.toolName, risk: testCase.risk, output: json.RawMessage(`{"local":true}`)}
			registry := singleToolRegistry{tool: approvalBoundTestTool{Tool: inner, target: target}}
			if err := worker.executeApprovedTool(context.Background(), run, RunFence{}, registry, approval); err != nil {
				t.Fatalf("executeApprovedTool() error = %v", err)
			}
			if runner.calls != 1 || runner.request.Tool != testCase.toolName || runner.request.MCP != nil {
				t.Fatalf("sandbox request = %+v", runner.request)
			}
			if runner.request.UserID == nil || *runner.request.UserID != run.UserID ||
				runner.request.RunID == nil || *runner.request.RunID != run.ID {
				t.Fatalf("sandbox identity = %+v, want user %s run %s",
					runner.request, run.UserID, run.ID)
			}
			if inner.calls != 0 {
				t.Fatal("builtin tool executed in process while sandbox is enforced")
			}
			if got := base.approvals[0]; got.Status != ApprovalExecuted || got.DeliveredAt == nil {
				t.Fatalf("approval = %+v", got)
			}
		})
	}
}

// TestInvokeToolRoutesLowRiskBuiltinToSandbox 验证低风险内置工具在强制沙箱下也不会进程内执行。
func TestInvokeToolRoutesLowRiskBuiltinToSandbox(t *testing.T) {
	runner := &fakeSandboxRunner{output: json.RawMessage(`{"sandbox":true}`)}
	worker := &ExecutionWorker{
		sandbox: runner, config: WorkerConfig{RequireSandboxForHighRisk: true, MCPTimeout: time.Second}.normalize(),
	}
	inner := &fakeRuntimeTool{name: builtinToolReadFile, risk: "low", output: json.RawMessage(`{"local":true}`)}
	tool := approvalBoundTestTool{Tool: inner, target: builtinToolIdentity(builtinToolReadFile)}
	run := Run{ID: uuid.New(), UserID: uuid.New()}
	ctx := withRunToolIdentity(context.Background(), run)
	output, err := worker.invokeTool(ctx, tool, json.RawMessage(`{"path":"README.md"}`))
	if err != nil || string(output) != `{"sandbox":true}` || inner.calls != 0 {
		t.Fatalf("output=%s err=%v localCalls=%d", output, err, inner.calls)
	}
	if runner.request.Tool != builtinToolReadFile {
		t.Fatalf("sandbox request = %+v", runner.request)
	}
	if runner.request.UserID == nil || *runner.request.UserID != run.UserID ||
		runner.request.RunID == nil || *runner.request.RunID != run.ID {
		t.Fatalf("sandbox request is missing run identity: %+v", runner.request)
	}
}

// TestInvokeToolRequiresBuiltinIdentity 验证强制沙箱下内置工具缺少用户维度时失败关闭。
func TestInvokeToolRequiresBuiltinIdentity(t *testing.T) {
	runner := &fakeSandboxRunner{output: json.RawMessage(`{"sandbox":true}`)}
	worker := &ExecutionWorker{
		sandbox: runner, config: WorkerConfig{RequireSandboxForHighRisk: true, MCPTimeout: time.Second}.normalize(),
	}
	inner := &fakeRuntimeTool{name: builtinToolReadFile, risk: "low", output: json.RawMessage(`{"local":true}`)}
	tool := approvalBoundTestTool{Tool: inner, target: builtinToolIdentity(builtinToolReadFile)}
	if _, err := worker.invokeTool(context.Background(), tool, json.RawMessage(`{"path":"README.md"}`)); !errors.Is(err, ErrBuiltinIdentityMissing) {
		t.Fatalf("error = %v", err)
	}
	if runner.calls != 0 || inner.calls != 0 {
		t.Fatalf("runner calls=%d local calls=%d", runner.calls, inner.calls)
	}
}

// TestBuiltinUserWorkspaceIsolation 验证不同用户派生出不同的工作区。
func TestBuiltinUserWorkspaceIsolation(t *testing.T) {
	base := t.TempDir()
	userA, userB := uuid.New(), uuid.New()
	rootA := builtinUserWorkspace(base, userA)
	rootB := builtinUserWorkspace(base, userB)
	if rootA == rootB || filepath.Base(rootA) != userA.String() || filepath.Base(rootB) != userB.String() {
		t.Fatalf("workspaces are not isolated: %q %q", rootA, rootB)
	}
	if !strings.HasPrefix(rootA, base+string(filepath.Separator)) {
		t.Fatalf("workspace %q is outside base %q", rootA, base)
	}
	defaultA := builtinUserWorkspace("", userA)
	defaultB := builtinUserWorkspace("", userB)
	if defaultA == defaultB || !strings.HasSuffix(defaultA, userA.String()) {
		t.Fatalf("default workspaces are not isolated: %q %q", defaultA, defaultB)
	}
}

// TestPrepareBuiltinToolsIsolatesUsers 验证注册内置工具时按用户重写文件根与 Shell 工作目录。
func TestPrepareBuiltinToolsIsolatesUsers(t *testing.T) {
	base := t.TempDir()
	worker := &ExecutionWorker{config: WorkerConfig{BuiltinTools: BuiltinToolConfig{
		Files: BuiltinFilesConfig{
			Enabled: true, AllowLocalExecution: true, Root: base,
			MaxReadBytes: 1024, MaxWriteBytes: 1024, MaxResults: 10,
		},
		Shell: BuiltinShellConfig{
			Enabled: true, AllowLocalExecution: true, WorkDir: base,
			Timeout: time.Second, MaxOutputBytes: 1024,
		},
	}}.normalize()}
	for _, userID := range []uuid.UUID{uuid.New(), uuid.New()} {
		registry := worker.prepareBuiltinTools(Run{ID: uuid.New(), UserID: userID})
		fileToolRaw, ok := registry.Lookup(builtinToolReadFile)
		if !ok {
			t.Fatal("fs_read_file is missing")
		}
		fileToolValue, ok := fileToolRaw.(*fileTool)
		if !ok || fileToolValue.config.Root != filepath.Join(base, userID.String()) {
			t.Fatalf("file root = %+v", fileToolRaw)
		}
		shellRaw, ok := registry.Lookup(builtinToolShell)
		if !ok {
			t.Fatal("shell_exec is missing")
		}
		shell, ok := shellRaw.(*shellExecTool)
		if !ok || shell.config.WorkDir != filepath.Join(base, userID.String()) {
			t.Fatalf("shell workdir = %+v", shellRaw)
		}
	}
	if registry := worker.prepareBuiltinTools(Run{}); registry != nil {
		t.Fatalf("nil user registry = %#v", registry)
	}
}

// TestPrepareBuiltinToolsHonorsAgentBinding 验证 Agent 发布快照可以收窄内置工具集合。
func TestPrepareBuiltinToolsHonorsAgentBinding(t *testing.T) {
	base := t.TempDir()
	worker := &ExecutionWorker{config: WorkerConfig{BuiltinTools: BuiltinToolConfig{
		Files: BuiltinFilesConfig{
			Enabled: true, AllowLocalExecution: true, Root: base,
			MaxReadBytes: 1024, MaxWriteBytes: 1024, MaxResults: 10,
		},
		Shell: BuiltinShellConfig{
			Enabled: true, AllowLocalExecution: true, WorkDir: base,
			Timeout: time.Second, MaxOutputBytes: 1024,
		},
	}}.normalize()}
	userID := uuid.New()
	snapshot := func(tools any) map[string]any {
		return map[string]any{"published_snapshot": map[string]any{"builtin_tools": tools}}
	}
	restricted := worker.prepareBuiltinTools(Run{
		ID: uuid.New(), UserID: userID,
		ConfigSnapshot: snapshot([]any{builtinToolReadFile, "unknown_tool"}),
	})
	if restricted == nil || len(restricted.List()) != 1 {
		t.Fatalf("restricted registry = %#v", restricted)
	}
	if _, ok := restricted.Lookup(builtinToolReadFile); !ok {
		t.Fatal("bound tool is missing")
	}
	if _, ok := restricted.Lookup(builtinToolShell); ok {
		t.Fatal("unbound tool is exposed")
	}

	if registry := worker.prepareBuiltinTools(Run{
		ID: uuid.New(), UserID: userID, ConfigSnapshot: snapshot([]any{}),
	}); registry != nil {
		t.Fatalf("explicit empty binding = %#v", registry)
	}
	// 字段缺失或为 null 时继承平台全部启用工具。
	for name, config := range map[string]map[string]any{
		"missing": {},
		"null":    snapshot(nil),
	} {
		registry := worker.prepareBuiltinTools(Run{ID: uuid.New(), UserID: userID, ConfigSnapshot: config})
		if registry == nil || len(registry.List()) != 7 {
			t.Fatalf("%s binding registry = %#v", name, registry)
		}
	}
}

// TestHTTPRequestToolCapsResponseHeaders 验证响应头数量与大小受控并标记截断。
func TestHTTPRequestToolCapsResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for index := 0; index < maxBuiltinHTTPHeaders+20; index++ {
			w.Header().Set(fmt.Sprintf("X-Header-%03d", index), "value")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	tool := &httpRequestTool{config: BuiltinHTTPConfig{
		AllowLocalExecution: true, Timeout: 5 * time.Second, MaxResponseBytes: 1024,
	}.normalize()}
	output, err := tool.Invoke(context.Background(), json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL)))
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	var result struct {
		Headers          map[string][]string `json:"headers"`
		HeadersTruncated bool                `json:"headers_truncated"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if !result.HeadersTruncated || len(result.Headers) > maxBuiltinHTTPHeaders {
		t.Fatalf("headers=%d truncated=%v", len(result.Headers), result.HeadersTruncated)
	}
}

// TestFileToolRejectsBinaryRead 验证读取二进制文件时失败关闭。
func TestFileToolRejectsBinaryRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.bin"), []byte{0x00, 0x01, 0x02}, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &fileTool{name: builtinToolReadFile, config: BuiltinFilesConfig{
		AllowLocalExecution: true, Root: root, MaxReadBytes: 4096, MaxWriteBytes: 4096, MaxResults: 10,
	}.normalize()}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"data.bin"}`)); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("error = %v", err)
	}
}

// TestConcurrentFileEditsDoNotLoseUpdates 验证并发编辑同一文件不会互相覆盖。
func TestConcurrentFileEditsDoNotLoseUpdates(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "data.txt")
	if err := os.WriteFile(target, []byte("one two"), 0o600); err != nil {
		t.Fatal(err)
	}
	newTool := func() *fileTool {
		return &fileTool{name: builtinToolEditFile, config: BuiltinFilesConfig{
			AllowLocalExecution: true, Root: root, MaxReadBytes: 4096, MaxWriteBytes: 4096, MaxResults: 10,
		}.normalize()}
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, edit := range [][2]string{{"one", "1"}, {"two", "2"}} {
		wait.Add(1)
		go func(oldValue, newValue string) {
			defer wait.Done()
			<-start
			payload, _ := json.Marshal(map[string]string{"path": "data.txt", "old_string": oldValue, "new_string": newValue})
			_, err := newTool().Invoke(context.Background(), payload)
			errs <- err
		}(edit[0], edit[1])
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("edit error = %v", err)
		}
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "1 2" {
		t.Fatalf("content = %q, %v", content, err)
	}
}

// TestToolExecutionTimeoutUsesPerToolConfig 验证内置工具使用各自超时并允许模型请求更短时间。
func TestToolExecutionTimeoutUsesPerToolConfig(t *testing.T) {
	worker := &ExecutionWorker{config: WorkerConfig{
		MCPTimeout: 20 * time.Second,
		BuiltinTools: BuiltinToolConfig{
			Shell: BuiltinShellConfig{Enabled: true, AllowLocalExecution: true, Timeout: 120 * time.Second, MaxOutputBytes: 1024},
			HTTP:  BuiltinHTTPConfig{Enabled: true, AllowLocalExecution: true, Timeout: 60 * time.Second, MaxResponseBytes: 1024},
		},
	}.normalize()}
	shell := &shellExecTool{config: worker.config.BuiltinTools.Shell.normalize(), shell: resolveShellBinary("")}
	if got := worker.toolExecutionTimeout(shell, json.RawMessage(`{"command":"echo"}`)); got != 120*time.Second {
		t.Fatalf("shell timeout = %v", got)
	}
	if got := worker.toolExecutionTimeout(shell, json.RawMessage(`{"command":"echo","timeout_seconds":5}`)); got != 5*time.Second {
		t.Fatalf("short shell timeout = %v", got)
	}
	httpTool := &httpRequestTool{config: worker.config.BuiltinTools.HTTP.normalize()}
	if got := worker.toolExecutionTimeout(httpTool, json.RawMessage(`{"url":"http://127.0.0.1"}`)); got != 60*time.Second {
		t.Fatalf("http timeout = %v", got)
	}
	mcpTool := &fakeRuntimeTool{name: "remote_tool", risk: "high"}
	if got := worker.toolExecutionTimeout(mcpTool, json.RawMessage(`{"timeout_seconds":1}`)); got != 20*time.Second {
		t.Fatalf("mcp timeout = %v", got)
	}
}

// TestInvokeToolSendsConfiguredSandboxTimeout 验证内置工具把自身超时传给 Sandbox Runner。
func TestInvokeToolSendsConfiguredSandboxTimeout(t *testing.T) {
	runner := &fakeSandboxRunner{output: json.RawMessage(`{"ok":true}`)}
	worker := &ExecutionWorker{sandbox: runner, config: WorkerConfig{
		RequireSandboxForHighRisk: true, MCPTimeout: 20 * time.Second,
		BuiltinTools: BuiltinToolConfig{
			Shell: BuiltinShellConfig{Enabled: true, AllowLocalExecution: true, Timeout: 120 * time.Second, MaxOutputBytes: 1024},
		},
	}.normalize()}
	inner := &fakeRuntimeTool{name: builtinToolShell, risk: "high", output: json.RawMessage(`{"local":true}`)}
	tool := approvalBoundTestTool{Tool: inner, target: builtinToolIdentity(builtinToolShell)}
	ctx := withRunToolIdentity(context.Background(), Run{ID: uuid.New(), UserID: uuid.New()})
	if _, err := worker.invokeTool(ctx, tool, json.RawMessage(`{"command":"echo"}`)); err != nil {
		t.Fatalf("invokeTool() error = %v", err)
	}
	if runner.request.TimeoutMS != (120 * time.Second).Milliseconds() {
		t.Fatalf("timeout_ms = %d", runner.request.TimeoutMS)
	}
}

// TestShellExecToolRequiresUnsafeAck 验证仅开启本机执行但未显式确认风险时仍失败关闭。
func TestShellExecToolRequiresUnsafeAck(t *testing.T) {
	tool := &shellExecTool{config: BuiltinShellConfig{AllowLocalExecution: true, Timeout: time.Second, MaxOutputBytes: 1024}, shell: resolveShellBinary("")}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":"echo hi"}`)); !errors.Is(err, ErrBuiltinToolUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

// cancelAfterContext 在第 N 次 Err 调用后返回取消，用于验证目录遍历中途响应取消。
type cancelAfterContext struct {
	context.Context
	remaining atomic.Int32
}

// Err 递减计数并在耗尽后返回取消错误。
func (c *cancelAfterContext) Err() error {
	if c.remaining.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// TestSearchToolsCancelDuringWalk 验证 glob/grep 在遍历途中响应取消，而不是扫完整个目录。
func TestSearchToolsCancelDuringWalk(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 20; index++ {
		name := filepath.Join(root, fmt.Sprintf("file-%02d.txt", index))
		if err := os.WriteFile(name, []byte("hello harness"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct {
		name string
		args string
	}{
		{name: builtinToolGlob, args: `{"pattern":"**/*.txt"}`},
		{name: builtinToolGrep, args: `{"pattern":"harness"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tool := &fileTool{name: testCase.name, config: BuiltinFilesConfig{
				AllowLocalExecution: true, Root: root,
				MaxReadBytes: 4096, MaxWriteBytes: 4096, MaxResults: 50,
			}.normalize()}
			ctx := &cancelAfterContext{Context: context.Background()}
			ctx.remaining.Store(3)
			if _, err := tool.Invoke(ctx, json.RawMessage(testCase.args)); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// TestWriteFilePreservesMode 验证覆盖文件保留原权限，新文件使用默认 0644。
func TestWriteFilePreservesMode(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "run.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := &fileTool{name: builtinToolWriteFile, config: BuiltinFilesConfig{
		AllowLocalExecution: true, Root: root, MaxReadBytes: 4096, MaxWriteBytes: 4096, MaxResults: 10,
	}.normalize()}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"run.sh","content":"#!/bin/sh\necho hi\n"}`)); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing mode = %v, %v", info.Mode().Perm(), err)
	}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"new.txt","content":"x"}`)); err != nil {
		t.Fatalf("write new: %v", err)
	}
	info, err = os.Stat(filepath.Join(root, "new.txt"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("new mode = %v, %v", info.Mode().Perm(), err)
	}
}

// 编译期断言内置工具实现 react.Tool 与审批目标接口。
var (
	_ react.Tool = (*shellExecTool)(nil)
	_ react.Tool = (*httpRequestTool)(nil)
	_ react.Tool = (*fileTool)(nil)
)
