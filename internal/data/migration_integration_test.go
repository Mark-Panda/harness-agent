//go:build integration

package data

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// migrationDirectory 返回仓库的迁移目录绝对路径；集成测试从包目录运行，必须显式上溯。
func migrationDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations directory: %v", err)
	}
	return directory
}

// latestMigrationVersion 解析目录中最高的迁移版本号。
func latestMigrationVersion(t *testing.T, directory string) int {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	latest := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("invalid migration name %s", name)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		t.Fatal("no migrations found")
	}
	return latest
}

// createMigrationDatabase 创建独立的临时数据库，返回其 DSN 与清理函数。
func createMigrationDatabase(t *testing.T, adminDSN string, admin *gorm.DB) (string, func()) {
	t.Helper()
	name := "harness_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatalf("create temp database: %v", err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("admin DSN must be a URL: %v", err)
	}
	parsed.Path = "/" + name
	cleanup := func() {
		if err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error; err != nil {
			t.Logf("drop temp database %s: %v", name, err)
		}
	}
	return parsed.String(), cleanup
}

// openMigrator 打开指向临时数据库的迁移器，并在测试结束时关闭。
func openMigrator(t *testing.T, sourceURL, dsn string) *migrate.Migrate {
	t.Helper()
	m, err := migrate.New(sourceURL, dsn)
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	t.Cleanup(func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil || databaseErr != nil {
			t.Logf("close migrator: source=%v database=%v", sourceErr, databaseErr)
		}
	})
	return m
}

// assertMigrationVersion 断言当前版本与 dirty 标记。
func assertMigrationVersion(t *testing.T, m *migrate.Migrate, want int, wantDirty bool) {
	t.Helper()
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if int(version) != want || dirty != wantDirty {
		t.Fatalf("migration state = version %d dirty %t, want %d dirty %t", version, dirty, want, wantDirty)
	}
}

// copyMigrations 复制全部迁移文件到临时目录，供失败重入测试改写单个文件。
func copyMigrations(t *testing.T, source string) string {
	t.Helper()
	target := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		from, err := os.Open(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name(), err)
		}
		to, err := os.Create(filepath.Join(target, entry.Name()))
		if err != nil {
			_ = from.Close()
			t.Fatalf("create %s: %v", entry.Name(), err)
		}
		if _, err := io.Copy(to, from); err != nil {
			_ = from.Close()
			_ = to.Close()
			t.Fatalf("copy %s: %v", entry.Name(), err)
		}
		_ = from.Close()
		if err := to.Close(); err != nil {
			t.Fatalf("close %s: %v", entry.Name(), err)
		}
	}
	return target
}

// writeMigration 写入指定版本与名称的迁移文件。
func writeMigration(t *testing.T, directory string, version int, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, fmt.Sprintf("%06d_%s.up.sql", version, name))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write migration %s: %v", path, err)
	}
	return path
}

// TestMigrationEmptyDatabaseUpgrade 验证空库可一次性升级到最新版本且 schema_migrations 不为 dirty。
func TestMigrationEmptyDatabaseUpgrade(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("integration database is not required")
	}
	adminDSN := integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable")
	admin := openIntegrationDatabase(t, adminDSN)
	dsn, cleanup := createMigrationDatabase(t, adminDSN, admin)
	t.Cleanup(cleanup)

	directory := migrationDirectory(t)
	latest := latestMigrationVersion(t, directory)
	m := openMigrator(t, "file://"+directory, dsn)
	if err := m.Up(); err != nil {
		t.Fatalf("empty database upgrade: %v", err)
	}
	assertMigrationVersion(t, m, latest, false)

	// 关键表与向量扩展必须存在于临时库；否则后续 Repository 集成测试会静默失效。
	temp := openIntegrationDatabase(t, dsn)
	for _, table := range []string{"users", "sessions", "messages", "memory_vectors", "password_reset_tokens", "audit_logs"} {
		var count int64
		if err := temp.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = ?", table).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("table %s missing after migration: count=%d err=%v", table, count, err)
		}
	}
	var extension string
	if err := temp.Raw("SELECT extversion FROM pg_extension WHERE extname = 'vector'").Scan(&extension).Error; err != nil || extension == "" {
		t.Fatalf("pgvector extension missing: version=%q err=%v", extension, err)
	}

	// 应用角色对一次性重置令牌只保留账号删除所需的 SELECT/DELETE；
	// 若历史迁移的 GRANT ALL 漂移回来，这里会阻止升级。
	var grants []string
	if err := temp.Raw(`SELECT privilege_type FROM information_schema.role_table_grants
		WHERE grantee = 'harness_app' AND table_name = 'password_reset_tokens'
		ORDER BY privilege_type`).Scan(&grants).Error; err != nil {
		t.Fatalf("read password reset grants: %v", err)
	}
	want := []string{"DELETE", "SELECT"}
	if len(grants) != len(want) || grants[0] != want[0] || grants[1] != want[1] {
		t.Fatalf("password_reset_tokens grants = %v, want %v", grants, want)
	}
}

// TestMigrationVersionToVersionUpgrade 验证版本间升级，并证明旧代码使用的列在新 schema 下仍然可用。
func TestMigrationVersionToVersionUpgrade(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("integration database is not required")
	}
	adminDSN := integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable")
	admin := openIntegrationDatabase(t, adminDSN)
	dsn, cleanup := createMigrationDatabase(t, adminDSN, admin)
	t.Cleanup(cleanup)

	directory := migrationDirectory(t)
	latest := latestMigrationVersion(t, directory)
	m := openMigrator(t, "file://"+directory, dsn)
	half := latest / 2
	if err := m.Steps(half); err != nil {
		t.Fatalf("apply first %d migrations: %v", half, err)
	}
	assertMigrationVersion(t, m, half, false)
	temp := openIntegrationDatabase(t, dsn)

	// 中间版本执行旧代码查询：这些列在后续 expand 迁移中不得消失。
	var sessionCount, userCount int64
	if err := temp.Raw("SELECT count(*) FROM sessions WHERE created_at >= now() - interval '1 day'").Scan(&sessionCount).Error; err != nil {
		t.Fatalf("legacy sessions query at version %d: %v", half, err)
	}
	if err := temp.Raw("SELECT count(*) FROM users WHERE status = 'active'").Scan(&userCount).Error; err != nil {
		t.Fatalf("legacy users query at version %d: %v", half, err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("upgrade from version %d to %d: %v", half, latest, err)
	}
	assertMigrationVersion(t, m, latest, false)
	// 升级后旧查询仍必须成功，证明 Expand 阶段保持向后兼容。
	if err := temp.Raw("SELECT count(*) FROM sessions WHERE created_at >= now() - interval '1 day'").Scan(&sessionCount).Error; err != nil {
		t.Fatalf("legacy sessions query at latest version: %v", err)
	}
	if err := temp.Raw("SELECT count(*) FROM users WHERE status = 'active'").Scan(&userCount).Error; err != nil {
		t.Fatalf("legacy users query at latest version: %v", err)
	}
}

// TestMigrationFailureReentryAndRollForward 验证失败迁移留下 dirty 标记后，
// 修复 SQL、force 回退版本并重新 up 可以前滚修复，不需要人工删库。
func TestMigrationFailureReentryAndRollForward(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("integration database is not required")
	}
	adminDSN := integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable")
	admin := openIntegrationDatabase(t, adminDSN)
	dsn, cleanup := createMigrationDatabase(t, adminDSN, admin)
	t.Cleanup(cleanup)

	source := copyMigrations(t, migrationDirectory(t))
	latest := latestMigrationVersion(t, source)
	writeMigration(t, source, latest+1, "intentionally_broken", "CREATE TABLE broken (;")
	m := openMigrator(t, "file://"+source, dsn)
	if err := m.Up(); err == nil {
		t.Fatal("expected migration failure, got nil")
	}
	assertMigrationVersion(t, m, latest+1, true)

	// 修复 SQL 后回退到失败前版本，再次 up；迁移器应重新执行并清除 dirty。
	writeMigration(t, source, latest+1, "intentionally_broken", "CREATE TABLE repaired (id bigint primary key);")
	if err := m.Force(latest); err != nil {
		t.Fatalf("force version %d: %v", latest, err)
	}
	assertMigrationVersion(t, m, latest, false)
	if err := m.Up(); err != nil {
		t.Fatalf("roll forward after repair: %v", err)
	}
	assertMigrationVersion(t, m, latest+1, false)

	temp := openIntegrationDatabase(t, dsn)
	var count int64
	if err := temp.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'repaired'").Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("repaired table missing: count=%d err=%v", count, err)
	}
}

// TestMigrationVersionsAreOrdered 验证迁移版本连续且无重复；失败时提示缺失版本。
func TestMigrationVersionsAreOrdered(t *testing.T) {
	directory := migrationDirectory(t)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	versions := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			t.Fatalf("invalid migration name %s", entry.Name())
		}
		versions = append(versions, version)
	}
	sort.Ints(versions)
	for index := 1; index < len(versions); index++ {
		if versions[index] != versions[index-1]+1 {
			t.Fatalf("migration versions are not contiguous: %d after %d", versions[index], versions[index-1])
		}
	}
}
