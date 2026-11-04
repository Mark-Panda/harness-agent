package biz_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
)

// exportVectorReader 是导出测试使用的向量只读替身。
type exportVectorReader struct {
	vectors []biz.MemoryVector
}

// UpsertVectors 返回 nil，测试不使用。
func (f *exportVectorReader) UpsertVectors(context.Context, uuid.UUID, []biz.MemoryVector) error {
	return nil
}

// SearchVectors 返回空列表，测试不使用。
func (f *exportVectorReader) SearchVectors(context.Context, uuid.UUID, []float32, string, int) ([]biz.MemoryMatch, error) {
	return nil, nil
}

// ListSourceHashes 返回空映射，测试不使用。
func (f *exportVectorReader) ListSourceHashes(context.Context, uuid.UUID, string, []uuid.UUID) (map[uuid.UUID]string, error) {
	return nil, nil
}

// DeleteVectorsForSources 返回 nil，测试不使用。
func (f *exportVectorReader) DeleteVectorsForSources(context.Context, uuid.UUID, []uuid.UUID, time.Time) error {
	return nil
}

// ListVectors 返回预设向量。
func (f *exportVectorReader) ListVectors(context.Context, uuid.UUID, *uuid.UUID, int) ([]biz.MemoryVector, error) {
	return f.vectors, nil
}

// TestExportRedactsSecrets 验证导出对正文、工具调用、摘要、事实与向量元数据做二次脱敏。
func TestExportRedactsSecrets(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, sessionID := uuid.New(), uuid.New()
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).
		Return(biz.Session{ID: sessionID, UserID: userID, Title: "导出脱敏"}, nil)
	message := biz.Message{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser,
		Content: "你好，请用 Bearer abcdefghijklmnop 访问 postgres://user:pass@db:5432/app，密钥 sk-live-abcdefghijklmnopqrstuvwx",
		ToolCalls: []map[string]any{{
			"name": "http",
			"arguments": map[string]any{
				"api_key": "sk-abcdefghijklmnopqrstuvwx",
				"url":     "https://example.com/report",
			},
		}},
		CreatedAt: time.Now().UTC(),
	}
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 100, "").
		Return(biz.Page[biz.Message]{Items: []biz.Message{message}}, nil)
	summaries := &fakeSummaryRepository{saved: []biz.SessionSummary{{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 1,
		SummaryText: "用户提供了 token ghp_abcdefghijklmnopqrstuvwxyz0123",
		Status:      biz.SummaryActive, CreatedAt: time.Now().UTC(),
	}}}
	facts := &fakeFactRepository{saved: []biz.MemoryFact{{
		ID: uuid.New(), SessionID: &sessionID, Key: "credential.aws",
		Value:      json.RawMessage(`{"value":"AKIAIOSFODNN7EXAMPLE","password":"hunter2"}`),
		Confidence: 0.9, Version: 1, ValidFrom: time.Now().UTC(),
	}}}
	vectors := &exportVectorReader{vectors: []biz.MemoryVector{{
		ID: uuid.New(), SessionID: &sessionID, SourceType: biz.MemorySourceMessage,
		Content:  "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
		Metadata: map[string]any{"authorization": "Bearer super-secret-token"},
		Version:  "v1", CreatedAt: time.Now().UTC(),
	}}}
	service, err := biz.NewExportService(sessions, summaries, facts, vectors, nil)
	if err != nil {
		t.Fatal(err)
	}
	export, err := service.ExportSession(context.Background(), biz.Principal{UserID: userID}, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Messages) != 1 || export.Messages[0].Content == "" {
		t.Fatalf("messages = %#v", export.Messages)
	}
	// 正常问候必须保留，凭证与带凭据 URL 必须替换为占位符。
	if !strings.Contains(export.Messages[0].Content, "你好") ||
		strings.Contains(export.Messages[0].Content, "abcdefghijklmnop") ||
		strings.Contains(export.Messages[0].Content, "user:pass") {
		t.Fatalf("message content = %q", export.Messages[0].Content)
	}
	arguments, _ := export.Messages[0].ToolCalls[0]["arguments"].(map[string]any)
	if arguments["api_key"] != "[REDACTED]" || arguments["url"] != "https://example.com/report" {
		t.Fatalf("tool arguments = %#v", arguments)
	}
	if strings.Contains(export.Summaries[0].SummaryText, "ghp_") {
		t.Fatalf("summary leaked token: %q", export.Summaries[0].SummaryText)
	}
	var factValue map[string]any
	if err := json.Unmarshal(export.Facts[0].Value, &factValue); err != nil {
		t.Fatalf("decode fact value: %v", err)
	}
	if factValue["password"] != "[REDACTED]" || strings.Contains(string(export.Facts[0].Value), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("fact value = %s", export.Facts[0].Value)
	}
	if strings.Contains(export.Vectors[0].Content, "PRIVATE KEY") ||
		export.Vectors[0].Metadata["authorization"] != "[REDACTED]" {
		t.Fatalf("vector = %#v", export.Vectors[0])
	}
}

// TestExportAccountIncludesAgentsAndRedactsConfig 验证账号导出包含 Agent 与会话并脱敏配置。
func TestExportAccountIncludesAgentsAndRedactsConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, sessionID, agentID := uuid.New(), uuid.New(), uuid.New()
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().ListSessions(gomock.Any(), userID, 50, "").Return(biz.Page[biz.Session]{
		Items: []biz.Session{{ID: sessionID, UserID: userID, Title: "账号会话", Status: biz.SessionActive}}, NextCursor: "",
	}, nil)
	sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).
		Return(biz.Session{ID: sessionID, UserID: userID, Title: "账号会话", Status: biz.SessionActive}, nil)
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 100, "").
		Return(biz.Page[biz.Message]{Items: []biz.Message{{Role: biz.MessageUser, Content: "你好"}}}, nil)
	agents := mocks.NewMockAgentRepository(ctrl)
	agents.EXPECT().ListAgents(gomock.Any(), userID, true, 50, "").Return(biz.Page[biz.Agent]{
		Items: []biz.Agent{{
			ID: agentID, UserID: userID, Name: "助手", Mode: biz.AgentModeReact, Version: 1, Status: biz.AgentPublished,
			ConfigSnapshot: map[string]any{"password": "hunter2", "model": "m1"},
		}},
	}, nil)
	service, err := biz.NewExportService(sessions, &fakeSummaryRepository{}, &fakeFactRepository{}, &exportVectorReader{}, nil,
		biz.WithExportAgents(agents))
	if err != nil {
		t.Fatal(err)
	}
	export, err := service.ExportAccount(context.Background(), biz.Principal{UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if export.UserID != userID || len(export.Sessions) != 1 || len(export.Agents) != 1 {
		t.Fatalf("account export = %#v", export)
	}
	if export.Agents[0].ConfigSnapshot["password"] != "[REDACTED]" {
		t.Fatalf("agent config was not redacted: %#v", export.Agents[0].ConfigSnapshot)
	}
}

func TestExportSessionAndMemory(t *testing.T) {
	ctrl := gomock.NewController(t)
	userID, sessionID := uuid.New(), uuid.New()
	otherSession := uuid.New()
	sessions := mocks.NewMockSessionRepository(ctrl)
	sessions.EXPECT().GetSession(gomock.Any(), userID, sessionID).
		Return(biz.Session{ID: sessionID, UserID: userID, Title: "导出会话"}, nil)
	earlier := biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageUser, Content: "较早", CreatedAt: time.Now().UTC()}
	later := biz.Message{ID: uuid.New(), SessionID: sessionID, UserID: userID, Role: biz.MessageAssistant, Content: "较晚", CreatedAt: time.Now().UTC()}
	sessions.EXPECT().ListMessages(gomock.Any(), userID, sessionID, 100, "").
		Return(biz.Page[biz.Message]{Items: []biz.Message{later, earlier}}, nil)

	summaries := &fakeSummaryRepository{saved: []biz.SessionSummary{{
		ID: uuid.New(), SessionID: sessionID, UserID: userID, Version: 1, SummaryText: "摘要", Status: biz.SummaryActive,
		SourceMessageIDs: []uuid.UUID{earlier.ID}, CoveredMessageCount: 1, CreatedAt: time.Now().UTC(),
	}}}
	facts := &fakeFactRepository{saved: []biz.MemoryFact{
		{ID: uuid.New(), SessionID: &sessionID, Key: "preference.language", Value: json.RawMessage(`"中文"`), Confidence: 0.9, Version: 1, ValidFrom: time.Now().UTC()},
		{ID: uuid.New(), SessionID: &otherSession, Key: "preference.theme", Value: json.RawMessage(`"dark"`), Confidence: 0.8, Version: 1, ValidFrom: time.Now().UTC()},
	}}
	vectors := &exportVectorReader{vectors: []biz.MemoryVector{{
		ID: uuid.New(), SessionID: &sessionID, SourceType: biz.MemorySourceMessage,
		Content: "记忆内容", Version: "v1", CreatedAt: time.Now().UTC(),
	}}}
	service, err := biz.NewExportService(sessions, summaries, facts, vectors, nil)
	if err != nil {
		t.Fatal(err)
	}
	export, err := service.ExportSession(context.Background(), biz.Principal{UserID: userID}, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Messages) != 2 || export.Messages[0].Content != "较早" || export.Messages[1].Content != "较晚" {
		t.Fatalf("messages = %#v", export.Messages)
	}
	if len(export.Summaries) != 1 || len(export.Facts) != 1 || export.Facts[0].Key != "preference.language" {
		t.Fatalf("summaries=%d facts=%#v", len(export.Summaries), export.Facts)
	}
	if len(export.Vectors) != 1 || export.Vectors[0].Content != "记忆内容" {
		t.Fatalf("vectors = %#v", export.Vectors)
	}
	if export.ExportedAt.IsZero() {
		t.Fatal("exported_at is missing")
	}

	memory, err := service.ExportMemory(context.Background(), biz.Principal{UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if len(memory.Summaries) != 1 || len(memory.Facts) != 2 || len(memory.Vectors) != 1 {
		t.Fatalf("memory export = %#v", memory)
	}

	if _, err := service.ExportSession(context.Background(), biz.Principal{}, sessionID); err == nil {
		t.Fatal("anonymous export must fail")
	}
	if _, err := biz.NewExportService(nil, nil, nil, nil, nil); err == nil {
		t.Fatal("constructor must reject missing dependencies")
	}
}
