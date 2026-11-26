package biz

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFileTool 构造限制在临时根目录的文件工具。
func newTestFileTool(t *testing.T, name string) *fileTool {
	t.Helper()
	return &fileTool{name: name, config: BuiltinFilesConfig{
		AllowLocalExecution: true, Root: t.TempDir(),
		MaxReadBytes: 64 << 10, MaxWriteBytes: 64 << 10, MaxResults: 50,
	}.normalize()}
}

// TestFileToolsReadWriteEditList 验证写入、读取、编辑与目录列举的基本链路。
func TestFileToolsReadWriteEditList(t *testing.T) {
	write := newTestFileTool(t, builtinToolWriteFile)
	root := write.config.Root
	output, err := write.Invoke(context.Background(), json.RawMessage(`{"path":"notes/hello.txt","content":"hello\nworld\n"}`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var writeResult struct {
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(output, &writeResult); err != nil || !writeResult.Created {
		t.Fatalf("write result = %s, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "hello.txt")); err != nil {
		t.Fatalf("file was not created: %v", err)
	}

	read := newTestFileTool(t, builtinToolReadFile)
	read.config.Root = root
	output, err = read.Invoke(context.Background(), json.RawMessage(`{"path":"notes/hello.txt"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var readResult struct {
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
	}
	if err := json.Unmarshal(output, &readResult); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if readResult.Content != "hello\nworld\n" || readResult.TotalLines != 3 {
		t.Fatalf("read result = %+v", readResult)
	}

	edit := newTestFileTool(t, builtinToolEditFile)
	edit.config.Root = root
	if _, err := edit.Invoke(context.Background(), json.RawMessage(`{"path":"notes/hello.txt","old_string":"world","new_string":"there"}`)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes", "hello.txt"))
	if err != nil || string(data) != "hello\nthere\n" {
		t.Fatalf("edited content = %q, %v", data, err)
	}

	list := newTestFileTool(t, builtinToolListDir)
	list.config.Root = root
	output, err = list.Invoke(context.Background(), json.RawMessage(`{"path":"notes"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listResult struct {
		Entries []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(output, &listResult); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResult.Entries) != 1 || listResult.Entries[0].Name != "hello.txt" || listResult.Entries[0].Type != "file" {
		t.Fatalf("list result = %+v", listResult)
	}
}

// TestFileToolRejectsPathEscape 验证绝对路径与 .. 越界都会被拒绝。
func TestFileToolRejectsPathEscape(t *testing.T) {
	tool := newTestFileTool(t, builtinToolReadFile)
	for _, path := range []string{"../secret.txt", "/etc/passwd", "notes/../../secret.txt"} {
		payload, _ := json.Marshal(map[string]string{"path": path})
		if _, err := tool.Invoke(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("path %q error = %v", path, err)
		}
	}
}

// TestFileToolRejectsSymlinkEscape 验证指向根目录之外的符号链接不能绕过限制。
func TestFileToolRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	tool := &fileTool{name: builtinToolReadFile, config: BuiltinFilesConfig{
		AllowLocalExecution: true, Root: root, MaxReadBytes: 4096, MaxWriteBytes: 4096, MaxResults: 10,
	}.normalize()}
	for _, path := range []string{"linked/secret.txt", "linked/new.txt"} {
		payload, _ := json.Marshal(map[string]string{"path": path})
		if _, err := tool.Invoke(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("path %q error = %v", path, err)
		}
	}
}

// TestGlobAndGrepRespectRootAndIgnoreDirs 验证 glob/grep 跳过依赖目录并限制在根目录内。
func TestGlobAndGrepRespectRootAndIgnoreDirs(t *testing.T) {
	glob := newTestFileTool(t, builtinToolGlob)
	root := glob.config.Root
	files := map[string]string{
		"a.go":                  "package a\nfunc Foo() {}\n",
		"sub/b.go":              "package b\n// TODO fix\n",
		"node_modules/skip.js":  "TODO\n",
		"sub/node_modules/x.go": "TODO\n",
	}
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := glob.Invoke(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var globResult struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(output, &globResult); err != nil {
		t.Fatalf("decode glob: %v", err)
	}
	if len(globResult.Matches) != 2 || globResult.Matches[0] != "a.go" || globResult.Matches[1] != "sub/b.go" {
		t.Fatalf("glob matches = %v", globResult.Matches)
	}

	grep := newTestFileTool(t, builtinToolGrep)
	grep.config.Root = root
	output, err = grep.Invoke(context.Background(), json.RawMessage(`{"pattern":"TODO"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	var grepResult struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"matches"`
		FilesScanned int `json:"files_scanned"`
	}
	if err := json.Unmarshal(output, &grepResult); err != nil {
		t.Fatalf("decode grep: %v", err)
	}
	if len(grepResult.Matches) != 1 || grepResult.Matches[0].Path != "sub/b.go" || grepResult.Matches[0].Line != 2 {
		t.Fatalf("grep matches = %+v", grepResult.Matches)
	}
	if grepResult.FilesScanned != 2 {
		t.Fatalf("files scanned = %d, want 2", grepResult.FilesScanned)
	}
}

// TestFileToolRequiresLocalExecution 验证未开启本机访问时失败关闭。
func TestFileToolRequiresLocalExecution(t *testing.T) {
	tool := &fileTool{name: builtinToolReadFile, config: BuiltinFilesConfig{}.normalize()}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"a.txt"}`)); !errors.Is(err, ErrBuiltinToolUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

// TestFileToolRiskLevels 验证写操作需要审批，读取与检索为低风险。
func TestFileToolRiskLevels(t *testing.T) {
	for name, want := range map[string]string{
		builtinToolReadFile: "low", builtinToolListDir: "low", builtinToolGlob: "low", builtinToolGrep: "low",
		builtinToolWriteFile: "medium", builtinToolEditFile: "medium",
	} {
		tool := &fileTool{name: name}
		if got := tool.RiskLevel(); got != want {
			t.Fatalf("%s risk = %q, want %q", name, got, want)
		}
	}
	if _, ok := builtinToolName(builtinToolIdentity(builtinToolReadFile)); !ok {
		t.Fatal("file tool identity is not recognized")
	}
}
