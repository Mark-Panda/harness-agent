package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Algorithm 是本系统备份加密算法标识。
const Algorithm = "AES-256-GCM"

// Manifest 描述一次加密备份的来源、校验和与恢复验收基线。
type Manifest struct {
	// Version Manifest 版本，用于后续格式演进。
	Version int `json:"version"`
	// CreatedAt 备份创建时间（UTC）。
	CreatedAt time.Time `json:"created_at"`
	// Database 备份的逻辑数据库名。
	Database string `json:"database"`
	// KeyID 加密密钥标识，不包含密钥本身。
	KeyID string `json:"key_id"`
	// Algorithm 加密算法。
	Algorithm string `json:"algorithm"`
	// PlaintextSHA256 解密后备份文件哈希，用于恢复前校验完整性。
	PlaintextSHA256 string `json:"plaintext_sha256"`
	// CiphertextSHA256 加密文件哈希，用于跨故障域副本比对。
	CiphertextSHA256 string `json:"ciphertext_sha256"`
	// PlaintextBytes 解密后大小。
	PlaintextBytes int64 `json:"plaintext_bytes"`
	// CiphertextBytes 加密文件大小。
	CiphertextBytes int64 `json:"ciphertext_bytes"`
	// Stats 恢复验收基线：迁移版本、表行数、RLS 与业务一致性。
	Stats Stats `json:"stats"`
}

// Stats 是数据库恢复验收基线。
type Stats struct {
	// CapturedAt 采集时间。
	CapturedAt time.Time `json:"captured_at"`
	// MigrationVersion 迁移版本。
	MigrationVersion int64 `json:"migration_version"`
	// MigrationDirty 是否存在失败迁移。
	MigrationDirty bool `json:"migration_dirty"`
	// TableCounts 各业务表精确行数。
	TableCounts map[string]int64 `json:"table_counts"`
	// RLSEnabledTables 启用行级安全的表，必须与恢复目标完全一致。
	RLSEnabledTables []string `json:"rls_enabled_tables"`
	// RLSPolicyCount 行级安全策略总数。
	RLSPolicyCount int64 `json:"rls_policy_count"`
	// Extensions 已安装扩展及版本，恢复目标必须包含相同向量扩展。
	Extensions map[string]string `json:"extensions"`
	// VectorEmbeddingType memory_vectors.embedding 的列类型，用于验证 pgvector 可用。
	VectorEmbeddingType string `json:"vector_embedding_type"`
	// BusinessChecks 业务一致性检查的孤儿记录数，正常必须全部为 0。
	BusinessChecks map[string]int64 `json:"business_checks"`
}

// businessCheckQueries 定义恢复后必须为零的孤儿记录检查。
// 说明：这些检查不依赖具体业务状态，只验证外键式关联与租户边界未被破坏。
var businessCheckQueries = map[string]string{
	"messages_without_session":          `SELECT count(*) FROM messages m LEFT JOIN sessions s ON s.id = m.session_id WHERE m.session_id IS NOT NULL AND s.id IS NULL`,
	"agent_runs_without_session":        `SELECT count(*) FROM agent_runs r LEFT JOIN sessions s ON s.id = r.session_id WHERE r.session_id IS NOT NULL AND s.id IS NULL`,
	"execution_steps_without_run":       `SELECT count(*) FROM execution_steps e LEFT JOIN agent_runs r ON r.id = e.run_id WHERE e.run_id IS NOT NULL AND r.id IS NULL`,
	"memory_vectors_without_session":    `SELECT count(*) FROM memory_vectors v LEFT JOIN sessions s ON s.id = v.session_id WHERE v.session_id IS NOT NULL AND s.id IS NULL`,
	"session_summaries_without_session": `SELECT count(*) FROM session_summaries x LEFT JOIN sessions s ON s.id = x.session_id WHERE x.session_id IS NOT NULL AND s.id IS NULL`,
	"plans_without_session":             `SELECT count(*) FROM plans p LEFT JOIN sessions s ON s.id = p.session_id WHERE p.session_id IS NOT NULL AND s.id IS NULL`,
}

// CaptureStats 采集当前数据库的恢复验收基线。
// 关键步骤：表名来自 pg_tables，逐表精确计数；同时记录 RLS 表集合与策略总数。
func CaptureStats(ctx context.Context, db *gorm.DB) (Stats, error) {
	if db == nil {
		return Stats{}, fmt.Errorf("database is required")
	}
	stats := Stats{CapturedAt: time.Now().UTC(), TableCounts: map[string]int64{}, BusinessChecks: map[string]int64{}}
	tables, err := queryStrings(ctx, db, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		return Stats{}, fmt.Errorf("list backup tables: %w", err)
	}
	for _, table := range tables {
		count, err := queryInt64(ctx, db, fmt.Sprintf("SELECT count(*) FROM %s", quoteIdentifier(table)))
		if err != nil {
			return Stats{}, fmt.Errorf("count table %s: %w", table, err)
		}
		stats.TableCounts[table] = count
	}
	rlsTables, err := queryStrings(ctx, db, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity
		ORDER BY c.relname`)
	if err != nil {
		return Stats{}, fmt.Errorf("list RLS tables: %w", err)
	}
	stats.RLSEnabledTables = rlsTables
	stats.RLSPolicyCount, err = queryInt64(ctx, db, `SELECT count(*) FROM pg_policies WHERE schemaname = 'public'`)
	if err != nil {
		return Stats{}, fmt.Errorf("count RLS policies: %w", err)
	}
	// 向量与扩展属于事实数据：恢复目标必须安装相同版本 pgvector。
	stats.Extensions, err = queryExtensionVersions(ctx, db)
	if err != nil {
		return Stats{}, err
	}
	vectorType, err := queryStrings(ctx, db, `
		SELECT udt_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'memory_vectors' AND column_name = 'embedding'`)
	if err != nil {
		return Stats{}, fmt.Errorf("read vector column type: %w", err)
	}
	if len(vectorType) > 0 {
		stats.VectorEmbeddingType = vectorType[0]
	}
	version, dirty, err := queryMigrationState(ctx, db)
	if err != nil {
		return Stats{}, err
	}
	stats.MigrationVersion, stats.MigrationDirty = version, dirty
	for name, query := range businessCheckQueries {
		count, err := queryInt64(ctx, db, query)
		if err != nil {
			return Stats{}, fmt.Errorf("business check %s: %w", name, err)
		}
		stats.BusinessChecks[name] = count
	}
	return stats, nil
}

// Verify 校验恢复目标与 Manifest 基线一致。
// 关键步骤：先校验备份文件哈希，再比对迁移版本、表行数、RLS 表集合与孤儿记录。
func Verify(ctx context.Context, db *gorm.DB, manifest Manifest, plaintextPath, ciphertextPath string) error {
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d", manifest.Version)
	}
	if manifest.Algorithm != Algorithm {
		return fmt.Errorf("unsupported backup algorithm %q", manifest.Algorithm)
	}
	var problems []string
	if plaintextPath != "" {
		actual, err := HashFile(plaintextPath)
		if err != nil {
			return err
		}
		if actual != manifest.PlaintextSHA256 {
			problems = append(problems, fmt.Sprintf("plaintext sha256 = %s, want %s", actual, manifest.PlaintextSHA256))
		}
	}
	if ciphertextPath != "" {
		actual, err := HashFile(ciphertextPath)
		if err != nil {
			return err
		}
		if actual != manifest.CiphertextSHA256 {
			problems = append(problems, fmt.Sprintf("ciphertext sha256 = %s, want %s", actual, manifest.CiphertextSHA256))
		}
	}
	actual, err := CaptureStats(ctx, db)
	if err != nil {
		return fmt.Errorf("capture restore stats: %w", err)
	}
	problems = append(problems, compareStats(manifest.Stats, actual)...)
	if len(problems) > 0 {
		return fmt.Errorf("backup verification failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// compareStats 返回基线与实际统计的差异列表，空列表表示一致。
func compareStats(want, got Stats) []string {
	var problems []string
	if want.MigrationVersion != got.MigrationVersion || want.MigrationDirty != got.MigrationDirty {
		problems = append(problems, fmt.Sprintf("migration state = %d/%t, want %d/%t",
			got.MigrationVersion, got.MigrationDirty, want.MigrationVersion, want.MigrationDirty))
	}
	if len(want.TableCounts) != len(got.TableCounts) {
		problems = append(problems, fmt.Sprintf("table count = %d, want %d", len(got.TableCounts), len(want.TableCounts)))
	}
	for table, expected := range want.TableCounts {
		if actual, ok := got.TableCounts[table]; !ok || actual != expected {
			problems = append(problems, fmt.Sprintf("table %s = %d (present=%t), want %d", table, actual, ok, expected))
		}
	}
	wantRLS, gotRLS := append([]string(nil), want.RLSEnabledTables...), append([]string(nil), got.RLSEnabledTables...)
	sort.Strings(wantRLS)
	sort.Strings(gotRLS)
	if strings.Join(wantRLS, ",") != strings.Join(gotRLS, ",") {
		problems = append(problems, fmt.Sprintf("RLS tables = %v, want %v", gotRLS, wantRLS))
	}
	if want.RLSPolicyCount != got.RLSPolicyCount {
		problems = append(problems, fmt.Sprintf("RLS policies = %d, want %d", got.RLSPolicyCount, want.RLSPolicyCount))
	}
	if want.VectorEmbeddingType != got.VectorEmbeddingType {
		problems = append(problems, fmt.Sprintf("vector embedding type = %q, want %q", got.VectorEmbeddingType, want.VectorEmbeddingType))
	}
	for name, version := range want.Extensions {
		if actual, ok := got.Extensions[name]; !ok || actual != version {
			problems = append(problems, fmt.Sprintf("extension %s = %q (present=%t), want %q", name, actual, ok, version))
		}
	}
	for name, expected := range want.BusinessChecks {
		actual, ok := got.BusinessChecks[name]
		if !ok || actual != expected {
			problems = append(problems, fmt.Sprintf("business check %s = %d (present=%t), want %d", name, actual, ok, expected))
		}
	}
	return problems
}

// queryStrings 执行返回单列文本的查询。
func queryStrings(ctx context.Context, db *gorm.DB, query string) ([]string, error) {
	rows, err := db.WithContext(ctx).Raw(query).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// queryInt64 执行返回单列整数的查询。
func queryInt64(ctx context.Context, db *gorm.DB, query string) (int64, error) {
	rows, err := db.WithContext(ctx).Raw(query).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, fmt.Errorf("query returned no rows")
	}
	var value int64
	if err := rows.Scan(&value); err != nil {
		return 0, err
	}
	return value, rows.Err()
}

// queryExtensionVersions 读取已安装扩展及版本。
func queryExtensionVersions(ctx context.Context, db *gorm.DB) (map[string]string, error) {
	rows, err := db.WithContext(ctx).Raw(`SELECT extname, extversion FROM pg_extension ORDER BY extname`).Rows()
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	defer rows.Close()
	versions := map[string]string{}
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			return nil, fmt.Errorf("scan extension: %w", err)
		}
		versions[name] = version
	}
	return versions, rows.Err()
}

// queryMigrationState 读取 schema_migrations 当前版本与 dirty 标记。
func queryMigrationState(ctx context.Context, db *gorm.DB) (int64, bool, error) {
	rows, err := db.WithContext(ctx).Raw(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Rows()
	if err != nil {
		return 0, false, fmt.Errorf("read migration state: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false, fmt.Errorf("schema_migrations is empty")
	}
	var version int64
	var dirty bool
	if err := rows.Scan(&version, &dirty); err != nil {
		return 0, false, fmt.Errorf("scan migration state: %w", err)
	}
	return version, dirty, rows.Err()
}

// quoteIdentifier 为表名添加双引号，表名来自数据库目录且不允许注入。
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// HashFile 计算文件 SHA-256。
func HashFile(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- 备份文件路径由运维命令显式指定
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	sum, err := HashReader(file)
	if err != nil {
		return "", err
	}
	return sum, nil
}

// HashReader 计算流 SHA-256。
func HashReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", fmt.Errorf("hash backup: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// WriteManifest 以缩进 JSON 写入 Manifest，便于人工审阅与跨系统传递。
func WriteManifest(path string, manifest Manifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil { // #nosec G304 -- Manifest 路径由运维命令显式指定
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// ReadManifest 读取并校验 Manifest 基本结构。
func ReadManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- Manifest 路径由运维命令显式指定
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}
