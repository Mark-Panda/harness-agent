package biz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeJobQueue 是 JobQueue 的内存实现，用于 Worker 单元测试。
type fakeJobQueue struct {
	mu        sync.Mutex
	jobs      map[uuid.UUID]Job
	statuses  map[uuid.UUID]string
	claimed   []uuid.UUID
	completed []uuid.UUID
	failed    []uuid.UUID
}

func newFakeJobQueue() *fakeJobQueue {
	return &fakeJobQueue{jobs: map[uuid.UUID]Job{}, statuses: map[uuid.UUID]string{}}
}

func (f *fakeJobQueue) EnqueueRun(context.Context, uuid.UUID, uuid.UUID, time.Time) error { return nil }

func (f *fakeJobQueue) Claim(_ context.Context, _, _ string, _ time.Duration, _ time.Time) (Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, job := range f.jobs {
		if f.statuses[id] == "pending" {
			job.FencingToken++
			f.jobs[id] = job
			f.statuses[id] = "claimed"
			f.claimed = append(f.claimed, id)
			return job, true, nil
		}
	}
	return Job{}, false, nil
}

func (f *fakeJobQueue) Renew(_ context.Context, jobID uuid.UUID, _ string, token int64, _ time.Duration, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok || job.FencingToken != token {
		return ErrResourceConflict
	}
	return nil
}

func (f *fakeJobQueue) Complete(_ context.Context, jobID uuid.UUID, _ string, token int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok || job.FencingToken != token {
		return ErrResourceConflict
	}
	f.statuses[jobID] = "completed"
	f.completed = append(f.completed, jobID)
	return nil
}

func (f *fakeJobQueue) Fail(_ context.Context, jobID uuid.UUID, _ string, token int64, _ time.Time, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok || job.FencingToken != token {
		return ErrResourceConflict
	}
	job.Attempts++
	f.jobs[jobID] = job
	f.statuses[jobID] = "failed"
	f.failed = append(f.failed, jobID)
	return nil
}

func (f *fakeJobQueue) Release(_ context.Context, _ uuid.UUID, _ string, _ int64, _ time.Time, _ time.Time) error {
	return nil
}

// fakeSkillSynthesisSettingsStore 同时实现设置读取与用户设置仓储，供 Worker、维护和提炼测试复用。
type fakeSkillSynthesisSettingsStore struct {
	items          map[uuid.UUID]UserSkillSynthesisSettings
	defaultEnabled bool
	getErr         error
	upsertErr      error
	listErr        error
	enabledQueries [][]uuid.UUID
}

func (f *fakeSkillSynthesisSettingsStore) GetUserSkillSynthesisSettings(_ context.Context, userID uuid.UUID) (UserSkillSynthesisSettings, error) {
	if f.getErr != nil {
		return UserSkillSynthesisSettings{}, f.getErr
	}
	if settings, ok := f.items[userID]; ok {
		return settings, nil
	}
	return UserSkillSynthesisSettings{UserID: userID, Enabled: f.defaultEnabled}, nil
}

func (f *fakeSkillSynthesisSettingsStore) UpsertUserSkillSynthesisSettings(_ context.Context, settings UserSkillSynthesisSettings, _ OutboxEvent) (UserSkillSynthesisSettings, error) {
	if f.upsertErr != nil {
		return UserSkillSynthesisSettings{}, f.upsertErr
	}
	if f.items == nil {
		f.items = make(map[uuid.UUID]UserSkillSynthesisSettings)
	}
	now := time.Now().UTC()
	if previous, ok := f.items[settings.UserID]; ok {
		settings.CreatedAt = previous.CreatedAt
	} else {
		settings.CreatedAt = now
	}
	settings.UpdatedAt = now
	f.items[settings.UserID] = settings
	return settings, nil
}

func (f *fakeSkillSynthesisSettingsStore) ListEnabledUserIDs(_ context.Context, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	f.enabledQueries = append(f.enabledQueries, append([]uuid.UUID(nil), userIDs...))
	if f.listErr != nil {
		return nil, f.listErr
	}
	result := make([]uuid.UUID, 0, len(userIDs))
	for _, userID := range userIDs {
		settings, ok := f.items[userID]
		if (ok && settings.Enabled) || (!ok && f.defaultEnabled) {
			result = append(result, userID)
		}
	}
	return result, nil
}

// fakeSkillSessionReader 是 SkillSynthesisSessionReader 的桩实现。
type fakeSkillSessionReader struct {
	sessions          []Session
	messages          map[uuid.UUID][]Message
	listSessionsCalls int
	listMessagesCalls int
}

func (f *fakeSkillSessionReader) ListSessionsActiveSince(_ context.Context, _ uuid.UUID, _ time.Time, _ int) ([]Session, error) {
	f.listSessionsCalls++
	return f.sessions, nil
}

func (f *fakeSkillSessionReader) ListRecentMessages(_ context.Context, _ uuid.UUID, sessionID uuid.UUID, _ int) ([]Message, error) {
	f.listMessagesCalls++
	if f.messages == nil {
		return nil, nil
	}
	// 返回倒序，Worker 会反转。
	msgs := f.messages[sessionID]
	reversed := make([]Message, len(msgs))
	for i, m := range msgs {
		reversed[len(msgs)-1-i] = m
	}
	return reversed, nil
}

// fakeSkillExtractor 是 SkillExtractor 的桩实现。
type fakeSkillExtractor struct {
	candidate SkillCandidate
	err       error
	calls     int
}

func (f *fakeSkillExtractor) Extract(context.Context, uuid.UUID, []Session, map[uuid.UUID][]Message) (SkillCandidate, error) {
	f.calls++
	return f.candidate, f.err
}

func makeSkillSynthesisJob(userID uuid.UUID) Job {
	return Job{
		ID: uuid.New(), UserID: &userID, Type: SkillSynthesisJobType,
		Attempts: 1, FencingToken: 0,
		Payload: map[string]any{"user_id": userID.String(), "date": "2026-11-14"},
	}
}

func newSkillWorker(t *testing.T, jobs JobQueue, sessions SkillSynthesisSessionReader, skills *UserSkillService, extractor SkillExtractor, settings ...UserSkillSynthesisSettingsReader) *SkillSynthesisWorker {
	t.Helper()
	var reader UserSkillSynthesisSettingsReader = &fakeSkillSynthesisSettingsStore{defaultEnabled: true}
	if len(settings) > 0 {
		reader = settings[0]
	}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: reader, Sessions: sessions, UserSkills: skills, Extractor: extractor,
	}, SkillSynthesisConfig{MinConfidence: 0.5, MaxSessionsPerUser: 10, MaxMessagesPerSession: 20})
	if err != nil {
		t.Fatalf("NewSkillSynthesisWorker() error = %v", err)
	}
	return worker
}

func TestSkillSynthesisWorkerCreatesSkillWhenWorthwhile(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	reader := &fakeSkillSessionReader{
		sessions: []Session{{ID: sessionID, UserID: userID}},
		messages: map[uuid.UUID][]Message{
			sessionID: {
				{Role: MessageUser, Content: "帮我做周报"},
				{Role: MessageAssistant, Content: "好的"},
			},
		},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{
		ShouldCreate: true, Confidence: 0.9, Name: "weekly-report",
		PromptTemplate: "总结 {{content}}",
	}}

	worker := newSkillWorker(t, jobs, reader, skills, extractor)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("expected 1 created skill, got %d", len(repo.skills))
	}
	if len(jobs.completed) != 1 {
		t.Fatalf("expected job completed, got completed=%d failed=%d", len(jobs.completed), len(jobs.failed))
	}
}

func TestSkillSynthesisWorkerSkipsWhenNotWorthwhile(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	reader := &fakeSkillSessionReader{
		sessions: []Session{{ID: sessionID, UserID: userID}},
		messages: map[uuid.UUID][]Message{sessionID: {
			{Role: MessageUser, Content: "hi"}, {Role: MessageAssistant, Content: "hello"},
		}},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{ShouldCreate: false}}

	worker := newSkillWorker(t, jobs, reader, skills, extractor)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(repo.skills) != 0 {
		t.Fatalf("expected no skill created, got %d", len(repo.skills))
	}
	if len(jobs.completed) != 1 {
		t.Fatalf("expected job completed, got completed=%d", len(jobs.completed))
	}
}

func TestSkillSynthesisWorkerCompletesWhenNoSessions(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	reader := &fakeSkillSessionReader{}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{ShouldCreate: true}}

	worker := newSkillWorker(t, jobs, reader, skills, extractor)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(jobs.completed) != 1 {
		t.Fatalf("expected job completed when no sessions, got completed=%d failed=%d", len(jobs.completed), len(jobs.failed))
	}
}

func TestSkillSynthesisWorkerSkipsBelowConfidenceThreshold(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	reader := &fakeSkillSessionReader{
		sessions: []Session{{ID: sessionID, UserID: userID}},
		messages: map[uuid.UUID][]Message{sessionID: {
			{Role: MessageUser, Content: "x"}, {Role: MessageAssistant, Content: "y"},
		}},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	// confidence 0.3 < threshold 0.5
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{ShouldCreate: true, Confidence: 0.3, Name: "x", PromptTemplate: "p"}}

	worker := newSkillWorker(t, jobs, reader, skills, extractor)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(repo.skills) != 0 {
		t.Fatalf("expected no skill below threshold, got %d", len(repo.skills))
	}
}

func TestSkillSynthesisWorkerSkipsDisabledUserBeforeReadingSessions(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	reader := &fakeSkillSessionReader{sessions: []Session{{ID: uuid.New(), UserID: userID}}}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{ShouldCreate: true}}
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		userID: {UserID: userID, Enabled: false},
	}}

	worker := newSkillWorker(t, jobs, reader, skills, extractor, settings)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if reader.listSessionsCalls != 0 || reader.listMessagesCalls != 0 || extractor.calls != 0 {
		t.Fatalf("disabled user triggered reads or extraction: sessions=%d messages=%d extractor=%d", reader.listSessionsCalls, reader.listMessagesCalls, extractor.calls)
	}
	if len(jobs.completed) != 1 || len(jobs.failed) != 0 {
		t.Fatalf("expected disabled job to complete, got completed=%d failed=%d", len(jobs.completed), len(jobs.failed))
	}
}

func TestSkillSynthesisWorkerFailsWhenSettingsReadFails(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	reader := &fakeSkillSessionReader{}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{}
	settings := &fakeSkillSynthesisSettingsStore{getErr: errors.New("settings unavailable")}

	worker := newSkillWorker(t, jobs, reader, skills, extractor, settings)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(jobs.failed) != 1 || len(jobs.completed) != 0 {
		t.Fatalf("expected settings failure to be retryable, got completed=%d failed=%d", len(jobs.completed), len(jobs.failed))
	}
	if reader.listSessionsCalls != 0 || extractor.calls != 0 {
		t.Fatal("settings failure must stop processing before reading sessions")
	}
}

func TestSkillSynthesisWorkerCompletesWhenExtractionUnavailable(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	reader := &fakeSkillSessionReader{
		sessions: []Session{{ID: sessionID, UserID: userID}},
		messages: map[uuid.UUID][]Message{sessionID: {
			{Role: MessageUser, Content: "x"}, {Role: MessageAssistant, Content: "y"},
		}},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{err: ErrUserSkillSynthesisUnavailable}
	worker := newSkillWorker(t, jobs, reader, skills, extractor)
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(jobs.completed) != 1 || len(jobs.failed) != 0 || len(repo.skills) != 0 {
		t.Fatalf("permanent unavailable extraction must complete without a skill: completed=%d failed=%d skills=%d", len(jobs.completed), len(jobs.failed), len(repo.skills))
	}
}

type fakeSkillSynthesisUsers struct {
	items []User
	err   error
}

func (f *fakeSkillSynthesisUsers) CreateUser(context.Context, User, OutboxEvent) (User, error) {
	return User{}, errors.New("not implemented")
}
func (f *fakeSkillSynthesisUsers) GetUserByID(context.Context, uuid.UUID) (User, error) {
	return User{}, errors.New("not implemented")
}
func (f *fakeSkillSynthesisUsers) GetUserByNormalizedUsername(context.Context, string) (User, error) {
	return User{}, errors.New("not implemented")
}
func (f *fakeSkillSynthesisUsers) UpdateUser(context.Context, User, OutboxEvent) (User, error) {
	return User{}, errors.New("not implemented")
}
func (f *fakeSkillSynthesisUsers) UpdateUserRole(context.Context, uuid.UUID, Role, OutboxEvent) error {
	return errors.New("not implemented")
}
func (f *fakeSkillSynthesisUsers) ListUsers(context.Context, UserFilter, int, string) (Page[User], error) {
	if f.err != nil {
		return Page[User]{}, f.err
	}
	return Page[User]{Items: f.items}, nil
}

type fakeSkillSynthesisQueue struct {
	tasks []SkillSynthesisTask
	err   error
}

func (f *fakeSkillSynthesisQueue) EnqueueSkillSynthesis(_ context.Context, task SkillSynthesisTask, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.tasks = append(f.tasks, task)
	return nil
}

func TestSkillSynthesisMaintenanceQueuesOnlyActiveEnabledUsers(t *testing.T) {
	activeEnabled := uuid.New()
	activeDisabled := uuid.New()
	inactiveEnabled := uuid.New()
	users := &fakeSkillSynthesisUsers{items: []User{
		{ID: activeEnabled, Status: UserStatusActive},
		{ID: activeDisabled, Status: UserStatusActive},
		{ID: inactiveEnabled, Status: UserStatusDisabled},
	}}
	settings := &fakeSkillSynthesisSettingsStore{items: map[uuid.UUID]UserSkillSynthesisSettings{
		activeEnabled:   {UserID: activeEnabled, Enabled: true},
		activeDisabled:  {UserID: activeDisabled, Enabled: false},
		inactiveEnabled: {UserID: inactiveEnabled, Enabled: true},
	}}
	queue := &fakeSkillSynthesisQueue{}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	maintenance, err := NewSkillSynthesisMaintenance(users, settings, queue, SkillSynthesisConfig{}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(queue.tasks) != 1 || queue.tasks[0].UserID != activeEnabled || queue.tasks[0].Date != "2026-11-14" {
		t.Fatalf("unexpected queued tasks: %#v", queue.tasks)
	}
	if len(settings.enabledQueries) != 1 || len(settings.enabledQueries[0]) != 2 {
		t.Fatalf("expected one batch query for active users only, got %#v", settings.enabledQueries)
	}
}

func TestSkillSynthesisMaintenanceReturnsSettingsQueryError(t *testing.T) {
	userID := uuid.New()
	settings := &fakeSkillSynthesisSettingsStore{listErr: errors.New("query failed")}
	queue := &fakeSkillSynthesisQueue{}
	maintenance, err := NewSkillSynthesisMaintenance(
		&fakeSkillSynthesisUsers{items: []User{{ID: userID, Status: UserStatusActive}}},
		settings, queue, SkillSynthesisConfig{}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.RunOnce(context.Background()); !errors.Is(err, settings.listErr) {
		t.Fatalf("expected settings query error, got %v", err)
	}
	if len(queue.tasks) != 0 {
		t.Fatal("no task should be queued when enabled users cannot be loaded")
	}
}

func TestFilterEffectiveMessages(t *testing.T) {
	// ListRecentMessages 返回倒序（最新在前），filterEffectiveMessages 会反转为正序。
	messages := []Message{
		{Role: MessageAssistant, Content: "world"},
		{Role: MessageUser, Content: "hello"},
		{Role: MessageUser, Content: "   "},
		{Role: MessageSystem, Content: "sys"},
	}
	filtered := filterEffectiveMessages(messages)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 effective messages, got %d", len(filtered))
	}
	if filtered[0].Content != "hello" || filtered[1].Content != "world" {
		t.Fatalf("unexpected filtered messages: %#v", filtered)
	}
}

func TestDecodeSkillSynthesisTask(t *testing.T) {
	userID := uuid.New()
	job := Job{Payload: map[string]any{"user_id": userID.String(), "date": "2026-11-14"}}
	task, err := decodeSkillSynthesisTask(job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.UserID != userID || task.Date != "2026-11-14" {
		t.Fatalf("unexpected task: %#v", task)
	}
}

func TestSkillSynthesisConfigNormalize(t *testing.T) {
	cfg := SkillSynthesisConfig{}.normalize()
	if cfg.Interval != 24*time.Hour {
		t.Fatalf("expected default interval 24h, got %v", cfg.Interval)
	}
	if cfg.MinConfidence != 0.6 {
		t.Fatalf("expected default min confidence 0.6, got %f", cfg.MinConfidence)
	}
}
