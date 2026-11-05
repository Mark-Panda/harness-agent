package biz_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// graphSessionReader 是固定会话列表的最小读取替身。
type graphSessionReader struct {
	sessions []biz.Session
	userIDs  []uuid.UUID
}

func (r *graphSessionReader) ListSessions(_ context.Context, userID uuid.UUID, _ int, _ string) (biz.Page[biz.Session], error) {
	r.userIDs = append(r.userIDs, userID)
	return biz.Page[biz.Session]{Items: r.sessions}, nil
}

// graphSummaryReader 是固定摘要列表的最小读取替身。
type graphSummaryReader struct {
	summaries []biz.SessionSummary
	userIDs   []uuid.UUID
}

func (r *graphSummaryReader) ListUserSummaries(_ context.Context, userID uuid.UUID, _ int) ([]biz.SessionSummary, error) {
	r.userIDs = append(r.userIDs, userID)
	return r.summaries, nil
}

// graphFactReader 是固定事实列表的最小读取替身。
type graphFactReader struct {
	facts   []biz.MemoryFact
	userIDs []uuid.UUID
}

func (r *graphFactReader) ListActiveFacts(_ context.Context, userID uuid.UUID, _ int) ([]biz.MemoryFact, error) {
	r.userIDs = append(r.userIDs, userID)
	return r.facts, nil
}

// graphVectorReader 是固定向量列表的最小读取替身。
type graphVectorReader struct {
	vectors []biz.MemoryVector
	userIDs []uuid.UUID
}

func (r *graphVectorReader) ListVectors(_ context.Context, userID uuid.UUID, _ *uuid.UUID, _ int) ([]biz.MemoryVector, error) {
	r.userIDs = append(r.userIDs, userID)
	return r.vectors, nil
}

// TestMemoryGraphBuildsSessionFactSummaryAndVector 验证图谱把会话、事实、摘要与向量按归属连接。
func TestMemoryGraphBuildsSessionFactSummaryAndVector(t *testing.T) {
	userID, sessionID, factID, summaryID, vectorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sessionAt := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	value, _ := json.Marshal(map[string]string{"city": "上海"})
	sessions := &graphSessionReader{sessions: []biz.Session{{ID: sessionID, UserID: userID, Title: "出差计划", CreatedAt: sessionAt}}}
	facts := &graphFactReader{facts: []biz.MemoryFact{{
		ID: factID, UserID: userID, SessionID: &sessionID, Key: "profile.city",
		Value: value, Confidence: 0.95, SourceKind: biz.FactSourceUser, CreatedAt: sessionAt,
	}}}
	summaries := &graphSummaryReader{summaries: []biz.SessionSummary{{
		ID: summaryID, UserID: userID, SessionID: sessionID, Version: 1, Status: biz.SummaryActive,
		SummaryText: "用户计划去上海出差。", CreatedAt: sessionAt,
	}}}
	vectors := &graphVectorReader{vectors: []biz.MemoryVector{{
		ID: vectorID, UserID: userID, SessionID: &sessionID, SourceType: biz.MemorySourceSummary,
		SourceID: &summaryID, Content: "用户计划去上海出差。", CreatedAt: sessionAt,
	}}}
	service, err := biz.NewMemoryGraphService(sessions, summaries, facts, vectors, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph(context.Background(), biz.Principal{UserID: userID})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.Stats.Sessions != 1 || graph.Stats.Facts != 1 || graph.Stats.Summaries != 1 || graph.Stats.Vectors != 1 {
		t.Fatalf("unexpected stats: %+v", graph.Stats)
	}
	if len(graph.Nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(graph.Nodes))
	}
	edgeKinds := map[string]bool{}
	for _, edge := range graph.Edges {
		edgeKinds[edge.Kind] = true
	}
	for _, kind := range []string{
		biz.MemoryGraphEdgeHasFact, biz.MemoryGraphEdgeHasSummary,
		biz.MemoryGraphEdgeHasVector, biz.MemoryGraphEdgeFromSummary,
	} {
		if !edgeKinds[kind] {
			t.Fatalf("missing edge kind %s in %+v", kind, graph.Edges)
		}
	}
	if graph.Nodes[0].Kind != biz.MemoryGraphNodeSession || graph.Nodes[0].Label != "出差计划" {
		t.Fatalf("session node should be first and titled: %+v", graph.Nodes[0])
	}
	if graph.Nodes[1].Kind != biz.MemoryGraphNodeFact || graph.Nodes[1].Label != "profile.city" {
		t.Fatalf("fact node label mismatch: %+v", graph.Nodes[1])
	}
	if graph.Nodes[1].Preview != `{"city":"上海"}` {
		t.Fatalf("fact preview mismatch: %q", graph.Nodes[1].Preview)
	}
	for _, readerUserIDs := range [][]uuid.UUID{sessions.userIDs, summaries.userIDs, facts.userIDs, vectors.userIDs} {
		for _, got := range readerUserIDs {
			if got != userID {
				t.Fatalf("reader must be scoped to principal user, got %s", got)
			}
		}
	}
}

// TestMemoryGraphSkipsSupersededSummaries 验证只展示生效摘要版本。
func TestMemoryGraphSkipsSupersededSummaries(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	summaries := &graphSummaryReader{summaries: []biz.SessionSummary{
		{ID: uuid.New(), UserID: userID, SessionID: sessionID, Version: 2, Status: biz.SummaryActive, SummaryText: "最新摘要"},
		{ID: uuid.New(), UserID: userID, SessionID: sessionID, Version: 1, Status: biz.SummarySuperseded, SummaryText: "旧摘要"},
	}}
	service, err := biz.NewMemoryGraphService(
		&graphSessionReader{sessions: []biz.Session{{ID: sessionID, UserID: userID, Title: "会话"}}},
		summaries, &graphFactReader{}, &graphVectorReader{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph(context.Background(), biz.Principal{UserID: userID})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.Stats.Summaries != 1 || len(graph.Nodes) != 2 {
		t.Fatalf("expected one active summary and session node, got stats=%+v nodes=%d", graph.Stats, len(graph.Nodes))
	}
}

// TestMemoryGraphRelatesFactsSharingTopic 验证同主题事实之间建立关联边。
func TestMemoryGraphRelatesFactsSharingTopic(t *testing.T) {
	userID := uuid.New()
	first, second := uuid.New(), uuid.New()
	value, _ := json.Marshal("上海")
	service, err := biz.NewMemoryGraphService(
		&graphSessionReader{}, &graphSummaryReader{}, &graphFactReader{facts: []biz.MemoryFact{
			{ID: first, UserID: userID, Key: "profile.city", Value: value, Confidence: 0.9},
			{ID: second, UserID: userID, Key: "profile.job", Value: value, Confidence: 0.7},
		}}, &graphVectorReader{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := service.Graph(context.Background(), biz.Principal{UserID: userID})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	found := false
	for _, edge := range graph.Edges {
		if edge.Kind == biz.MemoryGraphEdgeRelated {
			found = true
			if edge.Label != "同一主题：profile" {
				t.Fatalf("unexpected related label: %q", edge.Label)
			}
		}
	}
	if !found {
		t.Fatalf("expected related edge for shared topic, got %+v", graph.Edges)
	}
}

// TestMemoryGraphRejectsAnonymousPrincipal 验证匿名用户不能读取任何记忆。
func TestMemoryGraphRejectsAnonymousPrincipal(t *testing.T) {
	service, err := biz.NewMemoryGraphService(
		&graphSessionReader{}, &graphSummaryReader{}, &graphFactReader{}, &graphVectorReader{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Graph(context.Background(), biz.Principal{}); err != biz.ErrForbidden {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}
