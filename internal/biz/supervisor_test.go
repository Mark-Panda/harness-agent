package biz_test

import (
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
)

func TestParseSupervisorDraft(t *testing.T) {
	valid := `{"workers":[{"name":"数据","task":"收集数据","tools":[]},{"name":"总结","task":"生成摘要","tools":[]}]}`
	draft, err := biz.ParseSupervisorDraft(valid, 4)
	if err != nil || len(draft.Workers) != 2 || draft.Workers[1].Name != "总结" {
		t.Fatalf("ParseSupervisorDraft() = %#v, %v", draft, err)
	}
	cases := map[string]string{
		"empty":           `{"workers":[]}`,
		"too_many":        `{"workers":[{"name":"a","task":"t"},{"name":"b","task":"t"},{"name":"c","task":"t"},{"name":"d","task":"t"},{"name":"e","task":"t"}]}`,
		"duplicate_name":  `{"workers":[{"name":"a","task":"t"},{"name":"a","task":"t"}]}`,
		"empty_task":      `{"workers":[{"name":"a","task":"  "}]}`,
		"unknown_field":   `{"workers":[{"name":"a","task":"t","extra":true}]}`,
		"duplicate_tools": `{"workers":[{"name":"a","task":"t","tools":["x","x"]}]}`,
	}
	for name, raw := range cases {
		if _, err := biz.ParseSupervisorDraft(raw, 4); !errors.Is(err, biz.ErrSupervisorInvalid) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if _, err := biz.ParseSupervisorDraft(valid, 1); !errors.Is(err, biz.ErrSupervisorInvalid) {
		t.Fatalf("max workers error = %v", err)
	}
	if _, err := biz.ParseSupervisorDraft("no json", 4); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("non json error = %v", err)
	}
	if !biz.SupervisorTaskTerminal(biz.SupervisorTaskCompleted) || biz.SupervisorTaskTerminal(biz.SupervisorTaskRunning) {
		t.Fatal("terminal status helper mismatch")
	}
}
