package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 个人记忆图谱节点类型。
const (
	// MemoryGraphNodeSession 会话节点，作为记忆归属的聚类中心。
	MemoryGraphNodeSession MemoryGraphNodeKind = "session"
	// MemoryGraphNodeFact 结构化事实节点。
	MemoryGraphNodeFact MemoryGraphNodeKind = "fact"
	// MemoryGraphNodeVector 长期记忆向量节点。
	MemoryGraphNodeVector MemoryGraphNodeKind = "vector"
	// MemoryGraphNodeSummary 会话摘要节点。
	MemoryGraphNodeSummary MemoryGraphNodeKind = "summary"
)

// 个人记忆图谱边类型。
const (
	// MemoryGraphEdgeHasFact 会话派生出事实。
	MemoryGraphEdgeHasFact = "has_fact"
	// MemoryGraphEdgeHasVector 会话派生出长期记忆。
	MemoryGraphEdgeHasVector = "has_vector"
	// MemoryGraphEdgeHasSummary 会话派生出摘要。
	MemoryGraphEdgeHasSummary = "has_summary"
	// MemoryGraphEdgeFromFact 长期记忆由事实向量化而来。
	MemoryGraphEdgeFromFact = "from_fact"
	// MemoryGraphEdgeFromSummary 长期记忆由摘要向量化而来。
	MemoryGraphEdgeFromSummary = "from_summary"
	// MemoryGraphEdgeRelated 事实之间共享同一主题。
	MemoryGraphEdgeRelated = "related"
)

// 图谱容量上限，防止单个用户记忆过多时拖垮页面。
const (
	// memoryGraphMaxSessions 单次最多装载的会话节点来源数量。
	memoryGraphMaxSessions = 200
	// memoryGraphMaxFacts 单次最多展示的生效事实数量。
	memoryGraphMaxFacts = 100
	// memoryGraphMaxVectors 单次最多展示的长期记忆数量。
	memoryGraphMaxVectors = 200
	// memoryGraphMaxSummaries 单次最多展示的生效摘要数量。
	memoryGraphMaxSummaries = 200
	// memoryGraphPreviewChars 节点预览文本的最大字符数。
	memoryGraphPreviewChars = 160
	// memoryGraphMaxRelatedPerRoot 同一主题下最多建立的事实关联数。
	memoryGraphMaxRelatedPerRoot = 8
)

// MemoryGraphNodeKind 是图谱节点类型。
type MemoryGraphNodeKind string

// MemoryGraphNode 是个人记忆图谱中的一个节点。
type MemoryGraphNode struct {
	// ID 节点稳定标识，格式为 kind:uuid。
	ID string `json:"id"`
	// Kind 节点类型。
	Kind MemoryGraphNodeKind `json:"kind"`
	// Label 节点主标题。
	Label string `json:"label"`
	// Preview 节点内容预览，已截断并脱敏。
	Preview string `json:"preview,omitempty"`
	// SessionID 归属会话，仅会话节点为空。
	SessionID *uuid.UUID `json:"session_id,omitempty"`
	// CreatedAt 节点创建时间。
	CreatedAt time.Time `json:"created_at"`
	// Weight 节点权重，用于前端调整节点大小。
	Weight int `json:"weight"`
	// Metadata 节点补充信息，均为公开字段。
	Metadata map[string]any `json:"metadata,omitempty"`
}

// MemoryGraphEdge 是个人记忆图谱中的一条有向边。
type MemoryGraphEdge struct {
	// ID 边稳定标识。
	ID string `json:"id"`
	// Source 起点节点 ID。
	Source string `json:"source"`
	// Target 终点节点 ID。
	Target string `json:"target"`
	// Kind 边类型。
	Kind string `json:"kind"`
	// Label 边的人类可读说明。
	Label string `json:"label"`
	// Weight 边权重。
	Weight int `json:"weight"`
}

// MemoryGraphStats 是图谱节点与边的数量统计。
type MemoryGraphStats struct {
	// Sessions 会话节点数。
	Sessions int `json:"sessions"`
	// Facts 生效事实节点数。
	Facts int `json:"facts"`
	// Vectors 长期记忆节点数。
	Vectors int `json:"vectors"`
	// Summaries 生效摘要节点数。
	Summaries int `json:"summaries"`
	// Edges 边总数。
	Edges int `json:"edges"`
	// Truncated 是否有记忆因容量上限被截断。
	Truncated bool `json:"truncated"`
}

// MemoryGraph 是个人记忆知识图谱的完整只读视图。
type MemoryGraph struct {
	// GeneratedAt 图谱生成时间。
	GeneratedAt time.Time `json:"generated_at"`
	// Nodes 节点列表。
	Nodes []MemoryGraphNode `json:"nodes"`
	// Edges 边列表。
	Edges []MemoryGraphEdge `json:"edges"`
	// Stats 数量统计。
	Stats MemoryGraphStats `json:"stats"`
}

// MemoryGraphSessionReader 是图谱装载会话标题的最小只读端口。
type MemoryGraphSessionReader interface {
	// ListSessions 按用户游标分页列出会话。
	ListSessions(ctx context.Context, userID uuid.UUID, limit int, cursor string) (Page[Session], error)
}

// MemoryGraphSummaryReader 是图谱装载摘要的最小只读端口。
type MemoryGraphSummaryReader interface {
	// ListUserSummaries 按创建时间倒序列出用户全部摘要版本。
	ListUserSummaries(ctx context.Context, userID uuid.UUID, limit int) ([]SessionSummary, error)
}

// MemoryGraphFactReader 是图谱装载生效事实的最小只读端口。
type MemoryGraphFactReader interface {
	// ListActiveFacts 按更新时间倒序列出当前生效事实。
	ListActiveFacts(ctx context.Context, userID uuid.UUID, limit int) ([]MemoryFact, error)
}

// MemoryGraphVectorReader 是图谱装载长期记忆的最小只读端口。
type MemoryGraphVectorReader interface {
	// ListVectors 按创建时间倒序列出用户向量内容与元数据。
	ListVectors(ctx context.Context, userID uuid.UUID, sessionID *uuid.UUID, limit int) ([]MemoryVector, error)
}

// MemoryGraphService 聚合会话、事实、摘要与向量记忆，构建当前用户的只读图谱。
// 所有仓储读取均携带 principal.UserID，越权数据不会进入图谱。
type MemoryGraphService struct {
	sessions  MemoryGraphSessionReader
	summaries MemoryGraphSummaryReader
	facts     MemoryGraphFactReader
	vectors   MemoryGraphVectorReader
	clock     func() time.Time
}

// NewMemoryGraphService 构造个人记忆图谱服务。
func NewMemoryGraphService(sessions MemoryGraphSessionReader, summaries MemoryGraphSummaryReader, facts MemoryGraphFactReader,
	vectors MemoryGraphVectorReader, clock func() time.Time) (*MemoryGraphService, error) {
	if sessions == nil || summaries == nil || facts == nil || vectors == nil {
		return nil, invalid("memory graph service requires sessions, summaries, facts and vectors")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryGraphService{sessions: sessions, summaries: summaries, facts: facts, vectors: vectors, clock: clock}, nil
}

// Graph 构建当前用户的个人记忆图谱；没有记忆时返回空节点列表。
func (s *MemoryGraphService) Graph(ctx context.Context, principal Principal) (MemoryGraph, error) {
	graph := MemoryGraph{GeneratedAt: s.clock(), Nodes: []MemoryGraphNode{}, Edges: []MemoryGraphEdge{}}
	if principal.UserID == uuid.Nil {
		return graph, ErrForbidden
	}
	sessions, truncatedSessions, err := s.loadSessions(ctx, principal.UserID)
	if err != nil {
		return MemoryGraph{}, err
	}
	facts, err := s.facts.ListActiveFacts(ctx, principal.UserID, memoryGraphMaxFacts)
	if err != nil {
		return MemoryGraph{}, err
	}
	vectors, err := s.vectors.ListVectors(ctx, principal.UserID, nil, memoryGraphMaxVectors)
	if err != nil {
		return MemoryGraph{}, err
	}
	summaries, err := s.summaries.ListUserSummaries(ctx, principal.UserID, memoryGraphMaxSummaries*4)
	if err != nil {
		return MemoryGraph{}, err
	}
	graph.Stats.Truncated = truncatedSessions || len(facts) >= memoryGraphMaxFacts || len(vectors) >= memoryGraphMaxVectors

	// 先确定真正被记忆引用的会话，避免把大量无记忆会话画成孤立节点。
	referenced := map[uuid.UUID]bool{}
	for _, fact := range facts {
		if fact.SessionID != nil {
			referenced[*fact.SessionID] = true
		}
	}
	for _, vector := range vectors {
		if vector.SessionID != nil {
			referenced[*vector.SessionID] = true
		}
	}
	activeSummaries := make([]SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Status != SummaryActive {
			continue
		}
		if len(activeSummaries) >= memoryGraphMaxSummaries {
			graph.Stats.Truncated = true
			break
		}
		activeSummaries = append(activeSummaries, summary)
		referenced[summary.SessionID] = true
	}

	nodeIDs := map[string]bool{}
	addNode := func(node MemoryGraphNode) {
		if node.ID == "" || nodeIDs[node.ID] {
			return
		}
		nodeIDs[node.ID] = true
		graph.Nodes = append(graph.Nodes, node)
	}
	addEdge := func(edge MemoryGraphEdge) {
		if edge.Source == "" || edge.Target == "" || edge.Source == edge.Target {
			return
		}
		graph.Edges = append(graph.Edges, edge)
	}

	sessionNodeIDs := map[uuid.UUID]string{}
	for sessionID := range referenced {
		sessionNodeIDs[sessionID] = sessionGraphID(sessionID)
		addNode(MemoryGraphNode{
			ID: sessionNodeIDs[sessionID], Kind: MemoryGraphNodeSession, Label: sessionLabel(sessions, sessionID),
			SessionID: sessionIDPtr(sessionID), CreatedAt: sessions[sessionID].CreatedAt,
			Weight: 3, Metadata: sessionGraphMetadata(sessions[sessionID]),
		})
	}
	graph.Stats.Sessions = len(sessionNodeIDs)

	for _, fact := range facts {
		node := MemoryGraphNode{
			ID: factGraphID(fact.ID), Kind: MemoryGraphNodeFact, Label: fact.Key,
			Preview: factPreview(fact), CreatedAt: fact.CreatedAt, Weight: factWeight(fact),
			Metadata: map[string]any{
				"key": fact.Key, "confidence": fact.Confidence, "source_kind": fact.SourceKind,
				"corrected": fact.Corrected, "version": fact.Version,
			},
		}
		if fact.SessionID != nil {
			node.SessionID = sessionIDPtr(*fact.SessionID)
		}
		addNode(node)
		if fact.SessionID != nil {
			if sessionID, ok := sessionNodeIDs[*fact.SessionID]; ok {
				addEdge(MemoryGraphEdge{
					ID: sessionID + "->" + node.ID, Source: sessionID, Target: node.ID,
					Kind: MemoryGraphEdgeHasFact, Label: "会话提取事实", Weight: 1,
				})
			}
		}
	}
	graph.Stats.Facts = len(facts)

	summaryNodeIDs := map[uuid.UUID]string{}
	for _, summary := range activeSummaries {
		nodeID := summaryGraphID(summary.ID)
		summaryNodeIDs[summary.ID] = nodeID
		addNode(MemoryGraphNode{
			ID: nodeID, Kind: MemoryGraphNodeSummary, Label: fmt.Sprintf("会话摘要 v%d", summary.Version),
			Preview:   truncateGraphText(RedactExportText(summary.SummaryText), memoryGraphPreviewChars),
			SessionID: sessionIDPtr(summary.SessionID), CreatedAt: summary.CreatedAt,
			Weight: 2, Metadata: map[string]any{
				"version": summary.Version, "covered_message_count": summary.CoveredMessageCount,
			},
		})
		if sessionID, ok := sessionNodeIDs[summary.SessionID]; ok {
			addEdge(MemoryGraphEdge{
				ID: sessionID + "->" + nodeID, Source: sessionID, Target: nodeID,
				Kind: MemoryGraphEdgeHasSummary, Label: "会话摘要", Weight: 2,
			})
		}
	}
	graph.Stats.Summaries = len(activeSummaries)

	factNodeIDs := map[uuid.UUID]string{}
	for _, fact := range facts {
		factNodeIDs[fact.ID] = factGraphID(fact.ID)
	}
	for _, vector := range vectors {
		nodeID := vectorGraphID(vector.ID)
		node := MemoryGraphNode{
			ID: nodeID, Kind: MemoryGraphNodeVector, Label: vectorSourceLabel(vector.SourceType),
			Preview:   truncateGraphText(RedactExportText(vector.Content), memoryGraphPreviewChars),
			CreatedAt: vector.CreatedAt, Weight: 1,
			Metadata: map[string]any{
				"source_type": vector.SourceType, "model": vector.Model, "dimension": vector.Dimension,
			},
		}
		if vector.SessionID != nil {
			node.SessionID = sessionIDPtr(*vector.SessionID)
		}
		addNode(node)
		if vector.SessionID != nil {
			if sessionID, ok := sessionNodeIDs[*vector.SessionID]; ok {
				addEdge(MemoryGraphEdge{
					ID: sessionID + "->" + nodeID, Source: sessionID, Target: nodeID,
					Kind: MemoryGraphEdgeHasVector, Label: "长期记忆", Weight: 1,
				})
			}
		}
		if vector.SourceID == nil {
			continue
		}
		switch vector.SourceType {
		case MemorySourceFact:
			if factNodeID, ok := factNodeIDs[*vector.SourceID]; ok {
				addEdge(MemoryGraphEdge{
					ID: factNodeID + "->" + nodeID, Source: factNodeID, Target: nodeID,
					Kind: MemoryGraphEdgeFromFact, Label: "事实向量化", Weight: 2,
				})
			}
		case MemorySourceSummary:
			if summaryNodeID, ok := summaryNodeIDs[*vector.SourceID]; ok {
				addEdge(MemoryGraphEdge{
					ID: summaryNodeID + "->" + nodeID, Source: summaryNodeID, Target: nodeID,
					Kind: MemoryGraphEdgeFromSummary, Label: "摘要向量化", Weight: 2,
				})
			}
		}
	}
	graph.Stats.Vectors = len(vectors)

	// 同一主题的事实之间建立弱关联，让图谱呈现偏好主题聚类。
	for _, edge := range factRelationEdges(facts) {
		addEdge(edge)
	}
	graph.Stats.Edges = len(graph.Edges)
	return graph, nil
}

// loadSessions 分页装载用户会话标题，最多 memoryGraphMaxSessions 个。
func (s *MemoryGraphService) loadSessions(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]Session, bool, error) {
	sessions := make(map[uuid.UUID]Session, memoryGraphMaxSessions)
	cursor := ""
	for {
		page, err := s.sessions.ListSessions(ctx, userID, 100, cursor)
		if err != nil {
			return nil, false, err
		}
		for _, session := range page.Items {
			sessions[session.ID] = session
		}
		if page.NextCursor == "" {
			return sessions, false, nil
		}
		cursor = page.NextCursor
		if len(sessions) >= memoryGraphMaxSessions {
			return sessions, true, nil
		}
	}
}

// sessionLabel 返回会话标题，缺失时使用短 ID 兜底。
func sessionLabel(sessions map[uuid.UUID]Session, sessionID uuid.UUID) string {
	if session, ok := sessions[sessionID]; ok && strings.TrimSpace(session.Title) != "" {
		return session.Title
	}
	return "会话 " + sessionID.String()[:8]
}

// sessionGraphMetadata 返回会话节点的公开元数据。
func sessionGraphMetadata(session Session) map[string]any {
	if session.ID == uuid.Nil {
		return nil
	}
	return map[string]any{"source": string(session.Source), "status": string(session.Status), "memory_policy": string(session.MemoryPolicy)}
}

// factPreview 把事实 JSON 值压缩为可读预览。
func factPreview(fact MemoryFact) string {
	raw := strings.TrimSpace(string(fact.Value))
	if raw == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal(fact.Value, &value); err == nil {
		if encoded, err := json.Marshal(value); err == nil {
			raw = string(encoded)
		}
	}
	return truncateGraphText(RedactExportText(raw), memoryGraphPreviewChars)
}

// factWeight 按置信度与纠正标记计算节点权重，突出更可信的记忆。
func factWeight(fact MemoryFact) int {
	weight := 2
	if fact.Confidence >= 0.9 {
		weight = 3
	}
	if fact.Corrected {
		weight += 1
	}
	return weight
}

// factRelationEdges 为共享主题的事实建立弱关联边，每主题限制数量避免边爆炸。
func factRelationEdges(facts []MemoryFact) []MemoryGraphEdge {
	groups := map[string][]MemoryFact{}
	order := make([]string, 0, len(facts))
	for _, fact := range facts {
		root := factRootKey(fact.Key)
		if root == "" {
			continue
		}
		if _, exists := groups[root]; !exists {
			order = append(order, root)
		}
		groups[root] = append(groups[root], fact)
	}
	edges := make([]MemoryGraphEdge, 0, len(facts))
	for _, root := range order {
		members := groups[root]
		if len(members) < 2 {
			continue
		}
		limit := len(members)
		if limit > memoryGraphMaxRelatedPerRoot {
			limit = memoryGraphMaxRelatedPerRoot
		}
		anchors := append([]MemoryFact(nil), members...)
		sort.SliceStable(anchors, func(i, j int) bool {
			if anchors[i].Confidence == anchors[j].Confidence {
				return anchors[i].CreatedAt.Before(anchors[j].CreatedAt)
			}
			return anchors[i].Confidence > anchors[j].Confidence
		})
		for index := 1; index < limit; index++ {
			source := factGraphID(anchors[index-1].ID)
			target := factGraphID(anchors[index].ID)
			edges = append(edges, MemoryGraphEdge{
				ID: source + "~" + target, Source: source, Target: target,
				Kind: MemoryGraphEdgeRelated, Label: "同一主题：" + root, Weight: 1,
			})
		}
	}
	return edges
}

// factRootKey 取点分层事实键的首段作为主题。
func factRootKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if index := strings.Index(key, "."); index > 0 {
		return key[:index]
	}
	return key
}

// vectorSourceLabel 把向量来源类型翻译为节点标题。
func vectorSourceLabel(sourceType string) string {
	switch sourceType {
	case MemorySourceMessage:
		return "对话记忆"
	case MemorySourceSummary:
		return "摘要记忆"
	case MemorySourceFact:
		return "事实记忆"
	default:
		return "长期记忆"
	}
}

// truncateGraphText 按字符截断文本并附加省略标记。
func truncateGraphText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// sessionGraphID 构造会话节点 ID。
func sessionGraphID(id uuid.UUID) string { return "session:" + id.String() }

// factGraphID 构造事实节点 ID。
func factGraphID(id uuid.UUID) string { return "fact:" + id.String() }

// vectorGraphID 构造长期记忆节点 ID。
func vectorGraphID(id uuid.UUID) string { return "vector:" + id.String() }

// summaryGraphID 构造摘要节点 ID。
func summaryGraphID(id uuid.UUID) string { return "summary:" + id.String() }

// sessionIDPtr 返回会话 ID 的只读副本指针。
func sessionIDPtr(id uuid.UUID) *uuid.UUID {
	copied := id
	return &copied
}
