//go:build integration

package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// speakerFactTask 记录群聊发言人事实提取请求。
type speakerFactTask struct {
	OwnerUserID   uuid.UUID
	SessionID     uuid.UUID
	FactOwnerID   uuid.UUID
	SpeakerOpenID string
}

// capturingFactTrigger 是 FactTrigger 的测试替身，只记录群聊发言人任务。
type capturingFactTrigger struct {
	tasks []speakerFactTask
}

func (t *capturingFactTrigger) MaybeEnqueue(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (t *capturingFactTrigger) MaybeEnqueueSpeaker(_ context.Context, ownerUserID, sessionID, factOwnerID uuid.UUID, speakerOpenID string) error {
	t.tasks = append(t.tasks, speakerFactTask{
		OwnerUserID: ownerUserID, SessionID: sessionID, FactOwnerID: factOwnerID, SpeakerOpenID: speakerOpenID,
	})
	return nil
}

// TestFeishuGroupMultiSpeakerEndToEnd 验证群聊多发言人从事件回执、身份映射、共享绑定、
// 独立投影、Run 创建、外发记录到发言人事实提取的完整异步链路。
func TestFeishuGroupMultiSpeakerEndToEnd(t *testing.T) {
	adminDB, appDB, authDB := feishuIntegrationDBs(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tenant := "tenant_" + suffix
	appID := "cli_group_" + suffix
	chatID := "oc_group_" + suffix
	threadID := "omt_thread_" + suffix
	botOpenID := "ou_bot_" + suffix

	speakerA := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_group_a_"+suffix)
	speakerB := createFeishuTestUser(t, ctx, adminDB, authDB, tenant, "ou_group_b_"+suffix)
	policyID := createGroupPolicy(t, ctx, adminDB, appDB, speakerA, suffix)

	jobs := NewJobRepository(appDB)
	events := NewFeishuEventRepository(appDB, nil)
	conversations := NewFeishuConversationRepository(appDB)
	sessions := NewSessionRepository(appDB)
	messages := NewFeishuMessageRepository(appDB)
	outbound := NewFeishuOutboundRepository(appDB)
	runs := NewRunRepository(appDB)
	runService, err := biz.NewRunService(runs, sessions,
		biz.WithRunSnapshots(NewRunSnapshotRepository(NewAgentRepository(appDB), NewCatalogRepository(appDB))),
		biz.WithRunEnqueuer(jobs),
	)
	if err != nil {
		t.Fatal(err)
	}
	facts := &capturingFactTrigger{}
	agentID, agentVersion := uuid.New(), 1
	worker, err := biz.NewFeishuEventWorker(biz.FeishuEventWorkerDependencies{
		Jobs: jobs, Events: events, Identities: NewFeishuIdentityRepository(authDB),
		Conversations: conversations, Sessions: sessions, Messages: messages, GroupMessages: messages,
		Runs: runService, Outbound: outbound, SpeakerFacts: facts,
	}, biz.FeishuEventWorkerConfig{
		WorkerID: "group-e2e-" + suffix, Lease: 30 * time.Second,
		DefaultAppID: appID, DefaultAgentID: &agentID, DefaultAgentVersion: &agentVersion,
		DefaultAgentOwnerID: &speakerA, Mode: biz.AgentModeReact,
		BotOpenID: botOpenID, RequireTopic: true,
		SharedChats: []biz.FeishuSharedChat{{AppID: appID, TenantKey: tenant, ChatID: chatID, PolicyID: policyID}},
	})
	if err != nil {
		t.Fatal(err)
	}

	process := func(eventID, messageID, speakerOpenID, text string) biz.FeishuEventRecord {
		t.Helper()
		body := buildGroupEvent(t, eventID, appID, tenant, chatID, threadID, messageID, speakerOpenID, botOpenID, text)
		if _, err := events.RecordEvent(ctx, biz.FeishuEventRecord{
			EventID: eventID, EventType: feishu.EventTypeMessageReceive, TenantKey: tenant, AppID: appID,
			Payload: map[string]any{"default_route": true, "body": string(body)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.EnqueueFeishuEvent(ctx, eventID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		processed, err := worker.ProcessOne(ctx)
		if err != nil || !processed {
			t.Fatalf("process event %s: processed=%v err=%v", eventID, processed, err)
		}
		record, err := events.GetEvent(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != biz.FeishuEventProcessed {
			t.Fatalf("event %s status = %q, last_error=%q", eventID, record.Status, record.LastError)
		}
		return record
	}
	process("event-a-"+suffix, "om_a_"+suffix, "ou_group_a_"+suffix, "部署窗口是周六")
	process("event-b-"+suffix, "om_b_"+suffix, "ou_group_b_"+suffix, "部署窗口是周日")

	binding, found, err := conversations.FindConversationForApp(ctx, appID, tenant, chatID, threadID, "")
	if err != nil || !found {
		t.Fatalf("binding found=%v err=%v", found, err)
	}
	if binding.UserID != speakerA || binding.Status != biz.FeishuBindingActive || binding.SessionID == uuid.Nil {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	anchor, err := sessions.GetSession(ctx, speakerA, binding.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if anchor.MemoryPolicy != biz.SessionMemoryGroup || anchor.Source != biz.SessionSourceFeishu {
		t.Fatalf("anchor session = %+v", anchor)
	}

	var groupMessages []struct {
		SpeakerOpenID       string
		Content             string
		SenderUserID        uuid.UUID
		ProjectionSessionID *uuid.UUID
	}
	if err := adminDB.Raw(`SELECT speaker_open_id, content, sender_user_id, projection_session_id
FROM feishu_group_messages WHERE binding_id = ? ORDER BY occurred_at, external_message_id`, binding.ID).
		Scan(&groupMessages).Error; err != nil {
		t.Fatal(err)
	}
	if len(groupMessages) != 2 {
		t.Fatalf("group messages = %d, want 2", len(groupMessages))
	}
	expectedSpeakers := []string{"ou_group_a_" + suffix, "ou_group_b_" + suffix}
	expectedUsers := []uuid.UUID{speakerA, speakerB}
	messageIndex := make(map[string]int, len(groupMessages))
	for index, message := range groupMessages {
		messageIndex[message.SpeakerOpenID] = index
	}
	for userIndex, speaker := range expectedSpeakers {
		index, exists := messageIndex[speaker]
		if !exists {
			t.Fatalf("missing message for speaker %s: %+v", speaker, groupMessages)
		}
		message := groupMessages[index]
		if message.SenderUserID != expectedUsers[userIndex] ||
			message.ProjectionSessionID == nil || *message.ProjectionSessionID == binding.SessionID {
			t.Fatalf("message %d = %+v", index, message)
		}
		projection, err := sessions.GetSession(ctx, expectedUsers[userIndex], *message.ProjectionSessionID)
		if err != nil || !projection.FeishuGroupProjection || projection.MemoryPolicy != biz.SessionMemoryGroup {
			t.Fatalf("projection %d = %+v, %v", index, projection, err)
		}
	}
	if *groupMessages[messageIndex[expectedSpeakers[0]]].ProjectionSessionID ==
		*groupMessages[messageIndex[expectedSpeakers[1]]].ProjectionSessionID {
		t.Fatal("speakers must have independent projection sessions")
	}

	var runsRows []struct {
		ID        uuid.UUID
		UserID    uuid.UUID
		SessionID uuid.UUID
		Mode      string
		Shared    bool
		PolicyID  string
		AgentID   *uuid.UUID
		AttemptNo int
	}
	if err := adminDB.Raw(`SELECT id, user_id, session_id, mode,
  COALESCE((config_snapshot->>'feishu_shared_group')::boolean, false) AS shared,
  COALESCE(model_policy_snapshot->>'policy_id', '') AS policy_id, agent_id, attempt_no
FROM agent_runs WHERE user_id IN (?, ?) AND idempotency_key LIKE 'feishu.group:%'
ORDER BY created_at, id`, speakerA, speakerB).Scan(&runsRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(runsRows) != 2 {
		t.Fatalf("group runs = %d, want 2", len(runsRows))
	}
	runIndexByUser := make(map[uuid.UUID]int, len(runsRows))
	for index, run := range runsRows {
		runIndexByUser[run.UserID] = index
	}
	for userIndex, userID := range expectedUsers {
		index, exists := runIndexByUser[userID]
		if !exists {
			t.Fatalf("missing run for user %s", userID)
		}
		run := runsRows[index]
		expectedMessage := groupMessages[messageIndex[expectedSpeakers[userIndex]]]
		if run.Mode != string(biz.AgentModeReact) || !run.Shared || run.PolicyID != policyID.String() ||
			run.AgentID != nil || run.SessionID != *expectedMessage.ProjectionSessionID {
			t.Fatalf("run %d = %+v", index, run)
		}
	}
	runA := runsRows[runIndexByUser[speakerA]]
	runB := runsRows[runIndexByUser[speakerB]]

	var outboundRows []struct {
		UserID           uuid.UUID
		SessionID        uuid.UUID
		RunID            *uuid.UUID
		ChatID           string
		ThreadID         string
		ReplyToMessageID string
		Kind             string
		Status           string
		AppID            string
		DefaultRoute     bool
	}
	if err := adminDB.Raw(`SELECT user_id, session_id, run_id, chat_id, thread_id,
  reply_to_message_id, kind, status, app_id, default_route
FROM feishu_outbound_messages WHERE chat_id = ? AND thread_id = ? ORDER BY created_at, id`, chatID, threadID).
		Scan(&outboundRows).Error; err != nil {
		t.Fatal(err)
	}
	if len(outboundRows) != 2 {
		t.Fatalf("outbound rows = %d, want 2", len(outboundRows))
	}
	outboundIndexByUser := make(map[uuid.UUID]int, len(outboundRows))
	for index, row := range outboundRows {
		outboundIndexByUser[row.UserID] = index
	}
	expectedReplyIDs := []string{"om_a_" + suffix, "om_b_" + suffix}
	for userIndex, userID := range expectedUsers {
		index, exists := outboundIndexByUser[userID]
		if !exists {
			t.Fatalf("missing outbound for user %s", userID)
		}
		row := outboundRows[index]
		expectedMessage := groupMessages[messageIndex[expectedSpeakers[userIndex]]]
		expectedRun := runsRows[runIndexByUser[userID]]
		if row.SessionID != *expectedMessage.ProjectionSessionID ||
			row.RunID == nil || *row.RunID != expectedRun.ID || row.ChatID != chatID ||
			row.ThreadID != threadID || row.ReplyToMessageID != expectedReplyIDs[userIndex] ||
			row.Kind != "card" || row.Status != "pending" || row.AppID != appID || !row.DefaultRoute {
			t.Fatalf("outbound %d = %+v", index, row)
		}
	}

	var queuedJobs []struct {
		Lane      string
		Status    string
		DedupeKey string
	}
	if err := adminDB.Raw(`SELECT lane, status, dedupe_key FROM jobs
WHERE job_type = 'agent.run' AND dedupe_key IN (?, ?)
ORDER BY created_at, id`, "run:"+runA.ID.String(), "run:"+runB.ID.String()).
		Scan(&queuedJobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(queuedJobs) != 2 {
		t.Fatalf("queued run jobs = %d, want 2", len(queuedJobs))
	}
	for _, job := range queuedJobs {
		if job.Lane != biz.JobLaneReact || job.Status != "pending" {
			t.Fatalf("queued job = %+v", job)
		}
	}

	// 共享群聊禁用个人长期记忆：即使多名发言人各自创建 Run，也不得触发私人事实提取。
	if len(facts.tasks) != 0 {
		t.Fatalf("shared group triggered personal fact extraction: %+v", facts.tasks)
	}

	// 第二位发言人的投影会带入更早的共享历史；其他发言人的私有投影不可读取。
	historyA, err := sessions.ListFeishuGroupRunHistory(ctx, speakerA, runA.SessionID, runA.ID)
	if err != nil || len(historyA) != 1 || historyA[0].SpeakerOpenID != expectedSpeakers[0] {
		t.Fatalf("speaker A history = %+v, %v", historyA, err)
	}
	historyB, err := sessions.ListFeishuGroupRunHistory(ctx, speakerB, runB.SessionID, runB.ID)
	if err != nil || len(historyB) != 2 ||
		historyB[0].SpeakerOpenID != expectedSpeakers[0] || historyB[1].SpeakerOpenID != expectedSpeakers[1] {
		t.Fatalf("speaker B history = %+v, %v", historyB, err)
	}
	if foreign, err := sessions.ListFeishuGroupRunHistory(ctx, speakerA, runB.SessionID, runB.ID); err != nil || len(foreign) != 0 {
		t.Fatalf("speaker A read speaker B projection: %+v, %v", foreign, err)
	}
}

// buildGroupEvent 构造群聊消息接收事件，包含话题、@ 机器人与发言人 open_id。
func buildGroupEvent(t *testing.T, eventID, appID, tenant, chatID, threadID, messageID, speakerOpenID, botOpenID, text string) []byte {
	t.Helper()
	content, err := json.Marshal(map[string]string{"text": text + " @_user_1"})
	if err != nil {
		t.Fatal(err)
	}
	createTime := fmt.Sprintf("%d", time.Now().UnixMilli())
	payload := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": eventID, "event_type": feishu.EventTypeMessageReceive,
			"create_time": createTime, "app_id": appID, "tenant_key": tenant,
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]string{"open_id": speakerOpenID}, "sender_type": "user",
			},
			"message": map[string]any{
				"message_id": messageID, "root_id": threadID, "thread_id": threadID,
				"chat_id": chatID, "chat_type": feishu.ChatTypeGroup, "message_type": "text",
				"content": string(content), "create_time": createTime,
				"mentions": []map[string]string{{"key": "@_user_1", "open_id": botOpenID, "name": "Bot"}},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// createGroupPolicy 创建共享群聊使用的已发布模型策略及可路由候选。
func createGroupPolicy(t *testing.T, ctx context.Context, adminDB, appDB *gorm.DB, actorID uuid.UUID, suffix string) uuid.UUID {
	t.Helper()
	catalog := NewCatalogRepository(appDB)
	providerID, modelID, policyID := uuid.New(), uuid.New(), uuid.New()
	providerName := "group_provider_" + suffix
	if _, err := catalog.CreateProvider(ctx, actorID, biz.Provider{
		ID: providerID, Name: providerName, ProviderType: "openai_compatible",
		BaseURL: "https://models.example.com", Status: biz.ProviderEnabled,
	}, biz.ProviderCredential{Encrypted: []byte("encrypted"), KeyVersion: "v1"},
		testOutboxEvent(actorID, "test.group.provider")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelName := "group-model-" + suffix
	if _, err := catalog.CreateModel(ctx, actorID, biz.Model{
		ID: modelID, ProviderID: providerID, RemoteModelName: modelName, Kind: biz.ModelKindChat,
		ContextWindowTokens: 1_000_000, MaxOutputTokens: 4096, Status: biz.ModelEnabled,
		Capabilities:       biz.ModelCapabilities{Streaming: true, Tools: true},
		VerificationSource: "integration-test", VerifiedAt: &now,
	}, testOutboxEvent(actorID, "test.group.model")); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.CreatePolicy(ctx, actorID, biz.ModelPolicy{
		ID: policyID, Name: "group-policy-" + suffix, Version: 1,
		MinContextTokens: 1_000_000, Status: biz.PolicyPublished,
		Candidates: []biz.PolicyCandidate{{ModelID: modelID, Priority: 1}},
	}, testOutboxEvent(actorID, "test.group.policy")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = adminDB.Exec("DELETE FROM model_policy_candidates WHERE policy_id = ?", policyID).Error
		_ = adminDB.Exec("DELETE FROM model_policies WHERE id = ?", policyID).Error
		_ = adminDB.Exec("DELETE FROM llm_models WHERE id = ?", modelID).Error
		_ = adminDB.Exec("DELETE FROM llm_providers WHERE id = ?", providerID).Error
	})
	return policyID
}
