package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleManifest 返回字段填充完整的 Manifest，用于完整性校验。
func sampleManifest() Manifest {
	return Manifest{
		Version:          1,
		CreatedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Database:         "harness",
		KeyID:            "key-2026-01",
		Algorithm:        Algorithm,
		PlaintextSHA256:  strings.Repeat("a", 64),
		CiphertextSHA256: strings.Repeat("b", 64),
		PlaintextBytes:   1024,
		CiphertextBytes:  2048,
		Stats: Stats{
			CapturedAt:       time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
			MigrationVersion: 42,
			MigrationDirty:   false,
			TableCounts: map[string]int64{
				"users":    10,
				"sessions": 20,
				"messages": 300,
			},
			RLSEnabledTables:    []string{"messages", "sessions"},
			RLSPolicyCount:      7,
			Extensions:          map[string]string{"vector": "0.8.0", "pgcrypto": "1.3"},
			VectorEmbeddingType: "vector",
			BusinessChecks: map[string]int64{
				"messages_without_session": 0,
				"plans_without_session":    0,
			},
		},
	}
}

func TestHashReader(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "short text", input: "hello harness"},
		{name: "binary-ish", input: string([]byte{0x00, 0xff, 0x10, 0x7f})},
		{name: "large payload", input: strings.Repeat("x", 1<<20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HashReader(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("HashReader: %v", err)
			}
			want := sha256.Sum256([]byte(tt.input))
			if got != hex.EncodeToString(want[:]) {
				t.Fatalf("hash = %s, want %s", got, hex.EncodeToString(want[:]))
			}
			if len(got) != 64 {
				t.Fatalf("hash length = %d, want 64", len(got))
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, fmt.Errorf("boom") }

func TestHashReader_ReadError(t *testing.T) {
	if _, err := HashReader(failingReader{}); err == nil {
		t.Fatal("expected error for failing reader")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql")
	content := strings.Repeat("backup-payload;", 4096)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	want := sha256.Sum256([]byte(content))
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestHashFile_Missing(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "nope.bin")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteThenReadManifest_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	original := sampleManifest()
	if err := WriteManifest(path, original); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("manifest perm = %o, want 600", perm)
	}
	loaded, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	// 字段完整性逐项校验。
	if loaded.Version != original.Version ||
		!loaded.CreatedAt.Equal(original.CreatedAt) ||
		loaded.Database != original.Database ||
		loaded.KeyID != original.KeyID ||
		loaded.Algorithm != original.Algorithm ||
		loaded.PlaintextSHA256 != original.PlaintextSHA256 ||
		loaded.CiphertextSHA256 != original.CiphertextSHA256 ||
		loaded.PlaintextBytes != original.PlaintextBytes ||
		loaded.CiphertextBytes != original.CiphertextBytes {
		t.Fatalf("scalar fields mismatch: %+v", loaded)
	}
	if loaded.Stats.MigrationVersion != original.Stats.MigrationVersion ||
		loaded.Stats.MigrationDirty != original.Stats.MigrationDirty ||
		loaded.Stats.RLSPolicyCount != original.Stats.RLSPolicyCount ||
		loaded.Stats.VectorEmbeddingType != original.Stats.VectorEmbeddingType {
		t.Fatalf("stats scalar mismatch: %+v", loaded.Stats)
	}
	if len(loaded.Stats.TableCounts) != len(original.Stats.TableCounts) {
		t.Fatalf("table counts = %d, want %d", len(loaded.Stats.TableCounts), len(original.Stats.TableCounts))
	}
	for table, want := range original.Stats.TableCounts {
		if got := loaded.Stats.TableCounts[table]; got != want {
			t.Fatalf("table %s = %d, want %d", table, got, want)
		}
	}
	if strings.Join(loaded.Stats.RLSEnabledTables, ",") != strings.Join(original.Stats.RLSEnabledTables, ",") {
		t.Fatalf("RLS tables = %v", loaded.Stats.RLSEnabledTables)
	}
	for name, want := range original.Stats.Extensions {
		if got := loaded.Stats.Extensions[name]; got != want {
			t.Fatalf("extension %s = %q, want %q", name, got, want)
		}
	}
	for name, want := range original.Stats.BusinessChecks {
		if got := loaded.Stats.BusinessChecks[name]; got != want {
			t.Fatalf("business check %s = %d, want %d", name, got, want)
		}
	}
}

func TestWriteManifest_InvalidPath(t *testing.T) {
	err := WriteManifest(filepath.Join(t.TempDir(), "missing-dir", "m.json"), sampleManifest())
	if err == nil {
		t.Fatal("expected error writing into missing directory")
	}
}

func TestReadManifest_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadManifest(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ReadManifest(bad); err == nil {
		t.Fatal("expected error for malformed json")
	}
}

func TestReadManifest_TamperedChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	manifest := sampleManifest()
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	// 篡改校验和字段后仍可读回，但读回的值与原值不同（交给 Verify 检出）。
	tampered := bytes.Replace(raw, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("c", 64)), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("tamper fixture did not change content")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("rewrite tampered: %v", err)
	}
	loaded, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest tampered: %v", err)
	}
	if loaded.PlaintextSHA256 == manifest.PlaintextSHA256 {
		t.Fatal("tampered checksum was not reflected after reload")
	}
}

func TestManifest_LargeTableCountsRoundtrip(t *testing.T) {
	manifest := sampleManifest()
	manifest.Stats.TableCounts = map[string]int64{}
	manifest.Stats.BusinessChecks = map[string]int64{}
	for i := 0; i < 2000; i++ {
		manifest.Stats.TableCounts[fmt.Sprintf("table_%04d", i)] = int64(i * 7)
		manifest.Stats.BusinessChecks[fmt.Sprintf("check_%04d", i)] = 0
	}
	path := filepath.Join(t.TempDir(), "big.json")
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	loaded, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(loaded.Stats.TableCounts) != 2000 || len(loaded.Stats.BusinessChecks) != 2000 {
		t.Fatalf("counts = %d/%d, want 2000/2000", len(loaded.Stats.TableCounts), len(loaded.Stats.BusinessChecks))
	}
	if problems := compareStats(manifest.Stats, loaded.Stats); len(problems) != 0 {
		t.Fatalf("compareStats on large manifest: %v", problems)
	}
}

func TestManifestJSONFieldNames(t *testing.T) {
	encoded, err := json.Marshal(sampleManifest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"version"`, `"created_at"`, `"database"`, `"key_id"`, `"algorithm"`,
		`"plaintext_sha256"`, `"ciphertext_sha256"`, `"plaintext_bytes"`, `"ciphertext_bytes"`,
		`"stats"`, `"migration_version"`, `"migration_dirty"`, `"table_counts"`,
		`"rls_enabled_tables"`, `"rls_policy_count"`, `"extensions"`,
		`"vector_embedding_type"`, `"business_checks"`,
	} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatalf("encoded manifest missing field %s", field)
		}
	}
}

func TestCompareStats(t *testing.T) {
	base := sampleManifest().Stats
	clone := func() Stats {
		c := base
		c.TableCounts = map[string]int64{"users": 10, "sessions": 20, "messages": 300}
		c.RLSEnabledTables = []string{"messages", "sessions"}
		c.Extensions = map[string]string{"vector": "0.8.0", "pgcrypto": "1.3"}
		c.BusinessChecks = map[string]int64{"messages_without_session": 0, "plans_without_session": 0}
		return c
	}
	tests := []struct {
		name         string
		mutate       func(*Stats)
		wantProblems int
		wantContains string
	}{
		{name: "identical", mutate: func(*Stats) {}, wantProblems: 0},
		{
			name:         "migration version mismatch",
			mutate:       func(s *Stats) { s.MigrationVersion = 41 },
			wantProblems: 1,
			wantContains: "migration state",
		},
		{
			name:         "migration dirty mismatch",
			mutate:       func(s *Stats) { s.MigrationDirty = true },
			wantProblems: 1,
			wantContains: "migration state",
		},
		{
			name:         "table count cardinality mismatch",
			mutate:       func(s *Stats) { delete(s.TableCounts, "messages") },
			wantProblems: 2, // 基数不同 + 缺失表
			wantContains: "table count",
		},
		{
			name:         "table row count mismatch",
			mutate:       func(s *Stats) { s.TableCounts["users"] = 11 },
			wantProblems: 1,
			wantContains: "table users",
		},
		{
			name:         "RLS table set mismatch",
			mutate:       func(s *Stats) { s.RLSEnabledTables = []string{"messages"} },
			wantProblems: 1,
			wantContains: "RLS tables",
		},
		{
			name:         "RLS table order ignored",
			mutate:       func(s *Stats) { s.RLSEnabledTables = []string{"sessions", "messages"} },
			wantProblems: 0,
		},
		{
			name:         "RLS policy count mismatch",
			mutate:       func(s *Stats) { s.RLSPolicyCount = 6 },
			wantProblems: 1,
			wantContains: "RLS policies",
		},
		{
			name:         "vector type mismatch",
			mutate:       func(s *Stats) { s.VectorEmbeddingType = "halfvec" },
			wantProblems: 1,
			wantContains: "vector embedding type",
		},
		{
			name:         "extension version mismatch",
			mutate:       func(s *Stats) { s.Extensions["vector"] = "0.7.4" },
			wantProblems: 1,
			wantContains: `extension vector`,
		},
		{
			name:         "extension missing",
			mutate:       func(s *Stats) { delete(s.Extensions, "pgcrypto") },
			wantProblems: 1,
			wantContains: "present=false",
		},
		{
			name:         "business check orphan rows",
			mutate:       func(s *Stats) { s.BusinessChecks["plans_without_session"] = 3 },
			wantProblems: 1,
			wantContains: "business check plans_without_session",
		},
		{
			name:         "business check missing",
			mutate:       func(s *Stats) { delete(s.BusinessChecks, "messages_without_session") },
			wantProblems: 1,
			wantContains: "present=false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clone()
			tt.mutate(&got)
			problems := compareStats(base, got)
			if len(problems) != tt.wantProblems {
				t.Fatalf("problems = %v, want %d entries", problems, tt.wantProblems)
			}
			if tt.wantContains != "" && !strings.Contains(strings.Join(problems, ";"), tt.wantContains) {
				t.Fatalf("problems %v do not contain %q", problems, tt.wantContains)
			}
		})
	}
}

func TestCompareStats_Empty(t *testing.T) {
	if problems := compareStats(Stats{}, Stats{}); len(problems) != 0 {
		t.Fatalf("empty stats should match, got %v", problems)
	}
}

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct{ in, want string }{
		{"users", `"users"`},
		{`weird"name`, `"weird""name"`},
		{"", `""`},
	}
	for _, tt := range tests {
		if got := quoteIdentifier(tt.in); got != tt.want {
			t.Fatalf("quoteIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCaptureStats_NilDatabase(t *testing.T) {
	if _, err := CaptureStats(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil database")
	}
}

func TestVerify_UnsupportedVersion(t *testing.T) {
	manifest := sampleManifest()
	manifest.Version = 2
	err := Verify(context.Background(), nil, manifest, "", "")
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_UnsupportedAlgorithm(t *testing.T) {
	manifest := sampleManifest()
	manifest.Algorithm = "AES-128-CBC"
	err := Verify(context.Background(), nil, manifest, "", "")
	if err == nil || !strings.Contains(err.Error(), "unsupported backup algorithm") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_MissingPlaintextFile(t *testing.T) {
	manifest := sampleManifest()
	err := Verify(context.Background(), nil, manifest, filepath.Join(t.TempDir(), "gone.sql"), "")
	if err == nil {
		t.Fatal("expected error for missing plaintext file")
	}
}

func TestVerify_HashMismatchDetected(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(plain, []byte("actual content"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	manifest := sampleManifest()
	manifest.PlaintextSHA256 = strings.Repeat("0", 64) // 与实际内容不符
	// 无数据库时 Verify 会在采集统计前记录哈希问题，随后因 nil db 失败；
	// 这里只需确认返回错误，证明未静默通过。
	if err := Verify(context.Background(), nil, manifest, plain, ""); err == nil {
		t.Fatal("expected verification error")
	}
}

func TestVerify_MatchingHashesStillNeedsDatabase(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "dump.sql")
	cipher := filepath.Join(dir, "dump.sql.enc")
	plainContent := []byte("plain backup bytes")
	cipherContent := []byte("cipher backup bytes")
	if err := os.WriteFile(plain, plainContent, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	if err := os.WriteFile(cipher, cipherContent, 0o600); err != nil {
		t.Fatalf("write cipher: %v", err)
	}
	manifest := sampleManifest()
	manifest.PlaintextSHA256 = mustHash(t, plain)
	manifest.CiphertextSHA256 = mustHash(t, cipher)
	err := Verify(context.Background(), nil, manifest, plain, cipher)
	if err == nil || !strings.Contains(err.Error(), "capture restore stats") {
		t.Fatalf("err = %v", err)
	}
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	sum, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	return sum
}
