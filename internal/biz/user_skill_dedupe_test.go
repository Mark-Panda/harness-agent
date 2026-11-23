package biz

import (
	"testing"

	"github.com/google/uuid"
)

const duplicateExistingPrompt = `你是教育平板管控系统的排查助手。用户反馈：{{issue_description}}。涉及设备SN：{{device_sns}}。时间范围：{{time_range}}。

请按以下流程排查，每一步都要给出证据来源（日志/数据库/代码），无法证实的推断必须明确标注“未证实”：

1. SN校验：核对SN字符是否与系统格式一致（注意 I/l、0/O 等易混字符），不一致先要求用户确认。
2. 设备当前状态：查设备-用户绑定关系表，输出每台SN的绑定用户、绑定生效时间、online_status、last_heartbeat_time、screen_status。
3. 操作历史定位：在操作历史表中按SN和时间窗查锁屏/解锁记录，输出时间线表格：时间、操作类型、操作人、层级、范围台数、成功/失败/离线计数、记录ID。
4. 批量名单对比：若涉及多次批量下发，取各次完整SN名单，计算交集、各自独有集合，指出用户预期数量与实际差异的原因。
5. 根因归类：将问题归入以下类别之一或组合，并说明依据：指令未下发、设备离线、绑定抖动、锁了又解、记录粒度不足。
6. 证据边界声明：明确列出“能确认的事实”与“不能确认的推断”，撤回无证据支持的结论。
7. 建议动作：给出短期规避、中期修复与长期产品改进。`

const duplicateCandidatePrompt = `你是生产环境问题排查助手。请针对以下设备管控异常进行证据化排查：

问题描述：{{problem_description}}
设备标识：{{device_ids}}
时间范围：{{time_range}}
可用数据源/工具：{{data_sources}}
系统与业务上下文：{{system_context}}

排查要求：
1. 先核对设备标识字符是否准确，说明标识错误可能导致的假象；无法确认时明确标注。
2. 按链路排查：指令是否发出、是否到达设备、是否仅个别设备异常、是否存在其他操作覆盖或误操作。
3. 只基于可获取的证据下结论。每条关键结论注明数据来源；区分“已确认事实”“合理推断”“无法确认”。
4. 对批量操作，输出时间线，并对比不同批次/范围的设备名单交集、差集与覆盖情况；不要预设数量差异。
5. 若日志查询返回空结果，先判断是“无记录”还是“查询通道异常”，检查扫描量、权限、时间窗等指标。
6. 若发现先前结论有误，明确更正并说明原因。
7. 最终输出：结论摘要、关键证据与时间线、名单覆盖分析、不确定性说明、建议的验证或处置动作。
8. 不编造数据，不泄露个人隐私，不执行与排查无关的权限变更。`

func TestFindDuplicateSkillMatchesSameSourceAndSimilarPrompt(t *testing.T) {
	sourceIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	existing := []UserSkill{{
		ID: uuid.New(), Name: "tablet-lock-issue-investigation",
		Description:      "面向教育平板管控系统的锁屏/解锁异常排查工作流。",
		PromptTemplate:   duplicateExistingPrompt,
		SourceSessionIDs: sourceIDs,
	}}
	candidate := SkillCandidate{
		Name:           "device-control-issue-forensics",
		Description:    "用于排查设备批量管控指令未生效或异常触发的问题。",
		PromptTemplate: duplicateCandidatePrompt,
	}
	t.Logf("prompt containment=%f", tokenContainment(skillTokenSet(candidate.PromptTemplate), skillTokenSet(existing[0].PromptTemplate)))
	if matched, ok := findDuplicateSkill(candidate, sourceIDs, existing); !ok || matched.Name != existing[0].Name {
		t.Fatalf("expected duplicate match, got %#v, %v", matched, ok)
	}
}

func TestFindDuplicateSkillNormalizesName(t *testing.T) {
	existing := []UserSkill{{Name: "Tablet_Lock_Issue"}}
	candidate := SkillCandidate{Name: "tablet-lock-issue", PromptTemplate: "short"}
	if matched, ok := findDuplicateSkill(candidate, nil, existing); !ok || matched.Name != "Tablet_Lock_Issue" {
		t.Fatalf("expected normalized name match, got %#v, %v", matched, ok)
	}
}

func TestFindDuplicateSkillMatchesDescriptionAndPrompt(t *testing.T) {
	existing := []UserSkill{{
		Name:           "existing-skill",
		Description:    "把本周零散工作整理成结构化周报，适用于每周五收工时。",
		PromptTemplate: duplicateExistingPrompt,
	}}
	candidate := SkillCandidate{
		Name:           "weekly-report",
		Description:    "把本周零散工作整理成结构化周报，适用于每周五收工时。",
		PromptTemplate: duplicateExistingPrompt,
	}
	if _, ok := findDuplicateSkill(candidate, nil, existing); !ok {
		t.Fatal("expected description/prompt duplicate match")
	}
}

func TestFindDuplicateSkillRejectsDifferentSkill(t *testing.T) {
	sourceIDs := []uuid.UUID{uuid.New()}
	existing := []UserSkill{{
		Name:             "tablet-lock-issue-investigation",
		Description:      "锁屏异常排查。",
		PromptTemplate:   duplicateExistingPrompt,
		SourceSessionIDs: sourceIDs,
	}}
	candidate := SkillCandidate{
		Name:           "weekly-report",
		Description:    "把本周零散工作整理成结构化周报，适用于每周五收工时。",
		PromptTemplate: "## 何时使用\n每周五使用。\n\n## 步骤\n1. 汇总工作。\n2. 输出周报。\n\n## 输出格式\nMarkdown。\n\n## 约束\n不得编造。",
	}
	if _, ok := findDuplicateSkill(candidate, []uuid.UUID{uuid.New()}, existing); ok {
		t.Fatal("unrelated skill must not be treated as duplicate")
	}
}

func TestSkillTokenSetHandlesChineseBigramsAndEnglishWords(t *testing.T) {
	tokens := skillTokenSet("设备 online_status 排查")
	for _, want := range []string{"设备", "online", "status"} {
		if _, ok := tokens[want]; !ok {
			t.Fatalf("token %q missing: %#v", want, tokens)
		}
	}
}
