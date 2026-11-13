//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// TestFactRepositoryLifecycle 验证版本递增、旧版本关闭、过期写入拒绝、删除与跨用户隔离。
func TestFactRepositoryLifecycle(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{
		Username: "fact_owner_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{
		Username: "fact_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error }()

	sessions := NewSessionRepository(appDB)
	ownerSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "fact-"+suffix)
	facts := NewFactRepository(appDB)

	first := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &ownerSession, Key: "preference.language",
		Value: json.RawMessage(`"中文"`), Confidence: 0.8, SourceKind: biz.FactSourceUser,
		ValidFrom: time.Now().UTC(), Version: 1, CreatedAt: time.Now().UTC(),
	}
	_, applied, err := facts.SaveFact(ctx, first, 0)
	if err != nil || !applied {
		t.Fatalf("save first fact: applied=%v err=%v", applied, err)
	}
	active, found, err := facts.GetActiveFact(ctx, owner.ID, "preference.language")
	if err != nil || !found || active.Version != 1 || string(active.Value) != `"中文"` {
		t.Fatalf("active fact = %#v found=%v err=%v", active, found, err)
	}

	// 旧期望版本不得覆盖新事实。
	stale := first
	stale.ID = uuid.New()
	if _, applied, err := facts.SaveFact(ctx, stale, 0); err != nil || applied {
		t.Fatalf("stale save: applied=%v err=%v", applied, err)
	}

	second := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &ownerSession, Key: "preference.language",
		Value: json.RawMessage(`"英文"`), Confidence: 0.95, SourceKind: biz.FactSourceCorrection,
		Corrected: true, ValidFrom: time.Now().UTC(), Version: 2, CreatedAt: time.Now().UTC(),
	}
	if _, applied, err := facts.SaveFact(ctx, second, 1); err != nil || !applied {
		t.Fatalf("save second fact: applied=%v err=%v", applied, err)
	}
	active, found, err = facts.GetActiveFact(ctx, owner.ID, "preference.language")
	if err != nil || !found || active.Version != 2 || !active.Corrected {
		t.Fatalf("second active = %#v found=%v err=%v", active, found, err)
	}
	versions, err := facts.ListFactVersions(ctx, owner.ID, "preference.language", 10)
	if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].ValidTo == nil {
		t.Fatalf("versions = %#v err=%v", versions, err)
	}

	// 删除只关闭 active 版本，历史仍可审计。
	if err := facts.DeleteFact(ctx, owner.ID, "preference.language", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := facts.GetActiveFact(ctx, owner.ID, "preference.language"); err != nil || found {
		t.Fatalf("deleted active found=%v err=%v", found, err)
	}
	versions, err = facts.ListFactVersions(ctx, owner.ID, "preference.language", 10)
	if err != nil || len(versions) != 2 || versions[0].ValidTo == nil {
		t.Fatalf("history after delete = %#v err=%v", versions, err)
	}

	// 跨用户读取必须未命中。
	if _, found, err := facts.GetActiveFact(ctx, other.ID, "preference.language"); err != nil || found {
		t.Fatalf("cross user found=%v err=%v", found, err)
	}
}

// TestFactRepositoryRejectsInvalidValue 验证非法或超长 JSON 值被拒绝写入。
func TestFactRepositoryRejectsInvalidValue(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	user, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "fact_invalid_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	facts := NewFactRepository(appDB)
	invalid := biz.MemoryFact{
		ID: uuid.New(), UserID: user.ID, Key: "preference.bad", Value: json.RawMessage(`not-json`),
		Confidence: 0.9, Version: 1,
	}
	if _, applied, err := facts.SaveFact(ctx, invalid, 0); err == nil || applied {
		t.Fatalf("invalid value accepted: applied=%v err=%v", applied, err)
	}
}

// TestFactRepositorySaveExtractedFactChecksJobFence 验证失租与过期任务不能提交事实，有效租约可以写入。
func TestFactRepositorySaveExtractedFactChecksJobFence(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	owner, err := NewUserRepository(appDB, authDB).CreateUser(ctx, biz.User{
		Username: "fact_fence_" + uuid.NewString(), PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", owner.ID).Error })
	sessions := NewSessionRepository(appDB)
	sessionID := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "fact-fence")
	messages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, sessionID, 1)
	source := biz.FactTask{
		UserID: owner.ID, SessionID: sessionID, MessageIDs: []uuid.UUID{messages[0].ID},
		Fingerprint: biz.SummarySourceFingerprint(messages),
	}
	jobID := uuid.New()
	fence := biz.JobWriteFence{JobID: jobID, Owner: "fact-worker", Token: 2}
	if err := adminDB.Exec(`INSERT INTO jobs (id, user_id, job_type, status, attempts, lease_owner, lease_expires_at, fencing_token)
VALUES (?, ?, ?, 'running', 1, ?, ?, ?)`, jobID, owner.ID, biz.FactExtractJobType, fence.Owner, time.Now().UTC().Add(time.Hour), fence.Token).Error; err != nil {
		t.Fatal(err)
	}
	sourceMessageID := messages[0].ID
	fact := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionID, SourceMessageID: &sourceMessageID,
		Key: "preference.fence", Value: json.RawMessage(`"first"`), Confidence: 0.9,
		SourceKind: biz.FactSourceUser, Version: 1, ValidFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	stale := fence
	stale.Token--
	if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, fact, 0, source, stale); !errors.Is(err, biz.ErrResourceConflict) || applied {
		t.Fatalf("stale fence write: applied=%v err=%v", applied, err)
	}
	if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, fact, 0, source, fence); err != nil || !applied {
		t.Fatalf("valid fence write: applied=%v err=%v", applied, err)
	}
	if err := adminDB.Exec("UPDATE jobs SET lease_expires_at = ? WHERE id = ?", time.Now().UTC().Add(-time.Second), jobID).Error; err != nil {
		t.Fatal(err)
	}
	second := fact
	second.ID = uuid.New()
	second.Version = 2
	second.Value = json.RawMessage(`"expired"`)
	if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, second, 1, source, fence); !errors.Is(err, biz.ErrResourceConflict) || applied {
		t.Fatalf("expired fence write: applied=%v err=%v", applied, err)
	}
	versions, err := NewFactRepository(appDB).ListFactVersions(ctx, owner.ID, fact.Key, 10)
	if err != nil || len(versions) != 1 || string(versions[0].Value) != `"first"` {
		t.Fatalf("facts after rejected writes = %#v, %v", versions, err)
	}
}
