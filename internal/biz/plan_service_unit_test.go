package biz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// planServiceRunRepo 是 PlanService 测试用 Run 仓储。
type planServiceRunRepo struct {
	RunRepository
	mu  sync.Mutex
	run Run
}

// GetRun 返回当前 Run。
func (r *planServiceRunRepo) GetRun(_ context.Context, _ uuid.UUID, _ uuid.UUID) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.run, nil
}

// TransitionRun 应用状态与结束时间。
func (r *planServiceRunRepo) TransitionRun(_ context.Context, _ uuid.UUID, _ uuid.UUID, _, to RunStatus, fields map[string]any) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.run.Status = to
	if finished, ok := fields["finished_at"].(time.Time); ok {
		r.run.FinishedAt = &finished
	}
	return r.run, nil
}

// planServiceQueue 记录入队次数。
type planServiceQueue struct {
	JobQueue
	mu       sync.Mutex
	enqueued int
}

// EnqueueRun 记录一次恢复入队。
func (q *planServiceQueue) EnqueueRun(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued++
	return nil
}

// planServiceSessions 仅满足构造函数依赖。
type planServiceSessions struct {
	SessionRepository
}

// planServiceFixture 构造 PlanService 与内存计划仓储。
func planServiceFixture(t *testing.T) (*PlanService, *memoryPlanRepo, *planServiceRunRepo, *planServiceQueue) {
	t.Helper()
	repo := &planServiceRunRepo{run: Run{ID: uuid.New(), Status: RunRunning}}
	queue := &planServiceQueue{}
	runs, err := NewRunService(repo, planServiceSessions{}, WithRunEnqueuer(queue))
	if err != nil {
		t.Fatal(err)
	}
	plans := newMemoryPlanRepo()
	service, err := NewPlanService(plans, runs)
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return time.Now().UTC() }
	return service, plans, repo, queue
}

// seedPlanForRun 预置指定状态的计划。
func seedPlanForRun(plans *memoryPlanRepo, runID, userID uuid.UUID, status PlanStatus) Plan {
	plan := Plan{
		ID: uuid.New(), UserID: userID, RunID: runID, SessionID: uuid.New(), Version: 1,
		Status: status, Goal: "g", MaxSteps: 50, Revision: 0,
		Steps: []PlanStep{{ID: uuid.New(), StepNo: 1, Title: "s", Status: PlanStepPending, MaxAttempts: 2}},
	}
	plans.seedPlan(plan)
	return plan
}

func TestPlanServiceCancel(t *testing.T) {
	service, plans, repo, _ := planServiceFixture(t)
	principal := Principal{UserID: repo.run.UserID, Role: RoleUser}
	if principal.UserID == uuid.Nil {
		principal.UserID = uuid.New()
		repo.run.UserID = principal.UserID
	}
	plan := seedPlanForRun(plans, repo.run.ID, principal.UserID, PlanExecuting)
	if _, err := service.GetByRun(context.Background(), principal, repo.run.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := service.Cancel(context.Background(), principal, repo.run.ID)
	if err != nil || cancelled.Status != PlanCancelled || cancelled.FinishedAt == nil {
		t.Fatalf("Cancel() = %#v, %v", cancelled, err)
	}
	if repo.run.Status != RunCancelled {
		t.Fatalf("run status = %s", repo.run.Status)
	}

	plan.Status = PlanCompleted
	plans.seedPlan(plan)
	if _, err := service.Cancel(context.Background(), principal, repo.run.ID); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("terminal cancel error = %v", err)
	}
}

func TestPlanServicePauseResume(t *testing.T) {
	service, plans, repo, queue := planServiceFixture(t)
	principal := Principal{UserID: uuid.New(), Role: RoleUser}
	repo.run.UserID = principal.UserID
	seedPlanForRun(plans, repo.run.ID, principal.UserID, PlanExecuting)
	paused, err := service.Pause(context.Background(), principal, repo.run.ID)
	if err != nil || paused.Status != PlanPaused {
		t.Fatalf("Pause() = %#v, %v", paused, err)
	}
	// 已暂停计划不能再次暂停。
	if _, err := service.Pause(context.Background(), principal, repo.run.ID); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("double pause error = %v", err)
	}
	resumed, err := service.Resume(context.Background(), principal, repo.run.ID)
	if err != nil || resumed.Status != PlanExecuting {
		t.Fatalf("Resume() = %#v, %v", resumed, err)
	}
	queue.mu.Lock()
	enqueued := queue.enqueued
	queue.mu.Unlock()
	if enqueued != 1 {
		t.Fatalf("resume enqueue count = %d", enqueued)
	}
	// 执行中的计划不能恢复。
	if _, err := service.Resume(context.Background(), principal, repo.run.ID); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("resume executing error = %v", err)
	}
}

func TestPlanServiceVersionsAndForbidden(t *testing.T) {
	service, plans, repo, _ := planServiceFixture(t)
	principal := Principal{UserID: uuid.New(), Role: RoleUser}
	repo.run.UserID = principal.UserID
	seedPlanForRun(plans, repo.run.ID, principal.UserID, PlanExecuting)
	versions, err := service.ListVersions(context.Background(), principal, repo.run.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	if _, err := service.GetByRun(context.Background(), Principal{}, repo.run.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous error = %v", err)
	}
}

func TestPlanServiceCancelIgnoresTerminalRun(t *testing.T) {
	service, plans, repo, _ := planServiceFixture(t)
	principal := Principal{UserID: uuid.New(), Role: RoleUser}
	repo.run.UserID = principal.UserID
	repo.run.Status = RunCompleted
	seedPlanForRun(plans, repo.run.ID, principal.UserID, PlanExecuting)
	cancelled, err := service.Cancel(context.Background(), principal, repo.run.ID)
	if err != nil || cancelled.Status != PlanCancelled {
		t.Fatalf("terminal run cancel = %#v, %v", cancelled, err)
	}
}

func TestPlanServiceForbiddenQueries(t *testing.T) {
	service, _, repo, _ := planServiceFixture(t)
	if _, err := service.ListVersions(context.Background(), Principal{}, repo.run.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous versions error = %v", err)
	}
	if _, err := service.Pause(context.Background(), Principal{}, repo.run.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous pause error = %v", err)
	}
	if _, err := service.Resume(context.Background(), Principal{}, repo.run.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous resume error = %v", err)
	}
	if _, err := service.Cancel(context.Background(), Principal{}, repo.run.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous cancel error = %v", err)
	}
}

func TestPlanServiceReviseCreatesNewVersion(t *testing.T) {
	service, plans, repo, queue := planServiceFixture(t)
	principal := Principal{UserID: uuid.New(), Role: RoleUser}
	repo.run.UserID = principal.UserID
	original := seedPlanForRun(plans, repo.run.ID, principal.UserID, PlanPaused)
	original.ConfigSnapshot = map[string]any{"agent": map[string]any{"name": "原快照"}}
	plans.seedPlan(original)
	draft := PlanDraft{Goal: "修订目标", Steps: []PlanDraftStep{
		{StepNo: 1, Title: "新步骤", Description: "说明", DependsOn: []int{}},
	}}
	if _, err := service.Revise(context.Background(), principal, repo.run.ID, PlanDraft{Goal: "", Steps: []PlanDraftStep{{StepNo: 1, Title: "s"}}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid draft error = %v", err)
	}
	revised, err := service.Revise(context.Background(), principal, repo.run.ID, draft)
	if err != nil || revised.Version != 2 || revised.ParentPlanID == nil || *revised.ParentPlanID != original.ID {
		t.Fatalf("Revise() = %#v, %v", revised, err)
	}
	if revised.ConfigSnapshot["agent"] == nil {
		t.Fatalf("revised plan did not preserve snapshot: %#v", revised.ConfigSnapshot)
	}
	if len(revised.Steps) != 1 || revised.Steps[0].Title != "新步骤" {
		t.Fatalf("revised steps = %#v", revised.Steps)
	}
	queue.mu.Lock()
	enqueued := queue.enqueued
	queue.mu.Unlock()
	if enqueued != 1 {
		t.Fatalf("revise enqueue count = %d", enqueued)
	}
	if _, err := service.Revise(context.Background(), principal, repo.run.ID, PlanDraft{Goal: "g", Steps: []PlanDraftStep{{StepNo: 1, Title: "s"}}}); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("non-paused revise error = %v", err)
	}
}
