//go:build unix

package biz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestShellExecReclaimsBackgroundChildren 验证命令结束后后台子进程会被进程组回收。
func TestShellExecReclaimsBackgroundChildren(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "leaked.txt")
	tool := &shellExecTool{
		config: BuiltinShellConfig{
			AllowLocalExecution: true, AllowUnsafeLocalExecution: true,
			Timeout: 5 * time.Second, MaxOutputBytes: 4096, WorkDir: workDir,
		}.normalize(),
		shell: resolveShellBinary(""),
	}
	command := "(sleep 1; echo leaked > '" + marker + "') & echo started"
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":`+jsonQuote(command)+`}`)); err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	// 子进程若存活会在 1 秒后写标记文件。
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("background child survived and wrote %s", marker)
	}
}

// jsonQuote 返回 JSON 字符串字面量，避免手工拼接转义。
func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
