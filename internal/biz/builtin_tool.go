package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/google/uuid"
)

// 内置工具名称；模型侧通过这两个名字发起调用。
const (
	builtinToolShell = "shell_exec"
	builtinToolHTTP  = "http_request"
)

// 内置工具默认护栏。
const (
	defaultBuiltinShellTimeout  = 30 * time.Second
	defaultBuiltinShellOutput   = 64 << 10
	defaultBuiltinHTTPTimeout   = 30 * time.Second
	defaultBuiltinHTTPResponse  = 1024 * 1024
	defaultBuiltinHTTPRedirects = 5
	maxBuiltinConcurrentShells  = 2
	maxBuiltinHTTPHeaders       = 64
	maxBuiltinHTTPHeaderBytes   = 8 << 10
	maxBuiltinTimeoutSeconds    = 300
	// maxBuiltinConcurrentHTTPRequests 单实例并发 HTTP 工具上限。
	maxBuiltinConcurrentHTTPRequests = 4
	// maxBuiltinConcurrentFileOperations 单实例并发文件工具上限。
	maxBuiltinConcurrentFileOperations = 4
)

// 内置 HTTP 与文件工具为进程级并发舱壁，避免单轮并行调用打满资源。
var (
	builtinHTTPSem = make(chan struct{}, maxBuiltinConcurrentHTTPRequests)
	builtinFileSem = make(chan struct{}, maxBuiltinConcurrentFileOperations)
)

// acquireBuiltinSemaphore 获取并发槽位并响应上下文取消。
func acquireBuiltinSemaphore(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrBuiltinToolUnavailable 表示内置工具在当前环境不可执行。
var ErrBuiltinToolUnavailable = errors.New("builtin tool is unavailable in this environment")

// ErrBuiltinIdentityMissing 表示内置工具缺少运行身份，无法安全转发到 Sandbox。
var ErrBuiltinIdentityMissing = errors.New("builtin tool requires run identity")

// BuiltinToolNames 返回平台支持的内置工具名称清单。
func BuiltinToolNames() []string {
	return []string{
		builtinToolShell, builtinToolHTTP,
		builtinToolReadFile, builtinToolWriteFile, builtinToolEditFile,
		builtinToolListDir, builtinToolGlob, builtinToolGrep,
	}
}

// builtinToolNameSet 用于 Agent 绑定校验，拒绝未知工具名。
var builtinToolNameSet = func() map[string]struct{} {
	names := BuiltinToolNames()
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

// IsBuiltinToolName 判断名称是否为平台内置工具。
func IsBuiltinToolName(name string) bool {
	_, ok := builtinToolNameSet[name]
	return ok
}

// BuiltinToolConfig 是内置 shell、HTTP、文件与检索工具的启用与执行护栏。
type BuiltinToolConfig struct {
	// Shell shell_exec 配置。
	Shell BuiltinShellConfig
	// HTTP http_request 配置。
	HTTP BuiltinHTTPConfig
	// Files 文件操作与代码检索配置。
	Files BuiltinFilesConfig
}

// BuiltinShellConfig 是 shell_exec 的本机执行配置。
type BuiltinShellConfig struct {
	// Enabled 是否向模型暴露 shell_exec。
	Enabled bool
	// AllowLocalExecution 是否允许在本机进程执行命令；仅开发环境可开启。
	AllowLocalExecution bool
	// AllowUnsafeLocalExecution 显式确认本机 Shell 共享服务进程用户与密钥环境。
	AllowUnsafeLocalExecution bool
	// Timeout 单次命令默认超时。
	Timeout time.Duration
	// MaxOutputBytes 单次输出截断上限。
	MaxOutputBytes int
	// WorkDir 命令工作目录的基础路径；运行时按用户 ID 派生子目录，为空时使用系统临时目录。
	WorkDir string
	// ShellPath 指定 Shell 解释器；为空时优先 bash，其次 sh。
	ShellPath string
}

// normalize 补齐 shell_exec 默认值。
func (c BuiltinShellConfig) normalize() BuiltinShellConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultBuiltinShellTimeout
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = defaultBuiltinShellOutput
	}
	return c
}

// BuiltinHTTPConfig 是 http_request 的本机执行配置。
type BuiltinHTTPConfig struct {
	// Enabled 是否向模型暴露 http_request。
	Enabled bool
	// AllowLocalExecution 是否允许在本机进程发起请求；仅开发环境可开启。
	AllowLocalExecution bool
	// Timeout 单次请求默认超时。
	Timeout time.Duration
	// MaxResponseBytes 响应正文保留上限。
	MaxResponseBytes int64
	// MaxRedirects 最大重定向次数；零值使用默认 5。
	MaxRedirects int
}

// normalize 补齐 http_request 默认值。
func (c BuiltinHTTPConfig) normalize() BuiltinHTTPConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultBuiltinHTTPTimeout
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = defaultBuiltinHTTPResponse
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = defaultBuiltinHTTPRedirects
	}
	return c
}

// builtinToolServerID 是内置工具在审批表中的稳定服务标识，与 MCP 服务隔离。
var builtinToolServerID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("harness-builtin-tool"))

// builtinToolIdentity 生成内置工具的审批目标；名称即 Sandbox Runner 的工具名。
func builtinToolIdentity(name string) mcp.ToolIdentity {
	digest := sha256.Sum256([]byte("harness-builtin-tool:" + name))
	return mcp.ToolIdentity{
		ServerID: builtinToolServerID, OriginalName: name, EndpointFingerprint: hex.EncodeToString(digest[:16]),
	}
}

// builtinToolName 判断审批目标是否属于内置工具，并返回 Sandbox Runner 工具名。
func builtinToolName(identity mcp.ToolIdentity) (string, bool) {
	if identity.ServerID != builtinToolServerID || strings.TrimSpace(identity.OriginalName) == "" {
		return "", false
	}
	return identity.OriginalName, true
}

// builtinToolRegistry 是内置工具注册表，实现 react.Registry。
type builtinToolRegistry struct {
	bindings map[string]react.Tool
	specs    []react.ToolSpec
}

// newBuiltinToolRegistry 按配置注册内置工具；全部关闭时返回 nil。
func newBuiltinToolRegistry(config BuiltinToolConfig, shellSem chan struct{}) react.Registry {
	registry := &builtinToolRegistry{bindings: map[string]react.Tool{}}
	add := func(tool react.Tool) {
		registry.bindings[tool.Spec().Name] = tool
		registry.specs = append(registry.specs, tool.Spec())
	}
	if config.Shell.Enabled {
		if config.Shell.AllowLocalExecution {
			ensureBuiltinWorkdir(config.Shell.WorkDir)
		}
		add(&shellExecTool{config: config.Shell.normalize(), sem: shellSem, shell: resolveShellBinary(config.Shell.ShellPath)})
	}
	if config.HTTP.Enabled {
		add(&httpRequestTool{config: config.HTTP.normalize()})
	}
	if config.Files.Enabled {
		if config.Files.AllowLocalExecution {
			ensureBuiltinWorkdir(config.Files.Root)
		}
		filesConfig := config.Files.normalize()
		for _, name := range []string{
			builtinToolReadFile, builtinToolWriteFile, builtinToolEditFile,
			builtinToolListDir, builtinToolGlob, builtinToolGrep,
		} {
			add(&fileTool{name: name, config: filesConfig})
		}
	}
	if len(registry.specs) == 0 {
		return nil
	}
	return registry
}

// ensureBuiltinWorkdir 在本地开发模式下创建用户隔离工作目录；创建失败时由工具调用失败关闭。
func ensureBuiltinWorkdir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	// 使用 0700，避免同宿主上的其他系统账号读取用户工作区。
	_ = os.MkdirAll(dir, 0o700)
}

// Lookup 按名称查找内置工具。
func (r *builtinToolRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.bindings[name]
	return tool, ok
}

// List 返回全部内置工具定义。
func (r *builtinToolRegistry) List() []react.ToolSpec {
	return append([]react.ToolSpec(nil), r.specs...)
}

// prepareBuiltinTools 按 Worker 配置注册内置工具。
// 本机执行时按用户派生独立工作区，禁止不同用户共享同一目录。
func (w *ExecutionWorker) prepareBuiltinTools(run Run) react.Registry {
	if run.UserID == uuid.Nil {
		return nil
	}
	config := w.config.BuiltinTools
	if config.Files.AllowLocalExecution {
		config.Files.Root = builtinUserWorkspace(config.Files.Root, run.UserID)
	}
	if config.Shell.AllowLocalExecution {
		config.Shell.WorkDir = builtinUserWorkspace(config.Shell.WorkDir, run.UserID)
	}
	registry := newBuiltinToolRegistry(config, w.builtinExecSem)
	allowed, restricted := snapshotBuiltinTools(run)
	if !restricted {
		// 历史 Agent 快照没有 builtin_tools 字段，继承平台全部启用工具。
		return registry
	}
	return filterBuiltinRegistry(registry, allowed)
}

// snapshotBuiltinTools 读取 Agent 发布快照中的内置工具绑定。
// 字段缺失或为 null：继承平台配置；空数组：显式禁用全部；数组：仅启用列出的工具。
func snapshotBuiltinTools(run Run) ([]string, bool) {
	root := run.ConfigSnapshot
	if nested, ok := root["published_snapshot"].(map[string]any); ok {
		root = nested
	}
	raw, exists := root["builtin_tools"]
	if !exists || raw == nil {
		return nil, false
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		names := make([]string, 0, len(values))
		for _, value := range values {
			if name, ok := value.(string); ok {
				names = append(names, name)
			}
		}
		return names, true
	default:
		// 快照结构非法时按禁用全部处理，避免意外暴露未绑定工具。
		return nil, true
	}
}

// filterBuiltinRegistry 仅保留 Agent 显式绑定的内置工具。
func filterBuiltinRegistry(registry react.Registry, allowed []string) react.Registry {
	if registry == nil {
		return nil
	}
	bindings := make(map[string]react.Tool, len(allowed))
	specs := make([]react.ToolSpec, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		tool, ok := registry.Lookup(name)
		if !ok {
			continue
		}
		bindings[name] = tool
		specs = append(specs, tool.Spec())
	}
	if len(specs) == 0 {
		return nil
	}
	return &builtinToolRegistry{bindings: bindings, specs: specs}
}

// builtinUserWorkspace 为每个用户派生隔离工作区。
// base 是平台配置的基础目录；为空时使用系统临时目录，避免回退到进程工作目录造成跨用户共享。
func builtinUserWorkspace(base string, userID uuid.UUID) string {
	root := strings.TrimSpace(base)
	if root == "" {
		root = filepath.Join(os.TempDir(), "harness-agent-users")
	}
	return filepath.Join(root, userID.String())
}

// shellExecTool 执行 Shell 命令；高风险，调用前必须经过人工审批。
type shellExecTool struct {
	config BuiltinShellConfig
	sem    chan struct{}
	shell  string
}

// Spec 返回 shell_exec 的工具定义。
func (t *shellExecTool) Spec() react.ToolSpec {
	return react.ToolSpec{
		Name:        builtinToolShell,
		Description: "在服务端受控 Shell 中执行命令，返回退出码与合并后的标准输出/错误输出（高风险，需要人工审批）。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 Shell 命令，支持管道与重定向"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"执行超时秒数，不能超过服务端配置上限"}},"required":["command"],"additionalProperties":false}`),
	}
}

// RiskLevel 返回 high：任意命令执行必须强制人工审批并禁止在共享群聊中使用。
func (t *shellExecTool) RiskLevel() string { return "high" }

// MCPIdentity 提供稳定的审批目标，恢复审批时据此重新绑定工具。
func (t *shellExecTool) MCPIdentity() mcp.ToolIdentity { return builtinToolIdentity(builtinToolShell) }

// shellExecArguments 是 shell_exec 入参。
type shellExecArguments struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// Invoke 在本机执行命令；启动后的超时或取消表示副作用未知，必须进入人工对账。
func (t *shellExecTool) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var request shellExecArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	commandText := strings.TrimSpace(request.Command)
	if commandText == "" {
		return nil, fmt.Errorf("command is required")
	}
	if !t.config.AllowLocalExecution {
		return nil, fmt.Errorf("%w: local shell execution is disabled", ErrBuiltinToolUnavailable)
	}
	if !t.config.AllowUnsafeLocalExecution {
		// 本机 Shell 与服务共享同一 OS 用户，可读取 /proc/1/environ 等密钥；
		// 只有显式确认的可信单用户开发环境才允许。
		return nil, fmt.Errorf("%w: local shell execution requires allow_unsafe_local_execution", ErrBuiltinToolUnavailable)
	}
	if t.config.WorkDir != "" {
		info, err := os.Stat(t.config.WorkDir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("shell workdir is unavailable: %s", t.config.WorkDir)
		}
	}
	timeout := t.config.Timeout
	if request.TimeoutSeconds > 0 {
		requested := time.Duration(request.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout > maxBuiltinTimeoutSeconds*time.Second {
		timeout = maxBuiltinTimeoutSeconds * time.Second
	}
	if t.sem != nil {
		select {
		case t.sem <- struct{}{}:
			defer func() { <-t.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, t.shell, "-c", commandText)
	configureShellProcessGroup(command)
	// 超时或取消时杀掉整个进程组；默认实现只会杀直接子进程。
	command.Cancel = func() error { return killShellProcessGroup(command) }
	command.Dir = t.config.WorkDir
	command.Env = builtinShellEnv(t.config.WorkDir)
	// Shell 的子进程可能继承输出管道；设置有界等待，避免取消后仍阻塞到孙进程退出。
	command.WaitDelay = 100 * time.Millisecond
	output := &truncatingBuffer{limit: t.config.MaxOutputBytes}
	command.Stdout = output
	command.Stderr = output

	started := time.Now()
	startErr := command.Start()
	if startErr != nil {
		// 解释器未启动时不存在命令副作用，维持确定性的 unavailable 结果。
		return json.Marshal(map[string]any{
			"status": "unavailable", "exit_code": -1, "duration_ms": time.Since(started).Milliseconds(),
			"truncated": output.Truncated(), "output": strings.ToValidUTF8(output.String(), "\uFFFD"),
			"error": startErr.Error(),
		})
	}
	// 命令正常结束后也可能留下后台子进程；统一回收整个进程组，避免残留守护进程。
	defer func() { _ = killShellProcessGroup(command) }()
	runErr := command.Wait()
	result := map[string]any{
		"status": "ok", "exit_code": 0, "duration_ms": time.Since(started).Milliseconds(),
		"truncated": output.Truncated(), "output": strings.ToValidUTF8(output.String(), "\uFFFD"),
	}
	if runErr != nil {
		switch {
		case runCtx.Err() != nil:
			status := "cancelled"
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				status = "timeout"
			}
			result["status"] = status
			result["exit_code"] = -1
			result["error"] = runErr.Error()
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return nil, errors.Join(ErrToolResultPersistence, runCtx.Err(), marshalErr)
			}
			return encoded, fmt.Errorf("%w: shell command %s after start; side effects are unknown: %w",
				ErrToolResultPersistence, status, runCtx.Err())
		default:
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				result["status"] = "failed"
				result["exit_code"] = exitErr.ExitCode()
			} else {
				result["status"] = "unavailable"
				result["exit_code"] = -1
				result["error"] = runErr.Error()
			}
		}
	}
	return json.Marshal(result)
}

// resolveShellBinary 解析 Shell 解释器；默认优先 bash，容器内无 bash 时回退 sh。
func resolveShellBinary(configured string) string {
	if trimmed := strings.TrimSpace(configured); trimmed != "" {
		return trimmed
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	if path, err := exec.LookPath("sh"); err == nil {
		return path
	}
	return "/bin/sh"
}

// builtinShellEnv 只透传白名单环境变量，避免把服务端凭证泄露给命令。
func builtinShellEnv(workDir string) []string {
	pathValue := os.Getenv("PATH")
	if strings.TrimSpace(pathValue) == "" {
		pathValue = "/usr/local/bin:/usr/bin:/bin"
	}
	lang := os.Getenv("LANG")
	if strings.TrimSpace(lang) == "" {
		lang = "C.UTF-8"
	}
	home := strings.TrimSpace(workDir)
	if home == "" {
		home = os.TempDir()
	}
	return []string{
		"PATH=" + pathValue,
		"LANG=" + lang,
		"LC_ALL=" + lang,
		"TZ=Asia/Shanghai",
		"HOME=" + home,
		"TMPDIR=" + os.TempDir(),
	}
}

// httpRequestTool 发起 HTTP 请求；高风险，调用前必须经过人工审批。
type httpRequestTool struct {
	config BuiltinHTTPConfig
}

// Spec 返回 http_request 的工具定义。
func (t *httpRequestTool) Spec() react.ToolSpec {
	return react.ToolSpec{
		Name:        builtinToolHTTP,
		Description: "发起一次 HTTP/HTTPS 请求并返回状态码、响应头与正文（高风险，需要人工审批）。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"method":{"type":"string","description":"HTTP 方法，默认 GET","enum":["GET","POST","PUT","PATCH","DELETE","HEAD","OPTIONS"]},"url":{"type":"string","description":"请求 URL，仅支持 http/https"},"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"请求头键值对"},"body":{"type":"string","description":"请求正文"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"请求超时秒数"}},"required":["url"],"additionalProperties":false}`),
	}
}

// RiskLevel 返回 high：外发请求可能携带数据，必须人工审批。
func (t *httpRequestTool) RiskLevel() string { return "high" }

// MCPIdentity 提供稳定的审批目标，恢复审批时据此重新绑定工具。
func (t *httpRequestTool) MCPIdentity() mcp.ToolIdentity { return builtinToolIdentity(builtinToolHTTP) }

// httpRequestArguments 是 http_request 入参。
type httpRequestArguments struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// Invoke 在本机发起 HTTP 请求；超时或取消可能已产生副作用，必须进入人工对账。
func (t *httpRequestTool) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var request httpRequestArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if !t.config.AllowLocalExecution {
		return nil, fmt.Errorf("%w: local http execution is disabled", ErrBuiltinToolUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := acquireBuiltinSemaphore(ctx, builtinHTTPSem); err != nil {
		return nil, err
	}
	defer func() { <-builtinHTTPSem }()
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return nil, fmt.Errorf("unsupported http method: %s", method)
	}
	target, err := url.Parse(strings.TrimSpace(request.URL))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("url must be a valid http(s) URL")
	}
	if len(request.Headers) > maxBuiltinHTTPHeaders {
		return nil, fmt.Errorf("too many request headers: %d", len(request.Headers))
	}
	var headerBytes int
	for name, value := range request.Headers {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("request header is invalid")
		}
		headerBytes += len(name) + len(value)
	}
	if headerBytes > maxBuiltinHTTPHeaderBytes {
		return nil, fmt.Errorf("request headers exceed the size limit")
	}
	timeout := t.config.Timeout
	if request.TimeoutSeconds > 0 {
		requested := time.Duration(request.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout > maxBuiltinTimeoutSeconds*time.Second {
		timeout = maxBuiltinTimeoutSeconds * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 请求一旦开始写出，服务端就可能已经产生副作用；后续网络错误必须标记结果未知，
	// 避免模型把非幂等请求当作普通失败重试。
	var wroteRequest atomic.Bool
	runCtx = httptrace.WithClientTrace(runCtx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	})
	var body io.Reader
	if request.Body != "" {
		body = strings.NewReader(request.Body)
	}
	httpRequest, err := http.NewRequestWithContext(runCtx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("invalid http request: %w", err)
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}
	client := &http.Client{
		Timeout: timeout, Transport: builtinHTTPTransport(),
		// 限制重定向次数；超过后返回最后一次响应，由模型自行决定下一步。
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= t.config.MaxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	started := time.Now()
	response, err := client.Do(httpRequest)
	if err != nil {
		if wroteRequest.Load() {
			return nil, fmt.Errorf("%w: http request %s after dispatch; side effects are unknown: %w",
				ErrToolResultPersistence, method, err)
		}
		if runCtx.Err() != nil {
			// 请求尚未写出，取消或超时没有产生服务端副作用。
			return nil, runCtx.Err()
		}
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, t.config.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read http response after start; side effects are unknown: %w", ErrToolResultPersistence, err)
	}
	truncated := int64(len(payload)) > t.config.MaxResponseBytes
	if truncated {
		payload = payload[:t.config.MaxResponseBytes]
	}
	headers := make(map[string][]string, len(response.Header))
	responseHeaderBytes := 0
	headersTruncated := false
	for name, values := range response.Header {
		if len(headers) >= maxBuiltinHTTPHeaders {
			headersTruncated = true
			break
		}
		for _, value := range values {
			responseHeaderBytes += len(name) + len(value)
		}
		if responseHeaderBytes > maxBuiltinHTTPHeaderBytes {
			headersTruncated = true
			break
		}
		headers[name] = values
	}
	result := map[string]any{
		"status": "ok", "status_code": response.StatusCode, "url": response.Request.URL.String(),
		"headers": headers, "headers_truncated": headersTruncated,
		"body":      strings.ToValidUTF8(string(payload), "\uFFFD"),
		"truncated": truncated, "duration_ms": time.Since(started).Milliseconds(),
	}
	return json.Marshal(result)
}

// builtinHTTPTransport 复用连接池参数，但禁用环境代理与不可信地址。
func builtinHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 环境代理会把用户数据转发到不可信出口；开发环境同样不应隐式继承。
	transport.Proxy = nil
	// 拨号期解析并校验目标地址，防止域名解析到云元数据或链路本地地址。
	transport.DialContext = builtinDialContext
	return transport
}

// builtinDialContext 拒绝元数据、链路本地、组播与未指定地址；回环与私网保留用于本地联调。
func builtinDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, resolved := range addresses {
		if blockedBuiltinAddress(resolved.IP) {
			return nil, fmt.Errorf("http target resolves to disallowed address %s", resolved.IP)
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("http target resolved no addresses")
	}
	return nil, lastErr
}

// blockedBuiltinAddress 判断地址是否属于云元数据、链路本地、组播或未指定地址。
func blockedBuiltinAddress(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	return ip.Equal(net.IPv4bcast)
}
