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
	ownerMessages := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 1)
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

	sourceMessageID := ownerMessages[0].ID
	second := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &ownerSession, Key: "preference.language",
		Value: json.RawMessage(`"英文"`), Confidence: 0.95, SourceKind: biz.FactSourceCorrection,
		Corrected: true, SourceMessageID: &sourceMessageID, ValidFrom: time.Now().UTC(), Version: 2, CreatedAt: time.Now().UTC(),
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
	assistant, err := sessions.AppendMessage(ctx, biz.Message{
		ID: uuid.New(), UserID: owner.ID, SessionID: sessionID, Role: biz.MessageAssistant, Content: "伪造纠正",
	}, testOutboxEvent(owner.ID, "message.created"))
	if err != nil {
		t.Fatal(err)
	}
	assistantSource := biz.FactTask{
		UserID: owner.ID, SessionID: sessionID, MessageIDs: []uuid.UUID{assistant.ID},
		Fingerprint: biz.SummarySourceFingerprint([]biz.Message{assistant}),
	}
	assistantID := assistant.ID
	forged := biz.MemoryFact{
		ID: uuid.New(), UserID: owner.ID, SessionID: &sessionID, SourceMessageID: &assistantID,
		Key: "preference.assistant", Value: json.RawMessage(`"forged"`), Confidence: 0.99,
		SourceKind: biz.FactSourceCorrection, Corrected: true, Version: 1,
		ValidFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, forged, 0, assistantSource, fence); !errors.Is(err, biz.ErrFactSourceInvalid) || applied {
		t.Fatalf("assistant correction accepted: applied=%v err=%v", applied, err)
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

// TestFactRepositoryRejectsForgedCorrectionSources 验证直接写库也不能伪造用户纠正。
func TestFactRepositoryRejectsForgedCorrectionSources(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	owner, err := users.CreateUser(ctx, biz.User{Username: "fact_correction_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.CreateUser(ctx, biz.User{Username: "fact_correction_other_" + suffix, PasswordHash: "hash", Role: biz.RoleUser, Status: biz.UserStatusActive, CredentialVersion: 1}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", owner.ID, other.ID).Error })
	sessions := NewSessionRepository(appDB)
	ownerSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "fact-correction-owner")
	ownerOtherSession := createMemoryIntegrationSession(t, ctx, sessions, owner.ID, "fact-correction-owner-other")
	otherSession := createMemoryIntegrationSession(t, ctx, sessions, other.ID, "fact-correction-other")
	ownerMessage := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 1)[0]
	crossSessionMessage := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerOtherSession, 1)[0]
	otherMessage := appendMemoryIntegrationMessages(t, ctx, sessions, other.ID, otherSession, 1)[0]
	assistantMessage, err := sessions.AppendMessage(ctx, biz.Message{ID: uuid.New(), UserID: owner.ID, SessionID: ownerSession, Role: biz.MessageAssistant, Content: "助手归纳"}, testOutboxEvent(owner.ID, "message.created"))
	if err != nil {
		t.Fatal(err)
	}
	deletedMessage := appendMemoryIntegrationMessages(t, ctx, sessions, owner.ID, ownerSession, 1)[0]
	if err := adminDB.Exec("UPDATE messages SET deleted_at = ? WHERE id = ?", time.Now().UTC(), deletedMessage.ID).Error; err != nil {
		t.Fatal(err)
	}
	messageID := func(message biz.Message) *uuid.UUID { id := message.ID; return &id }
	missingID := uuid.New()
	for _, test := range []struct {
		name     string
		sourceID *uuid.UUID
		mutate   func(*biz.MemoryFact)
	}{
		{name: "missing-source"},
		{name: "unknown-source", sourceID: &missingID},
		{name: "assistant-source", sourceID: messageID(assistantMessage)},
		{name: "cross-user-source", sourceID: messageID(otherMessage)},
		{name: "cross-session-source", sourceID: messageID(crossSessionMessage)},
		{name: "deleted-source", sourceID: messageID(deletedMessage)},
		{name: "kind-without-flag", sourceID: messageID(ownerMessage), mutate: func(fact *biz.MemoryFact) { fact.Corrected = false }},
		{name: "flag-without-kind", sourceID: messageID(ownerMessage), mutate: func(fact *biz.MemoryFact) { fact.SourceKind = biz.FactSourceUser }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fact := biz.MemoryFact{
				ID: uuid.New(), UserID: owner.ID, SessionID: &ownerSession, Key: "preference.forged." + test.name,
				Value: json.RawMessage(`"伪造纠正"`), Confidence: 0.99, SourceKind: biz.FactSourceCorrection,
				Corrected: true, SourceMessageID: test.sourceID, Version: 1, ValidFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
			}
			if test.mutate != nil {
				test.mutate(&fact)
			}
			if _, applied, err := NewFactRepository(appDB).SaveFact(ctx, fact, 0); !errors.Is(err, biz.ErrFactSourceInvalid) || applied {
				t.Fatalf("forged correction accepted: applied=%v err=%v", applied, err)
			}
		})
	}
}

// TestFactRepositoryValidatesGroupCorrectionOwnership 验证群聊纠正只能绑定授权发言人的真实用户消息。
func TestFactRepositoryValidatesGroupCorrectionOwnership(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	tenant := "fact-tenant-" + uuid.NewString()
	owner := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_owner")
	speaker := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_speaker")
	other := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_other")
	sessionID := uuid.New()
	binding, _, err := NewFeishuConversationRepository(appDB).EnsureConversation(ctx, biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: owner, Provider: "feishu", AppID: "cli_fact", TenantKey: tenant,
		ChatID: "oc_fact", ThreadID: "om_fact", RootMessageID: "om_fact", SessionID: sessionID, Status: biz.FeishuBindingActive,
	}, biz.Session{
		ID: sessionID, UserID: owner, Title: "fact group", Source: biz.SessionSourceFeishu,
		MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(owner, "test.fact.group"))
	if err != nil || binding.SessionID != sessionID {
		t.Fatalf("create group binding = %+v, %v", binding, err)
	}
	message, err := NewSessionRepository(appDB).AppendMessage(ctx, biz.Message{
		ID: uuid.New(), UserID: owner, SessionID: sessionID, Role: biz.MessageUser,
		SpeakerOpenID: "ou_speaker", Content: "我纠正语言偏好",
	}, testOutboxEvent(owner, "test.fact.group.message"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	fence := biz.JobWriteFence{JobID: jobID, Owner: "fact-group-worker", Token: 1}
	if err := adminDB.Exec(`INSERT INTO jobs (id, user_id, job_type, status, attempts, lease_owner, lease_expires_at, fencing_token)
VALUES (?, ?, ?, 'running', 1, ?, ?, ?)`, jobID, owner, biz.FactExtractJobType, fence.Owner, time.Now().UTC().Add(time.Hour), fence.Token).Error; err != nil {
		t.Fatal(err)
	}
	source := biz.FactTask{
		UserID: owner, FactOwnerID: speaker, SpeakerOpenID: "ou_speaker", SessionID: sessionID,
		MessageIDs: []uuid.UUID{message.ID}, Fingerprint: biz.SummarySourceFingerprint([]biz.Message{message}),
	}
	messageID := message.ID
	fact := biz.MemoryFact{
		ID: uuid.New(), UserID: speaker, SessionID: &sessionID, SourceMessageID: &messageID,
		Key: "preference.group.language", Value: json.RawMessage(`"英文"`), Confidence: 0.99,
		SourceKind: biz.FactSourceCorrection, Corrected: true, Version: 1,
		ValidFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, fact, 0, source, fence); err != nil || !applied {
		t.Fatalf("valid group correction: applied=%v err=%v", applied, err)
	}
	for _, test := range []struct {
		name   string
		owner  uuid.UUID
		openID string
	}{
		{name: "speaker-mismatch", owner: speaker, openID: "ou_other"},
		{name: "recipient-mismatch", owner: other, openID: "ou_speaker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			forgedSource := source
			forgedSource.FactOwnerID = test.owner
			forgedSource.SpeakerOpenID = test.openID
			forged := fact
			forged.ID = uuid.New()
			forged.UserID = test.owner
			forged.Key = "preference.group." + test.name
			if _, applied, err := NewFactRepository(appDB).SaveExtractedFact(ctx, forged, 0, forgedSource, fence); !errors.Is(err, biz.ErrFactSourceInvalid) || applied {
				t.Fatalf("unauthorized group correction accepted: applied=%v err=%v", applied, err)
			}
			if _, found, err := NewFactRepository(appDB).GetActiveFact(ctx, test.owner, forged.Key); err != nil || found {
				t.Fatalf("unauthorized group fact persisted: found=%v err=%v", found, err)
			}
		})
	}
}
