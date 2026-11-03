package data

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// expandContractBaselineVersion 是已上线的最后一个迁移版本；
// 高于此版本的新迁移必须遵守在线索引与幂等建表规则。
const expandContractBaselineVersion = 28

// destructiveMigrationPattern 匹配会破坏旧应用兼容性的迁移语句。
// 策略：只允许 expand-contract 的加法迁移；删除列/表、改类型、重命名必须由人工审批的 Contract 变更执行。
var destructiveMigrationPattern = regexp.MustCompile(`(?i)\b(DROP\s+TABLE|DROP\s+COLUMN|TRUNCATE|RENAME\s+(COLUMN|TO)|ALTER\s+COLUMN\s+[\w"]+\s+TYPE)\b`)

// addColumnPattern 匹配 ADD COLUMN 子句，用于检查新增列是否能在旧数据上安全回填。
var addColumnPattern = regexp.MustCompile(`(?is)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?[^,;]+`)

// createIndexPattern 定位 CREATE INDEX 语句起点。
var createIndexPattern = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\b`)

// createTablePattern 定位 CREATE TABLE 语句起点。
var createTablePattern = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNLOGGED\s+)?TABLE\b`)

// validateMigrationContent 校验单个迁移文件是否符合 Expand → Migrate → Contract 约束。
// 关键步骤：先拒绝破坏性 DDL，再检查新增列可回填，最后对基线之后的新迁移强制在线索引与幂等建表。
func validateMigrationContent(name string, version int, content string) error {
	if match := destructiveMigrationPattern.FindString(content); match != "" {
		return fmt.Errorf("migration %s contains destructive statement %q; move it to an approved contract change", name, match)
	}
	for _, clause := range addColumnPattern.FindAllString(content, -1) {
		lower := strings.ToLower(clause)
		if strings.Contains(lower, "not null") && !strings.Contains(lower, "default") {
			return fmt.Errorf("migration %s adds NOT NULL column without DEFAULT; old rows cannot be backfilled during expand", name)
		}
	}
	if version <= expandContractBaselineVersion {
		return nil
	}
	for _, location := range createIndexPattern.FindAllStringIndex(content, -1) {
		statement := strings.ToLower(statementUntilSemicolon(content, location[0]))
		if !strings.Contains(statement, "concurrently") || !strings.Contains(statement, "if not exists") {
			return fmt.Errorf("migration %s must use CREATE INDEX CONCURRENTLY IF NOT EXISTS to keep old code online", name)
		}
	}
	for _, location := range createTablePattern.FindAllStringIndex(content, -1) {
		statement := strings.ToLower(statementUntilSemicolon(content, location[0]))
		if !strings.Contains(statement, "if not exists") {
			return fmt.Errorf("migration %s must use CREATE TABLE IF NOT EXISTS for safe re-entry", name)
		}
	}
	return nil
}

// statementUntilSemicolon 截取从指定位置到下一个分号的语句，限制检查范围。
func statementUntilSemicolon(content string, start int) string {
	end := strings.Index(content[start:], ";")
	if end < 0 {
		return content[start:]
	}
	return content[start : start+end]
}

// TestMigrationsAreForwardCompatible 验证迁移只做加法变更且版本连续。
func TestMigrationsAreForwardCompatible(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no migrations found")
	}
	versions := map[int]string{}
	for _, path := range paths {
		name := filepath.Base(path)
		if strings.HasSuffix(name, ".down.sql") {
			t.Fatalf("destructive downgrade migration is forbidden: %s", name)
		}
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			t.Fatalf("invalid migration name %s", name)
		}
		versions[version] = name
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateMigrationContent(name, version, string(content)); err != nil {
			t.Fatal(err)
		}
	}
	keys := make([]int, 0, len(versions))
	for version := range versions {
		keys = append(keys, version)
	}
	sort.Ints(keys)
	for index, version := range keys {
		if index > 0 && version != keys[index-1]+1 {
			t.Fatalf("migration versions are not contiguous: %d after %d", version, keys[index-1])
		}
	}
}

// TestMigrationChecksumsAreImmutable 验证已发布迁移内容未被修改。
// 已应用的迁移一旦变更会导致新库与旧库 schema 分叉；新增迁移后必须重跑 scripts/migration_checksum.sh。
func TestMigrationChecksumsAreImmutable(t *testing.T) {
	directory := filepath.Join("..", "..", "migrations")
	contents, err := os.ReadFile(filepath.Join(directory, "checksums.txt"))
	if err != nil {
		t.Fatalf("read migration checksums: %v", err)
	}
	expected := map[string]string{}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid checksum line %q", line)
		}
		expected[parts[1]] = parts[0]
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		name := filepath.Base(path)
		seen[name] = true
		hash, ok := expected[name]
		if !ok {
			t.Fatalf("migration %s has no checksum; run scripts/migration_checksum.sh", name)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(content))
		if actual != hash {
			t.Fatalf("migration %s content changed after release; create a new migration instead", name)
		}
	}
	for name := range expected {
		if !seen[name] {
			t.Fatalf("checksum references missing migration %s", name)
		}
	}
}

// TestMigrationRulesRejectUnsafePatterns 使用合成 SQL 证明规则确实会拦截不安全变更。
func TestMigrationRulesRejectUnsafePatterns(t *testing.T) {
	cases := []struct {
		name    string
		version int
		content string
		wantErr bool
	}{
		{name: "contract drop", version: 29, content: "ALTER TABLE users DROP COLUMN legacy;", wantErr: true},
		{name: "not null without default", version: 29, content: "ALTER TABLE users ADD COLUMN locale text NOT NULL;", wantErr: true},
		{name: "nullable add", version: 29, content: "ALTER TABLE users ADD COLUMN locale text;", wantErr: false},
		{name: "not null with default", version: 29, content: "ALTER TABLE users ADD COLUMN locale text NOT NULL DEFAULT '';", wantErr: false},
		{name: "online index", version: 29, content: "CREATE INDEX CONCURRENTLY IF NOT EXISTS users_locale_idx ON users (locale);", wantErr: false},
		{name: "blocking index", version: 29, content: "CREATE INDEX users_locale_idx ON users (locale);", wantErr: true},
		{name: "baseline blocking index", version: 28, content: "CREATE INDEX users_locale_idx ON users (locale);", wantErr: false},
		{name: "idempotent table", version: 29, content: "CREATE TABLE IF NOT EXISTS feature_flags (id bigint primary key);", wantErr: false},
		{name: "non idempotent table", version: 29, content: "CREATE TABLE feature_flags (id bigint primary key);", wantErr: true},
	}
	for _, testCase := range cases {
		err := validateMigrationContent("synthetic.up.sql", testCase.version, testCase.content)
		if testCase.wantErr && err == nil {
			t.Errorf("%s: expected rejection", testCase.name)
		}
		if !testCase.wantErr && err != nil {
			t.Errorf("%s: unexpected rejection: %v", testCase.name, err)
		}
	}
}
