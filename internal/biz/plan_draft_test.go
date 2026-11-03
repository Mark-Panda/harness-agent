package biz_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

// validPlanJSON 是 Planner 合法输出样例。
const validPlanJSON = "```json\n" + `{"goal":"整理季度报告","steps":[
{"step_no":1,"title":"收集数据","description":"汇总销售数据","depends_on":[],"tool_name":"","arguments":{}},
{"step_no":2,"title":"生成摘要","description":"输出摘要","depends_on":[1],"tool_name":"","arguments":{}}
]}` + "\n```"

func TestParsePlanDraftValid(t *testing.T) {
	draft, err := biz.ParsePlanDraft(validPlanJSON, biz.DefaultPlanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if draft.Goal != "整理季度报告" || len(draft.Steps) != 2 || draft.Steps[1].DependsOn[0] != 1 {
		t.Fatalf("unexpected draft: %#v", draft)
	}
}

func TestParsePlanDraftRejectsInvalid(t *testing.T) {
	limits := biz.DefaultPlanLimits()
	cases := map[string]string{
		"not_json":        "no object here",
		"unknown_field":   `{"goal":"g","steps":[{"step_no":1,"title":"t","extra":true}]}`,
		"non_consecutive": `{"goal":"g","steps":[{"step_no":2,"title":"t"}]}`,
		"forward_dep":     `{"goal":"g","steps":[{"step_no":1,"title":"a"},{"step_no":2,"title":"b","depends_on":[3]}]}`,
		"self_dep":        `{"goal":"g","steps":[{"step_no":1,"title":"a","depends_on":[1]}]}`,
		"duplicate_dep":   `{"goal":"g","steps":[{"step_no":1,"title":"a"},{"step_no":2,"title":"b","depends_on":[1,1]}]}`,
		"empty_title":     `{"goal":"g","steps":[{"step_no":1,"title":"  "}]}`,
		"empty_goal":      `{"goal":"","steps":[{"step_no":1,"title":"a"}]}`,
	}
	for name, raw := range cases {
		if _, err := biz.ParsePlanDraft(raw, limits); !errors.Is(err, biz.ErrInvalidConfig) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

func TestParsePlanDraftDepthLimit(t *testing.T) {
	steps := make([]string, 0, 7)
	for index := 1; index <= 7; index++ {
		dependency := ""
		if index > 1 {
			dependency = `"depends_on":[` + itoa(index-1) + `],`
		}
		steps = append(steps, `{"step_no":`+itoa(index)+`,"title":"s",`+dependency+`"description":"d"}`)
	}
	raw := `{"goal":"g","steps":[` + strings.Join(steps, ",") + `]}`
	if _, err := biz.ParsePlanDraft(raw, biz.DefaultPlanLimits()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("depth limit error = %v", err)
	}
}

func TestParsePlanDraftStepLimit(t *testing.T) {
	steps := make([]string, 0, 51)
	for index := 1; index <= 51; index++ {
		steps = append(steps, `{"step_no":`+itoa(index)+`,"title":"s"}`)
	}
	raw := `{"goal":"g","steps":[` + strings.Join(steps, ",") + `]}`
	if _, err := biz.ParsePlanDraft(raw, biz.DefaultPlanLimits()); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("step limit error = %v", err)
	}
}

func TestParsePlanDraftArgumentLimit(t *testing.T) {
	limits := biz.DefaultPlanLimits()
	limits.MaxArgumentsBytes = 16
	raw := `{"goal":"g","steps":[{"step_no":1,"title":"s","arguments":{"blob":"` + strings.Repeat("x", 20) + `"}}]}`
	if _, err := biz.ParsePlanDraft(raw, limits); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("argument limit error = %v", err)
	}
}

// itoa 在测试中避免额外依赖 strconv。
func itoa(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}
