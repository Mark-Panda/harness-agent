package server

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestParseUUID 覆盖合法、非法与 Nil UUID 三种输入。
func TestParseUUID(t *testing.T) {
	valid := uuid.New()
	tests := []struct {
		name    string
		input   string
		want    uuid.UUID
		wantErr bool
	}{
		{name: "valid", input: valid.String(), want: valid},
		{name: "invalid format", input: "not-a-uuid", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "nil uuid rejected", input: uuid.Nil.String(), wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := parseUUID(testCase.input)
			if testCase.wantErr {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("parseUUID(%q) error = %v, want InvalidArgument", testCase.input, err)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("parseUUID(%q) = %v, %v", testCase.input, got, err)
			}
		})
	}
}

// TestOptionalUUID 覆盖 nil、空串、合法与非法（非法被静默丢弃）输入。
func TestOptionalUUID(t *testing.T) {
	valid := uuid.NewString()
	invalid := "bad"
	empty := ""
	tests := []struct {
		name  string
		input *string
		want  *uuid.UUID
	}{
		{name: "nil pointer", input: nil, want: nil},
		{name: "empty string", input: &empty, want: nil},
		{name: "valid", input: &valid, want: ptrUUID(uuid.MustParse(valid))},
		{name: "invalid dropped", input: &invalid, want: nil},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := optionalUUID(testCase.input)
			if (got == nil) != (testCase.want == nil) {
				t.Fatalf("optionalUUID = %v, want %v", got, testCase.want)
			}
			if got != nil && *got != *testCase.want {
				t.Fatalf("optionalUUID = %v, want %v", *got, *testCase.want)
			}
		})
	}
}

// TestOptionalInt 覆盖 nil 与正常值转换。
func TestOptionalInt(t *testing.T) {
	if got := optionalInt(nil); got != nil {
		t.Fatalf("optionalInt(nil) = %v, want nil", got)
	}
	value := int32(7)
	got := optionalInt(&value)
	if got == nil || *got != 7 {
		t.Fatalf("optionalInt(7) = %v, want 7", got)
	}
}

// TestParseUUIDList 验证合法项保留、非法项丢弃、空输入返回空切片。
func TestParseUUIDList(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	got := parseUUIDList([]string{first.String(), "garbage", second.String(), ""})
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("parseUUIDList = %v", got)
	}
	if empty := parseUUIDList(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("parseUUIDList(nil) = %v, want empty non-nil", empty)
	}
}

// TestStructConversion 验证 fromProtoStruct/toProtoStruct 的 nil 处理与字段守恒往返。
func TestStructConversion(t *testing.T) {
	if got := fromProtoStruct(nil); got != nil {
		t.Fatalf("fromProtoStruct(nil) = %v, want nil", got)
	}
	source := map[string]any{"region": "cn", "limit": float64(3), "tags": []any{"a", "b"}}
	roundTripped := fromProtoStruct(toProtoStruct(source))
	if !reflect.DeepEqual(roundTripped, source) {
		t.Fatalf("struct round trip = %#v, want %#v", roundTripped, source)
	}
	// nil map 必须返回非 nil 的空 Struct，保证 proto 字段可序列化。
	if empty := toProtoStruct(nil); empty == nil || len(empty.GetFields()) != 0 {
		t.Fatalf("toProtoStruct(nil) = %v, want empty struct", empty)
	}
	// 无法转为 protobuf 值的类型必须降级为空对象而不是 panic。
	fallback := toProtoStruct(map[string]any{"bad": make(chan int)})
	if fallback == nil || len(fallback.GetFields()) != 0 {
		t.Fatalf("toProtoStruct(unconvertible) = %v, want empty struct", fallback)
	}
}

// TestFromProtoCandidates 覆盖候选转换：正常字段守恒、空约束、非法 model_id 置零。
func TestFromProtoCandidates(t *testing.T) {
	if got := fromProtoCandidates(nil); got == nil || len(got) != 0 {
		t.Fatalf("fromProtoCandidates(nil) = %v, want empty non-nil", got)
	}
	modelID := uuid.New()
	constraints, err := structpb.NewStruct(map[string]any{"data_region": "cn"})
	if err != nil {
		t.Fatal(err)
	}
	got := fromProtoCandidates([]*harnessv1.PolicyCandidate{
		{ModelId: modelID.String(), Priority: 1, Constraints: constraints},
		{ModelId: "invalid", Priority: 2},
	})
	if len(got) != 2 {
		t.Fatalf("fromProtoCandidates len = %d", len(got))
	}
	if got[0].ModelID != modelID || got[0].Priority != 1 || got[0].Constraints["data_region"] != "cn" {
		t.Fatalf("first candidate = %#v", got[0])
	}
	if got[1].ModelID != uuid.Nil || got[1].Constraints != nil {
		t.Fatalf("invalid model candidate = %#v", got[1])
	}
}

// TestFromProtoModel 验证模型入参转换：全字段守恒、可选字段缺省、非法 UUID 报错。
func TestFromProtoModel(t *testing.T) {
	providerID := uuid.New()
	modelID := uuid.New()
	verifiedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	model, err := fromProtoModel(providerID.String(), modelID.String(), "gpt-x", "chat", 1048576, 4096,
		&harnessv1.ModelCapabilities{Streaming: true, Tools: true, StructuredOutput: true},
		1.5, 2.5, "cn", "manual", timestamppb.New(verifiedAt), "enabled")
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != modelID || model.ProviderID != providerID || model.RemoteModelName != "gpt-x" ||
		model.Kind != biz.ModelKind("chat") || model.ContextWindowTokens != 1048576 || model.MaxOutputTokens != 4096 ||
		!model.Capabilities.Streaming || !model.Capabilities.Tools || !model.Capabilities.StructuredOutput ||
		model.InputPricePerMillion != 1.5 || model.OutputPricePerMillion != 2.5 ||
		model.DataRegion != "cn" || model.VerificationSource != "manual" || model.Status != biz.ModelStatus("enabled") {
		t.Fatalf("model = %#v", model)
	}
	if model.VerifiedAt == nil || !model.VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("VerifiedAt = %v, want %v", model.VerifiedAt, verifiedAt)
	}

	// 创建路径：modelID 为空、capabilities 与 verifiedAt 缺省。
	created, err := fromProtoModel(providerID.String(), "", "m", "embedding", 0, 0, nil, 0, 0, "", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != uuid.Nil || created.VerifiedAt != nil || created.Capabilities != (biz.ModelCapabilities{}) {
		t.Fatalf("created model = %#v", created)
	}

	// 非法 provider/model ID 必须返回 InvalidArgument。
	if _, err := fromProtoModel("bad", "", "m", "chat", 0, 0, nil, 0, 0, "", "", nil, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid provider id error = %v", err)
	}
	if _, err := fromProtoModel(providerID.String(), "bad", "m", "chat", 0, 0, nil, 0, 0, "", "", nil, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid model id error = %v", err)
	}
}

// TestToProtoProvider 验证供应商出参字段守恒且不含密钥材料。
func TestToProtoProvider(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	provider := biz.Provider{
		ID: uuid.New(), Name: "primary", ProviderType: "openai_compatible", BaseURL: "https://models.example.com",
		KeyVersion: "v1", Status: biz.ProviderStatus("enabled"), DataRegion: "cn", CreatedAt: now, UpdatedAt: now,
	}
	got := toProtoProvider(provider)
	if got.GetId() != provider.ID.String() || got.GetName() != "primary" || got.GetProviderType() != "openai_compatible" ||
		got.GetBaseUrl() != "https://models.example.com" || got.GetKeyVersion() != "v1" ||
		got.GetStatus() != "enabled" || got.GetDataRegion() != "cn" {
		t.Fatalf("provider proto = %v", got)
	}
	if !got.GetCreatedAt().AsTime().Equal(now) || !got.GetUpdatedAt().AsTime().Equal(now) {
		t.Fatalf("timestamps = %v %v", got.GetCreatedAt(), got.GetUpdatedAt())
	}
	// 零值时间必须输出 nil 时间戳。
	if empty := toProtoProvider(biz.Provider{}); empty.GetCreatedAt() != nil || empty.GetUpdatedAt() != nil {
		t.Fatalf("zero-time provider = %v", empty)
	}
}

// TestToProtoModel 验证模型出参字段守恒与 VerifiedAt 可选项。
func TestToProtoModel(t *testing.T) {
	verifiedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	model := biz.Model{
		ID: uuid.New(), ProviderID: uuid.New(), RemoteModelName: "gpt-x", Kind: biz.ModelKind("chat"),
		ContextWindowTokens: 1048576, MaxOutputTokens: 4096,
		Capabilities:         biz.ModelCapabilities{Streaming: true, Tools: true},
		InputPricePerMillion: 1.5, OutputPricePerMillion: 2.5, DataRegion: "cn",
		VerificationSource: "manual", VerifiedAt: &verifiedAt, Status: biz.ModelStatus("enabled"),
	}
	got := toProtoModel(model)
	if got.GetId() != model.ID.String() || got.GetProviderId() != model.ProviderID.String() ||
		got.GetRemoteModelName() != "gpt-x" || got.GetKind() != "chat" || got.GetContextWindowTokens() != 1048576 ||
		got.GetMaxOutputTokens() != 4096 || !got.GetCapabilities().GetStreaming() || !got.GetCapabilities().GetTools() ||
		got.GetCapabilities().GetStructuredOutput() || got.GetInputPricePerMillion() != 1.5 ||
		got.GetOutputPricePerMillion() != 2.5 || got.GetDataRegion() != "cn" ||
		got.GetVerificationSource() != "manual" || got.GetStatus() != "enabled" {
		t.Fatalf("model proto = %v", got)
	}
	if !got.GetVerifiedAt().AsTime().Equal(verifiedAt) {
		t.Fatalf("VerifiedAt = %v", got.GetVerifiedAt())
	}
	// VerifiedAt 缺省时 proto 字段必须为 nil。
	if without := toProtoModel(biz.Model{}); without.GetVerifiedAt() != nil || without.GetCapabilities() == nil {
		t.Fatalf("empty model proto = %v", without)
	}
}

// TestToProtoPolicy 验证策略出参与候选列表的往返字段守恒。
func TestToProtoPolicy(t *testing.T) {
	modelID := uuid.New()
	policy := biz.ModelPolicy{
		ID: uuid.New(), Name: "route", Version: 3, MinContextTokens: 1048576,
		Rules:  map[string]any{"prefer": "cheap"},
		Status: biz.PolicyStatus("published"),
		Candidates: []biz.PolicyCandidate{
			{ModelID: modelID, Priority: 1, Constraints: map[string]any{"data_region": "cn"}},
		},
	}
	got := toProtoPolicy(policy)
	if got.GetId() != policy.ID.String() || got.GetName() != "route" || got.GetVersion() != 3 ||
		got.GetMinContextTokens() != 1048576 || got.GetStatus() != "published" ||
		got.GetRules().AsMap()["prefer"] != "cheap" || len(got.GetCandidates()) != 1 {
		t.Fatalf("policy proto = %v", got)
	}
	// 候选列表 proto -> biz 往返守恒。
	roundTripped := fromProtoCandidates(got.GetCandidates())
	if len(roundTripped) != 1 || roundTripped[0].ModelID != modelID || roundTripped[0].Priority != 1 ||
		roundTripped[0].Constraints["data_region"] != "cn" {
		t.Fatalf("candidate round trip = %#v", roundTripped)
	}
	// 空策略：Rules 为 nil 时返回空对象，候选为空切片。
	empty := toProtoPolicy(biz.ModelPolicy{})
	if empty.GetRules() == nil || len(empty.GetRules().GetFields()) != 0 || empty.GetCandidates() == nil {
		t.Fatalf("empty policy proto = %v", empty)
	}
}

// TestToProtoAgent 验证 Agent 出参及可选策略引用字段。
func TestToProtoAgent(t *testing.T) {
	policyID := uuid.New()
	policyVersion := 2
	skillID, mcpID := uuid.New(), uuid.New()
	agent := biz.Agent{
		ID: uuid.New(), UserID: uuid.New(), Name: "assistant", Description: "desc", Mode: biz.AgentMode("react"),
		ModelPolicyID: &policyID, ModelPolicyVersion: &policyVersion, Version: 4,
		ConfigSnapshot: map[string]any{"model": "gpt-x"}, SkillIDs: []uuid.UUID{skillID}, MCPIDs: []uuid.UUID{mcpID},
		Status: biz.AgentStatus("published"),
	}
	got := toProtoAgent(agent)
	if got.GetId() != agent.ID.String() || got.GetUserId() != agent.UserID.String() || got.GetName() != "assistant" ||
		got.GetDescription() != "desc" || got.GetMode() != "react" || got.GetVersion() != 4 ||
		got.GetStatus() != "published" || got.GetConfigSnapshot().AsMap()["model"] != "gpt-x" {
		t.Fatalf("agent proto = %v", got)
	}
	if got.GetModelPolicyId() != policyID.String() || got.GetModelPolicyVersion() != 2 {
		t.Fatalf("policy refs = %v %v", got.GetModelPolicyId(), got.GetModelPolicyVersion())
	}
	if len(got.GetSkillIds()) != 1 || got.GetSkillIds()[0] != skillID.String() ||
		len(got.GetMcpIds()) != 1 || got.GetMcpIds()[0] != mcpID.String() {
		t.Fatalf("bindings = %v %v", got.GetSkillIds(), got.GetMcpIds())
	}
	// 草稿 Agent：策略引用缺省时指针字段必须为 nil，快照降级为空对象。
	draft := toProtoAgent(biz.Agent{})
	if draft.GetModelPolicyId() != "" || draft.ModelPolicyId != nil || draft.ModelPolicyVersion != nil ||
		draft.GetConfigSnapshot() == nil {
		t.Fatalf("draft agent proto = %v", draft)
	}
}

// TestToProtoSession 验证会话出参及可选 Agent 绑定与最近消息时间。
func TestToProtoSession(t *testing.T) {
	agentID := uuid.New()
	lastMessageAt := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	session := biz.Session{
		ID: uuid.New(), AgentID: &agentID, Title: "chat", Source: biz.SessionSource("web"),
		Status: biz.SessionStatus("active"), Version: 2, MemoryPolicy: biz.SessionMemoryPolicy("private"),
		LastMessageAt: &lastMessageAt,
	}
	got := toProtoSession(session)
	if got.GetId() != session.ID.String() || got.GetAgentId() != agentID.String() || got.GetTitle() != "chat" ||
		got.GetSource() != "web" || got.GetStatus() != "active" || got.GetVersion() != 2 ||
		got.GetMemoryPolicy() != "private" || !got.GetLastMessageAt().AsTime().Equal(lastMessageAt) {
		t.Fatalf("session proto = %v", got)
	}
	// 无 Agent 绑定与无消息时指针字段必须为 nil。
	empty := toProtoSession(biz.Session{})
	if empty.AgentId != nil || empty.GetLastMessageAt() != nil {
		t.Fatalf("empty session proto = %v", empty)
	}
}

// TestToProtoMessage 验证消息出参与工具调用列表转换。
func TestToProtoMessage(t *testing.T) {
	message := biz.Message{
		ID: uuid.New(), SessionID: uuid.New(), Role: biz.MessageRole("assistant"), Content: "hi",
		TokenCount: 12, ToolCalls: []map[string]any{{"name": "search", "args": map[string]any{"q": "x"}}},
	}
	got := toProtoMessage(message)
	if got.GetId() != message.ID.String() || got.GetSessionId() != message.SessionID.String() ||
		got.GetRole() != "assistant" || got.GetContent() != "hi" || got.GetTokenCount() != 12 {
		t.Fatalf("message proto = %v", got)
	}
	if len(got.GetToolCalls()) != 1 || got.GetToolCalls()[0].AsMap()["name"] != "search" {
		t.Fatalf("tool calls = %v", got.GetToolCalls())
	}
	// 无工具调用时保持空切片而非 nil。
	if empty := toProtoMessage(biz.Message{}); empty.GetToolCalls() == nil || len(empty.GetToolCalls()) != 0 {
		t.Fatalf("empty message tool calls = %v", empty.GetToolCalls())
	}
}

// TestClampInt32 验证 int 到 int32 的饱和转换。
func TestClampInt32(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  int32
	}{
		{name: "zero", input: 0, want: 0},
		{name: "normal", input: 123, want: 123},
		{name: "negative", input: -123, want: -123},
		{name: "max boundary", input: math.MaxInt32, want: math.MaxInt32},
		{name: "min boundary", input: math.MinInt32, want: math.MinInt32},
		{name: "overflow saturates", input: math.MaxInt32 + 1, want: math.MaxInt32},
		{name: "underflow saturates", input: math.MinInt32 - 1, want: math.MinInt32},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := clampInt32(testCase.input); got != testCase.want {
				t.Fatalf("clampInt32(%d) = %d, want %d", testCase.input, got, testCase.want)
			}
		})
	}
}

// TestUUIDStrings 验证 UUID 列表转字符串列表。
func TestUUIDStrings(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	got := uuidStrings([]uuid.UUID{first, second})
	if len(got) != 2 || got[0] != first.String() || got[1] != second.String() {
		t.Fatalf("uuidStrings = %v", got)
	}
	if empty := uuidStrings(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("uuidStrings(nil) = %v, want empty non-nil", empty)
	}
}

// TestTimestamp 验证零值时间输出 nil，正常时间统一为 UTC。
func TestTimestamp(t *testing.T) {
	if got := timestamp(time.Time{}); got != nil {
		t.Fatalf("timestamp(zero) = %v, want nil", got)
	}
	value := time.Date(2026, 5, 6, 7, 8, 9, 10, time.FixedZone("CST", 8*3600))
	got := timestamp(value)
	if got == nil || !got.AsTime().Equal(value) || got.AsTime().Location() != time.UTC {
		t.Fatalf("timestamp = %v", got)
	}
}

// TestGRPCAgentUpdateConvertsOptOutsAndUpdatedAt 验证 gRPC 更新透传显式移除与并发令牌。
func TestGRPCAgentUpdateConvertsOptOutsAndUpdatedAt(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleUser)
	grpcServer := &platformGRPCServer{platform: fixture.platform}
	userID, agentID, skillID := fixture.principal.UserID, uuid.New(), uuid.New()
	loadedAt := time.Now().UTC().Truncate(time.Microsecond)
	current := biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact,
		Version: 1, Status: biz.AgentDraft, UpdatedAt: loadedAt,
	}
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(current, nil)
	fixture.agents.EXPECT().UpdateAgent(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, agent biz.Agent, _ biz.OutboxEvent) (biz.Agent, error) {
			if !reflect.DeepEqual(agent.UserSkillOptOuts, []uuid.UUID{skillID}) {
				t.Fatalf("opt-outs = %#v, want [%s]", agent.UserSkillOptOuts, skillID)
			}
			if !agent.UpdatedAt.Equal(loadedAt) {
				t.Fatalf("updated_at = %s, want %s", agent.UpdatedAt, loadedAt)
			}
			return agent, nil
		})
	response, err := grpcServer.UpdateAgent(biz.WithPrincipal(context.Background(), fixture.principal), &harnessv1.UpdateAgentRequest{
		Id: agentID.String(), Mode: "react", UserSkillOptouts: []string{skillID.String()},
		UpdatedAt: timestamppb.New(loadedAt),
	})
	if err != nil || response.GetAgent().GetId() != agentID.String() {
		t.Fatalf("UpdateAgent() = %#v, %v", response, err)
	}
}

// TestGRPCAgentGetReturnsInheritableUserSkills 验证草稿读取返回可继承的自动技能。
func TestGRPCAgentGetReturnsInheritableUserSkills(t *testing.T) {
	fixture := newPlatformCRUDFixture(t, biz.RoleUser)
	grpcServer := &platformGRPCServer{platform: fixture.platform}
	userID, agentID, siblingID := fixture.principal.UserID, uuid.New(), uuid.New()
	skillID := uuid.New()
	draft := biz.Agent{
		ID: agentID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact,
		Version: 2, Status: biz.AgentDraft,
	}
	sibling := biz.Agent{
		ID: siblingID, UserID: userID, Name: "assistant", Mode: biz.AgentModeReact,
		Version: 1, Status: biz.AgentPublished, UserSkillIDs: []uuid.UUID{skillID},
	}
	skill := biz.UserSkill{ID: skillID, UserID: userID, Name: "weekly-report", Status: biz.UserSkillPublished, Origin: biz.UserSkillOriginAuto}
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, agentID).Return(draft, nil).Times(2)
	fixture.agents.EXPECT().ListPublishedAgentIDsByName(gomock.Any(), userID, "assistant").Return([]uuid.UUID{siblingID}, nil)
	fixture.agents.EXPECT().GetAgent(gomock.Any(), userID, siblingID).Return(sibling, nil)
	fixture.agents.EXPECT().ListUserSkillOptOuts(gomock.Any(), userID, agentID).Return(nil, nil)
	fixture.agents.EXPECT().ListUserSkillsByIDs(gomock.Any(), userID, []uuid.UUID{skillID}).Return([]biz.UserSkill{skill}, nil)
	response, err := grpcServer.GetAgent(biz.WithPrincipal(context.Background(), fixture.principal), &harnessv1.GetAgentRequest{Id: agentID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetInheritableUserSkills()) != 1 || response.GetInheritableUserSkills()[0].GetName() != "weekly-report" {
		t.Fatalf("inheritable skills = %#v", response.GetInheritableUserSkills())
	}
}

// ptrUUID 返回 UUID 指针，供表驱动用例使用。
func ptrUUID(id uuid.UUID) *uuid.UUID {
	return &id
}
