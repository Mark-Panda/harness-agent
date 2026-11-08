//go:build integration

package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

func TestFeishuGroupProjectionSeparatesSpeakersAndTrustsOnlySharedHistory(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	tenant := "tenant-" + uuid.NewString()
	owner := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_owner")
	speaker := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_speaker")
	bindings := NewFeishuConversationRepository(appDB)
	messages := NewFeishuMessageRepository(appDB)
	sessions := NewSessionRepository(appDB)
	outbound := NewFeishuOutboundRepository(appDB)
	anchor := uuid.New()
	binding, _, err := bindings.EnsureConversation(ctx, biz.FeishuConversationBinding{
		ID: uuid.New(), UserID: owner, Provider: "feishu", AppID: "cli_test", TenantKey: tenant,
		ChatID: "oc_test", ThreadID: "msg-a", RootMessageID: "msg-a", SessionID: anchor, Status: biz.FeishuBindingActive,
	}, biz.Session{
		ID: anchor, UserID: owner, Title: "shared", Source: biz.SessionSourceFeishu,
		MemoryPolicy: biz.SessionMemoryGroup, Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(owner, "test.group.binding"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := biz.Message{UserID: speaker, Role: biz.MessageUser, ExternalMessageID: "msg-a",
		SpeakerOpenID: "ou_speaker", Content: "shared question", CreatedAt: now}
	projected, err := messages.ProjectGroupMessage(ctx, binding, first)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := messages.ProjectGroupMessage(ctx, binding, first)
	if err != nil || repeated.ID != projected.ID || repeated.SessionID != projected.SessionID {
		t.Fatalf("replayed projection = %+v, %v", repeated, err)
	}
	forged := first
	forged.UserID, forged.SpeakerOpenID = owner, "ou_owner"
	if _, err := messages.ProjectGroupMessage(ctx, binding, forged); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("event identity collision = %v, want forbidden", err)
	}
	if _, err := sessions.AppendMessage(ctx, biz.Message{
		ID: uuid.New(), SessionID: projected.SessionID, UserID: speaker, Role: biz.MessageUser, Content: "private Web injection",
	}, testOutboxEvent(speaker, "test.group.web")); err != nil {
		t.Fatal(err)
	}
	session, err := sessions.GetSession(ctx, speaker, projected.SessionID)
	if err != nil || !session.FeishuGroupProjection {
		t.Fatalf("projection marker = %+v, %v", session, err)
	}
	principal, _ := biz.NewPrincipal(speaker, biz.RoleUser)
	sessionService, err := biz.NewSessionService(sessions, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionService.SetMemoryPolicy(ctx, principal, projected.SessionID, biz.SessionMemoryPrivate); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("enable private projection memory = %v", err)
	}
	if _, err := sessionService.AppendMessage(ctx, principal, biz.Message{SessionID: projected.SessionID, Content: "Web input"}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("append Web message to projection = %v", err)
	}
	runService, err := biz.NewRunService(NewRunRepository(appDB), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runService.Create(ctx, principal, biz.CreateRunInput{SessionID: projected.SessionID, Mode: biz.AgentModeReact}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("ordinary run on projection = %v", err)
	}
	runID := createFeishuTestRun(t, adminDB, speaker, projected.SessionID)
	firstReply := biz.FeishuOutbound{
		ID: uuid.New(), UserID: speaker, SessionID: projected.SessionID, RunID: runID, AppID: binding.AppID,
		ChatID: binding.ChatID, ThreadID: binding.ThreadID, ReplyToMessageID: "msg-a", Kind: "card",
		DedupeKey: "test.group.reply:" + runID.String(),
	}
	wrongApp := firstReply
	wrongApp.AppID = "cli_wrong"
	if n, err := bindings.PromoteGroupConversationThread(ctx, wrongApp, "thread"); err != nil || n != 0 {
		t.Fatalf("promote cross-app binding = %d, %v", n, err)
	}
	if n, err := bindings.PromoteGroupConversationThread(ctx, firstReply, "thread"); err != nil || n != 1 {
		t.Fatalf("promote canonical binding = %d, %v", n, err)
	}
	promoted, found, err := bindings.FindConversationForApp(ctx, binding.AppID, tenant, binding.ChatID, "thread", "")
	if err != nil || !found || promoted.ID != binding.ID {
		t.Fatalf("canonical thread lookup = %+v, %t, %v", promoted, found, err)
	}
	if err := outbound.CreateOutbound(ctx, firstReply); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := outbound.ClaimOutbound(ctx, "group-test", time.Minute, now.Add(time.Second))
	if err != nil || !found {
		t.Fatalf("claim group reply = %t, %v", found, err)
	}
	if err := outbound.CompleteGroupReply(ctx, claimed, "group-test", "visible answer", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := messages.ProjectGroupMessage(ctx, promoted, biz.Message{
		UserID: owner, Role: biz.MessageUser, ExternalMessageID: "msg-b",
		SpeakerOpenID: "ou_owner", Content: "follow-up", CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || second.SessionID == projected.SessionID || second.UserID != owner {
		t.Fatalf("second speaker projection = %+v, %v", second, err)
	}
	history, err := sessions.ListFeishuGroupRunHistory(ctx, owner, second.SessionID, uuid.New())
	if err != nil || len(history) != 3 {
		t.Fatalf("shared history = %+v, %v", history, err)
	}
	if history[0].Content != first.Content || history[0].SpeakerOpenID != first.SpeakerOpenID ||
		history[1].Role != biz.MessageAssistant || history[1].Content != "visible answer" ||
		history[2].ID != second.ID || history[2].SpeakerOpenID != "ou_owner" {
		t.Fatalf("unexpected group context: %+v", history)
	}
	foreignHistory, err := sessions.ListFeishuGroupRunHistory(ctx, speaker, second.SessionID, uuid.New())
	if err != nil || len(foreignHistory) != 0 {
		t.Fatalf("speaker accessed another speaker's projection: %+v, %v", foreignHistory, err)
	}
	if err := NewDeletionExecutor(appDB, nil).DeleteAccount(ctx, speaker); err != nil {
		t.Fatal(err)
	}
	history, err = sessions.ListFeishuGroupRunHistory(ctx, owner, second.SessionID, uuid.New())
	if err != nil || len(history) != 1 || history[0].ID != second.ID {
		t.Fatalf("deleted speaker text remains in another projection: %+v, %v", history, err)
	}
	if _, err := messages.ProjectGroupMessage(ctx, promoted, first); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("delayed event recreated deleted speaker projection: %v", err)
	}
}
