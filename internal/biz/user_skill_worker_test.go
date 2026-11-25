package biz

import (
	"context"
	"errors"
	"sort"
	"strings"
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
	sessionsErr       error
	messagesErr       error
}

func (f *fakeSkillSessionReader) ListSessionsActiveSince(_ context.Context, _ uuid.UUID, _ time.Time, _ int) ([]Session, error) {
	f.listSessionsCalls++
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions, nil
}

func (f *fakeSkillSessionReader) ListRecentMessages(_ context.Context, _ uuid.UUID, sessionID uuid.UUID, _ int) ([]Message, error) {
	f.listMessagesCalls++
	if f.messagesErr != nil {
		return nil, f.messagesErr
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
	existing  []SkillDigest
	targets   []SkillSynthesisTarget
	byAgent   map[uuid.UUID]SkillCandidate
}

func (f *fakeSkillExtractor) Extract(_ context.Context, _ uuid.UUID, target SkillSynthesisTarget, _ []Session, _ map[uuid.UUID][]Message, existing []SkillDigest) (SkillCandidate, error) {
	f.calls++
	f.existing = existing
	f.targets = append(f.targets, target)
	if candidate, ok := f.byAgent[target.AgentID]; ok {
		return candidate, f.err
	}
	return f.candidate, f.err
}

// fakeSkillSynthesisAgents 是 SkillSynthesisAgentBinder 的内存桩，模拟原助手直接绑定与快照刷新。
type fakeSkillSynthesisAgents struct {
	agents        map[uuid.UUID]Agent
	attached      map[uuid.UUID]uuid.UUID
	attachedOrder []uuid.UUID
	refreshed     []uuid.UUID
	attachErr     error
	attachSkipped bool
	refreshErr    error
}

func newFakeSkillSynthesisAgents(agents ...Agent) *fakeSkillSynthesisAgents {
	store := &fakeSkillSynthesisAgents{agents: map[uuid.UUID]Agent{}, attached: map[uuid.UUID]uuid.UUID{}}
	for _, agent := range agents {
		store.agents[agent.ID] = agent
	}
	return store
}

func (f *fakeSkillSynthesisAgents) Get(_ context.Context, principal Principal, agentID uuid.UUID) (Agent, error) {
	agent, ok := f.agents[agentID]
	if !ok || agent.UserID != principal.UserID {
		return Agent{}, ErrResourceNotFound
	}
	return agent, nil
}

func (f *fakeSkillSynthesisAgents) AttachUserSkill(_ context.Context, principal Principal, agentID uuid.UUID, skill UserSkill) (bool, error) {
	if f.attachErr != nil {
		return false, f.attachErr
	}
	if f.attachSkipped {
		return false, nil
	}
	agent, ok := f.agents[agentID]
	if !ok || agent.UserID != principal.UserID {
		return false, ErrResourceNotFound
	}
	if !containsUUID(agent.UserSkillIDs, skill.ID) {
		agent.UserSkillIDs = append(agent.UserSkillIDs, skill.ID)
	}
	f.agents[agentID] = agent
	f.attached[skill.ID] = agentID
	f.attachedOrder = append(f.attachedOrder, agentID)
	return true, nil
}

func (f *fakeSkillSynthesisAgents) AttachUserSkillToAgentFamily(ctx context.Context, principal Principal, sourceAgentID uuid.UUID, skill UserSkill) (int, error) {
	source, ok := f.agents[sourceAgentID]
	if !ok || source.UserID != principal.UserID {
		return 0, ErrResourceNotFound
	}
	agentIDs := make([]uuid.UUID, 0)
	for _, agent := range f.agents {
		if agent.UserID == principal.UserID && agent.Name == source.Name && agent.Status == AgentPublished {
			agentIDs = append(agentIDs, agent.ID)
		}
	}
	if len(agentIDs) == 0 {
		agentIDs = append(agentIDs, sourceAgentID)
	}
	sort.Slice(agentIDs, func(i, j int) bool { return agentIDs[i].String() < agentIDs[j].String() })
	attached := 0
	for _, agentID := range agentIDs {
		ok, err := f.AttachUserSkill(ctx, principal, agentID, skill)
		if err != nil {
			return attached, err
		}
		if ok {
			attached++
		}
	}
	return attached, nil
}

func (f *fakeSkillSynthesisAgents) RefreshBoundAgentsForUserSkill(_ context.Context, principal Principal, skill UserSkill) error {
	if f.refreshErr != nil {
		return f.refreshErr
	}
	for _, agent := range f.agents {
		if agent.UserID == principal.UserID && containsUUID(agent.UserSkillIDs, skill.ID) {
			f.refreshed = append(f.refreshed, agent.ID)
		}
	}
	return nil
}

// TestSkillSynthesisWorkerSynthesizesPerAgentAndMounts 验证按助手分组提炼并自动发布挂载。
func TestSkillSynthesisWorkerSynthesizesPerAgentAndMounts(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	agentA := Agent{ID: uuid.New(), UserID: userID, Name: "report-agent", Description: "整理周报", Version: 1, Status: AgentPublished}
	agentB := Agent{ID: uuid.New(), UserID: userID, Name: "ticket-agent", Description: "排查工单", Version: 2, Status: AgentPublished}
	firstID, secondID := uuid.New(), uuid.New()
	first := Session{ID: firstID, UserID: userID, AgentID: &agentA.ID, Title: "周报会话", Status: SessionActive}
	second := Session{ID: secondID, UserID: userID, AgentID: &agentB.ID, Title: "工单会话", Status: SessionActive}
	now := time.Now().UTC()
	firstMessages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: now.Add(-2 * time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: now.Add(-time.Minute)},
	}
	secondMessages := []Message{
		{Role: MessageUser, Content: "这个工单的锁屏指令没有生效，请排查", CreatedAt: now.Add(-time.Minute)},
		{Role: MessageAssistant, Content: "我先核对设备状态与指令下发记录", CreatedAt: now},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{first, second},
		messages: map[uuid.UUID][]Message{firstID: firstMessages, secondID: secondMessages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	ticketCandidate := validWorkerCandidate(second, secondMessages, "ticket-triage")
	ticketCandidate.Description = "把设备指令未生效的排查过程整理成可复用步骤，适用于同类工单反复出现时。"
	ticketCandidate.PromptTemplate = "# ticket-triage\n\n## 何时使用\n设备指令未生效时使用。\n\n## 步骤\n1. 核对设备在线状态。\n2. 检查指令下发记录。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。"
	extractor := &fakeSkillExtractor{byAgent: map[uuid.UUID]SkillCandidate{
		agentA.ID: validWorkerCandidate(first, firstMessages, "weekly-report"),
		agentB.ID: ticketCandidate,
	}}
	agents := newFakeSkillSynthesisAgents(agentA, agentB)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: extractor, Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 2 {
		t.Fatalf("expected one extraction per agent, got %d", extractor.calls)
	}
	if len(repo.skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(repo.skills))
	}
	for _, skill := range repo.skills {
		if skill.Status != UserSkillPublished {
			t.Fatalf("auto skill was not published: %#v", skill)
		}
	}
	if len(agents.attached) != 2 {
		t.Fatalf("expected both agents to receive a skill, got %#v", agents.attached)
	}
	for _, agentID := range []uuid.UUID{agentA.ID, agentB.ID} {
		if len(agents.agents[agentID].UserSkillIDs) != 1 {
			t.Fatalf("agent %s bindings = %#v", agents.agents[agentID].Name, agents.agents[agentID].UserSkillIDs)
		}
	}
	if agents.agents[agentA.ID].Version != 1 || agents.agents[agentB.ID].Version != 2 {
		t.Fatalf("agent version changed: a=%d b=%d", agents.agents[agentA.ID].Version, agents.agents[agentB.ID].Version)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "created" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerAttachesToAllPublishedVersions 验证同名助手的全部已发布版本都会获得技能绑定。
func TestSkillSynthesisWorkerAttachesToAllPublishedVersions(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	agentV1 := Agent{ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报", Version: 1, Status: AgentPublished}
	agentV2 := Agent{ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报 v2", Version: 2, Status: AgentPublished}
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &agentV1.ID, Title: "周报会话", Status: SessionActive}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	repo := newFakeUserSkillRepository()
	agents := newFakeSkillSynthesisAgents(agentV1, agentV2)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: &fakeSkillSessionReader{
			sessions: []Session{session},
			messages: map[uuid.UUID][]Message{sessionID: messages},
		},
		UserSkills: mustUserSkillService(t, repo), Extractor: &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(repo.skills))
	}
	var skillID uuid.UUID
	for id := range repo.skills {
		skillID = id
	}
	if len(agents.attachedOrder) != 2 {
		t.Fatalf("expected both versions to be attached, got %#v", agents.attachedOrder)
	}
	for _, agent := range []Agent{agentV1, agentV2} {
		stored := agents.agents[agent.ID]
		if stored.Version != agent.Version || !containsUUID(stored.UserSkillIDs, skillID) {
			t.Fatalf("agent %s was not attached: %#v", agent.Name, stored)
		}
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "created" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerOptimizesMountedSkill 验证重复候选原地优化已挂载技能并刷新助手版本。
func TestSkillSynthesisWorkerOptimizesMountedSkill(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	agent := Agent{
		ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报", Version: 1, Status: AgentPublished,
	}
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &agent.ID, Title: "周报会话"}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报，并补充风险项", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报和风险项整理结果", CreatedAt: time.Now().UTC()},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 1,
		Description: "把重复出现的周报整理过程沉淀为可复用步骤，适用于每周固定汇报。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n每周需要汇报进展时使用。\n\n## 步骤\n1. 汇总进展。\n\n" +
			"## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Status: UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{sessionID}, Confidence: 0.7,
	}
	repo.skills[existing.ID] = existing
	agent.UserSkillIDs = []uuid.UUID{existing.ID}
	agent.ConfigSnapshot = map[string]any{"user_skills": []map[string]any{{
		"id": existing.ID.String(), "name": existing.Name, "description": existing.Description,
		"prompt_template": existing.PromptTemplate, "parameters": map[string]any{},
	}}}
	agents := newFakeSkillSynthesisAgents(agent)
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	candidate.PromptTemplate = strings.Replace(candidate.PromptTemplate, "1. 收集证据。", "1. 收集证据。\n2. 汇总风险项。", 1)
	runs := &fakeSkillSynthesisRuns{}
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	userSkills := mustUserSkillService(t, repo)
	userSkills.WithBundles(bundles)
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: userSkills, Extractor: &fakeSkillExtractor{candidate: candidate},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("optimization created a new skill: %d", len(repo.skills))
	}
	optimized := repo.skills[existing.ID]
	if optimized.PromptTemplate == existing.PromptTemplate || !strings.Contains(optimized.PromptTemplate, "汇总风险项") {
		t.Fatalf("skill was not optimized in place: %#v", optimized)
	}
	if optimized.Status != UserSkillPublished || optimized.Origin != UserSkillOriginAuto {
		t.Fatalf("optimized skill lost lifecycle state: %#v", optimized)
	}
	if optimized.Confidence < existing.Confidence {
		t.Fatalf("optimized confidence regressed: %v", optimized.Confidence)
	}
	if optimized.Bundle.Format != SkillBundleFormat || len(bundles.files) != 1 ||
		!strings.Contains(string(bundles.files[0].Content), "汇总风险项") {
		t.Fatalf("optimized bundle was not rebuilt: %#v files=%d", optimized.Bundle, len(bundles.files))
	}
	if agents.agents[agent.ID].Version != 1 || !containsUUID(agents.agents[agent.ID].UserSkillIDs, existing.ID) {
		t.Fatalf("agent was not refreshed in place: %#v", agents.agents[agent.ID])
	}
	if len(agents.refreshed) != 1 || agents.refreshed[0] != agent.ID {
		t.Fatalf("bound agent snapshot was not refreshed: %#v", agents.refreshed)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "optimized" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerSkipsRemountForIdenticalMountedSkill 验证内容一致且已挂载时不重复写入或创建版本。
func TestSkillSynthesisWorkerSkipsRemountForIdenticalMountedSkill(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, Title: "周报会话", Status: SessionActive}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	agent := Agent{
		ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报",
		Version: 1, Status: AgentPublished,
	}
	session.AgentID = &agent.ID
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: candidate.Name, Version: 1,
		Description: candidate.Description, PromptTemplate: candidate.PromptTemplate,
		Parameters: map[string]any{}, Status: UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{sessionID}, Confidence: 0.7,
	}
	bundleInfo, _, err := buildAutoSkillBundle(existing)
	if err != nil {
		t.Fatal(err)
	}
	existing.Bundle = bundleInfo
	repo := newFakeUserSkillRepository()
	repo.skills[existing.ID] = existing
	agent.UserSkillIDs = []uuid.UUID{existing.ID}
	agent.ConfigSnapshot = map[string]any{"user_skills": []map[string]any{{
		"id": existing.ID.String(), "name": existing.Name, "description": existing.Description,
		"prompt_template": existing.PromptTemplate, "parameters": existing.Parameters,
	}}}
	agents := newFakeSkillSynthesisAgents(agent)
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	userSkills := mustUserSkillService(t, repo)
	userSkills.WithBundles(bundles)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: userSkills, Extractor: &fakeSkillExtractor{candidate: candidate},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := repo.skills[existing.ID]
	if current.Confidence != existing.Confidence || current.PromptTemplate != existing.PromptTemplate {
		t.Fatalf("identical skill was rewritten: %#v", current)
	}
	if len(bundles.files) != 0 {
		t.Fatalf("identical skill bundle was rewritten: %d files", len(bundles.files))
	}
	if agents.attached[existing.ID] != agent.ID || agents.agents[agent.ID].Version != 1 {
		t.Fatalf("identical skill was not attached in place: %#v", agents.agents[agent.ID])
	}
	if len(agents.refreshed) != 1 || agents.refreshed[0] != agent.ID {
		t.Fatalf("bound agent snapshot was not refreshed: %#v", agents.refreshed)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" ||
		!strings.Contains(runs.finished[0].Detail, "内容一致") {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerMountsIdenticalSkillWithoutRewrite 验证内容一致但尚未挂载时只补挂载、不重写技能。
func TestSkillSynthesisWorkerMountsIdenticalSkillWithoutRewrite(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, Title: "周报会话", Status: SessionActive}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	agent := Agent{
		ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报",
		Version: 1, Status: AgentPublished,
	}
	session.AgentID = &agent.ID
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: candidate.Name, Version: 1,
		Description: candidate.Description, PromptTemplate: candidate.PromptTemplate,
		Parameters: map[string]any{}, Status: UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{sessionID}, Confidence: 0.7,
	}
	bundleInfo, _, err := buildAutoSkillBundle(existing)
	if err != nil {
		t.Fatal(err)
	}
	existing.Bundle = bundleInfo
	repo := newFakeUserSkillRepository()
	repo.skills[existing.ID] = existing
	agents := newFakeSkillSynthesisAgents(agent)
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	userSkills := mustUserSkillService(t, repo)
	userSkills.WithBundles(bundles)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: userSkills, Extractor: &fakeSkillExtractor{candidate: candidate},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := repo.skills[existing.ID]
	if current.Confidence != existing.Confidence || current.PromptTemplate != existing.PromptTemplate {
		t.Fatalf("identical skill was rewritten: %#v", current)
	}
	if len(bundles.files) != 0 {
		t.Fatalf("identical skill bundle was rewritten: %d files", len(bundles.files))
	}
	if !containsUUID(agents.agents[agent.ID].UserSkillIDs, existing.ID) || agents.agents[agent.ID].Version != 1 {
		t.Fatalf("identical skill was not attached in place: %#v", agents.agents[agent.ID])
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" ||
		!strings.Contains(runs.finished[0].Detail, "已绑定") {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerRebuildsStaleBundle 验证正文一致但技能包摘要过期时会重新生成技能包。
func TestSkillSynthesisWorkerRebuildsStaleBundle(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, Title: "周报会话", Status: SessionActive}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	agent := Agent{
		ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报",
		Version: 1, Status: AgentPublished,
	}
	session.AgentID = &agent.ID
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: candidate.Name, Version: 1,
		Description: candidate.Description, PromptTemplate: candidate.PromptTemplate,
		Parameters: map[string]any{}, Status: UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{sessionID}, Confidence: 0.7,
		Bundle: SkillBundleInfo{Format: SkillBundleFormat, Entrypoint: "SKILL.md", FileCount: 1, Digest: "stale"},
	}
	repo := newFakeUserSkillRepository()
	repo.skills[existing.ID] = existing
	agent.UserSkillIDs = []uuid.UUID{existing.ID}
	agent.ConfigSnapshot = map[string]any{"user_skills": []map[string]any{{
		"id": existing.ID.String(), "name": existing.Name, "description": existing.Description,
		"prompt_template": existing.PromptTemplate, "parameters": existing.Parameters,
	}}}
	agents := newFakeSkillSynthesisAgents(agent)
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	userSkills := mustUserSkillService(t, repo)
	userSkills.WithBundles(bundles)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: userSkills, Extractor: &fakeSkillExtractor{candidate: candidate},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bundles.files) != 1 || !strings.Contains(string(bundles.files[0].Content), "## 步骤") {
		t.Fatalf("stale bundle was not rebuilt: %#v", bundles.files)
	}
	current := repo.skills[existing.ID]
	if current.Bundle.Digest == "" || current.Bundle.Digest != bundles.info.Digest {
		t.Fatalf("bundle digest was not refreshed: %#v", current.Bundle)
	}
	if agents.agents[agent.ID].Version != 1 || !containsUUID(agents.agents[agent.ID].UserSkillIDs, existing.ID) {
		t.Fatalf("bundle repair changed the agent version: %#v", agents.agents[agent.ID])
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "optimized" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerSkipsUnrelatedSameNameSkill 验证同名但未挂载且来源不同的技能不会被覆盖。
func TestSkillSynthesisWorkerSkipsUnrelatedSameNameSkill(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"

	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, Title: "周报会话", Status: SessionActive}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	agent := Agent{
		ID: uuid.New(), UserID: userID, Name: "weekly-agent", Description: "整理周报",
		Version: 1, Status: AgentPublished,
	}
	session.AgentID = &agent.ID
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 1,
		Description:    "处理发票报销与凭证归档的固定流程，适用于财务报销场景。",
		PromptTemplate: "# weekly-report\n\n## 何时使用\n处理发票报销时使用。\n\n## 步骤\n1. 核对发票信息。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Parameters:     map[string]any{}, Status: UserSkillPublished, Origin: UserSkillOriginAuto,
		SourceSessionIDs: []uuid.UUID{uuid.New()}, Confidence: 0.8,
	}
	repo := newFakeUserSkillRepository()
	repo.skills[existing.ID] = existing
	agents := newFakeSkillSynthesisAgents(agent)
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	bundles := &fakeUserSkillBundleRepository{repo: repo}
	userSkills := mustUserSkillService(t, repo)
	userSkills.WithBundles(bundles)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: userSkills, Extractor: &fakeSkillExtractor{candidate: candidate},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := repo.skills[existing.ID]
	if current.PromptTemplate != existing.PromptTemplate || current.Confidence != existing.Confidence {
		t.Fatalf("unrelated same-name skill was overwritten: %#v", current)
	}
	if len(bundles.files) != 0 {
		t.Fatalf("unrelated skill bundle was rewritten: %d files", len(bundles.files))
	}
	if len(agents.agents[agent.ID].UserSkillIDs) != 0 {
		t.Fatalf("unrelated skill was attached: %#v", agents.agents[agent.ID].UserSkillIDs)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" ||
		!strings.Contains(runs.finished[0].Detail, "未自动覆盖") {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerDoesNotOptimizeManualOrDisabledSkill 验证手动与停用技能不会被自动改写。
func TestSkillSynthesisWorkerDoesNotOptimizeManualOrDisabledSkill(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin UserSkillOrigin
		status UserSkillStatus
	}{
		{"manual", UserSkillOriginManual, UserSkillPublished},
		{"disabled", UserSkillOriginAuto, UserSkillDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := uuid.New()
			jobs := newFakeJobQueue()
			job := makeSkillSynthesisJob(userID)
			jobs.jobs[job.ID] = job
			jobs.statuses[job.ID] = "pending"
			sessionID := uuid.New()
			session := Session{ID: sessionID, UserID: userID, Title: "周报会话"}
			messages := []Message{
				{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
				{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
			}
			reader := &fakeSkillSessionReader{
				sessions: []Session{session},
				messages: map[uuid.UUID][]Message{sessionID: messages},
			}
			repo := newFakeUserSkillRepository()
			existing := UserSkill{
				ID: uuid.New(), UserID: userID, Name: "weekly-report", Version: 1,
				Description: "已有技能描述", PromptTemplate: "已有正文",
				Status: tc.status, Origin: tc.origin,
			}
			repo.skills[existing.ID] = existing
			runs := &fakeSkillSynthesisRuns{}
			worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
				Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
				Sessions: reader, UserSkills: mustUserSkillService(t, repo),
				Extractor: &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")},
				Runs:      runs,
			}, SkillSynthesisConfig{MinConfidence: 0.5})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.ProcessOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repo.skills) != 1 {
				t.Fatalf("skill count changed: %d", len(repo.skills))
			}
			if repo.skills[existing.ID].PromptTemplate != "已有正文" || repo.skills[existing.ID].Status != tc.status {
				t.Fatalf("skill was modified: %#v", repo.skills[existing.ID])
			}
			if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" {
				t.Fatalf("run outcome = %#v", runs.finished)
			}
		})
	}
}

// TestSkillSynthesisWorkerReportsAttachFailure 验证绑定失败不影响技能发布，并把原因写入执行记录。
func TestSkillSynthesisWorkerReportsAttachFailure(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	agent := Agent{ID: uuid.New(), UserID: userID, Name: "weekly-agent", Version: 1, Status: AgentPublished}
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &agent.ID, Title: "周报会话"}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	repo := newFakeUserSkillRepository()
	agents := newFakeSkillSynthesisAgents(agent)
	agents.attachErr = errors.New("agent unavailable")
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: &fakeSkillSessionReader{
			sessions: []Session{session},
			messages: map[uuid.UUID][]Message{sessionID: messages},
		},
		UserSkills: mustUserSkillService(t, repo), Extractor: &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 || repo.skills[firstSkillID(repo)].Status != UserSkillPublished {
		t.Fatalf("skill should remain published after attach failure: %#v", repo.skills)
	}
	if len(agents.agents[agent.ID].UserSkillIDs) != 0 {
		t.Fatalf("failed attach changed agent bindings: %#v", agents.agents[agent.ID].UserSkillIDs)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "created" ||
		!strings.Contains(runs.finished[0].Detail, "自动挂载失败") {
		t.Fatalf("run detail = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerReportsRemovedVersionSkip 验证按版本移除后的挂载跳过会如实写入执行记录。
func TestSkillSynthesisWorkerReportsRemovedVersionSkip(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	agent := Agent{ID: uuid.New(), UserID: userID, Name: "weekly-agent", Version: 1, Status: AgentPublished}
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &agent.ID, Title: "周报会话"}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	repo := newFakeUserSkillRepository()
	agents := newFakeSkillSynthesisAgents(agent)
	agents.attachSkipped = true
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: &fakeSkillSessionReader{
			sessions: []Session{session},
			messages: map[uuid.UUID][]Message{sessionID: messages},
		},
		UserSkills: mustUserSkillService(t, repo), Extractor: &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 || repo.skills[firstSkillID(repo)].Status != UserSkillPublished {
		t.Fatalf("skill should remain published after skipped attach: %#v", repo.skills)
	}
	if len(agents.agents[agent.ID].UserSkillIDs) != 0 {
		t.Fatalf("skipped attach changed agent bindings: %#v", agents.agents[agent.ID].UserSkillIDs)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "created" ||
		!strings.Contains(runs.finished[0].Detail, "已按版本移除该技能") {
		t.Fatalf("run detail = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerReportsBoundAgentRefreshFailure 验证其他助手快照刷新失败只记录说明。
func TestSkillSynthesisWorkerReportsBoundAgentRefreshFailure(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	agent := Agent{ID: uuid.New(), UserID: userID, Name: "weekly-agent", Version: 1, Status: AgentPublished}
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &agent.ID, Title: "周报会话"}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	repo := newFakeUserSkillRepository()
	agents := newFakeSkillSynthesisAgents(agent)
	agents.refreshErr = errors.New("snapshot refresh failed")
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: &fakeSkillSessionReader{
			sessions: []Session{session},
			messages: map[uuid.UUID][]Message{sessionID: messages},
		},
		UserSkills: mustUserSkillService(t, repo), Extractor: &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")},
		Agents: agents, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("skill was not created: %#v", repo.skills)
	}
	if len(agents.agents[agent.ID].UserSkillIDs) != 1 {
		t.Fatalf("agent binding was not created: %#v", agents.agents[agent.ID].UserSkillIDs)
	}
	if agents.agents[agent.ID].Version != 1 {
		t.Fatalf("agent version changed: %#v", agents.agents[agent.ID])
	}
	if len(runs.finished) != 1 || !strings.Contains(runs.finished[0].Detail, "同步已绑定助手失败") {
		t.Fatalf("run detail = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerSkipsMissingAgent 验证会话绑定的助手不存在时跳过且不调用模型。
func TestSkillSynthesisWorkerSkipsMissingAgent(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	missingAgentID := uuid.New()
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID, AgentID: &missingAgentID, Title: "失效会话"}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: time.Now().UTC().Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: time.Now().UTC()},
	}
	extractor := &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")}
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: &fakeSkillSessionReader{
			sessions: []Session{session},
			messages: map[uuid.UUID][]Message{sessionID: messages},
		},
		UserSkills: mustUserSkillService(t, newFakeUserSkillRepository()), Extractor: extractor,
		Agents: newFakeSkillSynthesisAgents(), Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 0 {
		t.Fatalf("missing agent should not trigger extraction: %d", extractor.calls)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "agent_missing" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
	if len(runs.advanced) != 1 {
		t.Fatalf("permanent missing agent should advance watermark: %#v", runs.advanced)
	}
}

// mustUserSkillService 构造测试用用户技能服务。
func mustUserSkillService(t *testing.T, repo UserSkillRepository) *UserSkillService {
	t.Helper()
	service, err := NewUserSkillService(repo)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// firstSkillID 返回假仓库中唯一的技能 ID。
func firstSkillID(repo *fakeUserSkillRepository) uuid.UUID {
	for id := range repo.skills {
		return id
	}
	return uuid.Nil
}

func makeSkillSynthesisJob(userID uuid.UUID) Job {
	return Job{
		ID: uuid.New(), UserID: &userID, Type: SkillSynthesisJobType,
		Attempts: 1, FencingToken: 0,
		Payload: map[string]any{"user_id": userID.String(), "date": "2026-11-14"},
	}
}

// validWorkerCandidate 构造能通过自动技能确定性校验的候选，证据取自测试会话消息。
func validWorkerCandidate(session Session, messages []Message, name string) SkillCandidate {
	title := session.Title
	if title == "" {
		title = "(无标题)"
	}
	return SkillCandidate{
		ShouldCreate: true, Confidence: 0.9, Name: name,
		Description: "把重复出现的排查过程整理成可复用步骤，适用于同类问题再次出现时。",
		WhenToUse:   "当同类问题再次出现并需要按固定步骤排查时使用。",
		PromptTemplate: "# " + name + "\n\n## 何时使用\n同类问题再次出现时使用。\n\n" +
			"## 步骤\n1. 收集证据。\n2. 输出结论。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
		Parameters: map[string]any{},
		Evidence: []SkillEvidence{
			{Session: title, Quote: messages[0].Content},
			{Session: title, Quote: messages[1].Content},
		},
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
	session := Session{ID: sessionID, UserID: userID}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报"},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果"},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "weekly-report")}

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

func TestSkillSynthesisWorkerRecordsLongDescriptionWarning(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	now := time.Now().UTC()
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: now.Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: now},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	candidate := validWorkerCandidate(session, messages, "long-description")
	candidate.Description = strings.Repeat("描", 600)
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("expected skill created, got %d", len(repo.skills))
	}
	if len(runs.finished) != 1 || !strings.Contains(runs.finished[0].Detail, "500") {
		t.Fatalf("run detail missing description warning: %#v", runs.finished)
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

// TestSkillSynthesisWorkerSkipsInvalidCandidate 验证不合规候选不会落库，但会记录原因并推进水位。
func TestSkillSynthesisWorkerSkipsInvalidCandidate(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	now := time.Now().UTC()
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报", CreatedAt: now.Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果", CreatedAt: now},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	candidate := validWorkerCandidate(session, messages, "weekly-report")
	candidate.Name = "Weekly_Report"
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 0 {
		t.Fatalf("invalid candidate created a skill: %d", len(repo.skills))
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "invalid_candidate" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
	if len(runs.advanced) != 1 {
		t.Fatalf("invalid candidate must advance watermark, got %#v", runs.advanced)
	}
}

// TestSkillSynthesisWorkerSkipsSimilarDuplicate 验证不同名称但同来源、正文相似的候选会被判重。
func TestSkillSynthesisWorkerSkipsSimilarDuplicate(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	messages := []Message{
		{Role: MessageUser, Content: "锁屏指令下发后设备没有生效，请帮我排查"},
		{Role: MessageAssistant, Content: "我先核对设备状态与指令记录"},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	existing := UserSkill{
		ID: uuid.New(), UserID: userID, Name: "tablet-lock-issue-investigation",
		Description:    "面向教育平板管控系统的锁屏/解锁异常排查工作流。",
		PromptTemplate: duplicateExistingPrompt, SourceSessionIDs: []uuid.UUID{sessionID},
		Status: UserSkillAutoGenerated,
	}
	repo.skills[existing.ID] = existing
	skills, _ := NewUserSkillService(repo)
	candidate := validWorkerCandidate(session, messages, "device-control-issue-forensics")
	dedupedPrompt := strings.NewReplacer(
		"{{problem_description}}", "设备管控异常",
		"{{device_ids}}", "设备列表",
		"{{time_range}}", "时间范围",
		"{{data_sources}}", "日志系统",
		"{{system_context}}", "业务上下文",
	).Replace(duplicateCandidatePrompt)
	candidate.PromptTemplate = "# device-control-issue-forensics\n\n## 何时使用\n同类设备管控异常再次出现时使用。\n\n## 步骤\n" +
		dedupedPrompt + "\n\n## 输出格式\n表格与结论。\n\n## 约束\n不得编造。"
	candidate.Description = "用于排查设备批量管控指令未生效或异常触发的问题。"
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("similar duplicate created a skill: %d", len(repo.skills))
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerDoesNotAdvanceWatermarkOnReadFailure 验证消息读取失败时水位不推进。
func TestSkillSynthesisWorkerDoesNotAdvanceWatermarkOnReadFailure(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	reader := &fakeSkillSessionReader{
		sessions:    []Session{{ID: sessionID, UserID: userID}},
		messagesErr: errors.New("database unavailable"),
	}
	skills, _ := NewUserSkillService(newFakeUserSkillRepository())
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runs.advanced) != 0 {
		t.Fatalf("watermark advanced after read failure: %#v", runs.advanced)
	}
	if len(runs.finished) != 1 || !strings.Contains(runs.finished[0].Detail, "读取失败") {
		t.Fatalf("run detail missing scan failure: %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerReportsMessageSampling 验证消息数量截断会写入采样说明，并继续推进水位。
func TestSkillSynthesisWorkerReportsMessageSampling(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	now := time.Now().UTC()
	messages := []Message{
		{Role: MessageUser, Content: "这是较早的一条工作记录，用来触发消息数量截断", CreatedAt: now.Add(-2 * time.Minute)},
		{Role: MessageUser, Content: "请继续整理本周工作周报的中间结果", CreatedAt: now.Add(-time.Minute)},
		{Role: MessageAssistant, Content: "好的，这是最终的本周工作周报整理结果", CreatedAt: now},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	candidate := validWorkerCandidate(session, messages[1:], "sampled-report")
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5, MaxMessagesPerSession: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runs.finished) != 1 || !strings.Contains(runs.finished[0].Detail, "采样扫描") {
		t.Fatalf("run detail missing sampling note: %#v", runs.finished)
	}
	if len(runs.advanced) != 1 {
		t.Fatalf("partial scan should still advance watermark, got %#v", runs.advanced)
	}
}

// TestSkillSynthesisWorkerReportsSessionSampling 验证会话数量截断会写入采样说明。
func TestSkillSynthesisWorkerReportsSessionSampling(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	firstID, secondID := uuid.New(), uuid.New()
	first := Session{ID: firstID, UserID: userID}
	messages := []Message{
		{Role: MessageUser, Content: "请帮我整理一份本周工作周报"},
		{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果"},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{first, {ID: secondID, UserID: userID}},
		messages: map[uuid.UUID][]Message{firstID: messages},
	}
	skills, _ := NewUserSkillService(newFakeUserSkillRepository())
	candidate := validWorkerCandidate(first, messages, "sampled-session")
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate}, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5, MaxSessionsPerUser: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runs.finished) != 1 || !strings.Contains(runs.finished[0].Detail, "采样扫描") {
		t.Fatalf("run detail missing session sampling note: %#v", runs.finished)
	}
	if runs.finished[0].SessionsScanned != 1 {
		t.Fatalf("sessions scanned = %d", runs.finished[0].SessionsScanned)
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

// fakeSkillSynthesisRuns 记录每日提炼执行记录写入。
type fakeSkillSynthesisRuns struct {
	started      []SkillSynthesisRun
	finished     []SkillSynthesisRunUpdate
	listed       []SkillSynthesisRun
	watermark    time.Time
	hasWatermark bool
	autoSkills   int
	advanced     []time.Time
}

// StartSkillSynthesisRun 记录当天记录开启。
func (f *fakeSkillSynthesisRuns) StartSkillSynthesisRun(_ context.Context, run SkillSynthesisRun) (SkillSynthesisRun, error) {
	run.ID = uuid.New()
	f.started = append(f.started, run)
	return run, nil
}

// FinishSkillSynthesisRun 记录最终结果。
func (f *fakeSkillSynthesisRuns) FinishSkillSynthesisRun(_ context.Context, _ uuid.UUID, _ uuid.UUID, update SkillSynthesisRunUpdate) (SkillSynthesisRun, error) {
	f.finished = append(f.finished, update)
	return SkillSynthesisRun{Status: update.Status}, nil
}

// ListSkillSynthesisRuns 返回预设记录。
func (f *fakeSkillSynthesisRuns) ListSkillSynthesisRuns(context.Context, uuid.UUID, int) ([]SkillSynthesisRun, error) {
	return f.listed, nil
}

// GetSkillSynthesisWatermark 返回预设水位。
func (f *fakeSkillSynthesisRuns) GetSkillSynthesisWatermark(context.Context, uuid.UUID) (time.Time, bool, error) {
	return f.watermark, f.hasWatermark, nil
}

// AdvanceSkillSynthesisWatermark 记录水位推进。
func (f *fakeSkillSynthesisRuns) AdvanceSkillSynthesisWatermark(_ context.Context, _ uuid.UUID, coveredAt time.Time) error {
	f.advanced = append(f.advanced, coveredAt)
	return nil
}

// CountAutoUserSkillsSince 返回预设的当天自动技能数。
func (f *fakeSkillSynthesisRuns) CountAutoUserSkillsSince(context.Context, uuid.UUID, time.Time) (int, error) {
	return f.autoSkills, nil
}

// TestSkillSynthesisWorkerRecordsDailyRun 验证每次提炼执行都会写入当天记录并带上结果原因。
func TestSkillSynthesisWorkerRecordsDailyRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate SkillCandidate
		status    SkillSynthesisRunStatus
		outcome   string
	}{
		{"created", SkillCandidate{ShouldCreate: true, Confidence: 0.9, Name: "weekly", PromptTemplate: "总结"}, SkillSynthesisRunCompleted, "created"},
		{"not worthwhile", SkillCandidate{ShouldCreate: false}, SkillSynthesisRunSkipped, "not_worthwhile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := uuid.New()
			jobs := newFakeJobQueue()
			job := makeSkillSynthesisJob(userID)
			jobs.jobs[job.ID] = job
			jobs.statuses[job.ID] = "pending"
			sessionID := uuid.New()
			session := Session{ID: sessionID, UserID: userID}
			messages := []Message{
				{Role: MessageUser, Content: "请帮我整理一份本周工作周报"},
				{Role: MessageAssistant, Content: "好的，这是本周工作周报的整理结果"},
			}
			reader := &fakeSkillSessionReader{
				sessions: []Session{session},
				messages: map[uuid.UUID][]Message{sessionID: messages},
			}
			skills, _ := NewUserSkillService(newFakeUserSkillRepository())
			runs := &fakeSkillSynthesisRuns{}
			candidate := tc.candidate
			if tc.outcome == "created" {
				candidate = validWorkerCandidate(session, messages, "weekly-report")
			}
			worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
				Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
				Sessions: reader, UserSkills: skills, Extractor: &fakeSkillExtractor{candidate: candidate},
				Runs: runs,
			}, SkillSynthesisConfig{MinConfidence: 0.5})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.ProcessOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(runs.started) != 1 || runs.started[0].Date != "2026-11-14" {
				t.Fatalf("started runs = %#v", runs.started)
			}
			if len(runs.finished) != 1 {
				t.Fatalf("finished runs = %#v", runs.finished)
			}
			update := runs.finished[0]
			if update.Status != tc.status || update.Outcome != tc.outcome {
				t.Fatalf("run outcome = %#v, want %s/%s", update, tc.status, tc.outcome)
			}
			if update.SessionsUsed != 1 || update.SessionsScanned != 1 || update.MessagesUsed != 2 {
				t.Fatalf("run counters = %#v", update)
			}
		})
	}
}

// TestSkillSynthesisWorkerSkipsDuplicateSkill 验证同名已有技能不会被重复创建。
func TestSkillSynthesisWorkerSkipsDuplicateSkill(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	messages := []Message{
		{Role: MessageUser, Content: "锁屏指令下发后设备没有生效，请帮我排查"},
		{Role: MessageAssistant, Content: "我先核对设备状态与指令记录"},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	existing := UserSkill{ID: uuid.New(), UserID: userID, Name: "tablet_lock_issue_investigation", Description: "已存在的锁屏排查技能", Status: UserSkillAutoGenerated}
	repo.skills[existing.ID] = existing
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "tablet-lock-issue-investigation")}
	runs := &fakeSkillSynthesisRuns{}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: extractor, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 1 {
		t.Fatalf("duplicate skill was created: %d", len(repo.skills))
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "duplicate_skill" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
	if len(extractor.existing) != 1 || extractor.existing[0].Name != existing.Name {
		t.Fatalf("existing skills were not passed to extractor: %#v", extractor.existing)
	}
}

// TestSkillSynthesisWorkerSkipsWhenNoNewMessages 验证水位之后的旧消息不会再次进入提炼。
func TestSkillSynthesisWorkerSkipsWhenNoNewMessages(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	messageAt := time.Now().UTC().Add(-2 * time.Hour)
	reader := &fakeSkillSessionReader{
		sessions: []Session{{ID: sessionID, UserID: userID}},
		messages: map[uuid.UUID][]Message{sessionID: {
			{Role: MessageUser, Content: "旧问题", CreatedAt: messageAt},
			{Role: MessageAssistant, Content: "旧回答", CreatedAt: messageAt.Add(time.Minute)},
		}},
	}
	skills, _ := NewUserSkillService(newFakeUserSkillRepository())
	extractor := &fakeSkillExtractor{candidate: SkillCandidate{ShouldCreate: true, Confidence: 0.9, Name: "x", PromptTemplate: "y"}}
	runs := &fakeSkillSynthesisRuns{watermark: time.Now().UTC().Add(-time.Hour), hasWatermark: true}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: extractor, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 0 {
		t.Fatalf("extractor should not run for covered messages: %d", extractor.calls)
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "no_new_messages" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}

// TestSkillSynthesisWorkerRespectsDailyLimit 验证每日自动技能数量达到上限后停止创建。
func TestSkillSynthesisWorkerRespectsDailyLimit(t *testing.T) {
	userID := uuid.New()
	jobs := newFakeJobQueue()
	job := makeSkillSynthesisJob(userID)
	jobs.jobs[job.ID] = job
	jobs.statuses[job.ID] = "pending"
	sessionID := uuid.New()
	session := Session{ID: sessionID, UserID: userID}
	messages := []Message{
		{Role: MessageUser, Content: "本周有多个设备出现同样的离线问题，请整理排查步骤"},
		{Role: MessageAssistant, Content: "我先按心跳、绑定关系和操作记录逐项核对"},
	}
	reader := &fakeSkillSessionReader{
		sessions: []Session{session},
		messages: map[uuid.UUID][]Message{sessionID: messages},
	}
	repo := newFakeUserSkillRepository()
	skills, _ := NewUserSkillService(repo)
	extractor := &fakeSkillExtractor{candidate: validWorkerCandidate(session, messages, "new-skill")}
	runs := &fakeSkillSynthesisRuns{autoSkills: 3}
	worker, err := NewSkillSynthesisWorker(SkillSynthesisWorkerDependencies{
		Jobs: jobs, Settings: &fakeSkillSynthesisSettingsStore{defaultEnabled: true},
		Sessions: reader, UserSkills: skills, Extractor: extractor, Runs: runs,
	}, SkillSynthesisConfig{MinConfidence: 0.5, MaxSkillsPerUserPerDay: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.skills) != 0 {
		t.Fatalf("skill was created beyond daily limit: %d", len(repo.skills))
	}
	if len(runs.finished) != 1 || runs.finished[0].Outcome != "daily_limit_reached" {
		t.Fatalf("run outcome = %#v", runs.finished)
	}
}
