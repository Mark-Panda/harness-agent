package biz_test

import (
	"context"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/pkg/feishu"
	"github.com/google/uuid"
)

func TestFeishuSharedGroupRunsUnderEachSpeaker(t *testing.T) {
	ownerID, firstID, secondID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	agentID, version := uuid.New(), 1
	conversations := &feishuConversationStub{}
	groups := &feishuGroupStub{}
	runs := &feishuGroupRunsStub{}
	events := &feishuEventStoreStub{}
	identities := &feishuIdentityStub{}
	jobs := &feishuJobsStub{claimed: true}
	messages := &feishuMessagesStub{}
	outbound := &feishuOutboundStub{}
	worker, err := biz.NewFeishuEventWorker(biz.FeishuEventWorkerDependencies{
		Jobs: jobs, Events: events, Identities: identities, Conversations: &feishuScopedConversations{conversations},
		Sessions: &feishuSessionsStub{conversation: conversations}, Messages: messages,
		Runs: runs, Outbound: outbound, GroupMessages: groups,
	}, biz.FeishuEventWorkerConfig{
		WorkerID: "worker", BotOpenID: "ou_bot", DefaultAppID: "cli_default", DefaultAgentOwnerID: &ownerID,
		DefaultAgentID: &agentID, DefaultAgentVersion: &version,
		SharedChats: []biz.FeishuSharedChat{{AppID: "cli_default", TenantKey: "tenant", ChatID: "oc_shared", PolicyID: policyID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	process := func(actor uuid.UUID, openID, messageID, appID, tenant, chatID string) string {
		t.Helper()
		identities.userID = actor
		eventID := "event-" + messageID
		events.event = biz.FeishuEventRecord{EventID: eventID, TenantKey: tenant, AppID: appID,
			Payload: map[string]any{"body": buildFeishuBody(t, feishuBodyOptions{
				chatType: feishu.ChatTypeGroup, chatID: chatID, threadID: "omt_shared", senderOpenID: &openID,
				messageID: messageID, mentions: []map[string]string{{"open_id": "ou_bot"}},
			})}}
		jobs.job = feishuJob(eventID, 1, 3)
		if _, err := worker.ProcessOne(context.Background()); err != nil {
			t.Fatal(err)
		}
		return events.statuses[eventID]
	}
	for _, chat := range [][3]string{{"cli_other", "tenant", "oc_shared"}, {"cli_default", "other", "oc_shared"}, {"cli_default", "tenant", "oc_other"}} {
		if got := process(firstID, "ou_a", uuid.NewString(), chat[0], chat[1], chat[2]); got != biz.FeishuEventIgnored {
			t.Fatalf("unlisted scope got %q", got)
		}
	}
	if got := process(firstID, "ou_a", "om_a", "cli_default", "tenant", "oc_shared"); got != biz.FeishuEventProcessed {
		t.Fatalf("first event status = %q", got)
	}
	conversations.found = true
	if got := process(secondID, "ou_b", "om_b", "cli_default", "tenant", "oc_shared"); got != biz.FeishuEventProcessed {
		t.Fatalf("second event status = %q", got)
	}
	if len(groups.messages) != 2 || len(runs.actors) != 2 || runs.calls != 0 || messages.message.ID != uuid.Nil {
		t.Fatalf("group messages=%+v actors=%+v ordinary calls=%d", groups.messages, runs.actors, runs.calls)
	}
	for i, actor := range []uuid.UUID{firstID, secondID} {
		message := groups.messages[i]
		if message.UserID != actor || runs.actors[i] != actor || runs.policies[i] != policyID ||
			runs.inputs[i].AgentID != nil || runs.inputs[i].AgentVersion != nil ||
			runs.inputs[i].TriggerMessageID == nil || groups.bindings[i].UserID != ownerID ||
			message.SpeakerOpenID != []string{"ou_a", "ou_b"}[i] {
			t.Fatalf("speaker %d has message=%+v, actor=%v, input=%+v, binding=%+v", i, message, runs.actors[i], runs.inputs[i], groups.bindings[i])
		}
	}
	if groups.bindings[0].SessionID != groups.bindings[1].SessionID || runs.inputs[0].SessionID == runs.inputs[1].SessionID {
		t.Fatalf("bindings=%+v runs=%+v", groups.bindings, runs.inputs)
	}
	lastReply := outbound.created
	runs.unsafeSnapshot = true
	if got := process(firstID, "ou_a", "om_forged", "cli_default", "tenant", "oc_shared"); got != biz.FeishuEventFailed ||
		outbound.created != lastReply {
		t.Fatalf("unauthorized run produced an outbound record, status=%q outbound=%+v", got, outbound.created)
	}
}

type feishuScopedConversations struct{ *feishuConversationStub }

func (s *feishuScopedConversations) FindConversationForApp(ctx context.Context, appID, tenant, chat, thread, root string) (biz.FeishuConversationBinding, bool, error) {
	if appID == "" {
		return biz.FeishuConversationBinding{}, false, nil
	}
	return s.FindConversation(ctx, tenant, chat, thread, root)
}

type feishuGroupStub struct {
	messages []biz.Message
	bindings []biz.FeishuConversationBinding
}

func (s *feishuGroupStub) ProjectGroupMessage(_ context.Context, binding biz.FeishuConversationBinding, inbound biz.Message) (biz.Message, error) {
	s.messages = append(s.messages, inbound)
	s.bindings = append(s.bindings, binding)
	inbound.ID, inbound.SessionID = uuid.New(), uuid.New()
	return inbound, nil
}

type feishuGroupRunsStub struct {
	feishuRunCreatorStub
	actors         []uuid.UUID
	policies       []uuid.UUID
	inputs         []biz.CreateRunInput
	unsafeSnapshot bool
}

func (s *feishuGroupRunsStub) CreateFeishuGroup(_ context.Context, principal biz.Principal, policyID uuid.UUID, input biz.CreateRunInput) (biz.Run, error) {
	s.actors = append(s.actors, principal.UserID)
	s.policies = append(s.policies, policyID)
	s.inputs = append(s.inputs, input)
	return biz.Run{
		ID: uuid.New(), UserID: principal.UserID, SessionID: input.SessionID, Mode: input.Mode,
		TriggerMessageID: input.TriggerMessageID, ConfigSnapshot: map[string]any{"feishu_shared_group": !s.unsafeSnapshot},
		ModelPolicySnapshot: map[string]any{"policy_id": policyID.String()},
	}, nil
}
