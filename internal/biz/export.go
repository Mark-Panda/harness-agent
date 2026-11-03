package biz

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ExportMessage 是导出消息的稳定 JSON 结构。
type ExportMessage struct {
	// ID 消息 ID。
	ID uuid.UUID `json:"id"`
	// Role 角色。
	Role string `json:"role"`
	// Content 正文。
	Content string `json:"content"`
	// ToolCalls 工具调用元数据。
	ToolCalls []map[string]any `json:"tool_calls,omitempty"`
	// ToolCallID 工具调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName 工具名。
	ToolName string `json:"tool_name,omitempty"`
	// SpeakerOpenID 群聊发言人。
	SpeakerOpenID string `json:"speaker_open_id,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// ExportSummary 是导出摘要版本结构。
type ExportSummary struct {
	// ID 摘要版本 ID。
	ID uuid.UUID `json:"id"`
	// SessionID 所属会话。
	SessionID uuid.UUID `json:"session_id"`
	// Version 版本号。
	Version int64 `json:"version"`
	// SummaryText 摘要正文。
	SummaryText string `json:"summary_text"`
	// SourceMessageIDs 本版本来源消息。
	SourceMessageIDs []uuid.UUID `json:"source_message_ids"`
	// CoveredMessageCount 累计覆盖消息数。
	CoveredMessageCount int `json:"covered_message_count"`
	// Status 版本状态。
	Status string `json:"status"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// ExportFact 是导出结构化事实结构。
type ExportFact struct {
	// ID 事实版本 ID。
	ID uuid.UUID `json:"id"`
	// SessionID 来源会话。
	SessionID *uuid.UUID `json:"session_id,omitempty"`
	// Key 事实键。
	Key string `json:"key"`
	// Value JSON 值。
	Value json.RawMessage `json:"value"`
	// Confidence 置信度。
	Confidence float64 `json:"confidence"`
	// SourceKind 来源类型。
	SourceKind string `json:"source_kind"`
	// Corrected 是否显式纠正。
	Corrected bool `json:"corrected"`
	// ValidFrom 生效时间。
	ValidFrom time.Time `json:"valid_from"`
	// ValidTo 失效时间。
	ValidTo *time.Time `json:"valid_to,omitempty"`
	// Version 版本号。
	Version int64 `json:"version"`
}

// ExportVector 是导出长期记忆向量元数据，不包含数值向量。
type ExportVector struct {
	// ID 向量 ID。
	ID uuid.UUID `json:"id"`
	// SessionID 来源会话。
	SessionID *uuid.UUID `json:"session_id,omitempty"`
	// SourceType 来源类型。
	SourceType string `json:"source_type"`
	// SourceID 来源 ID。
	SourceID *uuid.UUID `json:"source_id,omitempty"`
	// Content 记忆文本。
	Content string `json:"content"`
	// Metadata 来源元数据。
	Metadata map[string]any `json:"metadata,omitempty"`
	// Version 索引版本。
	Version string `json:"version"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// SessionExport 是单个会话的完整导出。
type SessionExport struct {
	// ExportedAt 导出时间。
	ExportedAt time.Time `json:"exported_at"`
	// Session 会话元数据。
	Session Session `json:"session"`
	// Messages 全部消息。
	Messages []ExportMessage `json:"messages"`
	// Summaries 摘要版本。
	Summaries []ExportSummary `json:"summaries"`
	// Facts 来源为该会话的结构化事实。
	Facts []ExportFact `json:"facts"`
	// Vectors 来源为该会话的长期记忆元数据。
	Vectors []ExportVector `json:"vectors"`
}

// MemoryExport 是用户长期记忆的完整导出。
type MemoryExport struct {
	// ExportedAt 导出时间。
	ExportedAt time.Time `json:"exported_at"`
	// Summaries 全部摘要版本。
	Summaries []ExportSummary `json:"summaries"`
	// Facts 全部事实版本。
	Facts []ExportFact `json:"facts"`
	// Vectors 全部长期记忆元数据。
	Vectors []ExportVector `json:"vectors"`
}

// AccountExport 是账号级全量导出，包含 Agent、会话、消息与长期记忆。
type AccountExport struct {
	// ExportedAt 导出时间。
	ExportedAt time.Time `json:"exported_at"`
	// UserID 账号 ID。
	UserID uuid.UUID `json:"user_id"`
	// Agents 用户 Agent 配置（已脱敏）。
	Agents []ExportAgent `json:"agents"`
	// Sessions 全部会话及其消息/摘要/事实/向量。
	Sessions []SessionExport `json:"sessions"`
	// Memory 用户长期记忆汇总。
	Memory MemoryExport `json:"memory"`
}

// ExportAgent 是账号导出中的 Agent 配置，敏感字段已脱敏。
type ExportAgent struct {
	// ID Agent ID。
	ID uuid.UUID `json:"id"`
	// Name 名称。
	Name string `json:"name"`
	// Description 描述。
	Description string `json:"description"`
	// Mode 编排模式。
	Mode string `json:"mode"`
	// Version 版本。
	Version int `json:"version"`
	// Status 状态。
	Status string `json:"status"`
	// ConfigSnapshot 脱敏后的配置快照。
	ConfigSnapshot map[string]any `json:"config_snapshot"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 更新时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// ExportService 导出会话与长期记忆；导出只读取当前用户数据。
type ExportService struct {
	sessions  SessionRepository
	agents    AgentRepository
	summaries SummaryRepository
	facts     FactRepository
	vectors   MemoryVectorRepository
	clock     func() time.Time
}

// ExportServiceOption 配置导出服务可选依赖。
type ExportServiceOption func(*ExportService)

// WithExportAgents 注入 Agent 仓储，使账号导出包含 Agent 配置。
func WithExportAgents(repo AgentRepository) ExportServiceOption {
	return func(service *ExportService) {
		service.agents = repo
	}
}

// NewExportService 构造导出服务。
func NewExportService(sessions SessionRepository, summaries SummaryRepository, facts FactRepository, vectors MemoryVectorRepository, clock func() time.Time, options ...ExportServiceOption) (*ExportService, error) {
	if sessions == nil || summaries == nil || facts == nil || vectors == nil {
		return nil, invalid("export service requires sessions, summaries, facts and vectors")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	service := &ExportService{sessions: sessions, summaries: summaries, facts: facts, vectors: vectors, clock: clock}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

// ExportAccount 导出账号全部 Agent、会话与长期记忆；分页上限防止无界内存占用。
func (s *ExportService) ExportAccount(ctx context.Context, principal Principal) (AccountExport, error) {
	if principal.UserID == uuid.Nil {
		return AccountExport{}, ErrForbidden
	}
	export := AccountExport{ExportedAt: s.clock(), UserID: principal.UserID}
	cursor := ""
	for len(export.Sessions) < 200 {
		page, err := s.sessions.ListSessions(ctx, principal.UserID, 50, cursor)
		if err != nil {
			return AccountExport{}, err
		}
		for _, session := range page.Items {
			sessionExport, err := s.ExportSession(ctx, principal, session.ID)
			if err != nil {
				return AccountExport{}, err
			}
			export.Sessions = append(export.Sessions, sessionExport)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if s.agents != nil {
		cursor = ""
		for len(export.Agents) < 200 {
			page, err := s.agents.ListAgents(ctx, principal.UserID, 50, cursor)
			if err != nil {
				return AccountExport{}, err
			}
			for _, agent := range page.Items {
				export.Agents = append(export.Agents, ExportAgent{
					ID: agent.ID, Name: agent.Name, Description: agent.Description, Mode: string(agent.Mode),
					Version: agent.Version, Status: string(agent.Status), CreatedAt: agent.CreatedAt, UpdatedAt: agent.UpdatedAt,
					ConfigSnapshot: redactExportMap(agent.ConfigSnapshot),
				})
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	memory, err := s.ExportMemory(ctx, principal)
	if err != nil {
		return AccountExport{}, err
	}
	export.Memory = memory
	return export, nil
}

// ExportSession 导出单个会话及其全部派生记忆。
func (s *ExportService) ExportSession(ctx context.Context, principal Principal, sessionID uuid.UUID) (SessionExport, error) {
	if principal.UserID == uuid.Nil || sessionID == uuid.Nil {
		return SessionExport{}, ErrForbidden
	}
	session, err := s.sessions.GetSession(ctx, principal.UserID, sessionID)
	if err != nil {
		return SessionExport{}, err
	}
	messages, err := s.allMessages(ctx, principal.UserID, sessionID)
	if err != nil {
		return SessionExport{}, err
	}
	summaries, err := s.summaries.ListSummaries(ctx, principal.UserID, sessionID, 1000)
	if err != nil {
		return SessionExport{}, err
	}
	facts, err := s.facts.ListFacts(ctx, principal.UserID, 1000)
	if err != nil {
		return SessionExport{}, err
	}
	allVectors, err := s.vectors.ListVectors(ctx, principal.UserID, &sessionID, 1000)
	if err != nil {
		return SessionExport{}, err
	}
	export := SessionExport{ExportedAt: s.clock(), Session: session}
	for _, message := range messages {
		export.Messages = append(export.Messages, toExportMessage(message))
	}
	for _, summary := range summaries {
		export.Summaries = append(export.Summaries, toExportSummary(summary))
	}
	for _, fact := range facts {
		if fact.SessionID != nil && *fact.SessionID == sessionID {
			export.Facts = append(export.Facts, toExportFact(fact))
		}
	}
	for _, vector := range allVectors {
		export.Vectors = append(export.Vectors, toExportVector(vector))
	}
	return export, nil
}

// ExportMemory 导出用户全部长期记忆与摘要，不包含会话原文。
func (s *ExportService) ExportMemory(ctx context.Context, principal Principal) (MemoryExport, error) {
	if principal.UserID == uuid.Nil {
		return MemoryExport{}, ErrForbidden
	}
	summaries, err := s.summaries.ListUserSummaries(ctx, principal.UserID, 1000)
	if err != nil {
		return MemoryExport{}, err
	}
	facts, err := s.facts.ListFacts(ctx, principal.UserID, 1000)
	if err != nil {
		return MemoryExport{}, err
	}
	vectors, err := s.vectors.ListVectors(ctx, principal.UserID, nil, 1000)
	if err != nil {
		return MemoryExport{}, err
	}
	export := MemoryExport{ExportedAt: s.clock()}
	for _, summary := range summaries {
		export.Summaries = append(export.Summaries, toExportSummary(summary))
	}
	for _, fact := range facts {
		export.Facts = append(export.Facts, toExportFact(fact))
	}
	for _, vector := range vectors {
		export.Vectors = append(export.Vectors, toExportVector(vector))
	}
	return export, nil
}

// allMessages 分页读取会话全部消息，上限 10000 条，防止导出无界占用内存。
func (s *ExportService) allMessages(ctx context.Context, userID, sessionID uuid.UUID) ([]Message, error) {
	messages := make([]Message, 0, 200)
	cursor := ""
	for len(messages) < 10000 {
		page, err := s.sessions.ListMessages(ctx, userID, sessionID, 100, cursor)
		if err != nil {
			return nil, err
		}
		// 仓储按时间倒序返回，导出需要正序。
		for index := len(page.Items) - 1; index >= 0; index-- {
			messages = append(messages, page.Items[index])
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return messages, nil
}

// toExportMessage 转换消息导出结构，并对正文与工具调用做二次脱敏。
func toExportMessage(message Message) ExportMessage {
	return ExportMessage{
		ID: message.ID, Role: string(message.Role), Content: RedactExportText(message.Content),
		ToolCalls:  redactExportMaps(message.ToolCalls),
		ToolCallID: message.ToolCallID, ToolName: message.ToolName, SpeakerOpenID: message.SpeakerOpenID,
		CreatedAt: message.CreatedAt,
	}
}

// toExportSummary 转换摘要导出结构，并对摘要正文做二次脱敏。
func toExportSummary(summary SessionSummary) ExportSummary {
	return ExportSummary{
		ID: summary.ID, SessionID: summary.SessionID, Version: summary.Version, SummaryText: RedactExportText(summary.SummaryText),
		SourceMessageIDs: summary.SourceMessageIDs, CoveredMessageCount: summary.CoveredMessageCount,
		Status: string(summary.Status), CreatedAt: summary.CreatedAt,
	}
}

// toExportFact 转换事实导出结构，并对事实 JSON 值做二次脱敏。
func toExportFact(fact MemoryFact) ExportFact {
	return ExportFact{
		ID: fact.ID, SessionID: fact.SessionID, Key: fact.Key, Value: redactExportJSON(fact.Value), Confidence: fact.Confidence,
		SourceKind: fact.SourceKind, Corrected: fact.Corrected, ValidFrom: fact.ValidFrom, ValidTo: fact.ValidTo,
		Version: fact.Version,
	}
}

// toExportVector 转换向量导出结构，并对向量正文与元数据做二次脱敏。
func toExportVector(vector MemoryVector) ExportVector {
	return ExportVector{
		ID: vector.ID, SessionID: vector.SessionID, SourceType: vector.SourceType, SourceID: vector.SourceID,
		Content: RedactExportText(vector.Content), Metadata: redactExportMap(vector.Metadata),
		Version: vector.Version, CreatedAt: vector.CreatedAt,
	}
}

// exportRedactedMarker 是导出脱敏后的占位符。
const exportRedactedMarker = "[REDACTED]"

// exportSecretPatterns 是导出时二次脱敏的高置信凭证模式。
// 导出数据可能离开受控执行记录边界，因此即使入库时已脱敏仍需再扫描一次。
var exportSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)sk-[a-z0-9_-]{16,}`),
	regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[a-zA-Z0-9_]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}`),
	regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// exportSensitiveKeys 是导出时整值替换的敏感字段名片段。
var exportSensitiveKeys = []string{
	"password", "passwd", "secret", "token", "authorization", "api_key", "apikey",
	"credential", "private_key", "access_key", "refresh_token", "client_secret",
}

// RedactExportText 对导出文本做二次脱敏：移除高置信凭证、带凭据的 URL 与私钥块。
func RedactExportText(text string) string {
	for _, pattern := range exportSecretPatterns {
		text = pattern.ReplaceAllString(text, exportRedactedMarker)
	}
	return text
}

// redactExportMaps 复制并递归脱敏 map 列表，避免修改仓储返回的原始对象。
func redactExportMaps(input []map[string]any) []map[string]any {
	if input == nil {
		return nil
	}
	output := make([]map[string]any, 0, len(input))
	for _, item := range input {
		output = append(output, redactExportMap(item))
	}
	return output
}

// redactExportMap 复制并递归脱敏 map：敏感键整值替换，其余字符串按凭证模式脱敏。
func redactExportMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		if isExportSensitiveKey(key) {
			output[key] = exportRedactedMarker
			continue
		}
		output[key] = redactExportValue(value)
	}
	return output
}

// redactExportValue 递归处理字符串、map、切片与其他 JSON 友好值。
func redactExportValue(value any) any {
	switch typed := value.(type) {
	case string:
		return RedactExportText(typed)
	case map[string]any:
		return redactExportMap(typed)
	case []any:
		output := make([]any, 0, len(typed))
		for _, item := range typed {
			output = append(output, redactExportValue(item))
		}
		return output
	default:
		return value
	}
}

// redactExportJSON 解析 JSON 值并递归脱敏；非法 JSON 按文本脱敏后重新编码，避免导出原始凭证。
func redactExportJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		fallback, _ := json.Marshal(RedactExportText(string(raw)))
		return fallback
	}
	encoded, err := json.Marshal(redactExportValue(value))
	if err != nil {
		fallback, _ := json.Marshal(RedactExportText(string(raw)))
		return fallback
	}
	return encoded
}

// isExportSensitiveKey 判断字段名是否属于凭证类敏感键。
func isExportSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, fragment := range exportSensitiveKeys {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}
