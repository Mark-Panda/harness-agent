package backup_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readFile 读取备份部署文件，失败时终止测试。
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// TestBackupScriptsCoverEncryptionMirrorAndRetention 验证备份脚本包含加密、镜像与保留期。
func TestBackupScriptsCoverEncryptionMirrorAndRetention(t *testing.T) {
	content := readFile(t, "backup.sh")
	for _, token := range []string{
		"pg_dump", "stats -dsn", "encrypt", "BACKUP_MIRROR_DIR",
		"cmp ", "BACKUP_RETENTION_DAYS", "manifest.json", "expire.sh",
	} {
		if !strings.Contains(content, token) {
			t.Fatalf("backup.sh missing %q", token)
		}
	}
	for _, path := range []string{"backup.sh", "expire.sh"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", path, err)
		}
	}
}

// TestExpireScriptRemovesOnlyExpiredBackupArtifacts 实际执行过期清除脚本，
// 验证旧备份被删除、新备份与无关文件保留，且目录缺失时幂等成功。
func TestExpireScriptRemovesOnlyExpiredBackupArtifacts(t *testing.T) {
	dir := t.TempDir()
	oldAt := time.Now().Add(-40 * 24 * time.Hour)
	files := []struct {
		name string
		old  bool
	}{
		{"harness-20260101T000000Z.dump.enc", true},
		{"harness-20260101T000000Z.manifest.json", true},
		{"harness-20261102T000000Z.dump.enc", false},
		{"harness-20261102T000000Z.manifest.json", false},
		{"notes.txt", true},
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if file.old {
			if err := os.Chtimes(path, oldAt, oldAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	command := exec.Command("bash", "expire.sh", dir, "30")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("expire.sh failed: %v %s", err, output)
	}
	for _, file := range files {
		_, err := os.Stat(filepath.Join(dir, file.name))
		if file.old && file.name != "notes.txt" && err == nil {
			t.Fatalf("expired artifact was not removed: %s", file.name)
		}
		if (!file.old || file.name == "notes.txt") && err != nil {
			t.Fatalf("retained artifact was removed: %s err=%v", file.name, err)
		}
	}
	// 目录不存在时不得失败，便于定时任务幂等执行。
	if output, err := exec.Command("bash", "expire.sh", filepath.Join(dir, "missing"), "30").CombinedOutput(); err != nil {
		t.Fatalf("expire.sh on missing directory failed: %v %s", err, output)
	}
}

// TestRestoreScriptVerifiesHashAndDatabaseState 验证恢复脚本先校验哈希再验收。
func TestRestoreScriptVerifiesHashAndDatabaseState(t *testing.T) {
	content := readFile(t, "restore.sh")
	for _, token := range []string{
		"decrypt -in", "plaintext_sha256", "pg_restore",
		"CREATE DATABASE", "verify -dsn", "RECREATE_TARGET", "RTO=",
	} {
		if !strings.Contains(content, token) {
			t.Fatalf("restore.sh missing %q", token)
		}
	}
}

// TestPITROverlayEnablesWALArchiving 验证 PITR 覆盖配置启用 WAL 归档与恢复窗口。
func TestPITROverlayEnablesWALArchiving(t *testing.T) {
	overlay := readFile(t, "../compose/pitr.yaml")
	for _, token := range []string{
		"wal_level=replica", "archive_mode=on", "archive_timeout=60",
		"archive_command", "wal-archive",
	} {
		if !strings.Contains(overlay, token) {
			t.Fatalf("pitr overlay missing %q", token)
		}
	}
	script := readFile(t, "pitr-basebackup.sh")
	for _, token := range []string{"pg_basebackup", "start_lsn", "end_lsn", "pg_wal.tar.gz"} {
		if !strings.Contains(script, token) {
			t.Fatalf("pitr-basebackup.sh missing %q", token)
		}
	}
}

// TestBackupReadmeDocumentsRPOAndRTO 验证备份文档明确 RPO/RTO 与 Redis 恢复边界。
func TestBackupReadmeDocumentsRPOAndRTO(t *testing.T) {
	content := readFile(t, "README.md")
	for _, token := range []string{
		"RPO", "RTO", "15 分钟", "30 分钟", "Redis 只作为缓存", "不参与事实恢复",
	} {
		if !strings.Contains(content, token) {
			t.Fatalf("backup README missing %q", token)
		}
	}
}
