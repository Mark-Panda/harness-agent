package biz_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
)

// feishuEventStoreStub 记录事件写入与状态更新，用于验证幂等入库。
type feishuEventStoreStub struct {
	biz.FeishuEventStore
	event        biz.FeishuEventRecord
	recorded     int
	recordErr    error
	getErr       error
	statuses     map[string]string
	statusReason string
	boundUsers   map[string]uuid.UUID
}

func (s *feishuEventStoreStub) RecordEvent(_ context.Context, event biz.FeishuEventRecord) (bool, error) {
	if s.recordErr != nil {
		return false, s.recordErr
	}
	s.recorded++
	return true, nil
}

func (s *feishuEventStoreStub) GetEvent(_ context.Context, _ string) (biz.FeishuEventRecord, error) {
	return s.event, s.getErr
}

func (s *feishuEventStoreStub) MarkEventStatus(_ context.Context, eventID, status, reason string, _ time.Time) error {
	if s.statuses == nil {
		s.statuses = map[string]string{}
	}
	s.statuses[eventID] = status
	s.statusReason = reason
	return nil
}

// BindUser 记录事件归属用户。
func (s *feishuEventStoreStub) BindUser(_ context.Context, eventID string, userID uuid.UUID) error {
	if s.boundUsers == nil {
		s.boundUsers = map[string]uuid.UUID{}
	}
	s.boundUsers[eventID] = userID
	return nil
}

// feishuEnqueuerStub 记录入队事件 ID。
type feishuEnqueuerStub struct {
	biz.FeishuEventEnqueuer
	enqueued []string
	err      error
}

func (s *feishuEnqueuerStub) EnqueueFeishuEvent(_ context.Context, eventID string, _ time.Time) error {
	if s.err != nil {
		return s.err
	}
	s.enqueued = append(s.enqueued, eventID)
	return nil
}

// feishuJobsStub 模拟任务队列，按需返回单个任务。
type feishuJobsStub struct {
	biz.JobQueue
	job       biz.Job
	claimed   bool
	completed int
	failed    int
}

func (s *feishuJobsStub) Claim(_ context.Context, _ string, _ string, _ time.Duration, _ time.Time) (biz.Job, bool, error) {
	return s.job, s.claimed, nil
}

func (s *feishuJobsStub) Complete(_ context.Context, _ uuid.UUID, _ string, _ int64) error {
	s.completed++
	return nil
}

func (s *feishuJobsStub) Fail(_ context.Context, _ uuid.UUID, _ string, _ int64, _ time.Time, _ string, _ time.Time) error {
	s.failed++
	return nil
}

// feishuIdentityStub 返回固定用户，模拟 open_id 精确匹配。
type feishuIdentityStub struct {
	biz.FeishuIdentityStore
	userID uuid.UUID
	err    error
}

func (s *feishuIdentityStub) Resolve(_ context.Context, _, _ string) (biz.FeishuIdentity, error) {
	return biz.FeishuIdentity{UserID: s.userID}, s.err
}

// feishuConversationStub 模拟话题绑定查找、创建与解绑。
type feishuConversationStub struct {
	biz.FeishuConversationRepository
	found   bool
	binding biz.FeishuConversationBinding
	session biz.Session
	err     error
	// released 解绑影响的绑定数。
	released int
	// releaseErr 解绑失败错误。
	releaseErr error
	// releaseUserID 解绑调用时的用户 ID，用于断言按用户隔离。
	releaseUserID uuid.UUID
	// releaseSessionID 解绑调用时的会话 ID。
	releaseSessionID uuid.UUID
	// releaseStatus 目标绑定状态。
	releaseStatus string
	// releaseEvent 解绑审计事件。
	releaseEvent biz.OutboxEvent
	// releaseCalls 解绑调用次数。
	releaseCalls int
	// restored 恢复影响的绑定数。
	restored int
	// restoreErr 恢复失败错误。
	restoreErr error
	// restoreUserID 恢复调用时的用户 ID。
	restoreUserID uuid.UUID
	// restoreSessionID 恢复调用时的会话 ID。
	restoreSessionID uuid.UUID
	// restoreEvent 恢复审计事件。
	restoreEvent biz.OutboxEvent
	// restoreCalls 恢复调用次数。
	restoreCalls int
	// promoted 话题升级影响的绑定数。
	promoted int
	// promoteErr 话题升级失败错误。
	promoteErr error
	// promoteUserID 话题升级调用时的用户 ID。
	promoteUserID uuid.UUID
	// promoteSessionID 话题升级调用时的会话 ID。
	promoteSessionID uuid.UUID
	// promoteLegacyThreadID 过渡别名话题键。
	promoteLegacyThreadID string
	// promoteThreadID 飞书真实话题 ID。
	promoteThreadID string
	// promoteCalls 话题升级调用次数。
	promoteCalls int
}

func (s *feishuConversationStub) FindConversation(_ context.Context, _, _, _, _ string) (biz.FeishuConversationBinding, bool, error) {
	return s.binding, s.found, s.err
}

// RestoreConversation 记录恢复入参并返回预设受影响绑定数。
func (s *feishuConversationStub) RestoreConversation(_ context.Context, userID, sessionID uuid.UUID, event biz.OutboxEvent) (int, error) {
	s.restoreCalls++
	s.restoreUserID = userID
	s.restoreSessionID = sessionID
	s.restoreEvent = event
	if s.restoreErr != nil {
		return 0, s.restoreErr
	}
	return s.restored, nil
}

// PromoteConversationThread 记录话题升级入参，供断言过渡别名被真实话题替换。
func (s *feishuConversationStub) PromoteConversationThread(_ context.Context, userID, sessionID uuid.UUID, legacyThreadID, threadID string) (int, error) {
	s.promoteCalls++
	s.promoteUserID = userID
	s.promoteSessionID = sessionID
	s.promoteLegacyThreadID = legacyThreadID
	s.promoteThreadID = threadID
	if s.promoteErr != nil {
		return 0, s.promoteErr
	}
	return s.promoted, nil
}

func (s *feishuConversationStub) EnsureConversation(_ context.Context, binding biz.FeishuConversationBinding, session biz.Session, _ biz.OutboxEvent) (biz.FeishuConversationBinding, biz.Session, error) {
	if s.err != nil {
		return biz.FeishuConversationBinding{}, biz.Session{}, s.err
	}
	binding.SessionID = session.ID
	s.binding = binding
	s.session = session
	return binding, session, nil
}

// ReleaseConversation 记录解绑入参并返回预设的受影响绑定数。
func (s *feishuConversationStub) ReleaseConversation(_ context.Context, userID, sessionID uuid.UUID, status string, event biz.OutboxEvent) (int, error) {
	s.releaseCalls++
	s.releaseUserID = userID
	s.releaseSessionID = sessionID
	s.releaseStatus = status
	s.releaseEvent = event
	if s.releaseErr != nil {
		return 0, s.releaseErr
	}
	return s.released, nil
}

// feishuSessionsStub 返回固定活跃会话；绑定创建后返回同一会话，模拟真实事务结果。
type feishuSessionsStub struct {
	biz.SessionRepository
	conversation *feishuConversationStub
	session      biz.Session
}

func (s *feishuSessionsStub) GetSession(_ context.Context, _, _ uuid.UUID) (biz.Session, error) {
	if s.conversation != nil {
		return s.conversation.session, nil
	}
	return s.session, nil
}

// feishuFactTriggerStub 记录事实提取触发入参，用于断言发言人事实归属。
type feishuFactTriggerStub struct {
	biz.FactTrigger
	// ownerCalls 会话所有者事实提取调用次数。
	ownerCalls int
	// speakerCalls 发言人事实提取调用次数。
	speakerCalls int
	// ownerUserID 会话所有者 ID。
	ownerUserID uuid.UUID
	// sessionID 会话 ID。
	sessionID uuid.UUID
	// factOwnerID 事实写入用户 ID。
	factOwnerID uuid.UUID
	// speakerOpenID 发言人 open_id。
	speakerOpenID string
	// err 触发失败错误。
	err error
}

// MaybeEnqueue 记录会话所有者级事实提取调用。
func (s *feishuFactTriggerStub) MaybeEnqueue(_ context.Context, _, _ uuid.UUID) error {
	s.ownerCalls++
	return s.err
}

// MaybeEnqueueSpeaker 记录发言人级事实提取入参。
func (s *feishuFactTriggerStub) MaybeEnqueueSpeaker(_ context.Context, ownerUserID, sessionID, factOwnerID uuid.UUID, speakerOpenID string) error {
	s.speakerCalls++
	s.ownerUserID = ownerUserID
	s.sessionID = sessionID
	s.factOwnerID = factOwnerID
	s.speakerOpenID = speakerOpenID
	return s.err
}

// feishuMessagesStub 记录入站消息并控制是否新写入。
type feishuMessagesStub struct {
	biz.FeishuInboundMessageStore
	created   bool
	appendErr error
	message   biz.Message
}

func (s *feishuMessagesStub) AppendInbound(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, bool, error) {
	if s.appendErr != nil {
		return biz.Message{}, false, s.appendErr
	}
	s.message = message
	return message, s.created, nil
}

// feishuRunCreatorStub 记录 Run 创建入参。
type feishuRunCreatorStub struct {
	biz.FeishuRunCreator
	input biz.CreateRunInput
	run   biz.Run
	err   error
	calls int
}

func (s *feishuRunCreatorStub) Create(_ context.Context, _ biz.Principal, input biz.CreateRunInput) (biz.Run, error) {
	s.calls++
	s.input = input
	return s.run, s.err
}

// feishuOutboundStub 模拟外发记录的创建、领取与状态流转。
type feishuOutboundStub struct {
	biz.FeishuOutboundStore
	created           *biz.FeishuOutbound
	createErr         error
	claimed           biz.FeishuOutbound
	hasClaim          bool
	assistant         biz.Message
	lastRunID         *uuid.UUID
	updateIDs         []string
	cardFingerprints  []string
	lastCardUpdatedAt time.Time
	deferred          int
	completed         int
	failed            int
	failReason        string
	lastRetryAfter    time.Time
}

func (s *feishuOutboundStub) CreateOutbound(_ context.Context, outbound biz.FeishuOutbound) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.created = &outbound
	return nil
}

func (s *feishuOutboundStub) ClaimOutbound(_ context.Context, _ string, _ time.Duration, _ time.Time) (biz.FeishuOutbound, bool, error) {
	return s.claimed, s.hasClaim, nil
}

func (s *feishuOutboundStub) UpdateOutboundMessageID(_ context.Context, _ uuid.UUID, messageID string, _ time.Time) error {
	s.updateIDs = append(s.updateIDs, messageID)
	return nil
}

func (s *feishuOutboundStub) MarkOutboundCardUpdated(_ context.Context, _ uuid.UUID, fingerprint string, now time.Time) error {
	s.cardFingerprints = append(s.cardFingerprints, fingerprint)
	s.lastCardUpdatedAt = now
	return nil
}

func (s *feishuOutboundStub) DeferOutbound(_ context.Context, _ uuid.UUID, _ time.Time, _ time.Time) error {
	s.deferred++
	return nil
}

func (s *feishuOutboundStub) CompleteOutbound(_ context.Context, _ uuid.UUID, _ time.Time) error {
	s.completed++
	return nil
}

func (s *feishuOutboundStub) FailOutbound(_ context.Context, _ uuid.UUID, reason string, retryAt, _ time.Time) error {
	s.failed++
	s.failReason = reason
	s.lastRetryAfter = retryAt
	return nil
}

func (s *feishuOutboundStub) LatestAssistantMessage(_ context.Context, _, _ uuid.UUID, runID *uuid.UUID) (biz.Message, error) {
	s.lastRunID = runID
	return s.assistant, nil
}

// feishuRunsStub 返回固定 Run 状态。
type feishuRunsStub struct {
	biz.RunRepository
	run biz.Run
	err error
}

func (s *feishuRunsStub) GetRun(_ context.Context, _, _ uuid.UUID) (biz.Run, error) {
	return s.run, s.err
}

// feishuSenderStub 记录卡片与文本发送，支持注入失败。
type feishuSenderStub struct {
	biz.FeishuReplySender
	cardMessageID string
	cardErr       error
	updateErr     error
	textErr       error
	cardCalls     int
	updateCalls   int
	textCalls     int
	// threadID 话题内回复时飞书返回的真实话题 ID。
	threadID string
	// threadErr 话题内回复失败错误，用于验证降级与失败退避。
	threadErr error
	// threadCalls 话题内卡片回复次数。
	threadCalls int
	// textThreadCalls 话题内文本回复次数。
	textThreadCalls int
}

func (s *feishuSenderStub) ReplyCard(_ context.Context, _ string, _ any) (string, error) {
	s.cardCalls++
	return s.cardMessageID, s.cardErr
}

// ReplyCardInThread 模拟在指定消息下创建或复用话题并回复卡片。
// 默认返回脱敏后的话题 ID；threadErr 优先于 cardErr，便于构造降级场景。
func (s *feishuSenderStub) ReplyCardInThread(_ context.Context, _ string, _ any) (biz.FeishuThreadedMessage, error) {
	s.cardCalls++
	s.threadCalls++
	if s.threadErr != nil {
		return biz.FeishuThreadedMessage{}, s.threadErr
	}
	if s.cardErr != nil {
		return biz.FeishuThreadedMessage{}, s.cardErr
	}
	threadID := s.threadID
	if threadID == "" {
		threadID = "omt_created"
	}
	return biz.FeishuThreadedMessage{MessageID: s.cardMessageID, ThreadID: threadID}, nil
}

func (s *feishuSenderStub) UpdateCard(_ context.Context, _ string, _ any) error {
	s.updateCalls++
	return s.updateErr
}

func (s *feishuSenderStub) ReplyText(_ context.Context, _, _ string) (string, error) {
	s.textCalls++
	return "om_text", s.textErr
}

// ReplyTextInThread 模拟话题内文本回复；默认复用普通文本结果。
func (s *feishuSenderStub) ReplyTextInThread(_ context.Context, _, _ string) (biz.FeishuThreadedMessage, error) {
	s.textCalls++
	s.textThreadCalls++
	if s.textErr != nil {
		return biz.FeishuThreadedMessage{}, s.textErr
	}
	threadID := s.threadID
	if threadID == "" {
		threadID = "omt_created"
	}
	return biz.FeishuThreadedMessage{MessageID: "om_text", ThreadID: threadID}, nil
}

// feishuMessageBody 构造消息事件信封 JSON，供 Worker 测试复用。
func feishuMessageBody(t *testing.T, chatType, chatID, messageID, threadID string, mentions []map[string]string) string {
	t.Helper()
	payload := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-" + messageID, "event_type": feishu.EventTypeMessageReceive, "tenant_key": "tenant",
		},
		"event": map[string]any{
			"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_user"}, "sender_type": "user"},
			"message": map[string]any{
				"message_id": messageID, "chat_id": chatID, "chat_type": chatType,
				"message_type": "text", "content": `{"text":"你好"}`, "thread_id": threadID, "mentions": mentions,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal feishu body: %v", err)
	}
	return string(raw)
}

// TestFeishuIngestHandleSecurityAndIdempotency 覆盖 URL 验证、Token 校验、体积限制与重复事件。
func TestFeishuIngestHandleSecurityAndIdempotency(t *testing.T) {
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{VerificationToken: "verify-token"})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	events := &feishuEventStoreStub{}
	enqueuer := &feishuEnqueuerStub{}
	service, err := biz.NewFeishuIngestService(verifier, events, enqueuer, biz.FeishuIngestConfig{MaxBodyBytes: 4096})
	if err != nil {
		t.Fatalf("new ingest service: %v", err)
	}
	challengeBody := []byte(`{"type":"url_verification","token":"verify-token","challenge":"abc"}`)
	result, err := service.Handle(context.Background(), feishu.Request{Body: challengeBody})
	if err != nil || result.Challenge != "abc" || !result.Accepted {
		t.Fatalf("challenge = %+v err=%v", result, err)
	}
	if _, err := service.Handle(context.Background(), feishu.Request{Body: []byte(`{"type":"url_verification","token":"forged","challenge":"abc"}`)}); !errors.Is(err, feishu.ErrSignatureInvalid) {
		t.Fatalf("forged token err = %v", err)
	}
	eventBody := []byte(`{"schema":"2.0","header":{"event_id":"evt-1","event_type":"im.message.receive_v1","tenant_key":"tenant","token":"verify-token"},"event":{}}`)
	if _, err := service.Handle(context.Background(), feishu.Request{Body: eventBody}); err != nil {
		t.Fatalf("handle event: %v", err)
	}
	// 飞书重推同一事件时必须再次入队，修复上次入库成功但入队失败的任务。
	if _, err := service.Handle(context.Background(), feishu.Request{Body: eventBody}); err != nil {
		t.Fatalf("handle duplicate event: %v", err)
	}
	if events.recorded != 2 || len(enqueuer.enqueued) != 2 || enqueuer.enqueued[0] != "evt-1" {
		t.Fatalf("recorded=%d enqueued=%v", events.recorded, enqueuer.enqueued)
	}
	small, _ := biz.NewFeishuIngestService(verifier, events, enqueuer, biz.FeishuIngestConfig{MaxBodyBytes: 100})
	if _, err := small.Handle(context.Background(), feishu.Request{Body: []byte(strings.Repeat("x", 101))}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("limit body err = %v", err)
	}
	enqueuer.err = errors.New("queue unavailable")
	if _, err := service.Handle(context.Background(), feishu.Request{Body: eventBody}); err == nil {
		t.Fatalf("enqueue failure must surface")
	}
	if events.statuses["evt-1"] != biz.FeishuEventFailed {
		t.Fatalf("event status = %q", events.statuses["evt-1"])
	}
}

// TestFeishuEventWorkerP2PCreatesRun 覆盖单聊身份映射、会话创建、消息入库与 Run 创建。
func TestFeishuEventWorkerP2PCreatesRun(t *testing.T) {
	userID := uuid.New()
	events := &feishuEventStoreStub{event: biz.FeishuEventRecord{
		EventID: "evt-om_1", EventType: feishu.EventTypeMessageReceive, TenantKey: "tenant",
		Payload: map[string]any{"body": feishuMessageBody(t, feishu.ChatTypeP2P, "oc_1", "om_1", "", nil)},
		Status:  biz.FeishuEventReceived,
	}}
	conversations := &feishuConversationStub{}
	messages := &feishuMessagesStub{created: true}
	runID := uuid.New()
	runs := &feishuRunCreatorStub{run: biz.Run{ID: runID, UserID: userID, SessionID: uuid.New(), Status: biz.RunQueued}}
	agentID := uuid.New()
	version := 1
	hot := &feishuHotStoreStub{}
	worker, err := biz.NewFeishuEventWorker(biz.FeishuEventWorkerDependencies{
		Jobs:   &feishuJobsStub{job: biz.Job{ID: uuid.New(), Payload: map[string]any{"event_id": "evt-om_1"}, Attempts: 1, MaxAttempts: 3}, claimed: true},
		Events: events, Identities: &feishuIdentityStub{userID: userID}, Conversations: conversations,
		Sessions: &feishuSessionsStub{conversation: conversations}, Messages: messages, Runs: runs, Outbound: &feishuOutboundStub{},
		HotMessages: hot,
		Clock:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, biz.FeishuEventWorkerConfig{
		WorkerID: "worker-1", DefaultAgentID: &agentID, DefaultAgentVersion: &version,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if claimed, err := worker.ProcessOne(context.Background()); err != nil || !claimed {
		t.Fatalf("process one claimed=%v err=%v", claimed, err)
	}
	if runs.calls != 1 || runs.input.IdempotencyKey != "feishu:om_1" || runs.input.SessionID != conversations.session.ID {
		t.Fatalf("run input = %+v", runs.input)
	}
	if messages.message.UserID != userID || messages.message.ExternalMessageID != "om_1" || messages.message.SpeakerOpenID != "ou_user" {
		t.Fatalf("inbound message = %+v", messages.message)
	}
	if events.statuses["evt-om_1"] != biz.FeishuEventProcessed {
		t.Fatalf("event status = %q", events.statuses["evt-om_1"])
	}
	if conversations.binding.ThreadID != "p2p" || conversations.binding.SessionID == uuid.Nil {
		t.Fatalf("binding = %+v", conversations.binding)
	}
	if hot.invalidated != 1 {
		t.Fatalf("hot invalidated=%d, want 1", hot.invalidated)
	}
}

// TestFeishuEventWorkerGroupPolicies 覆盖群聊 @ 判断、无话题忽略与重复消息幂等。
func TestFeishuEventWorkerGroupPolicies(t *testing.T) {
	userID := uuid.New()
	agentID := uuid.New()
	version := 1
	build := func(body string, created bool) (*biz.FeishuEventWorker, *feishuEventStoreStub, *feishuMessagesStub, *feishuRunCreatorStub) {
		events := &feishuEventStoreStub{event: biz.FeishuEventRecord{
			EventID: "evt-group", EventType: feishu.EventTypeMessageReceive, TenantKey: "tenant",
			Payload: map[string]any{"body": body}, Status: biz.FeishuEventReceived,
		}}
		messages := &feishuMessagesStub{created: created}
		runs := &feishuRunCreatorStub{run: biz.Run{ID: uuid.New(), Status: biz.RunQueued}}
		conversations := &feishuConversationStub{}
		worker, err := biz.NewFeishuEventWorker(biz.FeishuEventWorkerDependencies{
			Jobs:   &feishuJobsStub{job: biz.Job{ID: uuid.New(), Payload: map[string]any{"event_id": "evt-group"}, Attempts: 1, MaxAttempts: 3}, claimed: true},
			Events: events, Identities: &feishuIdentityStub{userID: userID}, Conversations: conversations,
			Sessions: &feishuSessionsStub{conversation: conversations}, Messages: messages, Runs: runs, Outbound: &feishuOutboundStub{},
		}, biz.FeishuEventWorkerConfig{
			WorkerID: "worker-1", DefaultAgentID: &agentID, DefaultAgentVersion: &version,
			BotOpenID: "ou_bot", RequireTopic: true,
		})
		if err != nil {
			t.Fatalf("new worker: %v", err)
		}
		return worker, events, messages, runs
	}
	// 群聊未 @ 机器人时忽略，不产生会话或 Run。
	worker, events, _, runs := build(feishuMessageBody(t, feishu.ChatTypeGroup, "oc_group", "om_1", "omt_1", nil), true)
	if _, err := worker.ProcessOne(context.Background()); err != nil || runs.calls != 0 || events.statuses["evt-group"] != biz.FeishuEventIgnored {
		t.Fatalf("untriggered group calls=%d status=%q err=%v", runs.calls, events.statuses["evt-group"], err)
	}
	// 群聊无话题且禁止回退时必须忽略，禁止建立群级共享会话。
	worker, events, _, runs = build(feishuMessageBody(t, feishu.ChatTypeGroup, "oc_group", "om_2", "", []map[string]string{{"open_id": "ou_bot", "name": "机器人"}}), true)
	if _, err := worker.ProcessOne(context.Background()); err != nil || runs.calls != 0 || events.statuses["evt-group"] != biz.FeishuEventIgnored {
		t.Fatalf("untopiced group calls=%d status=%q err=%v", runs.calls, events.statuses["evt-group"], err)
	}
	// 重复消息已存在时仍继续触发 Run：创建按幂等键去重，避免首次处理中途失败的消息被永久丢弃。
	worker, events, _, runs = build(feishuMessageBody(t, feishu.ChatTypeP2P, "oc_1", "om_3", "", nil), false)
	if _, err := worker.ProcessOne(context.Background()); err != nil || runs.calls != 1 || events.statuses["evt-group"] != biz.FeishuEventProcessed {
		t.Fatalf("duplicate message calls=%d status=%q err=%v", runs.calls, events.statuses["evt-group"], err)
	}
}

// TestFeishuEventWorkerSpeakerFactOwnership 验证群聊话题只提取发言人事实并写入发言人私有记忆。
func TestFeishuEventWorkerSpeakerFactOwnership(t *testing.T) {
	ownerID, speakerID, sessionID := uuid.New(), uuid.New(), uuid.New()
	// 群聊话题：消息归属会话所有者，事实提取身份切换为发言人本人。
	groupRecord := biz.FeishuEventRecord{
		EventID: "evt-speaker-group", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
			chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: "om_speaker_group",
			parentID: "omt_parent", mentions: []map[string]string{{"open_id": "ou_bot", "name": "机器人"}},
		})},
	}
	groupFixture := newFeishuWorkerFixture(t, groupRecord, feishuJob("evt-speaker-group", 1, 3),
		biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot"})
	groupFixture.identities.userID = speakerID
	groupFixture.conversations.found = true
	groupFixture.conversations.binding = biz.FeishuConversationBinding{
		UserID: ownerID, SessionID: sessionID, ThreadID: "omt_parent", Status: biz.FeishuBindingActive,
	}
	groupFixture.conversations.session = biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}
	if _, err := groupFixture.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("group process: %v", err)
	}
	if groupFixture.facts.speakerCalls != 1 {
		t.Fatalf("speaker fact calls = %d", groupFixture.facts.speakerCalls)
	}
	if groupFixture.facts.ownerUserID != ownerID || groupFixture.facts.sessionID != sessionID ||
		groupFixture.facts.factOwnerID != speakerID || groupFixture.facts.speakerOpenID != "ou_user" {
		t.Fatalf("speaker fact input = %+v", groupFixture.facts)
	}
	if groupFixture.runs.calls != 1 {
		t.Fatalf("group runs = %d", groupFixture.runs.calls)
	}
	// 个人事实提取失败不得阻断回复链路。
	groupFixture.facts.err = errors.New("fact queue unavailable")
	groupFixture.runs.calls = 0
	if _, err := groupFixture.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("group process with fact failure: %v", err)
	}
	if groupFixture.runs.calls != 1 || groupFixture.jobs.failed != 0 {
		t.Fatalf("fact failure blocked reply: runs=%d failed=%d", groupFixture.runs.calls, groupFixture.jobs.failed)
	}
	// 单聊不触发发言人事实提取，个人事实仍由会话消息链路负责。
	p2pFixture := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-speaker-p2p", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{chatID: "oc_p2p", messageID: "om_speaker_p2p"})},
	}, feishuJob("evt-speaker-p2p", 1, 3), biz.FeishuEventWorkerConfig{})
	if _, err := p2pFixture.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("p2p process: %v", err)
	}
	if p2pFixture.facts.speakerCalls != 0 {
		t.Fatalf("p2p speaker fact calls = %d", p2pFixture.facts.speakerCalls)
	}
}

// TestFeishuReplyWorkerFinalCardAndFallback 覆盖终态卡片更新、未完成延后与文本降级。
func TestFeishuReplyWorkerFinalCardAndFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	runID := uuid.New()
	outbound := &feishuOutboundStub{
		claimed:  biz.FeishuOutbound{ID: uuid.New(), UserID: uuid.New(), RunID: runID, ChatID: "oc_1", ReplyToMessageID: "om_1", Kind: "card", Status: "sending", Attempts: 1, CreatedAt: now},
		hasClaim: true, assistant: biz.Message{Content: "最终答案"},
	}
	sender := &feishuSenderStub{cardMessageID: "om_card"}
	worker, err := biz.NewFeishuReplyWorker(biz.FeishuReplyWorkerDependencies{
		Outbound: outbound, Runs: &feishuRunsStub{run: biz.Run{ID: runID, Status: biz.RunCompleted, SessionID: uuid.New(), UserID: uuid.New()}}, Sender: sender,
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, biz.FeishuReplyWorkerConfig{WorkerID: "reply-1"})
	if err != nil {
		t.Fatalf("new reply worker: %v", err)
	}
	if claimed, err := worker.ProcessOne(context.Background()); err != nil || !claimed {
		t.Fatalf("process one claimed=%v err=%v", claimed, err)
	}
	// 回复必须读取本 Run 产生的助手消息，避免并发 Run 的答案串台。
	if outbound.lastRunID == nil || *outbound.lastRunID != runID {
		t.Fatalf("reply read run = %v, want %s", outbound.lastRunID, runID)
	}
	if sender.cardCalls != 1 || outbound.completed != 1 || len(outbound.updateIDs) != 1 {
		t.Fatalf("card calls=%d completed=%d updates=%v", sender.cardCalls, outbound.completed, outbound.updateIDs)
	}
	// 卡片发送失败时降级为纯文本，保证用户至少收到结果。
	sender.cardErr = errors.New("card unavailable")
	outbound.completed = 0
	if claimed, err := worker.ProcessOne(context.Background()); err != nil || !claimed {
		t.Fatalf("fallback claimed=%v err=%v", claimed, err)
	}
	if sender.textCalls != 1 || outbound.completed != 1 {
		t.Fatalf("text calls=%d completed=%d", sender.textCalls, outbound.completed)
	}
	// Run 未结束时发送占位卡片并延后检查，不消耗失败预算。
	outbound.deferred, outbound.completed, sender.cardCalls = 0, 0, 0
	runs := &feishuRunsStub{run: biz.Run{Status: biz.RunRunning}}
	worker, err = biz.NewFeishuReplyWorker(biz.FeishuReplyWorkerDependencies{
		Outbound: outbound, Runs: runs, Sender: sender,
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, biz.FeishuReplyWorkerConfig{WorkerID: "reply-1"})
	if err != nil {
		t.Fatalf("new pending worker: %v", err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("pending process: %v", err)
	}
	if sender.cardCalls != 1 || outbound.deferred != 1 || outbound.completed != 0 {
		t.Fatalf("pending card=%d deferred=%d completed=%d", sender.cardCalls, outbound.deferred, outbound.completed)
	}
}

// TestFeishuEventWorkerOutOfOrderAndConcurrentSpeakers 覆盖乱序事件与同一话题内的并发多发言人。
// 关键断言：消息时间取飞书 create_time（旧事件晚到也不改写发生时间），发言人与 Run 幂等键各自独立。
func TestFeishuEventWorkerOutOfOrderAndConcurrentSpeakers(t *testing.T) {
	ownerID, sessionID := uuid.New(), uuid.New()
	build := func(messageID, speaker, createTime string) *feishuWorkerFixture {
		record := biz.FeishuEventRecord{
			EventID: "evt-" + messageID, TenantKey: "tenant",
			Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: messageID,
				threadID: "omt_topic", senderOpenID: &speaker, createTime: createTime,
				mentions: []map[string]string{{"open_id": "ou_bot", "name": "机器人"}},
			})},
			Status: biz.FeishuEventReceived,
		}
		fixture := newFeishuWorkerFixture(t, record, feishuJob("evt-"+messageID, 1, 3),
			biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot", RequireTopic: true})
		fixture.identities.userID = ownerID
		fixture.conversations.found = true
		fixture.conversations.binding = biz.FeishuConversationBinding{
			ID: uuid.New(), UserID: ownerID, SessionID: sessionID, Status: biz.FeishuBindingActive,
		}
		fixture.conversations.session = biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}
		return fixture
	}

	// 新消息先被消费，旧消息后到：两者都必须按飞书发生时间落库，避免上下文顺序被投递顺序污染。
	newer := build("om_newer", "ou_speaker_a", "1700000000500")
	if _, err := newer.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("process newer event: %v", err)
	}
	older := build("om_older", "ou_speaker_b", "1700000000100")
	if _, err := older.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("process older event: %v", err)
	}
	if got := newer.messages.message.CreatedAt; !got.Equal(time.UnixMilli(1_700_000_000_500).UTC()) {
		t.Fatalf("newer message created at = %v", got)
	}
	if got := older.messages.message.CreatedAt; !got.Equal(time.UnixMilli(1_700_000_000_100).UTC()) {
		t.Fatalf("older message created at = %v", got)
	}
	// 同一话题内不同发言人复用同一 Session，但保留各自发言人标识与独立 Run 幂等键。
	if older.messages.message.SessionID != sessionID || older.messages.message.SpeakerOpenID != "ou_speaker_b" {
		t.Fatalf("older message = %+v", older.messages.message)
	}
	if newer.messages.message.SpeakerOpenID != "ou_speaker_a" || newer.messages.message.UserID != ownerID {
		t.Fatalf("newer message = %+v", newer.messages.message)
	}
	if newer.runs.calls != 1 || older.runs.calls != 1 ||
		newer.runs.input.IdempotencyKey != "feishu:om_newer" || older.runs.input.IdempotencyKey != "feishu:om_older" {
		t.Fatalf("runs newer=%+v older=%+v", newer.runs.input, older.runs.input)
	}
	if newer.events.statuses["evt-om_newer"] != biz.FeishuEventProcessed ||
		older.events.statuses["evt-om_older"] != biz.FeishuEventProcessed {
		t.Fatalf("event statuses newer=%q older=%q", newer.events.statuses, older.events.statuses)
	}

	// 缺少 create_time 时必须回退到服务端时钟，仍可正常入库与回复。
	missing := build("om_missing", "ou_speaker_a", "")
	if _, err := missing.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("process missing create_time: %v", err)
	}
	if got := missing.messages.message.CreatedAt; !got.Equal(time.Unix(1_700_000_000, 0).UTC()) {
		t.Fatalf("fallback created at = %v", got)
	}
}

// feishuBodyOptions 描述构造飞书消息事件信封的字段，零值使用单聊文本默认值。
type feishuBodyOptions struct {
	// eventType 事件类型，默认消息接收。
	eventType string
	// senderType 发送者类型，默认 user。
	senderType string
	// senderOpenID 发送者 open_id；为 nil 时默认 ou_user，指向空串时构造缺失场景。
	senderOpenID *string
	// messageType 消息类型，默认 text。
	messageType string
	// content 原始消息内容 JSON。
	content string
	// chatType 会话类型，默认单聊。
	chatType string
	// chatID 会话 ID。
	chatID string
	// messageID 消息 ID。
	messageID string
	// threadID 话题 ID。
	threadID string
	// parentID 父消息 ID。
	parentID string
	// rootID 话题根消息 ID。
	rootID string
	// mentions 被 @ 的用户列表。
	mentions []map[string]string
	// createTime 飞书消息创建时间（Unix 时间戳字符串），用于验证乱序保护。
	createTime string
}

// buildFeishuBody 按选项构造消息事件信封 JSON，供异常分支测试复用。
func buildFeishuBody(t *testing.T, options feishuBodyOptions) string {
	t.Helper()
	if options.eventType == "" {
		options.eventType = feishu.EventTypeMessageReceive
	}
	if options.senderType == "" {
		options.senderType = "user"
	}
	senderOpenID := "ou_user"
	if options.senderOpenID != nil {
		senderOpenID = *options.senderOpenID
	}
	if options.messageType == "" {
		options.messageType = "text"
	}
	if options.content == "" {
		options.content = `{"text":"你好"}`
	}
	if options.chatType == "" {
		options.chatType = feishu.ChatTypeP2P
	}
	messageID := options.messageID
	if messageID == "" {
		messageID = "om_default"
	}
	payload := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-" + messageID, "event_type": options.eventType, "tenant_key": "tenant",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]any{"open_id": senderOpenID}, "sender_type": options.senderType,
			},
			"message": map[string]any{
				"message_id": messageID, "root_id": options.rootID, "parent_id": options.parentID,
				"thread_id": options.threadID, "chat_id": options.chatID,
				"chat_type": options.chatType, "message_type": options.messageType,
				"content": options.content, "mentions": options.mentions,
				"create_time": options.createTime,
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal feishu body: %v", err)
	}
	return string(raw)
}

// feishuJob 构造待消费的飞书事件任务。
func feishuJob(eventID string, attempts, maxAttempts int) biz.Job {
	return biz.Job{
		ID: uuid.New(), Payload: map[string]any{"event_id": eventID},
		Attempts: attempts, MaxAttempts: maxAttempts, FencingToken: int64(attempts),
	}
}

// feishuBotRouterStub 模拟机器人注册与绑定解析。
type feishuBotRouterStub struct {
	bot          biz.FeishuBot
	botFound     bool
	binding      biz.FeishuBotBinding
	bindingFound bool
	err          error
	lookups      int
}

func (s *feishuBotRouterStub) LookupFeishuBot(context.Context, string) (biz.FeishuBot, bool, error) {
	s.lookups++
	return s.bot, s.botFound, s.err
}

func (s *feishuBotRouterStub) FindFeishuBotBinding(context.Context, uuid.UUID) (biz.FeishuBotBinding, bool, error) {
	return s.binding, s.bindingFound, s.err
}

// feishuWorkerFixture 聚合事件 Worker 测试桩，便于注入各异常分支。
type feishuWorkerFixture struct {
	worker        *biz.FeishuEventWorker
	events        *feishuEventStoreStub
	jobs          *feishuJobsStub
	identities    *feishuIdentityStub
	conversations *feishuConversationStub
	sessions      *feishuSessionsStub
	messages      *feishuMessagesStub
	runs          *feishuRunCreatorStub
	outbound      *feishuOutboundStub
	bots          *feishuBotRouterStub
	facts         *feishuFactTriggerStub
	userID        uuid.UUID
}

// newFeishuWorkerFixture 构造默认成功链路的桩并装配 Worker。
func newFeishuWorkerFixture(t *testing.T, record biz.FeishuEventRecord, job biz.Job, config biz.FeishuEventWorkerConfig) *feishuWorkerFixture {
	t.Helper()
	userID := uuid.New()
	agentID := uuid.New()
	version := 1
	fixture := &feishuWorkerFixture{
		userID:        userID,
		events:        &feishuEventStoreStub{event: record},
		jobs:          &feishuJobsStub{job: job, claimed: job.ID != uuid.Nil},
		identities:    &feishuIdentityStub{userID: userID},
		conversations: &feishuConversationStub{},
		sessions:      &feishuSessionsStub{},
		messages:      &feishuMessagesStub{created: true},
		runs:          &feishuRunCreatorStub{run: biz.Run{ID: uuid.New(), Status: biz.RunQueued}},
		outbound:      &feishuOutboundStub{},
		bots:          &feishuBotRouterStub{},
		facts:         &feishuFactTriggerStub{},
	}
	fixture.sessions.conversation = fixture.conversations
	if config.WorkerID == "" {
		config.WorkerID = "worker-1"
	}
	if config.DefaultAgentID == nil {
		config.DefaultAgentID = &agentID
	}
	if config.DefaultAgentVersion == nil {
		config.DefaultAgentVersion = &version
	}
	fixture.buildWithConfig(t, config)
	return fixture
}

// buildWithConfig 用同一组桩依赖重新构造 Worker，便于覆盖配置缺失等分支。
func (f *feishuWorkerFixture) buildWithConfig(t *testing.T, config biz.FeishuEventWorkerConfig) {
	t.Helper()
	worker, err := biz.NewFeishuEventWorker(biz.FeishuEventWorkerDependencies{
		Jobs: f.jobs, Events: f.events, Identities: f.identities, Conversations: f.conversations,
		Sessions: f.sessions, Messages: f.messages, Runs: f.runs, Outbound: f.outbound,
		Bots: f.bots, SpeakerFacts: f.facts,
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, config)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	f.worker = worker
}

// feishuHotStoreStub 记录会话热缓存的清理次数，用于验证入站消息后不会命中旧上下文。
type feishuHotStoreStub struct {
	invalidated int
}

func (s *feishuHotStoreStub) Append(context.Context, uuid.UUID, uuid.UUID, biz.Message) error {
	return nil
}

func (s *feishuHotStoreStub) Invalidate(context.Context, uuid.UUID, uuid.UUID) error {
	s.invalidated++
	return nil
}

func (s *feishuHotStoreStub) Load(context.Context, uuid.UUID, uuid.UUID) ([]biz.Message, bool, error) {
	return nil, false, nil
}

func (s *feishuHotStoreStub) Save(context.Context, uuid.UUID, uuid.UUID, []biz.Message) error {
	return nil
}

// feishuBotCredentialStub 模拟按机器人 ID 读取加密凭据。
type feishuBotCredentialStub struct {
	bot        biz.FeishuBot
	credential biz.ProviderCredential
	err        error
}

func (s *feishuBotCredentialStub) GetFeishuBotCredential(context.Context, uuid.UUID) (biz.FeishuBot, biz.ProviderCredential, error) {
	return s.bot, s.credential, s.err
}

// feishuTestOpener 去掉测试前缀后返回凭据 JSON。
type feishuTestOpener struct{}

func (feishuTestOpener) Open(_ string, ciphertext []byte) ([]byte, error) {
	if !strings.HasPrefix(string(ciphertext), "sealed:") {
		return nil, errors.New("invalid test ciphertext")
	}
	return []byte(strings.TrimPrefix(string(ciphertext), "sealed:")), nil
}

// TestFeishuIngestBotVerification 验证按机器人 ID 使用独立 Verification Token 验签。
func TestFeishuIngestBotVerification(t *testing.T) {
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{VerificationToken: "default-token"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &feishuBotCredentialStub{
		bot: biz.FeishuBot{ID: uuid.New(), Name: "bot", AppID: "cli_bot", Status: biz.FeishuBotEnabled},
		credential: biz.ProviderCredential{
			Encrypted: []byte(`sealed:{"app_secret":"secret","verification_token":"bot-token"}`), KeyVersion: "v1",
		},
	}
	service, err := biz.NewFeishuIngestService(verifier, &feishuEventStoreStub{}, &feishuEnqueuerStub{}, biz.FeishuIngestConfig{MaxBodyBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	service.WithBotVerification(reader, feishuTestOpener{})

	result, err := service.HandleForBot(context.Background(), reader.bot.ID, feishu.Request{
		Body: []byte(`{"type":"url_verification","token":"bot-token","challenge":"challenge-1"}`),
	})
	if err != nil || result.Challenge != "challenge-1" {
		t.Fatalf("bot verification = %#v, %v", result, err)
	}
	// 错误 Token 必须拒绝。
	if _, err := service.HandleForBot(context.Background(), reader.bot.ID, feishu.Request{
		Body: []byte(`{"type":"url_verification","token":"wrong","challenge":"x"}`),
	}); !errors.Is(err, feishu.ErrSignatureInvalid) {
		t.Fatalf("bad token error = %v", err)
	}
	// 停用机器人返回未找到，不暴露具体状态。
	reader.bot.Status = biz.FeishuBotDisabled
	if _, err := service.HandleForBot(context.Background(), reader.bot.ID, feishu.Request{
		Body: []byte(`{"type":"url_verification","token":"bot-token","challenge":"x"}`),
	}); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("disabled bot error = %v", err)
	}
	reader.err = biz.ErrResourceNotFound
	if _, err := service.HandleForBot(context.Background(), reader.bot.ID, feishu.Request{
		Body: []byte(`{"type":"url_verification","token":"bot-token","challenge":"x"}`),
	}); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("unknown bot error = %v", err)
	}
}

// TestFeishuIngestLimitsAndInvalidBody 覆盖请求体上限读取、空体、非法信封与入库失败。
func TestFeishuIngestLimitsAndInvalidBody(t *testing.T) {
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{VerificationToken: "verify-token"})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	service, err := biz.NewFeishuIngestService(verifier, &feishuEventStoreStub{}, &feishuEnqueuerStub{}, biz.FeishuIngestConfig{MaxBodyBytes: 2048})
	if err != nil {
		t.Fatalf("new ingest service: %v", err)
	}
	if service.MaxBodyBytes() != 2048 {
		t.Fatalf("max body bytes = %d", service.MaxBodyBytes())
	}
	if _, err := service.Handle(context.Background(), feishu.Request{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty body err = %v", err)
	}
	if _, err := service.Handle(context.Background(), feishu.Request{Body: []byte("{")}); err == nil {
		t.Fatal("malformed envelope must fail")
	}
	if _, err := service.Handle(context.Background(), feishu.Request{Body: []byte(`{"type":"url_verification","token":"verify-token","challenge":""}`)}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty challenge err = %v", err)
	}
	if _, err := service.Handle(context.Background(), feishu.Request{Body: []byte(`{"schema":"2.0","header":{"token":"verify-token"},"event":{}}`)}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("incomplete header err = %v", err)
	}
	// 事件入库失败时必须返回错误，让飞书重推。
	broken := &feishuEventStoreStub{recordErr: errors.New("event store unavailable")}
	brokenService, err := biz.NewFeishuIngestService(verifier, broken, &feishuEnqueuerStub{}, biz.FeishuIngestConfig{})
	if err != nil {
		t.Fatalf("new broken ingest service: %v", err)
	}
	if _, err := brokenService.Handle(context.Background(), feishu.Request{Body: []byte(`{"schema":"2.0","header":{"event_id":"evt-1","event_type":"im.message.receive_v1","token":"verify-token"},"event":{}}`)}); err == nil {
		t.Fatal("record failure must surface")
	}
}

// TestFeishuEventWorkerRetryAndDeadLetter 覆盖身份解析失败、读取失败与死信分支。
func TestFeishuEventWorkerRetryAndDeadLetter(t *testing.T) {
	body := buildFeishuBody(t, feishuBodyOptions{chatID: "oc_1", messageID: "om_1"})
	record := biz.FeishuEventRecord{
		EventID: "evt-om_1", EventType: feishu.EventTypeMessageReceive, TenantKey: "tenant",
		Payload: map[string]any{"body": body}, Status: biz.FeishuEventReceived,
	}
	// 可重试失败：身份解析错误写入 failed 并按预算退避。
	retry := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
	retry.identities.err = errors.New(strings.Repeat("x", 600))
	if _, err := retry.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("retry process: %v", err)
	}
	if retry.jobs.failed != 1 || retry.events.statuses["evt-om_1"] != biz.FeishuEventFailed {
		t.Fatalf("failed=%d status=%q", retry.jobs.failed, retry.events.statuses["evt-om_1"])
	}
	// 错误文本必须截断，避免把上游长响应写入数据库。
	if limit := len("feishu processing failed: ") + 512; len(retry.events.statusReason) > limit {
		t.Fatalf("reason length = %d", len(retry.events.statusReason))
	}
	// 超出重试预算：进入死信等待人工重放。
	dead := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 3, 3), biz.FeishuEventWorkerConfig{})
	dead.identities.err = errors.New("identity store unavailable")
	if _, err := dead.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("dead process: %v", err)
	}
	if dead.events.statuses["evt-om_1"] != biz.FeishuEventDead {
		t.Fatalf("dead status = %q", dead.events.statuses["evt-om_1"])
	}
	// 回执读取失败同样进入失败退避。
	readErr := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
	readErr.events.getErr = errors.New("event store unavailable")
	if _, err := readErr.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("read error process: %v", err)
	}
	if readErr.jobs.failed != 1 {
		t.Fatalf("read error failed=%d", readErr.jobs.failed)
	}
}

// TestFeishuEventWorkerTerminalAndIgnoredBranches 覆盖终态回执与无需处理的事件。
func TestFeishuEventWorkerTerminalAndIgnoredBranches(t *testing.T) {
	// 已终态事件只完成任务，不重复处理。
	terminal := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-1", Status: biz.FeishuEventProcessed,
	}, feishuJob("evt-1", 1, 3), biz.FeishuEventWorkerConfig{})
	if _, err := terminal.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("terminal process: %v", err)
	}
	if terminal.jobs.completed != 1 || len(terminal.events.statuses) != 0 {
		t.Fatalf("terminal completed=%d statuses=%v", terminal.jobs.completed, terminal.events.statuses)
	}
	// 缺少 event_id 的非法载荷直接完成，避免永久重试。
	invalid := newFeishuWorkerFixture(t, biz.FeishuEventRecord{EventID: "evt-2"},
		biz.Job{ID: uuid.New(), Payload: map[string]any{}}, biz.FeishuEventWorkerConfig{})
	if _, err := invalid.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("invalid payload process: %v", err)
	}
	if invalid.jobs.completed != 1 {
		t.Fatalf("invalid payload completed=%d", invalid.jobs.completed)
	}
	emptyBody := &feishuEventStoreStub{event: biz.FeishuEventRecord{EventID: "evt-empty", Payload: map[string]any{}}}
	emptyFixture := newFeishuWorkerFixture(t, emptyBody.event, feishuJob("evt-empty", 1, 3), biz.FeishuEventWorkerConfig{})
	if _, err := emptyFixture.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("empty body process: %v", err)
	}
	if emptyFixture.events.statuses["evt-empty"] != biz.FeishuEventIgnored {
		t.Fatalf("empty body status = %q", emptyFixture.events.statuses["evt-empty"])
	}
	emptyOpenID := ""
	mentions := []map[string]string{{"open_id": "ou_bot", "name": "机器人"}}
	cases := []struct {
		name   string
		event  biz.FeishuEventRecord
		config biz.FeishuEventWorkerConfig
	}{
		{
			name: "recalled message",
			event: biz.FeishuEventRecord{EventID: "evt-recalled", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				eventType: feishu.EventTypeMessageRecalled, chatID: "oc_1", messageID: "om_recalled",
			})}},
		},
		{
			name: "updated message",
			event: biz.FeishuEventRecord{EventID: "evt-updated", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				eventType: feishu.EventTypeMessageUpdated, chatID: "oc_1", messageID: "om_updated",
			})}},
		},
		{
			name: "unknown event type",
			event: biz.FeishuEventRecord{EventID: "evt-unknown", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				eventType: "im.unknown", chatID: "oc_1", messageID: "om_unknown",
			})}},
		},
		{
			name: "application sender",
			event: biz.FeishuEventRecord{EventID: "evt-app", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				senderType: "app", chatID: "oc_1", messageID: "om_app",
			})}},
		},
		{
			name: "image message",
			event: biz.FeishuEventRecord{EventID: "evt-image", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				messageType: "image", content: `{"image_key":"img"}`, chatID: "oc_1", messageID: "om_image",
			})}},
		},
		{
			name: "missing sender open id",
			event: biz.FeishuEventRecord{EventID: "evt-nosender", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				senderOpenID: &emptyOpenID, chatID: "oc_1", messageID: "om_nosender",
			})}},
		},
		{
			name: "group without mention",
			event: biz.FeishuEventRecord{EventID: "evt-nomention", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: "om_nomention",
			})}},
			config: biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot"},
		},
		{
			name: "group without topic",
			event: biz.FeishuEventRecord{EventID: "evt-notopic", Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: "om_notopic", mentions: mentions,
			})}},
			config: biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot", RequireTopic: true},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFeishuWorkerFixture(t, testCase.event, feishuJob(testCase.event.EventID, 1, 3), testCase.config)
			if _, err := fixture.worker.ProcessOne(context.Background()); err != nil {
				t.Fatalf("process: %v", err)
			}
			if fixture.events.statuses[testCase.event.EventID] != biz.FeishuEventIgnored {
				t.Fatalf("status = %q", fixture.events.statuses[testCase.event.EventID])
			}
		})
	}
}

// TestFeishuEventWorkerBindingAndPersistenceFailures 覆盖绑定、会话状态与持久化失败。
func TestFeishuEventWorkerBindingAndPersistenceFailures(t *testing.T) {
	body := buildFeishuBody(t, feishuBodyOptions{chatID: "oc_1", messageID: "om_1"})
	record := biz.FeishuEventRecord{
		EventID: "evt-om_1", EventType: feishu.EventTypeMessageReceive, TenantKey: "tenant",
		Payload: map[string]any{"body": body}, Status: biz.FeishuEventReceived,
	}
	// 绑定已关闭时忽略消息，不再写入会话。
	closed := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
	closed.conversations.found = true
	closed.conversations.binding = biz.FeishuConversationBinding{
		UserID: closed.userID, SessionID: uuid.New(), Status: biz.FeishuBindingClosed,
	}
	closed.conversations.session = biz.Session{Status: biz.SessionActive}
	if _, err := closed.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("closed binding process: %v", err)
	}
	if closed.events.statuses["evt-om_1"] != biz.FeishuEventIgnored {
		t.Fatalf("closed binding status = %q", closed.events.statuses["evt-om_1"])
	}
	// 会话已暂停时不继续对话。
	paused := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
	paused.conversations.found = true
	paused.conversations.binding = biz.FeishuConversationBinding{
		UserID: paused.userID, SessionID: uuid.New(), Status: biz.FeishuBindingActive,
	}
	paused.conversations.session = biz.Session{Status: "paused"}
	if _, err := paused.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("paused session process: %v", err)
	}
	if paused.events.statuses["evt-om_1"] != biz.FeishuEventIgnored {
		t.Fatalf("paused session status = %q", paused.events.statuses["evt-om_1"])
	}
	// 各类持久化错误都应进入失败退避，等待飞书重推。
	failures := []struct {
		name   string
		inject func(*feishuWorkerFixture)
	}{
		{"lookup binding", func(f *feishuWorkerFixture) { f.conversations.err = errors.New("binding store unavailable") }},
		{"append message", func(f *feishuWorkerFixture) { f.messages.appendErr = errors.New("message store unavailable") }},
		{"create run", func(f *feishuWorkerFixture) { f.runs.err = errors.New("run creation failed") }},
		{"create outbound", func(f *feishuWorkerFixture) { f.outbound.createErr = errors.New("outbound creation failed") }},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			fixture := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
			failure.inject(fixture)
			if _, err := fixture.worker.ProcessOne(context.Background()); err != nil {
				t.Fatalf("process: %v", err)
			}
			if fixture.jobs.failed != 1 || fixture.events.statuses["evt-om_1"] != biz.FeishuEventFailed {
				t.Fatalf("failed=%d status=%q", fixture.jobs.failed, fixture.events.statuses["evt-om_1"])
			}
		})
	}
	// 缺少默认 Agent 时忽略事件，避免在只使用注册机器人的部署中无限重试。
	noAgent := newFeishuWorkerFixture(t, record, feishuJob("evt-om_1", 1, 3), biz.FeishuEventWorkerConfig{})
	noAgent.buildWithConfig(t, biz.FeishuEventWorkerConfig{WorkerID: "worker-1"})
	if _, err := noAgent.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("no agent process: %v", err)
	}
	if noAgent.jobs.failed != 0 || noAgent.runs.calls != 0 || noAgent.events.statuses["evt-om_1"] != biz.FeishuEventIgnored {
		t.Fatalf("no agent failed=%d runs=%d status=%q", noAgent.jobs.failed, noAgent.runs.calls, noAgent.events.statuses["evt-om_1"])
	}
}

// TestFeishuEventWorkerRoutesRegisteredBotToBoundAgent 验证机器人绑定路由、所有者限制与停用/未绑定忽略。
func TestFeishuEventWorkerRoutesRegisteredBotToBoundAgent(t *testing.T) {
	body := buildFeishuBody(t, feishuBodyOptions{chatID: "oc_1", messageID: "om_bound"})
	record := biz.FeishuEventRecord{
		EventID: "evt-bound", EventType: feishu.EventTypeMessageReceive, TenantKey: "tenant", AppID: "app_bot",
		Payload: map[string]any{"body": body}, Status: biz.FeishuEventReceived,
	}
	botID, boundAgentID := uuid.New(), uuid.New()
	boundVersion := 7
	newFixture := func() *feishuWorkerFixture {
		fixture := newFeishuWorkerFixture(t, record, feishuJob("evt-bound", 1, 3), biz.FeishuEventWorkerConfig{})
		fixture.bots.bot = biz.FeishuBot{ID: botID, Name: "绑定机器人", AppID: "app_bot", Status: biz.FeishuBotEnabled}
		fixture.bots.botFound = true
		fixture.bots.binding = biz.FeishuBotBinding{
			BotID: botID, BotName: "绑定机器人", UserID: fixture.userID, AgentID: boundAgentID,
			AgentVersion: boundVersion, AgentStatus: biz.AgentPublished, AgentMode: biz.AgentModePlan,
		}
		fixture.bots.bindingFound = true
		return fixture
	}

	bound := newFixture()
	if _, err := bound.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("bound bot process: %v", err)
	}
	if bound.runs.calls != 1 || bound.runs.input.AgentID == nil || *bound.runs.input.AgentID != boundAgentID ||
		bound.runs.input.AgentVersion == nil || *bound.runs.input.AgentVersion != boundVersion || bound.runs.input.Mode != biz.AgentModePlan {
		t.Fatalf("bound route input = %#v", bound.runs.input)
	}
	if bound.outbound.created == nil || bound.outbound.created.AppID != "app_bot" {
		t.Fatalf("outbound app = %#v", bound.outbound.created)
	}

	// 非助手所有者发消息时忽略，不能消费他人助手额度。
	otherUser := newFixture()
	otherUser.bots.binding.UserID = uuid.New()
	if _, err := otherUser.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("other user process: %v", err)
	}
	if otherUser.runs.calls != 0 || otherUser.events.statuses["evt-bound"] != biz.FeishuEventIgnored {
		t.Fatalf("other user runs=%d status=%q", otherUser.runs.calls, otherUser.events.statuses["evt-bound"])
	}

	// 机器人未绑定助手时忽略。
	unbound := newFixture()
	unbound.bots.bindingFound = false
	if _, err := unbound.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("unbound bot process: %v", err)
	}
	if unbound.runs.calls != 0 || unbound.events.statuses["evt-bound"] != biz.FeishuEventIgnored {
		t.Fatalf("unbound runs=%d status=%q", unbound.runs.calls, unbound.events.statuses["evt-bound"])
	}

	// 机器人停用时不处理事件。
	disabled := newFixture()
	disabled.bots.bot.Status = biz.FeishuBotDisabled
	if _, err := disabled.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("disabled bot process: %v", err)
	}
	if disabled.runs.calls != 0 || disabled.events.statuses["evt-bound"] != biz.FeishuEventIgnored {
		t.Fatalf("disabled runs=%d status=%q", disabled.runs.calls, disabled.events.statuses["evt-bound"])
	}

	// 绑定助手已取消发布时忽略，避免使用不可变快照之外的配置。
	published := newFixture()
	published.bots.binding.AgentStatus = biz.AgentDraft
	if _, err := published.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("draft binding process: %v", err)
	}
	if published.runs.calls != 0 {
		t.Fatalf("draft binding runs=%d", published.runs.calls)
	}
}

// TestFeishuEventWorkerThreadFallbackAndTruncation 覆盖话题回退、根消息别名与长度限制。
func TestFeishuEventWorkerThreadFallbackAndTruncation(t *testing.T) {
	mentions := []map[string]string{{"open_id": "ou_bot", "name": "机器人"}}
	// 允许根消息回退时，用消息 ID 同时作为话题键与过渡别名。
	fallback := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-fallback", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
			chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: "om_root", mentions: mentions,
		})},
	}, feishuJob("evt-fallback", 1, 3), biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot", AllowRootReplyFallback: true})
	if _, err := fallback.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("fallback process: %v", err)
	}
	if fallback.conversations.binding.ThreadID != "om_root" || fallback.conversations.binding.RootMessageID != "om_root" {
		t.Fatalf("fallback binding = %+v", fallback.conversations.binding)
	}
	// parent_id 作为话题键时必须同时记录 root_message_id 便于后续合并。
	parent := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-parent", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
			chatType: feishu.ChatTypeGroup, chatID: "oc_group", messageID: "om_child", parentID: "om_parent", mentions: mentions,
		})},
	}, feishuJob("evt-parent", 1, 3), biz.FeishuEventWorkerConfig{BotOpenID: "ou_bot"})
	if _, err := parent.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("parent process: %v", err)
	}
	if parent.conversations.binding.ThreadID != "om_parent" || parent.conversations.binding.RootMessageID != "om_parent" {
		t.Fatalf("parent binding = %+v", parent.conversations.binding)
	}
	// 超长正文按配置上限截断后再入库。
	truncated := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-truncate", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
			chatID: "oc_1", messageID: "om_truncate", content: `{"text":"abcdefgh"}`,
		})},
	}, feishuJob("evt-truncate", 1, 3), biz.FeishuEventWorkerConfig{MaxMessageBytes: 4})
	if _, err := truncated.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("truncate process: %v", err)
	}
	if truncated.messages.message.Content != "abcd" {
		t.Fatalf("truncated content = %q", truncated.messages.message.Content)
	}
	// 超长 chat_id 生成标题时截断，避免标题无限膨胀。
	titleFixture := newFeishuWorkerFixture(t, biz.FeishuEventRecord{
		EventID: "evt-title", TenantKey: "tenant",
		Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
			chatID: strings.Repeat("c", 70), messageID: "om_title",
		})},
	}, feishuJob("evt-title", 1, 3), biz.FeishuEventWorkerConfig{})
	if _, err := titleFixture.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("title process: %v", err)
	}
	isTitleSuffix := strings.HasSuffix(titleFixture.conversations.session.Title, strings.Repeat("c", 60))
	if width := len("飞书会话 ") + 60; !isTitleSuffix || len(titleFixture.conversations.session.Title) != width {
		t.Fatalf("title = %q", titleFixture.conversations.session.Title)
	}
}

// feishuReplyFixture 聚合外发 Worker 测试桩。
type feishuReplyFixture struct {
	worker        *biz.FeishuReplyWorker
	outbound      *feishuOutboundStub
	runs          *feishuRunsStub
	sender        *feishuSenderStub
	conversations *feishuConversationStub
}

// newFeishuReplyFixture 构造外发 Worker 与桩依赖，时钟固定便于断言退避。
func newFeishuReplyFixture(t *testing.T, outbound biz.FeishuOutbound, run biz.Run, config biz.FeishuReplyWorkerConfig) *feishuReplyFixture {
	t.Helper()
	if config.WorkerID == "" {
		config.WorkerID = "reply-1"
	}
	outboundStub := &feishuOutboundStub{claimed: outbound, hasClaim: true}
	runs := &feishuRunsStub{run: run}
	sender := &feishuSenderStub{}
	conversations := &feishuConversationStub{}
	worker, err := biz.NewFeishuReplyWorker(biz.FeishuReplyWorkerDependencies{
		Outbound: outboundStub, Runs: runs, Sender: sender, Conversations: conversations,
		Clock: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, config)
	if err != nil {
		t.Fatalf("new reply worker: %v", err)
	}
	return &feishuReplyFixture{worker: worker, outbound: outboundStub, runs: runs, sender: sender, conversations: conversations}
}

// TestFeishuReplyWorkerRunStatesAndLookupFailure 覆盖 Run 读取失败与各终态卡片。
func TestFeishuReplyWorkerRunStatesAndLookupFailure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), ChatID: "oc_1",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now,
	}
	// Run 已被删除时直接结束外发，避免永久重试。
	missing := newFeishuReplyFixture(t, base, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	missing.runs.err = errors.New("run deleted")
	if _, err := missing.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("missing run process: %v", err)
	}
	if missing.outbound.completed != 1 || missing.sender.cardCalls != 0 {
		t.Fatalf("missing completed=%d cards=%d", missing.outbound.completed, missing.sender.cardCalls)
	}
	cases := []struct {
		name   string
		status biz.RunStatus
	}{
		{"failed", biz.RunFailed},
		{"cancelled", biz.RunCancelled},
		{"reconciliation", biz.RunReconciliationRequired},
		{"completed without assistant", biz.RunCompleted},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFeishuReplyFixture(t, base, biz.Run{Status: testCase.status}, biz.FeishuReplyWorkerConfig{})
			if _, err := fixture.worker.ProcessOne(context.Background()); err != nil {
				t.Fatalf("process: %v", err)
			}
			if fixture.sender.cardCalls != 1 || fixture.outbound.completed != 1 {
				t.Fatalf("cards=%d completed=%d", fixture.sender.cardCalls, fixture.outbound.completed)
			}
		})
	}
}

// TestFeishuReplyWorkerWaitTimeoutAndApproval 覆盖超时降级、审批增量更新与占位卡片。
func TestFeishuReplyWorkerWaitTimeoutAndApproval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// Run 长时间未完成时发送超时提示并结束外发。
	timeoutOutbound := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), MessageID: "om_card",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now.Add(-11 * time.Minute),
	}
	timeout := newFeishuReplyFixture(t, timeoutOutbound, biz.Run{Status: biz.RunRunning}, biz.FeishuReplyWorkerConfig{RunWaitTimeout: 10 * time.Minute})
	if _, err := timeout.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("timeout process: %v", err)
	}
	if timeout.sender.updateCalls != 1 || timeout.outbound.completed != 1 {
		t.Fatalf("timeout updates=%d completed=%d", timeout.sender.updateCalls, timeout.outbound.completed)
	}
	// 等待审批是重要节点，已有卡片时允许一次增量更新并延后检查。
	approvalOutbound := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), MessageID: "om_card",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now,
	}
	approval := newFeishuReplyFixture(t, approvalOutbound, biz.Run{Status: biz.RunWaitingApproval}, biz.FeishuReplyWorkerConfig{})
	if _, err := approval.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("approval process: %v", err)
	}
	if approval.sender.updateCalls != 1 || approval.outbound.deferred != 1 || approval.outbound.completed != 0 {
		t.Fatalf("approval updates=%d deferred=%d completed=%d", approval.sender.updateCalls, approval.outbound.deferred, approval.outbound.completed)
	}
	// 等待审批超过 RunWaitTimeout 不得按执行超时结束外发，否则审批通过后的最终回复会丢失。
	approvalTimeoutOutbound := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), MessageID: "om_card",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now.Add(-11 * time.Minute),
	}
	approvalTimeout := newFeishuReplyFixture(t, approvalTimeoutOutbound, biz.Run{Status: biz.RunWaitingApproval},
		biz.FeishuReplyWorkerConfig{RunWaitTimeout: 10 * time.Minute})
	if _, err := approvalTimeout.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("approval timeout process: %v", err)
	}
	if approvalTimeout.outbound.completed != 0 || approvalTimeout.outbound.deferred != 1 {
		t.Fatalf("approval timeout deferred=%d completed=%d", approvalTimeout.outbound.deferred, approvalTimeout.outbound.completed)
	}
	// 占位卡片发送失败不消耗失败预算，仅延后重试。
	pendingOutbound := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(),
		ReplyToMessageID: "om_1", Status: "pending", Attempts: 0, CreatedAt: now,
	}
	pending := newFeishuReplyFixture(t, pendingOutbound, biz.Run{Status: biz.RunRunning}, biz.FeishuReplyWorkerConfig{})
	pending.sender.cardErr = errors.New("card unavailable")
	if _, err := pending.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("pending process: %v", err)
	}
	if pending.sender.cardCalls != 1 || pending.outbound.deferred != 1 || pending.outbound.failed != 0 {
		t.Fatalf("pending cards=%d deferred=%d failed=%d", pending.sender.cardCalls, pending.outbound.deferred, pending.outbound.failed)
	}
	// 占位卡片成功时保存飞书消息 ID，供后续增量更新。
	saved := newFeishuReplyFixture(t, pendingOutbound, biz.Run{Status: biz.RunQueued}, biz.FeishuReplyWorkerConfig{})
	saved.sender.cardMessageID = "om_placeholder"
	if _, err := saved.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("placeholder process: %v", err)
	}
	if len(saved.outbound.updateIDs) != 1 || saved.outbound.updateIDs[0] != "om_placeholder" || saved.outbound.deferred != 1 {
		t.Fatalf("placeholder updates=%v deferred=%d", saved.outbound.updateIDs, saved.outbound.deferred)
	}
}

// TestFeishuReplyWorkerThreadReplyPromotionAndFallback 覆盖话题内回复、过渡别名升级与降级策略。
func TestFeishuReplyWorkerThreadReplyPromotionAndFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	newOutbound := func(threadID, replyTo string) biz.FeishuOutbound {
		return biz.FeishuOutbound{
			ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), ChatID: "oc_group",
			ThreadID: threadID, ReplyToMessageID: replyTo, Status: "sending", Attempts: 1, CreatedAt: now,
		}
	}
	run := biz.Run{Status: biz.RunCompleted}

	// 主时间线首条 @：以根消息 ID 作为过渡别名，回复成功后升级为飞书真实话题 ID。
	bootstrap := newFeishuReplyFixture(t, newOutbound("om_root", "om_root"), run, biz.FeishuReplyWorkerConfig{})
	bootstrap.sender.threadID = "omt_real"
	if _, err := bootstrap.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("bootstrap process: %v", err)
	}
	if bootstrap.sender.threadCalls != 1 || bootstrap.outbound.completed != 1 {
		t.Fatalf("bootstrap threads=%d completed=%d", bootstrap.sender.threadCalls, bootstrap.outbound.completed)
	}
	if bootstrap.conversations.promoteCalls != 1 || bootstrap.conversations.promoteLegacyThreadID != "om_root" ||
		bootstrap.conversations.promoteThreadID != "omt_real" {
		t.Fatalf("promote calls=%d legacy=%q thread=%q", bootstrap.conversations.promoteCalls,
			bootstrap.conversations.promoteLegacyThreadID, bootstrap.conversations.promoteThreadID)
	}

	// 真实话题内追问：直接复用话题回复，不再触发别名升级。
	existing := newFeishuReplyFixture(t, newOutbound("omt_real", "om_1"), run, biz.FeishuReplyWorkerConfig{})
	if _, err := existing.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("existing thread process: %v", err)
	}
	if existing.sender.threadCalls != 1 || existing.conversations.promoteCalls != 0 {
		t.Fatalf("existing threads=%d promote=%d", existing.sender.threadCalls, existing.conversations.promoteCalls)
	}

	// 普通群不支持话题：过渡别名回复失败时降级为主时间线文本，保证至少送达一次。
	fallback := newFeishuReplyFixture(t, newOutbound("om_root", "om_root"), run, biz.FeishuReplyWorkerConfig{})
	fallback.sender.cardErr = errors.New("thread card unavailable")
	if _, err := fallback.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("fallback process: %v", err)
	}
	if fallback.sender.threadCalls != 1 || fallback.sender.textThreadCalls != 1 || fallback.outbound.completed != 1 {
		t.Fatalf("fallback threads=%d textThreads=%d completed=%d",
			fallback.sender.threadCalls, fallback.sender.textThreadCalls, fallback.outbound.completed)
	}

	// 已存在真实话题时禁止降级到主时间线，避免不同话题的上下文串台；失败进入退避重试。
	broken := newFeishuReplyFixture(t, newOutbound("omt_real", "om_1"), run, biz.FeishuReplyWorkerConfig{})
	broken.sender.threadErr = errors.New("thread card unavailable")
	broken.sender.textErr = errors.New("thread text unavailable")
	if _, err := broken.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("broken thread process: %v", err)
	}
	if broken.outbound.failed != 1 || broken.outbound.completed != 0 || broken.sender.threadCalls != 1 {
		t.Fatalf("broken failed=%d completed=%d threads=%d", broken.outbound.failed, broken.outbound.completed, broken.sender.threadCalls)
	}

	// 话题回复被限流时不降级，按 Retry-After 退避，避免把限流放大为主时间线洪峰。
	limited := newFeishuReplyFixture(t, newOutbound("om_root", "om_root"), run, biz.FeishuReplyWorkerConfig{})
	limited.sender.threadErr = &feishu.APIError{StatusCode: 429, Code: 99991400, Message: "rate limited", RetryAfter: 2 * time.Second}
	if _, err := limited.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("limited thread process: %v", err)
	}
	if limited.outbound.failed != 1 || !limited.outbound.lastRetryAfter.Equal(now.Add(2*time.Second)) || limited.sender.textCalls != 0 {
		t.Fatalf("limited failed=%d retry=%s texts=%d", limited.outbound.failed, limited.outbound.lastRetryAfter, limited.sender.textCalls)
	}
}

// TestFeishuReplyWorkerRateLimitAndTextFallback 覆盖限流退避、文本降级与失败预算。
func TestFeishuReplyWorkerRateLimitAndTextFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), MessageID: "om_card",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now,
	}
	// 卡片更新被限流时必须按飞书建议的 RetryAfter 退避，且不降级文本。
	limited := newFeishuReplyFixture(t, base, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	limited.outbound.assistant = biz.Message{Content: "答案"}
	limited.sender.updateErr = &feishu.APIError{StatusCode: 429, Code: 99991400, Message: "rate limited", RetryAfter: 2 * time.Second}
	if _, err := limited.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("limited process: %v", err)
	}
	if limited.outbound.failed != 1 || !limited.outbound.lastRetryAfter.Equal(now.Add(2*time.Second)) || limited.sender.textCalls != 0 {
		t.Fatalf("limited failed=%d retry=%s texts=%d", limited.outbound.failed, limited.outbound.lastRetryAfter, limited.sender.textCalls)
	}
	// 包装后的限流错误同样必须识别，不能误降级为文本。
	wrapped := newFeishuReplyFixture(t, base, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	wrapped.outbound.assistant = biz.Message{Content: "答案"}
	wrapped.sender.updateErr = fmt.Errorf("feishu update: %w", &feishu.APIError{
		StatusCode: 429, Code: 99991400, Message: "rate limited", RetryAfter: 2 * time.Second,
	})
	if _, err := wrapped.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("wrapped process: %v", err)
	}
	if wrapped.outbound.failed != 1 || !wrapped.outbound.lastRetryAfter.Equal(now.Add(2*time.Second)) || wrapped.sender.textCalls != 0 {
		t.Fatalf("wrapped failed=%d retry=%s texts=%d", wrapped.outbound.failed, wrapped.outbound.lastRetryAfter, wrapped.sender.textCalls)
	}
	// 卡片更新返回普通错误时降级为纯文本，确保用户收到结果。
	fallback := newFeishuReplyFixture(t, base, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	fallback.outbound.assistant = biz.Message{Content: "答案"}
	fallback.sender.updateErr = errors.New("card update failed")
	if _, err := fallback.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("fallback process: %v", err)
	}
	if fallback.sender.textCalls != 1 || fallback.outbound.completed != 1 {
		t.Fatalf("fallback texts=%d completed=%d", fallback.sender.textCalls, fallback.outbound.completed)
	}
	// 卡片与文本均失败时按指数退避记录失败，超出预算由仓储进入死信。
	broken := newFeishuReplyFixture(t, base, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	broken.outbound.assistant = biz.Message{Content: "答案"}
	broken.sender.updateErr = errors.New("card update failed")
	broken.sender.textErr = errors.New("text reply failed")
	if _, err := broken.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("broken process: %v", err)
	}
	if broken.outbound.failed != 1 || !broken.outbound.lastRetryAfter.Equal(now.Add(2*time.Second)) {
		t.Fatalf("broken failed=%d retry=%s", broken.outbound.failed, broken.outbound.lastRetryAfter)
	}
	// 首次卡片发送被限流时同样尊重 RetryAfter，不发送文本。
	firstLimited := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(),
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now,
	}
	first := newFeishuReplyFixture(t, firstLimited, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	first.sender.cardErr = &feishu.APIError{StatusCode: 429, Code: 99991400, Message: "rate limited", RetryAfter: 2 * time.Second}
	if _, err := first.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("first limited process: %v", err)
	}
	if first.outbound.failed != 1 || first.sender.textCalls != 0 {
		t.Fatalf("first limited failed=%d texts=%d", first.outbound.failed, first.sender.textCalls)
	}
	// 首次卡片与文本均失败时进入退避。
	firstBroken := newFeishuReplyFixture(t, firstLimited, biz.Run{Status: biz.RunCompleted}, biz.FeishuReplyWorkerConfig{})
	firstBroken.sender.cardErr = errors.New("card unavailable")
	firstBroken.sender.textErr = errors.New("text unavailable")
	if _, err := firstBroken.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("first broken process: %v", err)
	}
	if firstBroken.outbound.failed != 1 {
		t.Fatalf("first broken failed=%d", firstBroken.outbound.failed)
	}
}

// TestFeishuReplyWorkerThrottlesCardUpdates 覆盖卡片增量更新的频率限制与内容去重。
func TestFeishuReplyWorkerThrottlesCardUpdates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := biz.FeishuOutbound{
		ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), ChatID: "oc_1", MessageID: "om_card",
		ReplyToMessageID: "om_1", Status: "sending", Attempts: 1, CreatedAt: now,
	}
	config := biz.FeishuReplyWorkerConfig{CardUpdateInterval: 5 * time.Second}
	// 达到最小间隔且内容指纹不同时允许一次增量更新。
	elapsed := base
	elapsed.CardUpdatedAt = now.Add(-10 * time.Second)
	elapsed.CardFingerprint = "stale"
	updated := newFeishuReplyFixture(t, elapsed, biz.Run{Status: biz.RunWaitingApproval}, config)
	if _, err := updated.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("elapsed process: %v", err)
	}
	if updated.sender.updateCalls != 1 || len(updated.outbound.cardFingerprints) != 1 || updated.outbound.deferred != 1 {
		t.Fatalf("elapsed updates=%d fingerprints=%d deferred=%d",
			updated.sender.updateCalls, len(updated.outbound.cardFingerprints), updated.outbound.deferred)
	}
	storedFingerprint := updated.outbound.cardFingerprints[0]
	// 内容未变化时即使超过间隔也不再更新，避免重复渲染同一状态。
	unchanged := base
	unchanged.CardUpdatedAt = now.Add(-10 * time.Second)
	unchanged.CardFingerprint = storedFingerprint
	skip := newFeishuReplyFixture(t, unchanged, biz.Run{Status: biz.RunWaitingApproval}, config)
	if _, err := skip.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("unchanged process: %v", err)
	}
	if skip.sender.updateCalls != 0 || skip.outbound.deferred != 1 {
		t.Fatalf("unchanged updates=%d deferred=%d", skip.sender.updateCalls, skip.outbound.deferred)
	}
	// 内容变化但未达到最小间隔时必须延后，不调用飞书卡片更新接口。
	throttled := base
	throttled.CardUpdatedAt = now.Add(-2 * time.Second)
	throttled.CardFingerprint = storedFingerprint
	pending := newFeishuReplyFixture(t, throttled, biz.Run{Status: biz.RunWaitingApproval}, config)
	if _, err := pending.worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("throttled process: %v", err)
	}
	if pending.sender.updateCalls != 0 || pending.outbound.deferred != 1 {
		t.Fatalf("throttled updates=%d deferred=%d", pending.sender.updateCalls, pending.outbound.deferred)
	}
}

// blockingFeishuOutboundStore 按队列返回外发记录，并记录延后次数。
type blockingFeishuOutboundStore struct {
	biz.FeishuOutboundStore
	mu       sync.Mutex
	queue    []biz.FeishuOutbound
	deferred atomic.Int32
}

// ClaimOutbound 依次弹出一条外发记录，模拟多个 Worker 竞争同一会话。
func (s *blockingFeishuOutboundStore) ClaimOutbound(_ context.Context, _ string, _ time.Duration, _ time.Time) (biz.FeishuOutbound, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return biz.FeishuOutbound{}, false, nil
	}
	outbound := s.queue[0]
	s.queue = s.queue[1:]
	return outbound, true, nil
}

// DeferOutbound 记录并发限制导致的延后。
func (s *blockingFeishuOutboundStore) DeferOutbound(_ context.Context, _ uuid.UUID, _ time.Time, _ time.Time) error {
	s.deferred.Add(1)
	return nil
}

func (s *blockingFeishuOutboundStore) UpdateOutboundMessageID(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}

func (s *blockingFeishuOutboundStore) MarkOutboundCardUpdated(_ context.Context, _ uuid.UUID, _ string, _ time.Time) error {
	return nil
}

func (s *blockingFeishuOutboundStore) CompleteOutbound(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return nil
}

func (s *blockingFeishuOutboundStore) FailOutbound(_ context.Context, _ uuid.UUID, _ string, _ time.Time, _ time.Time) error {
	return nil
}

// blockingFeishuSender 在卡片回复处阻塞，用于验证同一 chat 的并发投递限制。
type blockingFeishuSender struct {
	biz.FeishuReplySender
	started chan string
	release chan struct{}
}

func (s *blockingFeishuSender) ReplyCard(_ context.Context, replyTo string, _ any) (string, error) {
	s.started <- replyTo
	<-s.release
	return "om_card", nil
}

// TestFeishuReplyWorkerLimitsPerChatConcurrency 覆盖单会话并发限制与不同会话并行能力。
func TestFeishuReplyWorkerLimitsPerChatConcurrency(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	newOutbound := func(chatID string) biz.FeishuOutbound {
		return biz.FeishuOutbound{
			ID: uuid.New(), UserID: uuid.New(), RunID: uuid.New(), ChatID: chatID,
			ReplyToMessageID: "om_" + chatID, Status: "sending", Attempts: 1, CreatedAt: now,
		}
	}
	first := newOutbound("oc_same")
	second := newOutbound("oc_same")
	other := newOutbound("oc_other")
	store := &blockingFeishuOutboundStore{queue: []biz.FeishuOutbound{first, second, other}}
	sender := &blockingFeishuSender{started: make(chan string, 2), release: make(chan struct{})}
	worker, err := biz.NewFeishuReplyWorker(biz.FeishuReplyWorkerDependencies{
		Outbound: store, Runs: &feishuRunsStub{run: biz.Run{Status: biz.RunRunning}}, Sender: sender,
		Clock: func() time.Time { return now },
	}, biz.FeishuReplyWorkerConfig{WorkerID: "reply-limit", MaxConcurrentPerChat: 1, MaxConcurrentPerUser: 4})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		if _, processErr := worker.ProcessOne(context.Background()); processErr != nil {
			t.Errorf("first process: %v", processErr)
		}
	}()
	// 等待首个会话进入飞书回复调用，确保名额已被占用。
	if replyTo := <-sender.started; replyTo != first.ReplyToMessageID {
		t.Fatalf("first started reply = %q", replyTo)
	}
	// 同一 chat 的第二条记录必须延后，不得并发调用飞书。
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("second process: %v", err)
	}
	if store.deferred.Load() != 1 {
		t.Fatalf("same chat deferred = %d", store.deferred.Load())
	}
	// 不同 chat 不受单会话限制，可与首个会话并行投递。
	go func() {
		defer workers.Done()
		if _, processErr := worker.ProcessOne(context.Background()); processErr != nil {
			t.Errorf("other process: %v", processErr)
		}
	}()
	if replyTo := <-sender.started; replyTo != other.ReplyToMessageID {
		t.Fatalf("other started reply = %q", replyTo)
	}
	close(sender.release)
	workers.Wait()
	// 同 chat 的竞争记录在释放后仍需等待下一轮，最终共三次延后（含首次被限制的一次）。
	if store.deferred.Load() != 3 {
		t.Fatalf("deferred after release = %d", store.deferred.Load())
	}
}

// TestFeishuWorkersStopOnCancelledContext 覆盖两个 Worker 在上下文取消后退出轮询。
func TestFeishuWorkersStopOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eventFixture := newFeishuWorkerFixture(t, biz.FeishuEventRecord{EventID: "evt-stop"},
		biz.Job{ID: uuid.New(), Payload: map[string]any{"event_id": "evt-stop"}}, biz.FeishuEventWorkerConfig{})
	eventFixture.jobs.claimed = false
	eventFixture.worker.Start(ctx)
	replyFixture := newFeishuReplyFixture(t, biz.FeishuOutbound{ID: uuid.New()}, biz.Run{Status: biz.RunRunning}, biz.FeishuReplyWorkerConfig{})
	replyFixture.outbound.hasClaim = false
	replyFixture.worker.Start(ctx)
}

// TestAcceptTrustedPersistsAndEnqueues 覆盖长连接事件跳过 HTTP 验签后的入库与入队。
func TestAcceptTrustedPersistsAndEnqueues(t *testing.T) {
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	store := &feishuEventStoreStub{}
	enqueuer := &feishuEnqueuerStub{}
	service, err := biz.NewFeishuIngestService(verifier, store, enqueuer, biz.FeishuIngestConfig{MaxBodyBytes: 4096})
	if err != nil {
		t.Fatalf("new ingest service: %v", err)
	}
	payload := []byte(`{"schema":"2.0","header":{"event_id":"evt-longconn","event_type":"im.message.receive_v1","app_id":"cli_longconn"},"event":{}}`)
	result, err := service.AcceptTrusted(context.Background(), uuid.New(), "cli_longconn", payload)
	if err != nil {
		t.Fatalf("AcceptTrusted() error = %v", err)
	}
	if !result.Accepted || store.recorded != 1 {
		t.Fatalf("unexpected result %+v recorded=%d", result, store.recorded)
	}
	if len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != "evt-longconn" {
		t.Fatalf("unexpected enqueued events %v", enqueuer.enqueued)
	}
}

// TestAcceptTrustedRejectsAppIDMismatch 覆盖长连接事件不能串到其他应用。
func TestAcceptTrustedRejectsAppIDMismatch(t *testing.T) {
	verifier, err := feishu.NewVerifier(feishu.VerifierConfig{})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	service, err := biz.NewFeishuIngestService(verifier, &feishuEventStoreStub{}, &feishuEnqueuerStub{}, biz.FeishuIngestConfig{MaxBodyBytes: 4096})
	if err != nil {
		t.Fatalf("new ingest service: %v", err)
	}
	payload := []byte(`{"schema":"2.0","header":{"event_id":"evt-mismatch","event_type":"im.message.receive_v1","app_id":"cli_other"},"event":{}}`)
	if _, err := service.AcceptTrusted(context.Background(), uuid.New(), "cli_expected", payload); err == nil {
		t.Fatal("AcceptTrusted() should reject app id mismatch")
	}
}
