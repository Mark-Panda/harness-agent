package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/Mark-Panda/harness-agent/pkg/mcp"
)

// 内置文件与代码检索工具名称。
const (
	builtinToolReadFile  = "fs_read_file"
	builtinToolWriteFile = "fs_write_file"
	builtinToolEditFile  = "fs_edit_file"
	builtinToolListDir   = "fs_list_dir"
	builtinToolGlob      = "fs_glob"
	builtinToolGrep      = "fs_grep"
)

// 文件工具默认护栏。
const (
	defaultBuiltinFileReadBytes  = 256 << 10
	defaultBuiltinFileWriteBytes = 256 << 10
	defaultBuiltinFileResults    = 200
	maxBuiltinReadLines          = 2000
	maxBuiltinGrepFileBytes      = 1 << 20
	maxBuiltinGrepLineRunes      = 500
)

// builtinIgnoredDirs 是代码检索默认跳过的目录，避免扫描依赖与构建产物。
var builtinIgnoredDirs = map[string]struct{}{
	".git": {}, ".svn": {}, ".hg": {}, "node_modules": {}, "vendor": {},
	"dist": {}, "build": {}, "coverage": {}, "target": {}, ".next": {},
	".idea": {}, ".venv": {}, "venv": {}, "__pycache__": {},
}

// 文件写入按路径分片加锁，避免同一进程内并发 write/edit 互相覆盖。
const builtinFileLockStripes = 64

var builtinFileLocks [builtinFileLockStripes]sync.Mutex

// lockBuiltinFile 按路径哈希获取分片锁，返回解锁函数。
func lockBuiltinFile(target string) func() {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(target))
	lock := &builtinFileLocks[int(hasher.Sum32())%builtinFileLockStripes]
	lock.Lock()
	return lock.Unlock
}

// BuiltinFilesConfig 是文件操作与代码检索工具的本机执行配置。
type BuiltinFilesConfig struct {
	// Enabled 是否向模型暴露文件与检索工具。
	Enabled bool
	// AllowLocalExecution 是否允许在本机进程访问文件；仅开发环境可开启。
	AllowLocalExecution bool
	// Root 工具可访问的基础目录；运行时按用户 ID 派生子目录，为空时使用系统临时目录。
	Root string
	// MaxReadBytes 单次读取的最大字节数。
	MaxReadBytes int
	// MaxWriteBytes 单次写入或编辑的最大字节数。
	MaxWriteBytes int
	// MaxResults 列表、匹配与搜索结果的最大条数。
	MaxResults int
}

// normalize 补齐文件工具默认值。
func (c BuiltinFilesConfig) normalize() BuiltinFilesConfig {
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = defaultBuiltinFileReadBytes
	}
	if c.MaxWriteBytes <= 0 {
		c.MaxWriteBytes = defaultBuiltinFileWriteBytes
	}
	if c.MaxResults <= 0 {
		c.MaxResults = defaultBuiltinFileResults
	}
	return c
}

// fileTool 实现单个文件或检索工具；所有路径都限制在配置根目录内。
type fileTool struct {
	name   string
	config BuiltinFilesConfig
}

// Spec 返回工具定义。
func (t *fileTool) Spec() react.ToolSpec {
	switch t.name {
	case builtinToolWriteFile:
		return react.ToolSpec{
			Name:        t.name,
			Description: "写入文本文件，自动创建父目录并原子替换；高风险，需要人工审批。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"相对根目录的文件路径"},"content":{"type":"string","description":"完整文件内容"}},"required":["path","content"],"additionalProperties":false}`),
		}
	case builtinToolEditFile:
		return react.ToolSpec{
			Name:        t.name,
			Description: "在文本文件中替换指定字符串；old_string 必须唯一或显式 replace_all（高风险，需要人工审批）。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"相对根目录的文件路径"},"old_string":{"type":"string","description":"待替换的原文"},"new_string":{"type":"string","description":"替换后的内容"},"replace_all":{"type":"boolean","description":"是否替换全部匹配；默认要求唯一匹配"}},"required":["path","old_string","new_string"],"additionalProperties":false}`),
		}
	case builtinToolListDir:
		return react.ToolSpec{
			Name:        t.name,
			Description: "列出目录下的文件与子目录（不递归）。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"相对根目录的目录路径，默认根目录"}},"additionalProperties":false}`),
		}
	case builtinToolGlob:
		return react.ToolSpec{
			Name:        t.name,
			Description: "按 glob 模式查找文件，支持 *、** 与 ?；返回相对根目录的路径。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"glob 模式，例如 **/*.go"},"path":{"type":"string","description":"搜索起始目录，默认根目录"}},"required":["pattern"],"additionalProperties":false}`),
		}
	case builtinToolGrep:
		return react.ToolSpec{
			Name:        t.name,
			Description: "使用正则表达式检索文件内容，返回文件、行号与匹配行。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go 正则表达式"},"path":{"type":"string","description":"搜索起始目录，默认根目录"},"glob":{"type":"string","description":"可选文件名过滤，例如 **/*.go"},"case_insensitive":{"type":"boolean","description":"是否忽略大小写"}},"required":["pattern"],"additionalProperties":false}`),
		}
	default:
		return react.ToolSpec{
			Name:        builtinToolReadFile,
			Description: "读取文本文件内容，可按行截取；超大文件会被截断。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"相对根目录的文件路径"},"offset":{"type":"integer","minimum":0,"description":"起始行偏移，从 0 开始"},"limit":{"type":"integer","minimum":1,"maximum":2000,"description":"最多返回的行数"}},"required":["path"],"additionalProperties":false}`),
		}
	}
}

// RiskLevel 返回风险等级：写入与编辑需要审批，读取与检索可自动执行。
func (t *fileTool) RiskLevel() string {
	switch t.name {
	case builtinToolWriteFile, builtinToolEditFile:
		return "medium"
	default:
		return "low"
	}
}

// MCPIdentity 提供稳定的审批目标，恢复审批时据此重新绑定工具。
func (t *fileTool) MCPIdentity() mcp.ToolIdentity { return builtinToolIdentity(t.name) }

// Invoke 按工具类型执行文件操作。
func (t *fileTool) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if !t.config.AllowLocalExecution {
		return nil, fmt.Errorf("%w: local file access is disabled", ErrBuiltinToolUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := acquireBuiltinSemaphore(ctx, builtinFileSem); err != nil {
		return nil, err
	}
	defer func() { <-builtinFileSem }()
	switch t.name {
	case builtinToolWriteFile:
		return t.invokeWrite(arguments)
	case builtinToolEditFile:
		return t.invokeEdit(arguments)
	case builtinToolListDir:
		return t.invokeList(arguments)
	case builtinToolGlob:
		return t.invokeGlob(ctx, arguments)
	case builtinToolGrep:
		return t.invokeGrep(ctx, arguments)
	default:
		return t.invokeRead(arguments)
	}
}

// root 返回工具可访问的绝对根目录。
func (t *fileTool) root() (string, error) {
	root := strings.TrimSpace(t.config.Root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return cwd, nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve tools root: %w", err)
	}
	return absolute, nil
}

// resolvePath 把模型提供的相对路径限制在根目录内，返回绝对路径与斜杠相对路径。
// 符号链接会被解析到真实路径后再校验，避免通过链接逃逸出根目录。
func (t *fileTool) resolvePath(relative string) (string, string, error) {
	root, err := t.root()
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimSpace(relative)
	if trimmed == "" {
		trimmed = "."
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", "", fmt.Errorf("path contains an invalid character")
	}
	if filepath.IsAbs(trimmed) {
		return "", "", fmt.Errorf("absolute paths are not allowed")
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes the tools root")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve tools root: %w", err)
	}
	target := filepath.Join(root, cleaned)
	resolved, err := resolveWithinRoot(target, realRoot)
	if err != nil {
		return "", "", err
	}
	return resolved, filepath.ToSlash(cleaned), nil
}

// resolveWithinRoot 解析目标路径（含最近存在的祖先）的符号链接，并校验仍在根目录内。
// 写入新文件时目标尚不存在，因此需要逐级向上找到已存在的祖先再解析。
func resolveWithinRoot(target, realRoot string) (string, error) {
	existing := target
	suffix := make([]string, 0, 4)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		suffix = append([]string{filepath.Base(existing)}, suffix...)
		existing = parent
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolved := realExisting
	if len(suffix) > 0 {
		resolved = filepath.Join(append([]string{realExisting}, suffix...)...)
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the tools root")
	}
	return resolved, nil
}

// readArguments 是 fs_read_file 入参。
type readFileArguments struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// invokeRead 读取文本文件并按行截取。
func (t *fileTool) invokeRead(arguments json.RawMessage) (json.RawMessage, error) {
	var request readFileArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	target, relative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory; use %s", builtinToolListDir)
	}
	file, err := os.Open(target) // #nosec G304 -- 路径已限制在配置根目录内
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(t.config.MaxReadBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	truncated := len(data) > t.config.MaxReadBytes
	if truncated {
		data = data[:t.config.MaxReadBytes]
	}
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return nil, fmt.Errorf("file appears to be binary; use %s for text search", builtinToolGrep)
	}
	content := strings.ToValidUTF8(string(data), "\uFFFD")
	lines := strings.Split(content, "\n")
	total := len(lines)
	offset := request.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	limit := request.Limit
	if limit <= 0 || limit > maxBuiltinReadLines {
		limit = maxBuiltinReadLines
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return json.Marshal(map[string]any{
		"path": relative, "size_bytes": info.Size(), "truncated": truncated,
		"offset": offset, "limit": limit, "total_lines": total, "total_lines_partial": truncated,
		"content": strings.Join(lines[offset:end], "\n"),
	})
}

// writeFileArguments 是 fs_write_file 入参。
type writeFileArguments struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// invokeWrite 原子写入文件并自动创建父目录。
func (t *fileTool) invokeWrite(arguments json.RawMessage) (json.RawMessage, error) {
	var request writeFileArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(request.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	if len(request.Content) > t.config.MaxWriteBytes {
		return nil, fmt.Errorf("content exceeds the write limit of %d bytes", t.config.MaxWriteBytes)
	}
	target, relative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	unlock := lockBuiltinFile(target)
	defer unlock()
	_, statErr := os.Stat(target)
	if err := writeFileAtomically(target, request.Content); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"path": relative, "bytes_written": len(request.Content), "created": os.IsNotExist(statErr),
	})
}

// editFileArguments 是 fs_edit_file 入参。
type editFileArguments struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// invokeEdit 替换文本内容；未显式 replace_all 时要求唯一匹配。
func (t *fileTool) invokeEdit(arguments json.RawMessage) (json.RawMessage, error) {
	var request editFileArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if request.OldString == "" {
		return nil, fmt.Errorf("old_string is required")
	}
	target, relative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	unlock := lockBuiltinFile(target)
	defer unlock()
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("edit file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory")
	}
	if info.Size() > int64(t.config.MaxWriteBytes) {
		return nil, fmt.Errorf("file exceeds the edit limit of %d bytes", t.config.MaxWriteBytes)
	}
	data, err := os.ReadFile(target) // #nosec G304 -- 路径已限制在配置根目录内
	if err != nil {
		return nil, fmt.Errorf("edit file: %w", err)
	}
	original := string(data)
	count := strings.Count(original, request.OldString)
	if count == 0 {
		return nil, fmt.Errorf("old_string was not found")
	}
	if count > 1 && !request.ReplaceAll {
		return nil, fmt.Errorf("old_string matches %d locations; set replace_all or narrow the match", count)
	}
	replacements := count
	updated := original
	if request.ReplaceAll {
		updated = strings.ReplaceAll(original, request.OldString, request.NewString)
	} else {
		updated = strings.Replace(original, request.OldString, request.NewString, 1)
	}
	if len(updated) > t.config.MaxWriteBytes {
		return nil, fmt.Errorf("edited content exceeds the write limit of %d bytes", t.config.MaxWriteBytes)
	}
	if err := writeFileAtomically(target, updated); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"path": relative, "replacements": replacements, "bytes_written": len(updated),
	})
}

// listDirArguments 是 fs_list_dir 入参。
type listDirArguments struct {
	Path string `json:"path"`
}

// invokeList 列出目录项，不递归。
func (t *fileTool) invokeList(arguments json.RawMessage) (json.RawMessage, error) {
	var request listDirArguments
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &request); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	target, relative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}
	result := make([]map[string]any, 0, len(entries))
	truncated := false
	for _, entry := range entries {
		if len(result) >= t.config.MaxResults {
			truncated = true
			break
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		kind := "file"
		switch {
		case entry.IsDir():
			kind = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		result = append(result, map[string]any{
			"name": entry.Name(), "path": path.Join(relative, entry.Name()), "type": kind, "size_bytes": info.Size(),
		})
	}
	return json.Marshal(map[string]any{"path": relative, "entries": result, "truncated": truncated})
}

// globArguments 是 fs_glob 入参。
type globArguments struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// invokeGlob 按 glob 模式查找文件。
func (t *fileTool) invokeGlob(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var request globArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(request.Pattern) == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	matcher, err := compileGlob(request.Pattern)
	if err != nil {
		return nil, err
	}
	baseTarget, baseRelative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(baseTarget)
	if err != nil {
		return nil, fmt.Errorf("search path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("search path is not a directory")
	}
	matches := make([]string, 0)
	truncated := false
	walkErr := filepath.WalkDir(baseTarget, func(current string, entry fs.DirEntry, walkErr error) error {
		// 目录遍历必须响应取消，避免大仓库扫描拖过 Run deadline 并长期占用 Worker。
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if current != baseTarget && ignoreBuiltinDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		relativeToBase, err := filepath.Rel(baseTarget, current)
		if err != nil {
			return nil
		}
		slash := filepath.ToSlash(relativeToBase)
		if !matcher.MatchString(slash) {
			return nil
		}
		if len(matches) >= t.config.MaxResults {
			truncated = true
			return fs.SkipAll
		}
		matches = append(matches, path.Join(baseRelative, slash))
		return nil
	})
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("glob search: %w", walkErr)
	}
	sort.Strings(matches)
	return json.Marshal(map[string]any{"pattern": request.Pattern, "path": baseRelative, "matches": matches, "truncated": truncated})
}

// grepArguments 是 fs_grep 入参。
type grepArguments struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
}

// grepMatch 是单条内容匹配。
type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// invokeGrep 使用正则检索文件内容。
func (t *fileTool) invokeGrep(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var request grepArguments
	if err := json.Unmarshal(arguments, &request); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	expression := strings.TrimSpace(request.Pattern)
	if expression == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	if request.CaseInsensitive {
		expression = "(?i)" + expression
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression: %w", err)
	}
	var fileFilter *regexp.Regexp
	if strings.TrimSpace(request.Glob) != "" {
		fileFilter, err = compileGlob(request.Glob)
		if err != nil {
			return nil, err
		}
	}
	baseTarget, baseRelative, err := t.resolvePath(request.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(baseTarget)
	if err != nil {
		return nil, fmt.Errorf("search path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("search path is not a directory")
	}
	matches := make([]grepMatch, 0)
	filesScanned := 0
	truncated := false
	walkErr := filepath.WalkDir(baseTarget, func(current string, entry fs.DirEntry, walkErr error) error {
		// 与 glob 一致：每个条目都检查取消，保证 Run 取消能及时终止扫描。
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if current != baseTarget && ignoreBuiltinDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relativeToBase, err := filepath.Rel(baseTarget, current)
		if err != nil {
			return nil
		}
		slash := filepath.ToSlash(relativeToBase)
		if fileFilter != nil && !fileFilter.MatchString(slash) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Size() > maxBuiltinGrepFileBytes {
			return nil
		}
		data, readErr := os.ReadFile(current) // #nosec G304 -- 路径由 WalkDir 产生且已限制在根目录内
		if readErr != nil {
			return nil
		}
		// 二进制文件（前 8KB 含 NUL）跳过，避免把图片/压缩包内容送进模型。
		probe := data
		if len(probe) > 8192 {
			probe = probe[:8192]
		}
		if bytes.IndexByte(probe, 0) >= 0 {
			return nil
		}
		filesScanned++
		lines := strings.Split(strings.ToValidUTF8(string(data), "\uFFFD"), "\n")
		for index, line := range lines {
			if !matcher.MatchString(line) {
				continue
			}
			if len(matches) >= t.config.MaxResults {
				truncated = true
				return fs.SkipAll
			}
			matches = append(matches, grepMatch{
				Path: path.Join(baseRelative, slash), Line: index + 1, Text: truncateGrepLine(line),
			})
		}
		return nil
	})
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("grep search: %w", walkErr)
	}
	return json.Marshal(map[string]any{
		"pattern": request.Pattern, "path": baseRelative, "matches": matches,
		"files_scanned": filesScanned, "truncated": truncated,
	})
}

// compileGlob 把 glob 模式编译为正则，支持 *、** 与 ?。
func compileGlob(pattern string) (*regexp.Regexp, error) {
	normalized := filepath.ToSlash(strings.TrimSpace(pattern))
	if normalized == "" {
		return nil, fmt.Errorf("glob pattern is required")
	}
	var builder strings.Builder
	builder.WriteString("^")
	for index := 0; index < len(normalized); {
		switch normalized[index] {
		case '*':
			if index+1 < len(normalized) && normalized[index+1] == '*' {
				builder.WriteString(".*")
				index += 2
				// "**/" 同时匹配零层目录，保证 **/*.go 能命中根目录文件。
				if index < len(normalized) && normalized[index] == '/' {
					builder.WriteString("(?:.*/)?")
					index++
				}
				continue
			}
			builder.WriteString("[^/]*")
			index++
		case '?':
			builder.WriteString("[^/]")
			index++
		default:
			builder.WriteString(regexp.QuoteMeta(string(normalized[index])))
			index++
		}
	}
	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

// ignoreBuiltinDir 判断目录是否属于默认忽略列表。
func ignoreBuiltinDir(name string) bool {
	_, ignored := builtinIgnoredDirs[name]
	return ignored
}

// truncateGrepLine 限制单行输出长度，避免超长行撑爆上下文。
func truncateGrepLine(line string) string {
	runes := []rune(line)
	if len(runes) <= maxBuiltinGrepLineRunes {
		return line
	}
	return string(runes[:maxBuiltinGrepLineRunes]) + "..."
}

// writeFileAtomically 先写临时文件再原子替换，避免中途失败留下半截文件。
// 覆盖已有文件时保留原权限，防止编辑可执行脚本后丢失执行位。
func writeFileAtomically(target string, content string) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	file, err := os.CreateTemp(parent, ".harness-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporary := file.Name()
	cleanup := func() { _ = os.Remove(temporary) }
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("write file: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("set file mode: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		cleanup()
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}
