package biz

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type runSummaryStub struct {
	SummaryRepository
	summary SessionSummary
}

func (s runSummaryStub) GetActiveSummary(context.Context, uuid.UUID, uuid.UUID) (SessionSummary, bool, error) {
	return s.summary, true, nil
}

func TestRunSummaryHonorsTriggerBoundary(t *testing.T) {
	now := time.Now()
	earlierID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	triggerID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	laterID := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	future := now.Add(time.Second)
	for _, tc := range []struct {
		name     string
		sourceID *uuid.UUID
		sourceAt *time.Time
		want     bool
	}{
		{name: "before trigger", sourceID: &earlierID, sourceAt: &now, want: true},
		{name: "later timestamp", sourceID: &earlierID, sourceAt: &future},
		{name: "later id at same timestamp", sourceID: &laterID, sourceAt: &now},
		{name: "unknown source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := newObservedWorker(nil)
			worker.sessions = fakeSessionRepository{messages: []Message{
				{ID: triggerID, Role: MessageUser, Content: "original question", CreatedAt: now},
			}}
			worker.summaries = runSummaryStub{summary: SessionSummary{
				SummaryText: "summary marker", SourceEndMessageID: tc.sourceID, SourceEndAt: tc.sourceAt,
			}}
			// Older Runs can have a trigger ID without a recorded trigger time.
			run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), TriggerMessageID: &triggerID}
			messages, err := worker.buildMessages(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				if strings.Contains(message.Content, "summary marker") {
					if !tc.want {
						t.Fatal("out-of-bound summary reached the model")
					}
					return
				}
			}
			if tc.want {
				t.Fatal("valid earlier summary was omitted")
			}
		})
	}
}
