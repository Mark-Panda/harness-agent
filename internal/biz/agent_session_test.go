package biz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// TestAgentListAllRequiresAdmin 验证跨用户助手视图只对管理员开放。
func TestAgentListAllRequiresAdmin(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockAgentRepository(ctrl)
	service, err := biz.NewAgentService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListAll(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, biz.AdminAgentFilter{}, 10, ""); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin ListAll() error = %v, want forbidden", err)
	}
	adminID := uuid.New()
	repo.EXPECT().ListAllAgents(gomock.Any(), biz.AdminAgentFilter{}, 10, "").Return(biz.Page[biz.AdminAgent]{}, nil)
	if _, err := service.ListAll(context.Background(), biz.Principal{UserID: adminID, Role: biz.RoleAdmin}, biz.AdminAgentFilter{}, 10, ""); err != nil {
		t.Fatalf("admin ListAll() error = %v", err)
	}
}

// TestAgentAdminDeleteRequiresAdmin 验证跨用户删除助手只对管理员开放。
func TestAgentAdminDeleteRequiresAdmin(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockAgentRepository(ctrl)
	service, err := biz.NewAgentService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	agentID := uuid.New()
	if err := service.AdminDelete(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}, agentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("non-admin AdminDelete() error = %v, want forbidden", err)
	}
	ownerID := uuid.New()
	repo.EXPECT().GetAgentAdmin(gomock.Any(), agentID).Return(biz.Agent{ID: agentID, UserID: ownerID, Name: "assistant", Status: biz.AgentPublished}, nil)
	repo.EXPECT().DeleteAgentAdmin(gomock.Any(), gomock.Any(), agentID, gomock.Any()).Return(nil)
	if err := service.AdminDelete(context.Background(), biz.Principal{UserID: uuid.New(), Role: biz.RoleAdmin}, agentID); err != nil {
		t.Fatalf("admin AdminDelete() error = %v", err)
	}
}

func TestAgentServiceVersionsAndImmutablePublication(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockAgentRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	policyID := uuid.New()
	policyVersion := 1
	modelID := uuid.New()
	skillID := uuid.New()
	mcpID := uuid.New()
	policy := biz.ModelPolicy{ID: policyID, Version: policyVersion, Status: biz.PolicyPublished, Candidates: []biz.PolicyCandidate{{ModelID: modelID, Priority: 1}}}
	service, err := biz.NewAgentService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	input := biz.Agent{
		Name: " assistant ", Description: "helpful", Mode: biz.AgentModeReact,
		ModelPolicyID: &policyID, ModelPolicyVersion: &policyVersion, SkillIDs: []uuid.UUID{skillID}, MCPIDs: []uuid.UUID{mcpID},
	}
	repo.EXPECT().CountAgents(gomock.Any(), principal.UserID).Return(int64(0), nil)
	repo.EXPECT().GetPolicy(gomock.Any(), policyID).Return(policy, nil).Times(4)
	repo.EXPECT().CreateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) {
			if agent.Name != "assistant" || agent.UserID != principal.UserID || agent.Status != biz.AgentDraft || agent.Version != 1 {
				t.Fatalf("invalid draft agent: %#v", agent)
			}
			return agent, nil
		},
	)
	created, err := service.Create(context.Background(), principal, input)
	if err != nil {
		t.Fatal(err)
	}
	created.ID = uuid.New()

	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, created.ID).Return(created, nil).Times(2)
	repo.EXPECT().UpdateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) { return agent, nil },
	).Times(2)
	created.Description = "updated"
	if _, err := service.Update(context.Background(), principal, created); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().ListModelsByIDs(gomock.Any(), []uuid.UUID{modelID}).Return([]biz.Model{{ID: modelID}}, nil)
	repo.EXPECT().ListSkillsByIDs(gomock.Any(), []uuid.UUID{skillID}).Return([]biz.Skill{{ID: skillID, Status: "published"}}, nil)
	repo.EXPECT().ListMCPsByIDs(gomock.Any(), []uuid.UUID{mcpID}).Return([]biz.MCP{{ID: mcpID, Status: "enabled"}}, nil)
	published, err := service.Publish(context.Background(), principal, created.ID)
	if err != nil || published.Status != biz.AgentPublished || published.ConfigSnapshot["model_policy"] == nil {
		t.Fatalf("Publish() = %#v, %v", published, err)
	}

	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, created.ID).Return(created, nil)
	repo.EXPECT().CountAgents(gomock.Any(), principal.UserID).Return(int64(1), nil)
	repo.EXPECT().NextAgentVersion(gomock.Any(), principal.UserID, created.Name).Return(2, nil)
	repo.EXPECT().CreateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) {
			if agent.Version != 2 || agent.ID == created.ID || agent.Status != biz.AgentDraft {
				t.Fatalf("invalid next agent version: %#v", agent)
			}
			return agent, nil
		},
	)
	if _, err := service.CreateVersion(context.Background(), principal, created.ID); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, created.ID).Return(created, nil).Times(2)
	repo.EXPECT().DeleteAgent(gomock.Any(), principal.UserID, created.ID, gomock.Any()).Return(nil)
	repo.EXPECT().ListAgents(gomock.Any(), principal.UserID, false, 20, "cursor").Return(biz.Page[biz.Agent]{Items: []biz.Agent{created}}, nil)
	if _, err := service.Get(context.Background(), principal, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), principal, false, 20, "cursor"); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), principal, created.ID); err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, created.ID).Return(published, nil)
	if err := service.Delete(context.Background(), principal, created.ID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("published delete error = %v", err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{}, input); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous create error = %v", err)
	}
	if _, err := biz.NewAgentService(nil, biz.QuotaLimits{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
	limited, _ := biz.NewAgentService(repo, biz.QuotaLimits{MaxAgentsPerUser: 1})
	repo.EXPECT().CountAgents(gomock.Any(), principal.UserID).Return(int64(1), nil)
	if _, err := limited.Create(context.Background(), principal, input); !errors.Is(err, biz.ErrQuotaExceeded) {
		t.Fatalf("agent quota error = %v", err)
	}
}

// TestAgentServiceUnpublishAndArchive 验证已发布版本可取消发布或归档，并拒绝非法状态转换。
func TestAgentServiceUnpublishAndArchive(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockAgentRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	service, err := biz.NewAgentService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	agentID := uuid.New()
	published := biz.Agent{ID: agentID, UserID: principal.UserID, Name: "assistant", Version: 1, Status: biz.AgentPublished, ConfigSnapshot: map[string]any{"model_policy": map[string]any{}}}
	draft := published
	draft.Status = biz.AgentDraft
	draft.ConfigSnapshot = nil
	archived := published
	archived.Status = biz.AgentArchived

	// 取消发布：已发布 -> 草稿，并清空已失效的发布快照。
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(published, nil)
	repo.EXPECT().UpdateAgentStatus(gomock.Any(), principal.UserID, agentID, biz.AgentPublished, biz.AgentDraft, true, gomock.Any()).Return(nil)
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(draft, nil)
	unpublished, err := service.Unpublish(context.Background(), principal, agentID)
	if err != nil || unpublished.Status != biz.AgentDraft || unpublished.ConfigSnapshot != nil {
		t.Fatalf("Unpublish() = %#v, %v", unpublished, err)
	}

	// 归档：已发布 -> 已归档，保留历史快照。
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(published, nil)
	repo.EXPECT().UpdateAgentStatus(gomock.Any(), principal.UserID, agentID, biz.AgentPublished, biz.AgentArchived, false, gomock.Any()).Return(nil)
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(archived, nil)
	result, err := service.Archive(context.Background(), principal, agentID)
	if err != nil || result.Status != biz.AgentArchived {
		t.Fatalf("Archive() = %#v, %v", result, err)
	}

	// 草稿既不能取消发布，也不能归档。
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(draft, nil).Times(2)
	if _, err := service.Unpublish(context.Background(), principal, agentID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("draft unpublish error = %v", err)
	}
	if _, err := service.Archive(context.Background(), principal, agentID); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("draft archive error = %v", err)
	}
	if _, err := service.Unpublish(context.Background(), biz.Principal{}, agentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous unpublish error = %v", err)
	}
	if _, err := service.Archive(context.Background(), biz.Principal{}, agentID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous archive error = %v", err)
	}
}

func TestSessionServiceIsolationPaginationAndMessageRules(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockSessionRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	agentID := uuid.New()
	service, err := biz.NewSessionService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().CountSessions(gomock.Any(), principal.UserID).Return(int64(0), nil)
	repo.EXPECT().GetAgent(gomock.Any(), principal.UserID, agentID).Return(biz.Agent{ID: agentID, Status: biz.AgentPublished}, nil)
	repo.EXPECT().CreateSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.UserID != principal.UserID || session.Status != biz.SessionActive || session.Source != biz.SessionSourceWeb || session.Version != 1 {
				t.Fatalf("invalid new session: %#v", session)
			}
			return session, nil
		},
	)
	session, err := service.Create(context.Background(), principal, biz.Session{AgentID: &agentID, Title: "private"})
	if err != nil {
		t.Fatal(err)
	}
	session.ID = uuid.New()

	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil).Times(2)
	repo.EXPECT().ListSessions(gomock.Any(), principal.UserID, 20, "").Return(biz.Page[biz.Session]{Items: []biz.Session{session}}, nil)
	repo.EXPECT().ArchiveSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, archived biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if archived.Status != biz.SessionArchived || archived.Version != 2 {
				t.Fatalf("invalid archive transition: %#v", archived)
			}
			return archived, nil
		},
	)
	if _, err := service.Get(context.Background(), principal, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), principal, 20, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Archive(context.Background(), principal, session.ID); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil).Times(2)
	repo.EXPECT().AppendMessage(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, message biz.Message, _ biz.OutboxEvent) (biz.Message, error) {
			if message.Role != biz.MessageUser || message.UserID != principal.UserID || message.TokenCount != 0 {
				t.Fatalf("invalid user message: %#v", message)
			}
			return message, nil
		},
	)
	message, err := service.AppendMessage(context.Background(), principal, biz.Message{SessionID: session.ID, Role: biz.MessageAssistant, Content: "hello", TokenCount: 100})
	if err != nil {
		t.Fatal(err)
	}
	repo.EXPECT().ListMessages(gomock.Any(), principal.UserID, session.ID, 50, "cursor").Return(biz.Page[biz.Message]{Items: []biz.Message{message}}, nil)
	if _, err := service.ListMessages(context.Background(), principal, session.ID, 50, "cursor"); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(biz.Session{ID: session.ID, Status: biz.SessionArchived}, nil)
	if _, err := service.AppendMessage(context.Background(), principal, biz.Message{SessionID: session.ID, Content: "blocked"}); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("archived session append error = %v", err)
	}
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	if _, err := service.AppendMessage(context.Background(), principal, biz.Message{SessionID: session.ID, Content: " "}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("empty message error = %v", err)
	}
	if _, err := service.Create(context.Background(), biz.Principal{}, biz.Session{}); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous session error = %v", err)
	}
	if _, err := biz.NewSessionService(nil, biz.QuotaLimits{}); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil dependency error = %v", err)
	}
	limited, _ := biz.NewSessionService(repo, biz.QuotaLimits{MaxSessionsPerUser: 1})
	repo.EXPECT().CountSessions(gomock.Any(), principal.UserID).Return(int64(1), nil)
	if _, err := limited.Create(context.Background(), principal, biz.Session{Title: "over-limit"}); !errors.Is(err, biz.ErrQuotaExceeded) {
		t.Fatalf("session quota error = %v", err)
	}
}

// TestSessionServiceSetMemoryPolicy 验证记忆策略变更的鉴权、校验与乐观锁递增。
func TestSessionServiceSetMemoryPolicy(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockSessionRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	service, err := biz.NewSessionService(repo, biz.QuotaLimits{})
	if err != nil {
		t.Fatal(err)
	}
	session := biz.Session{
		ID: uuid.New(), UserID: principal.UserID, Status: biz.SessionActive,
		MemoryPolicy: biz.SessionMemoryPrivate, Version: 3,
	}
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	repo.EXPECT().UpdateSessionMemoryPolicy(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if updated.MemoryPolicy != biz.SessionMemoryDisabled || updated.Version != 4 {
				t.Fatalf("invalid memory policy update: %#v", updated)
			}
			return updated, nil
		},
	)
	updated, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryDisabled)
	if err != nil || updated.MemoryPolicy != biz.SessionMemoryDisabled || updated.Version != 4 {
		t.Fatalf("SetMemoryPolicy() = %#v, %v", updated, err)
	}

	if _, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryPolicy("unknown")); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("invalid policy error = %v", err)
	}
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	if _, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryPrivate); err != nil {
		t.Fatalf("idempotent policy update error = %v", err)
	}
	archived := session
	archived.Status = biz.SessionArchived
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(archived, nil)
	if _, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryDisabled); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("archived policy update error = %v", err)
	}
	if _, err := service.SetMemoryPolicy(context.Background(), biz.Principal{}, session.ID, biz.SessionMemoryDisabled); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous policy update error = %v", err)
	}
}

// sharedSessionStub 返回预设的共享会话判定。
type sharedSessionStub struct {
	// shared 是否为共享会话。
	shared bool
	// err 判定失败错误。
	err error
	// calls 判定调用次数。
	calls int
}

// IsSharedSession 记录判定调用并返回预设结果。
func (s *sharedSessionStub) IsSharedSession(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	s.calls++
	return s.shared, s.err
}

// TestSessionServiceRejectsPrivateMemoryForSharedSession 验证群聊话题不得开启私有记忆。
func TestSessionServiceRejectsPrivateMemoryForSharedSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := mocks.NewMockSessionRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	detector := &sharedSessionStub{shared: true}
	service, err := biz.NewSessionService(repo, biz.QuotaLimits{}, biz.WithSharedSessionDetector(detector))
	if err != nil {
		t.Fatal(err)
	}
	session := biz.Session{
		ID: uuid.New(), UserID: principal.UserID, Status: biz.SessionActive,
		MemoryPolicy: biz.SessionMemoryGroup, Version: 2,
	}
	// 共享会话开启私有记忆必须拒绝，且不得触达仓储更新。
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	if _, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryPrivate); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("shared session private policy error = %v", err)
	}
	if detector.calls != 1 {
		t.Fatalf("shared detection calls = %d", detector.calls)
	}
	// 收紧为禁用记忆仍然允许。
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	repo.EXPECT().UpdateSessionMemoryPolicy(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, updated biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			return updated, nil
		},
	)
	if updated, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryDisabled); err != nil || updated.MemoryPolicy != biz.SessionMemoryDisabled {
		t.Fatalf("shared session disabled policy = %#v, %v", updated, err)
	}
	// 检测失败必须向上返回，不能静默放行。
	detector.err = errors.New("binding store unavailable")
	repo.EXPECT().GetSession(gomock.Any(), principal.UserID, session.ID).Return(session, nil)
	if _, err := service.SetMemoryPolicy(context.Background(), principal, session.ID, biz.SessionMemoryPrivate); !errors.Is(err, detector.err) {
		t.Fatalf("shared detection failure = %v", err)
	}
}

func TestRunStateMachineRejectsIllegalTransitions(t *testing.T) {
	valid := [][2]biz.RunStatus{
		{biz.RunQueued, biz.RunRunning}, {biz.RunRunning, biz.RunWaitingApproval},
		{biz.RunWaitingApproval, biz.RunRunning}, {biz.RunRunning, biz.RunReconciliationRequired},
		{biz.RunReconciliationRequired, biz.RunFailed}, {biz.RunRunning, biz.RunCompleted},
	}
	for _, transition := range valid {
		if err := biz.TransitionRunStatus(transition[0], transition[1]); err != nil {
			t.Fatalf("valid transition %v: %v", transition, err)
		}
	}
	invalid := [][2]biz.RunStatus{
		{biz.RunCompleted, biz.RunRunning}, {biz.RunCancelled, biz.RunQueued},
		{biz.RunQueued, biz.RunCompleted}, {biz.RunWaitingApproval, biz.RunCompleted},
	}
	for _, transition := range invalid {
		if err := biz.TransitionRunStatus(transition[0], transition[1]); !errors.Is(err, biz.ErrResourceConflict) {
			t.Fatalf("invalid transition %v error = %v", transition, err)
		}
	}
	if err := biz.TransitionRunStatus("unknown", biz.RunRunning); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("unknown status error = %v", err)
	}
	if !biz.RunTerminal(biz.RunCompleted) || !biz.RunTerminal(biz.RunFailed) || !biz.RunTerminal(biz.RunCancelled) || biz.RunTerminal(biz.RunRunning) {
		t.Fatal("terminal run states are incorrect")
	}
}
