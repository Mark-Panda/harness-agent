package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
	"github.com/Mark-Panda/harness-agent/pkg/skillpkg"
	"github.com/google/uuid"
)

// 技能工具默认护栏。
const (
	// defaultSkillExecTimeout 单次脚本执行默认超时。
	defaultSkillExecTimeout = 30 * time.Second
	// defaultSkillExecMaxOutput 单次脚本输出默认截断上限。
	defaultSkillExecMaxOutput = 64 << 10
	// maxSkillScriptArgs 单次脚本调用允许的最大参数数量。
	maxSkillScriptArgs = 20
	// maxSkillScriptArgBytes 单个脚本参数允许的最大字节数。
	maxSkillScriptArgBytes = 512
	// maxSkillResourcesTokens 技能资源清单注入系统提示的 Token 上限。
	maxSkillResourcesTokens = 1_200
	// maxSkillConcurrentScripts 单实例同时执行的技能脚本数量上限。
	maxSkillConcurrentScripts = 2
)

// SkillExecutionConfig 是运行时技能脚本执行配置。
type SkillExecutionConfig struct {
	// AllowLocalExecution 是否允许在本机进程执行脚本；生产必须为 false。
	AllowLocalExecution bool
	// Timeout 单次执行超时，零值使用默认值。
	Timeout time.Duration
	// MaxOutputBytes 单次输出截断上限，零值使用默认值。
	MaxOutputBytes int
}

// normalize 补齐脚本执行默认配置。
func (c SkillExecutionConfig) normalize() SkillExecutionConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultSkillExecTimeout
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = defaultSkillExecMaxOutput
	}
	return c
}

// skillRuntime 是物化到临时目录后的单个技能。
type skillRuntime struct {
	ID    uuid.UUID
	Name  string
	Owner SkillFileOwner
	Root  string
	Files map[string]SkillBundleFile
}

// prepareSkillTools 物化已绑定技能包并注册技能工具。
// 返回的清理函数必须在 Run 结束时调用，避免临时目录泄漏。
func (w *ExecutionWorker) prepareSkillTools(ctx context.Context, run Run) (react.Registry, string, func(), error) {
	noop := func() {}
	if w.skillBundles == nil || isFeishuSharedGroup(run) {
		return nil, "", noop, nil
	}
	platformSkills, userSkills := snapshotBoundSkills(run.ConfigSnapshot)
	platformIDs := skillIDs(platformSkills)
	userIDs := skillIDs(userSkills)
	if len(platformIDs) == 0 && len(userIDs) == 0 {
		return nil, "", noop, nil
	}
	files, err := w.skillBundles.ListBoundSkillFiles(ctx, run.UserID, platformIDs, userIDs)
	if err != nil {
		// 技能包读取失败只降级为无技能工具，不阻断 Run。
		w.logger.Warn("load skill bundle files failed; continuing without skill tools", "run_id", run.ID, "error", err)
		return nil, "", noop, nil
	}
	if len(files) == 0 {
		return nil, "", noop, nil
	}
	skills, cleanup, err := materializeSkillBundles(files)
	if err != nil {
		w.logger.Warn("materialize skill bundles failed; continuing without skill tools", "run_id", run.ID, "error", err)
		return nil, "", noop, nil
	}
	config := w.config.SkillExecution.normalize()
	registry := newSkillToolRegistry(skills, config, w.skillExecSem)
	if len(registry.specs) == 0 {
		cleanup()
		return nil, "", noop, nil
	}
	return registry, skillResourcePrompt(skills, config), cleanup, nil
}

// snapshotBoundSkills 从 Run 快照读取平台与用户技能绑定。
func snapshotBoundSkills(snapshot map[string]any) ([]snapshotSkill, []snapshotSkill) {
	if snapshot == nil {
		return nil, nil
	}
	root := snapshot
	if nested, ok := snapshot["published_snapshot"].(map[string]any); ok {
		root = nested
	}
	return decodeSnapshotSkills(root["skills"]), decodeSnapshotSkills(root["user_skills"])
}

// decodeSnapshotSkills 解码快照中的技能数组；非法数据按空处理。
func decodeSnapshotSkills(raw any) []snapshotSkill {
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var skills []snapshotSkill
	if err := json.Unmarshal(encoded, &skills); err != nil {
		return nil
	}
	return skills
}

// skillIDs 提取非空的技能 ID。
func skillIDs(skills []snapshotSkill) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(skills))
	for _, skill := range skills {
		id, err := uuid.Parse(strings.TrimSpace(skill.ID))
		if err != nil || id == uuid.Nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// materializeSkillBundles 把技能包文件写入隔离的临时目录，返回可执行视图。
func materializeSkillBundles(files []SkillBundleFile) ([]*skillRuntime, func(), error) {
	root, err := os.MkdirTemp("", "harness-skill-*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	grouped := map[string]*skillRuntime{}
	order := make([]string, 0)
	for _, file := range files {
		key := string(file.Owner) + ":" + file.SkillID.String()
		skill, exists := grouped[key]
		if !exists {
			dir := filepath.Join(root, file.SkillID.String())
			if err := os.MkdirAll(dir, 0o700); err != nil {
				cleanup()
				return nil, func() {}, err
			}
			skill = &skillRuntime{
				ID: file.SkillID, Name: file.SkillName, Owner: file.Owner, Root: dir,
				Files: map[string]SkillBundleFile{},
			}
			grouped[key] = skill
			order = append(order, key)
		}
		target, joinErr := joinSkillRoot(skill.Root, file.Path)
		if joinErr != nil {
			cleanup()
			return nil, func() {}, joinErr
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(target), 0o700); mkdirErr != nil {
			cleanup()
			return nil, func() {}, mkdirErr
		}
		if writeErr := os.WriteFile(target, file.Content, 0o600); writeErr != nil {
			cleanup()
			return nil, func() {}, writeErr
		}
		skill.Files[file.Path] = file
	}
	result := make([]*skillRuntime, 0, len(order))
	for _, key := range order {
		result = append(result, grouped[key])
	}
	return result, cleanup, nil
}

// joinSkillRoot 把相对路径安全地拼接到技能根目录，拒绝任何越界路径。
func joinSkillRoot(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("%w: invalid skill file path", ErrSkillBundleUnsupported)
	}
	cleaned := path.Clean(relative)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: skill file path escapes the bundle root", ErrSkillBundleUnsupported)
	}
	target := filepath.Join(root, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%w: skill file path escapes the bundle root", ErrSkillBundleUnsupported)
	}
	return target, nil
}

// skillToolRegistry 是技能工具注册表，实现 react.Registry。
type skillToolRegistry struct {
	bindings map[string]react.Tool
	specs    []react.ToolSpec
}

// newSkillToolRegistry 构造技能工具；执行开关关闭时不注册脚本工具。
func newSkillToolRegistry(skills []*skillRuntime, config SkillExecutionConfig, globalSem chan struct{}) *skillToolRegistry {
	config = config.normalize()
	registry := &skillToolRegistry{bindings: map[string]react.Tool{}}
	add := func(tool react.Tool) {
		registry.bindings[tool.Spec().Name] = tool
		registry.specs = append(registry.specs, tool.Spec())
	}
	add(&skillTool{kind: skillToolList, skills: skills, config: config})
	add(&skillTool{kind: skillToolRead, skills: skills, config: config})
	if config.AllowLocalExecution && hasExecutableScript(skills) {
		add(&skillTool{kind: skillToolRun, skills: skills, config: config, sem: make(chan struct{}, 1), globalSem: globalSem})
	}
	return registry
}

// Lookup 按名称查找技能工具。
func (r *skillToolRegistry) Lookup(name string) (react.Tool, bool) {
	tool, ok := r.bindings[name]
	return tool, ok
}

// List 返回全部技能工具定义。
func (r *skillToolRegistry) List() []react.ToolSpec {
	return append([]react.ToolSpec(nil), r.specs...)
}

// hasExecutableScript 判断是否存在白名单脚本。
func hasExecutableScript(skills []*skillRuntime) bool {
	for _, skill := range skills {
		for _, file := range skill.Files {
			if file.IsScript && file.ExecutionEnabled && skillpkg.Interpreter(file.Path) != "" {
				return true
			}
		}
	}
	return false
}

// skillToolKind 标识技能工具类型。
type skillToolKind string

const (
	skillToolList skillToolKind = "skill_list_files"
	skillToolRead skillToolKind = "skill_read_file"
	skillToolRun  skillToolKind = "skill_run_script"
)

// skillTool 实现 react.Tool，覆盖文件列举、读取与脚本执行。
type skillTool struct {
	kind      skillToolKind
	skills    []*skillRuntime
	config    SkillExecutionConfig
	sem       chan struct{}
	globalSem chan struct{}
}

// Spec 返回暴露给模型的工具定义。
func (t *skillTool) Spec() react.ToolSpec {
	switch t.kind {
	case skillToolRead:
		return react.ToolSpec{
			Name: string(skillToolRead), Description: "读取当前 Agent 绑定技能包中的文本文件。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"技能名称或 ID；只有一个技能时可省略"},"path":{"type":"string","description":"相对技能根目录的文件路径"}},"required":["path"],"additionalProperties":false}`),
		}
	case skillToolRun:
		return react.ToolSpec{
			Name: string(skillToolRun), Description: "执行技能包内的脚本（高风险，需要人工审批）。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"技能名称或 ID；只有一个技能时可省略"},"path":{"type":"string","description":"脚本相对路径"},"args":{"type":"array","items":{"type":"string"},"description":"传给脚本的参数"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"执行超时秒数"}},"required":["path"],"additionalProperties":false}`),
		}
	default:
		return react.ToolSpec{
			Name: string(skillToolList), Description: "列出当前 Agent 绑定技能包的目录树与脚本标记。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"可选：只查看指定技能"}},"additionalProperties":false}`),
		}
	}
}

// RiskLevel 返回风险等级；脚本执行属于高风险。
func (t *skillTool) RiskLevel() string {
	if t.kind == skillToolRun {
		return "high"
	}
	return "low"
}

// MCPIdentity 提供稳定的审批目标，使高风险脚本在执行前进入人工审批并在恢复时重新绑定。
func (t *skillTool) MCPIdentity() mcp.ToolIdentity {
	digest := sha256.Sum256([]byte("harness-skill-tool:" + string(t.kind)))
	return mcp.ToolIdentity{
		ServerID:            uuid.NewSHA1(uuid.NameSpaceOID, []byte("harness-skill-tool")),
		OriginalName:        string(t.kind),
		EndpointFingerprint: hex.EncodeToString(digest[:16]),
	}
}

// Invoke 执行技能工具并返回不可信 JSON 数据。
func (t *skillTool) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	switch t.kind {
	case skillToolRead:
		return t.invokeRead(arguments)
	case skillToolRun:
		return t.invokeRun(ctx, arguments)
	default:
		return t.invokeList(arguments)
	}
}

// skillListArguments 是技能列表入参。
type skillListArguments struct {
	Skill string `json:"skill"`
}

// skillReadArguments 是技能文件读取入参。
type skillReadArguments struct {
	Skill string `json:"skill"`
	Path  string `json:"path"`
}

// skillRunArguments 是脚本执行入参。
type skillRunArguments struct {
	Skill          string   `json:"skill"`
	Path           string   `json:"path"`
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// invokeList 返回技能目录树。
func (t *skillTool) invokeList(arguments json.RawMessage) (json.RawMessage, error) {
	var request skillListArguments
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &request); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	selected, err := t.selectSkills(request.Skill)
	if err != nil {
		return nil, err
	}
	payload := make([]map[string]any, 0, len(selected))
	for _, skill := range selected {
		files := make([]map[string]any, 0, len(skill.Files))
		paths := make([]string, 0, len(skill.Files))
		for name := range skill.Files {
			paths = append(paths, name)
		}
		sort.Strings(paths)
		for _, name := range paths {
			file := skill.Files[name]
			files = append(files, map[string]any{
				"path": file.Path, "size": file.SizeBytes, "script": file.IsScript, "mime": file.MIME,
				"executable": file.IsScript && t.config.AllowLocalExecution,
			})
		}
		payload = append(payload, map[string]any{
			"name": skill.Name, "id": skill.ID.String(), "owner": string(skill.Owner), "files": files,
		})
	}
	return json.Marshal(map[string]any{"skills": payload})
}

// invokeRead 读取技能包内的文本文件。
func (t *skillTool) invokeRead(arguments json.RawMessage) (json.RawMessage, error) {
	var request skillReadArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	skill, file, err := t.resolveFile(request.Skill, request.Path)
	if err != nil {
		return nil, err
	}
	if len(file.Content) > MaxSkillFileReadBytes {
		return nil, fmt.Errorf("%w: file exceeds the read limit", ErrSkillBundleUnsupported)
	}
	if !skillpkg.IsTextMIME(file.MIME) {
		return nil, fmt.Errorf("%w: only text files can be read", ErrSkillBundleUnsupported)
	}
	return json.Marshal(map[string]any{
		"skill": skill.Name, "path": file.Path, "mime": file.MIME, "content": string(file.Content),
	})
}

// invokeRun 执行技能包内脚本；失败与超时都作为结果返回，避免被误判为基础设施故障。
func (t *skillTool) invokeRun(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var request skillRunArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if !t.config.AllowLocalExecution {
		return nil, fmt.Errorf("%w: local script execution is disabled", ErrSkillBundleUnsupported)
	}
	skill, file, err := t.resolveFile(request.Skill, request.Path)
	if err != nil {
		return nil, err
	}
	interpreter := skillpkg.Interpreter(file.Path)
	if !file.IsScript || interpreter == "" {
		return nil, fmt.Errorf("%w: file is not an executable script", ErrSkillBundleUnsupported)
	}
	if !file.ExecutionEnabled {
		return nil, fmt.Errorf("%w: script execution is disabled for this skill", ErrSkillBundleUnsupported)
	}
	if len(request.Args) > maxSkillScriptArgs {
		return nil, fmt.Errorf("too many script arguments: %d", len(request.Args))
	}
	for _, arg := range request.Args {
		if len(arg) > maxSkillScriptArgBytes || strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("script argument is invalid")
		}
	}
	target, err := joinSkillRoot(skill.Root, file.Path)
	if err != nil {
		return nil, err
	}

	timeout := t.config.Timeout
	if request.TimeoutSeconds > 0 {
		requested := time.Duration(request.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	if t.sem != nil {
		select {
		case t.sem <- struct{}{}:
			defer func() { <-t.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if t.globalSem != nil {
		select {
		case t.globalSem <- struct{}{}:
			defer func() { <-t.globalSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 每个技能 Run 只允许串行执行脚本，避免单个 Run 占满 CPU。
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, interpreter, append([]string{target}, request.Args...)...)
	command.Dir = skill.Root
	command.Env = skillExecutionEnv(skill.Root)
	output := &truncatingBuffer{limit: t.config.MaxOutputBytes}
	command.Stdout = output
	command.Stderr = output

	started := time.Now()
	runErr := command.Run()
	duration := time.Since(started)
	result := map[string]any{
		"skill": skill.Name, "path": file.Path, "duration_ms": duration.Milliseconds(),
		"truncated": output.Truncated(), "output": strings.ToValidUTF8(output.String(), "\uFFFD"),
	}
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		result["status"] = "timeout"
		result["exit_code"] = -1
	case runErr == nil:
		result["status"] = "ok"
		result["exit_code"] = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result["status"] = "failed"
			result["exit_code"] = exitErr.ExitCode()
		} else {
			// 解释器缺失等环境问题：返回可读结果，由模型调整策略而不是反复重试。
			result["status"] = "unavailable"
			result["exit_code"] = -1
			result["error"] = runErr.Error()
		}
	}
	return json.Marshal(result)
}

// selectSkills 按名称或 ID 过滤技能；未指定时返回全部。
func (t *skillTool) selectSkills(name string) ([]*skillRuntime, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if len(t.skills) == 0 {
			return nil, fmt.Errorf("%w: no skill bundle is bound", ErrSkillBundleNotFound)
		}
		return t.skills, nil
	}
	for _, skill := range t.skills {
		if strings.EqualFold(skill.ID.String(), name) || strings.EqualFold(skill.Name, name) {
			return []*skillRuntime{skill}, nil
		}
	}
	return nil, fmt.Errorf("%w: skill %s is not bound", ErrSkillBundleNotFound, name)
}

// resolveFile 解析技能与文件；路径必须存在于已物化的文件清单中。
func (t *skillTool) resolveFile(skillName, filePath string) (*skillRuntime, SkillBundleFile, error) {
	selected, err := t.selectSkills(skillName)
	if err != nil {
		return nil, SkillBundleFile{}, err
	}
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" {
		return nil, SkillBundleFile{}, fmt.Errorf("file path is required")
	}
	for _, skill := range selected {
		if file, ok := skill.Files[trimmed]; ok {
			return skill, file, nil
		}
	}
	return nil, SkillBundleFile{}, fmt.Errorf("%w: file %s is not found", ErrSkillBundleNotFound, trimmed)
}

// skillExecutionEnv 只透传白名单环境变量，避免泄露服务端凭证。
func skillExecutionEnv(root string) []string {
	pathValue := os.Getenv("PATH")
	if strings.TrimSpace(pathValue) == "" {
		pathValue = "/usr/local/bin:/usr/bin:/bin"
	}
	lang := os.Getenv("LANG")
	if strings.TrimSpace(lang) == "" {
		lang = "C.UTF-8"
	}
	return []string{
		"PATH=" + pathValue,
		"LANG=" + lang,
		"LC_ALL=" + lang,
		"TZ=Asia/Shanghai",
		"HOME=" + root,
		"TMPDIR=" + os.TempDir(),
	}
}

// truncatingBuffer 限制输出总量并记录是否发生截断。
type truncatingBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
	mu        sync.Mutex
}

// Write 始终返回成功，避免子进程因管道写满而阻塞。
func (b *truncatingBuffer) Write(data []byte) (int, error) {
	written := len(data)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return written, nil
	}
	if len(data) > remaining {
		b.truncated = true
		data = data[:remaining]
	}
	_, _ = b.buffer.Write(data)
	return written, nil
}

// String 返回已捕获的输出。
func (b *truncatingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// Truncated 返回是否发生过截断。
func (b *truncatingBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// skillResourcePrompt 生成注入系统提示的技能资源清单。
func skillResourcePrompt(skills []*skillRuntime, config SkillExecutionConfig) string {
	var builder strings.Builder
	builder.WriteString("\n\n## Skill 资源文件\n")
	builder.WriteString("已绑定的技能包含以下文件；需要查看内容时调用 skill_read_file。")
	if config.AllowLocalExecution {
		builder.WriteString("脚本可通过 skill_run_script 执行，该操作需要人工审批。")
	} else {
		builder.WriteString("当前环境不允许执行技能脚本，只能读取文本资源。")
	}
	for _, skill := range skills {
		builder.WriteString("\n### ")
		builder.WriteString(skill.Name)
		paths := make([]string, 0, len(skill.Files))
		for name := range skill.Files {
			paths = append(paths, name)
		}
		sort.Strings(paths)
		for _, name := range paths {
			file := skill.Files[name]
			builder.WriteString("\n- ")
			builder.WriteString(file.Path)
			if file.IsScript {
				builder.WriteString("（脚本）")
			}
		}
	}
	return truncateTextByTokens(builder.String(), maxSkillResourcesTokens)
}

// combinedToolRegistry 把多个工具注册表合并为一个，用于 MCP 与技能工具共存。
type combinedToolRegistry struct {
	layers []react.Registry
}

// Lookup 按顺序在各层查找工具。
func (r combinedToolRegistry) Lookup(name string) (react.Tool, bool) {
	for _, layer := range r.layers {
		if layer == nil {
			continue
		}
		if tool, ok := layer.Lookup(name); ok {
			return tool, true
		}
	}
	return nil, false
}

// List 合并各层工具定义，名称重复时保留第一个。
func (r combinedToolRegistry) List() []react.ToolSpec {
	seen := map[string]struct{}{}
	specs := make([]react.ToolSpec, 0)
	for _, layer := range r.layers {
		if layer == nil {
			continue
		}
		for _, spec := range layer.List() {
			if _, exists := seen[spec.Name]; exists {
				continue
			}
			seen[spec.Name] = struct{}{}
			specs = append(specs, spec)
		}
	}
	return specs
}

// combineToolRegistries 合并多个注册表；空输入返回空注册表。
func combineToolRegistries(layers ...react.Registry) react.Registry {
	active := make([]react.Registry, 0, len(layers))
	for _, layer := range layers {
		if layer != nil {
			active = append(active, layer)
		}
	}
	switch len(active) {
	case 0:
		return emptyToolRegistry{}
	case 1:
		return active[0]
	default:
		return combinedToolRegistry{layers: active}
	}
}
