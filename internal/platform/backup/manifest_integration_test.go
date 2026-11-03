//go:build integration

package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/platform/backup"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openAdminDatabase 连接备份验收使用的管理员库；未要求数据库时跳过。
func openAdminDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("integration database is not required")
	}
	dsn := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://harness:harness@localhost:5432/harness?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestCaptureStatsAndVerifySameDatabase 验证基线采集可重放，RLS 与业务一致性检查通过。
func TestCaptureStatsAndVerifySameDatabase(t *testing.T) {
	db := openAdminDatabase(t)
	ctx := context.Background()
	stats, err := backup.CaptureStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MigrationVersion == 0 || stats.MigrationDirty {
		t.Fatalf("unexpected migration state: %d/%t", stats.MigrationVersion, stats.MigrationDirty)
	}
	if len(stats.TableCounts) == 0 || len(stats.RLSEnabledTables) == 0 || stats.RLSPolicyCount == 0 {
		t.Fatalf("incomplete stats: tables=%d rls=%d policies=%d",
			len(stats.TableCounts), len(stats.RLSEnabledTables), stats.RLSPolicyCount)
	}
	for name, count := range stats.BusinessChecks {
		if count != 0 {
			t.Fatalf("business check %s = %d, want 0", name, count)
		}
	}
	manifest := backup.Manifest{
		Version: 1, Database: "harness", Algorithm: backup.Algorithm,
		KeyID: "test", Stats: stats,
	}
	if err := backup.Verify(ctx, db, manifest, "", ""); err != nil {
		t.Fatalf("verify same database: %v", err)
	}
}

// TestVerifyDetectsTableDrift 验证恢复目标出现表集合变化时验收失败。
func TestVerifyDetectsTableDrift(t *testing.T) {
	db := openAdminDatabase(t)
	ctx := context.Background()
	stats, err := backup.CaptureStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	manifest := backup.Manifest{Version: 1, Database: "harness", Algorithm: backup.Algorithm, KeyID: "test", Stats: stats}
	probe := "backup_probe_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if err := db.Exec("CREATE TABLE " + probe + " (id int primary key)").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS " + probe).Error })
	err = backup.Verify(ctx, db, manifest, "", "")
	if err == nil || !strings.Contains(err.Error(), "table count") {
		t.Fatalf("verify drift error = %v", err)
	}
}

// TestManifestRoundTripAndHash 验证 Manifest 读写与文件哈希。
func TestManifestRoundTripAndHash(t *testing.T) {
	db := openAdminDatabase(t)
	ctx := context.Background()
	stats, err := backup.CaptureStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	manifest := backup.Manifest{Version: 1, Database: "harness", Algorithm: backup.Algorithm, KeyID: "test", Stats: stats}
	if err := backup.WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := backup.ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Database != "harness" || loaded.Stats.MigrationVersion != manifest.Stats.MigrationVersion {
		t.Fatalf("manifest roundtrip = %#v", loaded)
	}
	dump := filepath.Join(t.TempDir(), "dump.bin")
	if err := os.WriteFile(dump, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := backup.HashFile(dump)
	if err != nil || len(hash) != 64 {
		t.Fatalf("hash = %q, %v", hash, err)
	}
}
