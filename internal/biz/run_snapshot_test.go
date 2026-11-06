package biz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// fakeSnapshotRepository 提供可控的 Agent、策略、模型与供应商快照来源。
type fakeSnapshotRepository struct {
	agent       biz.Agent
	agentErr    error
	policy      biz.ModelPolicy
	policyErr   error
	models      []biz.Model
	modelsErr   error
	providers   map[uuid.UUID]biz.Provider
	providerErr error
}

// GetAgent 返回预设 Agent 或错误。
func (f *fakeSnapshotRepository) GetAgent(_ context.Context, _ uuid.UUID, _ uuid.UUID) (biz.Agent, error) {
	return f.agent, f.agentErr
}

// GetPolicy 返回预设策略或错误。
func (f *fakeSnapshotRepository) GetPolicy(_ context.Context, _ uuid.UUID) (biz.ModelPolicy, error) {
	return f.policy, f.policyErr
}

// ListModelsByIDs 返回预设模型或错误。
func (f *fakeSnapshotRepository) ListModelsByIDs(_ context.Context, _ []uuid.UUID) ([]biz.Model, error) {
	return f.models, f.modelsErr
}

// GetProvider 返回预设供应商或错误。
func (f *fakeSnapshotRepository) GetProvider(_ context.Context, providerID uuid.UUID) (biz.Provider, error) {
	if f.providerErr != nil {
		return biz.Provider{}, f.providerErr
	}
	return f.providers[providerID], nil
}

// publishedSnapshotFixture 构造可用于成功创建 Run 的快照数据。
func publishedSnapshotFixture() (*fakeSnapshotRepository, biz.Principal, uuid.UUID, int) {
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	policyID, modelID, providerID := uuid.New(), uuid.New(), uuid.New()
	version := 1
	now := time.Now().UTC()
	agent := biz.Agent{
		ID: uuid.New(), UserID: principal.UserID, Name: "assistant", Mode: biz.AgentModeReact,
		ModelPolicyID: &policyID, ModelPolicyVersion: &version, Version: 1, Status: biz.AgentPublished,
		ConfigSnapshot: map[string]any{"agent": map[string]any{"name": "assistant"}},
	}
	policy := biz.ModelPolicy{
		ID: policyID, Name: "policy", Version: version, MinContextTokens: biz.MinGenerationContextTokens,
		Status: biz.PolicyPublished, Candidates: []biz.PolicyCandidate{{ModelID: modelID, Priority: 1}},
	}
	model := biz.Model{
		ID: modelID, ProviderID: providerID, RemoteModelName: "generation", Kind: biz.ModelKindChat,
		ContextWindowTokens: biz.MinGenerationContextTokens, MaxOutputTokens: 8192,
		Status: biz.ModelEnabled, VerificationSource: "test", VerifiedAt: &now,
		Capabilities: biz.ModelCapabilities{Tools: true},
	}
	provider := biz.Provider{ID: providerID, Name: "provider", BaseURL: "https://models.example.com", Status: biz.ProviderEnabled}
	return &fakeSnapshotRepository{
		agent: agent, policy: policy, models: []biz.Model{model},
		providers: map[uuid.UUID]biz.Provider{providerID: provider},
	}, principal, agent.ID, version
}

// TestRunServiceBuildsImmutableSnapshots 验证 Run 创建时固化候选模型并幂等入队。
func TestRunServiceBuildsImmutableSnapshots(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	queue := mocks.NewMockJobQueue(ctrl)
	snapshots, principal, agentID, version := publishedSnapshotFixture()
	sessionID := uuid.New()
	service, err := biz.NewRunService(runRepo, sessionRepo, biz.WithRunSnapshots(snapshots), biz.WithRunEnqueuer(queue))
	if err != nil {
		t.Fatal(err)
	}
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(
		biz.Session{ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive}, nil,
	)
	triggerMessageID := uuid.New()
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").Return(
		biz.Page[biz.Message]{Items: []biz.Message{{ID: triggerMessageID, Role: biz.MessageUser}}}, nil,
	)
	runRepo.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, run biz.Run) (biz.Run, error) {
			if run.ConfigSnapshot == nil || run.ModelPolicySnapshot == nil {
				t.Fatalf("run snapshots were not frozen: %#v", run)
			}
			if run.TriggerMessageID == nil || *run.TriggerMessageID != triggerMessageID {
				t.Fatalf("trigger message boundary was not frozen: %#v", run)
			}
			candidates, _ := run.ModelPolicySnapshot["candidates"].([]map[string]any)
			if len(candidates) != 1 || candidates[0]["model_id"] == "" {
				t.Fatalf("unexpected candidates: %#v", run.ModelPolicySnapshot)
			}
			if candidates[0]["provider_base_url"] != "https://models.example.com" {
				t.Fatalf("provider base URL was not frozen: %#v", candidates[0])
			}
			return run, nil
		},
	)
	queue.EXPECT().EnqueueRun(gomock.Any(), principal.UserID, gomock.Any(), gomock.Any()).Return(nil)
	run, err := service.Create(context.Background(), principal, biz.CreateRunInput{
		SessionID: sessionID, AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != biz.RunQueued || run.ModelPolicySnapshot["policy_version"] != 1 {
		t.Fatalf("unexpected run: %#v", run)
	}
}

// TestRunServiceRejectsUnroutableSnapshots 验证未发布、版本不一致与不可路由候选被拒绝。
func TestRunServiceRejectsUnroutableSnapshots(t *testing.T) {
	base, principal, agentID, version := publishedSnapshotFixture()
	tests := []struct {
		name   string
		mutate func(*fakeSnapshotRepository)
		input  func() biz.CreateRunInput
		want   error
	}{
		{
			name: "agent required", mutate: func(*fakeSnapshotRepository) {},
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "agent lookup failed", mutate: func(repo *fakeSnapshotRepository) { repo.agentErr = biz.ErrResourceNotFound },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrResourceNotFound,
		},
		{
			name: "agent unpublished", mutate: func(repo *fakeSnapshotRepository) { repo.agent.Status = biz.AgentDraft },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrResourceConflict,
		},
		{
			name: "agent version mismatch", mutate: func(repo *fakeSnapshotRepository) { repo.agent.Version = 2 },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "mode mismatch", mutate: func(repo *fakeSnapshotRepository) { repo.agent.Mode = biz.AgentModePlan },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "policy missing", mutate: func(repo *fakeSnapshotRepository) {
				repo.agent.ModelPolicyID = nil
				repo.agent.ModelPolicyVersion = nil
			},
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "policy version mismatch", mutate: func(repo *fakeSnapshotRepository) { repo.policy.Version = 2 },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrResourceConflict,
		},
		{
			name: "model disabled", mutate: func(repo *fakeSnapshotRepository) { repo.models[0].Status = biz.ModelDisabled },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "model below policy context", mutate: func(repo *fakeSnapshotRepository) { repo.models[0].ContextWindowTokens = 900_000 },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "provider disabled", mutate: func(repo *fakeSnapshotRepository) {
				provider := repo.providers[repo.models[0].ProviderID]
				provider.Status = biz.ProviderDisabled
				repo.providers[repo.models[0].ProviderID] = provider
			},
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrInvalidConfig,
		},
		{
			name: "provider lookup failed", mutate: func(repo *fakeSnapshotRepository) { repo.providerErr = biz.ErrResourceNotFound },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrResourceNotFound,
		},
		{
			name: "models lookup failed", mutate: func(repo *fakeSnapshotRepository) { repo.modelsErr = errors.New("db down") },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: errors.New("db down"),
		},
		{
			name: "model missing", mutate: func(repo *fakeSnapshotRepository) { repo.models = nil },
			input: func() biz.CreateRunInput {
				return biz.CreateRunInput{AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact}
			},
			want: biz.ErrResourceConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			runRepo := mocks.NewMockRunRepository(ctrl)
			sessionRepo := mocks.NewMockSessionRepository(ctrl)
			repo := *base
			repo.models = append([]biz.Model(nil), base.models...)
			repo.providers = map[uuid.UUID]biz.Provider{}
			for key, value := range base.providers {
				repo.providers[key] = value
			}
			test.mutate(&repo)
			service, err := biz.NewRunService(runRepo, sessionRepo, biz.WithRunSnapshots(&repo))
			if err != nil {
				t.Fatal(err)
			}
			sessionID := uuid.New()
			sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(
				biz.Session{ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive}, nil,
			)
			sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").Return(
				biz.Page[biz.Message]{}, nil,
			)
			input := test.input()
			input.SessionID = sessionID
			_, err = service.Create(context.Background(), principal, input)
			if test.want == nil {
				t.Fatal("test case needs an expected error")
			}
			if !errors.Is(err, test.want) && err.Error() != test.want.Error() {
				t.Fatalf("Create() error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestRunServiceExecutionTrace 验证执行树查询校验 Run 归属并返回步骤与载荷。
func TestRunServiceExecutionTrace(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	steps := mocks.NewMockExecutionStepRepository(ctrl)
	service, err := biz.NewRunService(runRepo, sessionRepo, biz.WithExecutionSteps(steps))
	if err != nil {
		t.Fatal(err)
	}
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	runID := uuid.New()
	runRepo.EXPECT().GetRun(gomock.Any(), principal.UserID, runID).Return(biz.Run{ID: runID, UserID: principal.UserID}, nil).Times(2)
	steps.EXPECT().ListSteps(gomock.Any(), principal.UserID, runID, 500).Return([]biz.ExecutionStep{
		{ID: uuid.New(), RunID: runID, UserID: principal.UserID, StepSeq: 1, Kind: biz.StepKindModel, Status: biz.StepCompleted},
	}, nil)
	steps.EXPECT().ListPayloads(gomock.Any(), principal.UserID, runID, (*uuid.UUID)(nil), 200).Return([]biz.ExecutionPayload{
		{ID: uuid.New(), RunID: runID, UserID: principal.UserID, Type: biz.PayloadModelRequest, CaptureMode: biz.CaptureMetadataOnly},
	}, nil)
	resultSteps, err := service.ListSteps(context.Background(), principal, runID, 500)
	if err != nil || len(resultSteps) != 1 || resultSteps[0].StepSeq != 1 {
		t.Fatalf("ListSteps() = %#v, %v", resultSteps, err)
	}
	resultPayloads, err := service.ListPayloads(context.Background(), principal, runID, nil, 200)
	if err != nil || len(resultPayloads) != 1 || resultPayloads[0].Type != biz.PayloadModelRequest {
		t.Fatalf("ListPayloads() = %#v, %v", resultPayloads, err)
	}
	if _, err := service.ListSteps(context.Background(), biz.Principal{}, runID, 500); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous ListSteps error = %v", err)
	}
	// 未配置步骤仓储时返回空列表而不是报错，便于旧部署平滑升级。
	plain, _ := biz.NewRunService(runRepo, sessionRepo)
	if steps, err := plain.ListSteps(context.Background(), principal, runID, 500); err != nil || len(steps) != 0 {
		t.Fatalf("unconfigured ListSteps() = %#v, %v", steps, err)
	}
	if payloads, err := plain.ListPayloads(context.Background(), principal, runID, nil, 200); err != nil || len(payloads) != 0 {
		t.Fatalf("unconfigured ListPayloads() = %#v, %v", payloads, err)
	}
}

// TestRunServiceEnqueueFailureIsReturned 验证入队失败会让创建请求失败，客户端可安全重试。
func TestRunServiceEnqueueFailureIsReturned(t *testing.T) {
	ctrl := gomock.NewController(t)
	runRepo := mocks.NewMockRunRepository(ctrl)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	queue := mocks.NewMockJobQueue(ctrl)
	snapshots, principal, agentID, version := publishedSnapshotFixture()
	service, _ := biz.NewRunService(runRepo, sessionRepo, biz.WithRunSnapshots(snapshots), biz.WithRunEnqueuer(queue))
	sessionID := uuid.New()
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(
		biz.Session{ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive}, nil,
	)
	sessionRepo.EXPECT().ListMessages(gomock.Any(), principal.UserID, sessionID, gomock.Any(), "").Return(
		biz.Page[biz.Message]{}, nil,
	)
	runRepo.EXPECT().CreateRun(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, run biz.Run) (biz.Run, error) { return run, nil })
	queue.EXPECT().EnqueueRun(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("queue unavailable"))
	if _, err := service.Create(context.Background(), principal, biz.CreateRunInput{
		SessionID: sessionID, AgentID: &agentID, AgentVersion: &version, Mode: biz.AgentModeReact,
	}); err == nil {
		t.Fatal("enqueue failure must be surfaced")
	}
}
