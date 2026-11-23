// Package skillpkg 解析 Anthropic Agent Skills 标准技能包（zip）。
// 技能包是一个目录：根级 SKILL.md（YAML frontmatter）+ scripts/ + references/ + assets/。
// 解析结果会完整保留目录树与脚本内容，供运行时读取与执行。
package skillpkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// 技能包硬限制；任何超限都直接拒绝，避免大包或压缩炸弹挤占存储与上下文。
const (
	// MaxFiles 单个技能包允许的最大文件数。
	MaxFiles = 200
	// MaxFileBytes 单个文件允许的最大字节数。
	MaxFileBytes = 1 << 20
	// MaxScriptBytes 单个脚本允许的最大字节数。
	MaxScriptBytes = 512 << 10
	// MaxTotalBytes 单个技能包解压后的总字节数上限。
	MaxTotalBytes = 10 << 20
	// MaxArchiveBytes 上传的 zip 原始字节上限。
	MaxArchiveBytes = 12 << 20
	// MaxPathDepth 文件路径允许的最大层级。
	MaxPathDepth = 8
	// MaxNameLength 技能名称最大长度。
	MaxNameLength = 128
	// MaxDescriptionLength 技能描述最大长度。
	MaxDescriptionLength = 4096
	// MaxPromptBytes SKILL.md 正文允许的最大字节数。
	MaxPromptBytes = 256 << 10
)

// ErrInvalidPackage 表示技能包格式、路径或大小不合法。
var ErrInvalidPackage = errors.New("invalid skill package")

// File 是技能包内的单个文件。
type File struct {
	// Path 相对技能根目录的路径，使用 / 分隔。
	Path string
	// Content 文件原始内容。
	Content []byte
	// MIME 按扩展名推断的媒体类型。
	MIME string
	// SHA256 文件内容摘要。
	SHA256 string
	// IsScript 是否为可执行脚本。
	IsScript bool
}

// Package 是解析后的标准技能包。
type Package struct {
	// Name frontmatter 中的技能名称。
	Name string
	// Description frontmatter 中的技能描述。
	Description string
	// PromptTemplate SKILL.md 正文（去除 frontmatter），作为提示词注入。
	PromptTemplate string
	// Entrypoint 入口文件，固定为 SKILL.md。
	Entrypoint string
	// Files 全部文件（含 SKILL.md）。
	Files []File
	// Digest 目录树内容摘要，用于审计与快照。
	Digest string
	// TotalBytes 解压后的总字节数。
	TotalBytes int64
	// HasScripts 是否包含可执行脚本。
	HasScripts bool
}

// scriptExtensions 是允许执行的脚本后缀到解释器的映射。
var scriptExtensions = map[string][]string{
	".py":   {"python3"},
	".sh":   {"/bin/sh"},
	".bash": {"/bin/bash"},
	".js":   {"node"},
	".mjs":  {"node"},
	".rb":   {"ruby"},
	".pl":   {"perl"},
	".php":  {"php"},
}

// mimeByExtension 是常见资源的媒体类型映射；未知后缀按二进制处理。
var mimeByExtension = map[string]string{
	".md":   "text/markdown; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".json": "application/json",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
	".csv":  "text/csv; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".py":   "text/x-python; charset=utf-8",
	".sh":   "text/x-shellscript; charset=utf-8",
	".bash": "text/x-shellscript; charset=utf-8",
	".rb":   "text/x-ruby; charset=utf-8",
	".pl":   "text/x-perl; charset=utf-8",
	".php":  "text/x-php; charset=utf-8",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".pdf":  "application/pdf",
	".zip":  "application/zip",
}

// IsScriptPath 判断路径是否为允许执行的脚本。
func IsScriptPath(name string) bool {
	_, ok := scriptExtensions[strings.ToLower(path.Ext(name))]
	return ok
}

// Interpreter 返回脚本对应的解释器命令；非脚本返回空。
func Interpreter(name string) string {
	commands, ok := scriptExtensions[strings.ToLower(path.Ext(name))]
	if !ok || len(commands) == 0 {
		return ""
	}
	return commands[0]
}

// IsTextMIME 判断文件是否可作为文本读取。
func IsTextMIME(mime string) bool {
	lower := strings.ToLower(mime)
	return strings.HasPrefix(lower, "text/") || strings.Contains(lower, "json") ||
		strings.Contains(lower, "yaml") || strings.Contains(lower, "xml")
}

// ParseZip 解析 zip 技能包并校验路径、大小与入口文件。
// fallbackName 仅在 frontmatter 缺少 name 时使用。
func ParseZip(data []byte, fallbackName string) (*Package, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: archive is empty", ErrInvalidPackage)
	}
	if len(data) > MaxArchiveBytes {
		return nil, fmt.Errorf("%w: archive exceeds %d bytes", ErrInvalidPackage, MaxArchiveBytes)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: open zip: %v", ErrInvalidPackage, err)
	}

	raw := make([]rawFile, 0, len(reader.File))
	seen := make(map[string]struct{}, len(reader.File))
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		// 符号链接会绕过路径白名单，直接拒绝。
		if entry.Mode()&0o170000 == 0o120000 {
			return nil, fmt.Errorf("%w: symbolic link is not allowed: %s", ErrInvalidPackage, entry.Name)
		}
		cleaned, err := normalizePath(entry.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[cleaned]; exists {
			return nil, fmt.Errorf("%w: duplicate path %s", ErrInvalidPackage, cleaned)
		}
		seen[cleaned] = struct{}{}
		if len(raw) >= MaxFiles {
			return nil, fmt.Errorf("%w: file count exceeds %d", ErrInvalidPackage, MaxFiles)
		}
		// 以流式读取并限制单文件大小，避免解压炸弹。
		opened, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: open %s: %v", ErrInvalidPackage, cleaned, err)
		}
		content, err := io.ReadAll(io.LimitReader(opened, MaxFileBytes+1))
		closeErr := opened.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrInvalidPackage, cleaned, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%w: close %s: %v", ErrInvalidPackage, cleaned, closeErr)
		}
		if len(content) > MaxFileBytes {
			return nil, fmt.Errorf("%w: file %s exceeds %d bytes", ErrInvalidPackage, cleaned, MaxFileBytes)
		}
		raw = append(raw, rawFile{name: cleaned, content: content})
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: archive contains no files", ErrInvalidPackage)
	}

	raw = stripSingleRoot(raw)
	files := make([]File, 0, len(raw))
	var totalBytes int64
	hasScripts := false
	skillIndex := -1
	for index, item := range raw {
		if strings.EqualFold(item.name, "SKILL.md") && !strings.Contains(item.name, "/") {
			skillIndex = index
		}
		if IsScriptPath(item.name) && len(item.content) > MaxScriptBytes {
			return nil, fmt.Errorf("%w: script %s exceeds %d bytes", ErrInvalidPackage, item.name, MaxScriptBytes)
		}
		totalBytes += int64(len(item.content))
		if totalBytes > MaxTotalBytes {
			return nil, fmt.Errorf("%w: package exceeds %d bytes", ErrInvalidPackage, MaxTotalBytes)
		}
		digest := sha256.Sum256(item.content)
		isScript := IsScriptPath(item.name)
		hasScripts = hasScripts || isScript
		files = append(files, File{
			Path: item.name, Content: item.content, MIME: mimeFor(item.name),
			SHA256: hex.EncodeToString(digest[:]), IsScript: isScript,
		})
	}
	if skillIndex < 0 {
		return nil, fmt.Errorf("%w: root SKILL.md is required", ErrInvalidPackage)
	}

	meta, body, err := parseFrontmatter(raw[skillIndex].content)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = strings.TrimSpace(fallbackName)
	}
	if name == "" {
		name = "未命名技能"
	}
	if len(name) > MaxNameLength {
		return nil, fmt.Errorf("%w: skill name exceeds %d bytes", ErrInvalidPackage, MaxNameLength)
	}
	description := strings.TrimSpace(meta.Description)
	if description == "" {
		description = firstParagraph(body)
	}
	if len(description) > MaxDescriptionLength {
		description = description[:MaxDescriptionLength]
	}
	if len(body) > MaxPromptBytes {
		body = body[:MaxPromptBytes]
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: SKILL.md body is required", ErrInvalidPackage)
	}

	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return &Package{
		Name: name, Description: description, PromptTemplate: strings.TrimSpace(body),
		Entrypoint: "SKILL.md", Files: files, Digest: digestFiles(files),
		TotalBytes: totalBytes, HasScripts: hasScripts,
	}, nil
}

// ParseMarkdown 解析单个 SKILL.md 内容，复用 zip 包相同的 frontmatter 与正文规则。
// fallbackName 仅在 frontmatter 缺少 name 时使用。
func ParseMarkdown(content []byte, fallbackName string) (*Package, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("%w: SKILL.md is empty", ErrInvalidPackage)
	}
	meta, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = strings.TrimSpace(fallbackName)
	}
	if name == "" {
		name = "未命名技能"
	}
	if len(name) > MaxNameLength {
		return nil, fmt.Errorf("%w: skill name exceeds %d bytes", ErrInvalidPackage, MaxNameLength)
	}
	description := strings.TrimSpace(meta.Description)
	if description == "" {
		description = firstParagraph(body)
	}
	if len(description) > MaxDescriptionLength {
		description = description[:MaxDescriptionLength]
	}
	if len(body) > MaxPromptBytes {
		body = body[:MaxPromptBytes]
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: SKILL.md body is required", ErrInvalidPackage)
	}
	digest := sha256.Sum256(content)
	files := []File{{
		Path: "SKILL.md", Content: content, MIME: "text/markdown; charset=utf-8",
		SHA256: hex.EncodeToString(digest[:]), IsScript: false,
	}}
	return &Package{
		Name: name, Description: description, PromptTemplate: strings.TrimSpace(body),
		Entrypoint: "SKILL.md", Files: files, Digest: digestFiles(files),
		TotalBytes: int64(len(content)), HasScripts: false,
	}, nil
}

// normalizePath 校验并规范化 zip 内路径，拒绝目录穿越与非常规路径。
func normalizePath(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPackage)
	}
	if strings.Contains(trimmed, "\\") {
		return "", fmt.Errorf("%w: backslash is not allowed: %s", ErrInvalidPackage, name)
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("%w: absolute path is not allowed: %s", ErrInvalidPackage, name)
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: path traversal is not allowed: %s", ErrInvalidPackage, name)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: invalid path segment in %s", ErrInvalidPackage, name)
		}
	}
	if len(strings.Split(cleaned, "/")) > MaxPathDepth {
		return "", fmt.Errorf("%w: path depth exceeds %d: %s", ErrInvalidPackage, MaxPathDepth, name)
	}
	return cleaned, nil
}

// rawFile 是解压后尚未解析的原始文件。
type rawFile struct {
	name    string
	content []byte
}

// stripSingleRoot 在压缩包只有唯一顶层目录时剥离该目录，兼容 GitHub 打包习惯。
func stripSingleRoot(files []rawFile) []rawFile {
	if len(files) == 0 {
		return files
	}
	root := ""
	for _, item := range files {
		separator := strings.Index(item.name, "/")
		if separator <= 0 {
			return files
		}
		segment := item.name[:separator]
		if root == "" {
			root = segment
		} else if root != segment {
			return files
		}
	}
	stripped := make([]rawFile, 0, len(files))
	for _, item := range files {
		stripped = append(stripped, rawFile{name: strings.TrimPrefix(item.name, root+"/"), content: item.content})
	}
	return stripped
}

// frontmatter 是 SKILL.md 的 YAML 头信息。
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseFrontmatter 解析以 --- 包裹的 YAML frontmatter，并返回正文。
func parseFrontmatter(content []byte) (frontmatter, string, error) {
	text := strings.TrimPrefix(string(content), "\uFEFF")
	if !strings.HasPrefix(text, "---") {
		return frontmatter{}, text, nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	end := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			end = index
			break
		}
	}
	if end < 0 {
		return frontmatter{}, text, nil
	}
	var meta frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &meta); err != nil {
		return frontmatter{}, "", fmt.Errorf("%w: invalid frontmatter: %v", ErrInvalidPackage, err)
	}
	return meta, strings.Join(lines[end+1:], "\n"), nil
}

// firstParagraph 从正文提取首个非标题段落，作为缺失 description 时的兜底。
func firstParagraph(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		return trimmed
	}
	return ""
}

// mimeFor 按扩展名推断媒体类型。
func mimeFor(name string) string {
	if value, ok := mimeByExtension[strings.ToLower(path.Ext(name))]; ok {
		return value
	}
	return "application/octet-stream"
}

// digestFiles 计算目录树摘要，路径与内容任一变化都会改变摘要。
func digestFiles(files []File) string {
	hash := sha256.New()
	for _, file := range files {
		hash.Write([]byte(file.Path))
		hash.Write([]byte{0})
		hash.Write([]byte(file.SHA256))
		hash.Write([]byte("\n"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
