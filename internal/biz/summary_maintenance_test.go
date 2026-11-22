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

// fakeSummaryTrigger 记录补建扫描触发的会话。
type fakeSummaryTrigger struct {
	refs []biz.SummarySessionRef
	err  error
}

// MaybeEnqueue 记录规划调用。
func (f *fakeSummaryTrigger) MaybeEnqueue(_ context.Context, userID, sessionID uuid.UUID) error {
	f.refs = append(f.refs, biz.SummarySessionRef{UserID: userID, SessionID: sessionID})
	return f.err
}

// fakeSummarySessionReader 是补建扫描的会话来源。
type fakeSummarySessionReader struct {
	sessions []biz.SummarySessionRef
	err      error
}

// ListSummaryCandidateSessions 返回预置会话。
func (f *fakeSummarySessionReader) ListSummaryCandidateSessions(context.Context, int) ([]biz.SummarySessionRef, error) {
	return f.sessions, f.err
}

// TestSummaryMaintenanceReplansEveryCandidate 验证补建扫描逐个会话触发摘要规划并忽略单点失败。
func TestSummaryMaintenanceReplansEveryCandidate(t *testing.T) {
	sessions := []biz.SummarySessionRef{
		{UserID: uuid.New(), SessionID: uuid.New()},
		{UserID: uuid.New(), SessionID: uuid.New()},
	}
	trigger := &fakeSummaryTrigger{err: errors.New("enqueue failed")}
	maintenance, err := biz.NewSummaryMaintenance(&fakeSummarySessionReader{sessions: sessions}, trigger,
		biz.SummaryMaintenanceConfig{Interval: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance.RunOnce(context.Background())
	if len(trigger.refs) != len(sessions) || trigger.refs[1].SessionID != sessions[1].SessionID {
		t.Fatalf("triggered = %#v", trigger.refs)
	}

	// 扫描失败只记录日志，不 panic 且不调用规划器。
	failing := &fakeSummaryTrigger{}
	maintenance, err = biz.NewSummaryMaintenance(&fakeSummarySessionReader{err: errors.New("db down")}, failing,
		biz.SummaryMaintenanceConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance.RunOnce(context.Background())
	if len(failing.refs) != 0 {
		t.Fatalf("triggered after scan failure: %#v", failing.refs)
	}
	if _, err := biz.NewSummaryMaintenance(nil, failing, biz.SummaryMaintenanceConfig{}, nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}

// TestSummaryWorkerRequeuesUncoveredTailWhenStale 验证旧版本任务被更新摘要超越时，
// 仍会把当前生效边界之后、任务结束边界之前的消息补建为新任务，避免尾部永久丢失。
func TestSummaryWorkerRequeuesUncoveredTailWhenStale(t *testing.T) {
	ctrl := gomock.NewController(t)
	base := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	covered := biz.Message{ID: uuid.New(), Role: biz.MessageUser, Content: "已覆盖", CreatedAt: base}
	tail := biz.Message{ID: uuid.New(), Role: biz.MessageAssistant, Content: "新增尾部", CreatedAt: base.Add(time.Minute)}
	task := biz.SummaryTask{
		UserID: uuid.New(), SessionID: uuid.New(), BaseVersion: 0,
		SourceMessageIDs: []uuid.UUID{covered.ID, tail.ID},
		Fingerprint:      biz.SummarySourceFingerprint([]biz.Message{covered, tail}),
		EndMessageID:     tail.ID, EndAt: tail.CreatedAt, CoveredMessageCount: 2,
	}
	job := biz.Job{ID: uuid.New(), Type: biz.SummaryJobType, Payload: summaryJobPayload(task), Attempts: 1, FencingToken: 1}

	repository := &fakeSummaryRepository{latest: 1, applied: false}
	boundaryID, boundaryAt := covered.ID, covered.CreatedAt
	repository.active = biz.SessionSummary{
		ID: uuid.New(), SummaryText: "旧摘要", Status: biz.SummaryActive,
		SourceEndMessageID: &boundaryID, SourceEndAt: &boundaryAt, CoveredMessageCount: 1,
	}
	repository.found = true
	reader := &fakeSummaryReader{byIDs: []biz.Message{covered, tail}}
	queue := &fakeSummaryQueue{}
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(biz.Session{Status: biz.SessionActive}, nil).AnyTimes()

	worker, err := biz.NewSummaryWorker(biz.SummaryWorkerDependencies{
		Jobs: &fakeSummaryJobs{job: job}, Sessions: sessions, Messages: reader,
		Summaries: repository, Generator: &fakeSummaryGenerator{}, Queue: queue,
	}, biz.SummaryWorkerConfig{WorkerID: "summary-test", Heartbeat: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("enqueued tasks = %#v", queue.tasks)
	}
	follow := queue.tasks[0]
	if follow.BaseVersion != 1 || follow.EndMessageID != tail.ID || follow.CoveredMessageCount != 2 ||
		len(follow.SourceMessageIDs) != 1 || follow.SourceMessageIDs[0] != tail.ID {
		t.Fatalf("follow-up task = %#v", follow)
	}
}
