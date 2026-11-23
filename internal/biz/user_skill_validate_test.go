package biz

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const validSynthesizedPrompt = `# weekly-report

## 何时使用
每周五需要整理本周工作时使用。

## 输入
{{period}} 的工作记录。

## 步骤
1. 汇总本周完成事项。
2. 列出下周计划。

## 输出格式
Markdown 列表。

## 约束
不得编造未提供的工作内容。
`

func validSynthesizedCandidate() SkillCandidate {
	return SkillCandidate{
		ShouldCreate:   true,
		Confidence:     0.82,
		Name:           "weekly-report",
		Description:    "把本周零散工作整理成结构化周报，适用于每周五收工时。",
		WhenToUse:      "每周五收工前需要汇总本周工作并列出下周计划时使用。",
		PromptTemplate: validSynthesizedPrompt,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"period": map[string]any{"type": "string", "description": "周报覆盖的时间范围"},
			},
			"required": []any{"period"},
		},
		Evidence: []SkillEvidence{
			{Session: "工作", Quote: "请帮我写一份本周工作周报"},
			{Session: "工作", Quote: "下周一计划继续跟进"},
		},
	}
}

func validSynthesisFixture() ([]Session, map[uuid.UUID][]Message) {
	session := Session{ID: uuid.New(), Title: "工作"}
	messages := map[uuid.UUID][]Message{
		session.ID: {
			{Role: MessageUser, Content: "请帮我写一份本周工作周报，时间范围是本周。"},
			{Role: MessageAssistant, Content: "好的，下周一计划继续跟进。"},
		},
	}
	return []Session{session}, messages
}

func TestValidateSynthesizedSkillAcceptsStandardCandidate(t *testing.T) {
	sessions, messages := validSynthesisFixture()
	candidate := validSynthesizedCandidate()
	if err := validateSynthesizedSkill(&candidate, sessions, messages); err != nil {
		t.Fatalf("validateSynthesizedSkill() error = %v", err)
	}
	if !strings.HasPrefix(candidate.PromptTemplate, "# weekly-report") {
		t.Fatalf("prompt title not composed: %s", candidate.PromptTemplate)
	}
}

func TestValidateSynthesizedSkillComposesMissingTitleAndWhenToUse(t *testing.T) {
	sessions, messages := validSynthesisFixture()
	candidate := validSynthesizedCandidate()
	candidate.PromptTemplate = strings.Replace(validSynthesizedPrompt, "# weekly-report\n\n", "", 1)
	candidate.PromptTemplate = strings.Replace(candidate.PromptTemplate, "## 何时使用\n每周五需要整理本周工作时使用。\n", "", 1)
	if err := validateSynthesizedSkill(&candidate, sessions, messages); err != nil {
		t.Fatalf("validateSynthesizedSkill() error = %v", err)
	}
	for _, want := range []string{"# weekly-report", "## 何时使用", candidate.WhenToUse} {
		if !strings.Contains(candidate.PromptTemplate, want) {
			t.Fatalf("composed prompt missing %q: %s", want, candidate.PromptTemplate)
		}
	}
}

func TestValidateSynthesizedSkillRejectsInvalidCandidates(t *testing.T) {
	sessions, messages := validSynthesisFixture()
	cases := []struct {
		name   string
		mutate func(*SkillCandidate)
	}{
		{name: "bad name", mutate: func(candidate *SkillCandidate) { candidate.Name = "Weekly_Report" }},
		{name: "short description", mutate: func(candidate *SkillCandidate) { candidate.Description = "太短" }},
		{name: "long description", mutate: func(candidate *SkillCandidate) { candidate.Description = strings.Repeat("描", 1025) }},
		{name: "short when to use", mutate: func(candidate *SkillCandidate) { candidate.WhenToUse = "太短" }},
		{name: "missing steps heading", mutate: func(candidate *SkillCandidate) {
			candidate.PromptTemplate = strings.Replace(candidate.PromptTemplate, "## 步骤", "## 过程", 1)
		}},
		{name: "prompt too long", mutate: func(candidate *SkillCandidate) {
			candidate.PromptTemplate = candidate.PromptTemplate + strings.Repeat("步骤内容", 1200)
		}},
		{name: "placeholder without parameters", mutate: func(candidate *SkillCandidate) {
			candidate.Parameters = map[string]any{}
		}},
		{name: "parameters missing type", mutate: func(candidate *SkillCandidate) {
			candidate.Parameters = map[string]any{"properties": map[string]any{}}
		}},
		{name: "required not declared", mutate: func(candidate *SkillCandidate) {
			candidate.Parameters["required"] = []any{"missing"}
		}},
		{name: "property missing description", mutate: func(candidate *SkillCandidate) {
			candidate.Parameters["properties"] = map[string]any{"period": map[string]any{"type": "string"}}
		}},
		{name: "undeclared placeholder", mutate: func(candidate *SkillCandidate) {
			candidate.PromptTemplate = candidate.PromptTemplate + "\n{{unknown}}\n"
		}},
		{name: "missing evidence", mutate: func(candidate *SkillCandidate) {
			candidate.Evidence = candidate.Evidence[:1]
		}},
		{name: "short evidence quote", mutate: func(candidate *SkillCandidate) {
			candidate.Evidence = []SkillEvidence{{Session: "工作", Quote: "周报"}, {Session: "工作", Quote: "计划"}}
		}},
		{name: "unknown evidence session", mutate: func(candidate *SkillCandidate) {
			candidate.Evidence = []SkillEvidence{{Session: "不存在", Quote: "请帮我写一份本周工作周报"}, {Session: "工作", Quote: "下周一计划继续跟进"}}
		}},
		{name: "evidence quote not found", mutate: func(candidate *SkillCandidate) {
			candidate.Evidence = []SkillEvidence{{Session: "工作", Quote: "这段内容没有出现过"}, {Session: "工作", Quote: "下周一计划继续跟进"}}
		}},
		{name: "sensitive token", mutate: func(candidate *SkillCandidate) {
			candidate.PromptTemplate += "\nsk-abcdefghijklmnopqrstuvwxyz\n"
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := validSynthesizedCandidate()
			testCase.mutate(&candidate)
			if err := validateSynthesizedSkill(&candidate, sessions, messages); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateSkillParametersAllowsEmptyParametersWithoutPlaceholders(t *testing.T) {
	if err := validateSkillParameters(map[string]any{}, "## 步骤\n1. 执行。"); err != nil {
		t.Fatalf("validateSkillParameters() error = %v", err)
	}
}

func TestSynthesizedSkillWarnings(t *testing.T) {
	candidate := validSynthesizedCandidate()
	candidate.Description = strings.Repeat("描", 600)
	candidate.Parameters["required"] = []any{"period", "unused"}
	candidate.Parameters["properties"].(map[string]any)["unused"] = map[string]any{"type": "string", "description": "未使用参数"}
	warnings := synthesizedSkillWarnings(candidate)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v", warnings)
	}
	joined := strings.Join(warnings, "；")
	if !strings.Contains(joined, "500") || !strings.Contains(joined, "unused") {
		t.Fatalf("unexpected warnings: %s", joined)
	}
}
