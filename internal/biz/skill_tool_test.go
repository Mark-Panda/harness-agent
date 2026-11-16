package biz

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newSkillTestRegistry 构造带单个技能的工具注册表与清理函数。
func newSkillTestRegistry(t *testing.T, files []SkillBundleFile, config SkillExecutionConfig) (*skillToolRegistry, func()) {
	t.Helper()
	skills, cleanup, err := materializeSkillBundles(files)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return newSkillToolRegistry(skills, config, make(chan struct{}, 2)), cleanup
}

// skillFile 构造测试文件。
func skillFile(skillID uuid.UUID, path, content string) SkillBundleFile {
	isScript := strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".py")
	return SkillBundleFile{
		Owner: SkillFileOwnerUser, SkillID: skillID, SkillName: "测试技能",
		Path: path, Content: []byte(content), MIME: mimeForTest(path),
		SizeBytes: int64(len(content)), IsScript: isScript, ExecutionEnabled: isScript,
	}
}

// mimeForTest 返回测试文件的媒体类型。
func mimeForTest(path string) string {
	if strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".py") {
		return "text/x-shellscript; charset=utf-8"
	}
	return "text/markdown; charset=utf-8"
}

func TestMaterializeRejectsTraversal(t *testing.T) {
	_, _, err := materializeSkillBundles([]SkillBundleFile{{
		Owner: SkillFileOwnerUser, SkillID: uuid.New(), SkillName: "x",
		Path: "../evil.sh", Content: []byte("x"),
	}})
	if err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestSkillRegistryRespectsExecutionSwitch(t *testing.T) {
	skillID := uuid.New()
	files := []SkillBundleFile{
		skillFile(skillID, "SKILL.md", "# 说明"),
		skillFile(skillID, "scripts/run.sh", "#!/bin/sh\necho hi\n"),
	}
	disabled, cleanup := newSkillTestRegistry(t, files, SkillExecutionConfig{AllowLocalExecution: false})
	defer cleanup()
	if _, ok := disabled.Lookup("skill_run_script"); ok {
		t.Fatal("run tool must not be registered when execution is disabled")
	}
	if _, ok := disabled.Lookup("skill_read_file"); !ok {
		t.Fatal("read tool must always be registered")
	}

	enabled, cleanupEnabled := newSkillTestRegistry(t, files, SkillExecutionConfig{AllowLocalExecution: true})
	defer cleanupEnabled()
	if _, ok := enabled.Lookup("skill_run_script"); !ok {
		t.Fatal("run tool must be registered when execution is enabled")
	}
}

func TestSkillRegistryRespectsSkillExecutionToggle(t *testing.T) {
	skillID := uuid.New()
	disabled := skillFile(skillID, "scripts/run.sh", "#!/bin/sh\necho hi\n")
	disabled.ExecutionEnabled = false
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "SKILL.md", "# 说明"),
		disabled,
	}, SkillExecutionConfig{AllowLocalExecution: true})
	defer cleanup()
	if _, ok := registry.Lookup("skill_run_script"); ok {
		t.Fatal("run tool must not be registered when the skill execution toggle is off")
	}
}

func TestSkillReadFile(t *testing.T) {
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "references/guide.md", "# 指南"),
	}, SkillExecutionConfig{})
	defer cleanup()
	tool, ok := registry.Lookup("skill_read_file")
	if !ok {
		t.Fatal("read tool missing")
	}
	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"references/guide.md"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(output), "指南") {
		t.Fatalf("unexpected content: %s", output)
	}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"../etc/passwd"}`)); err == nil {
		t.Fatal("missing file must be rejected")
	}
}

func TestSkillRunScript(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "scripts/hello.sh", "#!/bin/sh\necho hello-from-skill\n"),
	}, SkillExecutionConfig{AllowLocalExecution: true, Timeout: 5 * time.Second, MaxOutputBytes: 1024})
	defer cleanup()
	tool, ok := registry.Lookup("skill_run_script")
	if !ok {
		t.Fatal("run tool missing")
	}
	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"scripts/hello.sh"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result["status"] != "ok" || !strings.Contains(result["output"].(string), "hello-from-skill") {
		t.Fatalf("unexpected run result: %s", output)
	}
}

func TestSkillRunScriptTimeoutAndTruncate(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "scripts/slow.sh", "#!/bin/sh\nsleep 5\n"),
		skillFile(skillID, "scripts/loud.sh", "#!/bin/sh\necho 012345678901234567890123456789\n"),
	}, SkillExecutionConfig{AllowLocalExecution: true, Timeout: 200 * time.Millisecond, MaxOutputBytes: 8})
	defer cleanup()
	tool, _ := registry.Lookup("skill_run_script")

	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"scripts/slow.sh"}`))
	if !errors.Is(err, ErrToolResultPersistence) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must require reconciliation: output=%s error=%v", output, err)
	}
	if !strings.Contains(string(output), `"status":"timeout"`) {
		t.Fatalf("expected bounded timeout diagnostics: %s", output)
	}

	output, err = tool.Invoke(context.Background(), json.RawMessage(`{"path":"scripts/loud.sh"}`))
	if err != nil {
		t.Fatalf("loud run: %v", err)
	}
	if !strings.Contains(string(output), `"truncated":true`) {
		t.Fatalf("expected truncated result: %s", output)
	}
}

func TestSkillRunScriptCancellationAfterStartRequiresReconciliation(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "scripts/slow.sh", "#!/bin/sh\nprintf started > started.marker\nsleep 5\n"),
	}, SkillExecutionConfig{AllowLocalExecution: true, Timeout: 10 * time.Second, MaxOutputBytes: 1024})
	defer cleanup()
	tool := registry.bindings["skill_run_script"].(*skillTool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type invocation struct {
		output json.RawMessage
		err    error
	}
	done := make(chan invocation, 1)
	go func() {
		output, err := tool.Invoke(ctx, json.RawMessage(`{"path":"scripts/slow.sh"}`))
		done <- invocation{output: output, err: err}
	}()
	marker := filepath.Join(tool.skills[0].Root, "started.marker")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			result := <-done
			t.Fatalf("script did not start: output=%s error=%v", result.output, result.err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	result := <-done
	if !errors.Is(result.err, ErrToolResultPersistence) || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled script must require reconciliation: output=%s error=%v", result.output, result.err)
	}
	if !strings.Contains(string(result.output), `"status":"cancelled"`) {
		t.Fatalf("expected bounded cancellation diagnostics: %s", result.output)
	}
}

func TestSkillRunScriptCancellationWhileWaitingDoesNotRequireReconciliation(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}
	for _, scope := range []string{"local", "global"} {
		t.Run(scope, func(t *testing.T) {
			skillID := uuid.New()
			registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
				skillFile(skillID, "scripts/run.sh", "#!/bin/sh\nexit 0\n"),
			}, SkillExecutionConfig{AllowLocalExecution: true, Timeout: time.Second})
			defer cleanup()
			tool := registry.bindings["skill_run_script"].(*skillTool)
			switch scope {
			case "local":
				tool.sem <- struct{}{}
				defer func() { <-tool.sem }()
			case "global":
				for len(tool.globalSem) < cap(tool.globalSem) {
					tool.globalSem <- struct{}{}
				}
				defer func() {
					for len(tool.globalSem) > 0 {
						<-tool.globalSem
					}
				}()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			output, err := tool.Invoke(ctx, json.RawMessage(`{"path":"scripts/run.sh"}`))
			if output != nil || !errors.Is(err, context.Canceled) || errors.Is(err, ErrToolResultPersistence) {
				t.Fatalf("cancellation before start: output=%s error=%v", output, err)
			}
		})
	}
}

func TestSkillRunScriptDeterministicFailuresDoNotRequireReconciliation(t *testing.T) {
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "scripts/fail.sh", "#!/bin/sh\nexit 7\n"),
		skillFile(skillID, "scripts/missing.py", "print('unreachable')\n"),
	}, SkillExecutionConfig{AllowLocalExecution: true, Timeout: time.Second})
	defer cleanup()
	tool := registry.bindings["skill_run_script"].(*skillTool)

	output, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"scripts/fail.sh"}`))
	if err != nil || errors.Is(err, ErrToolResultPersistence) ||
		!strings.Contains(string(output), `"status":"failed"`) || !strings.Contains(string(output), `"exit_code":7`) {
		t.Fatalf("ordinary non-zero exit: output=%s error=%v", output, err)
	}

	t.Setenv("PATH", t.TempDir())
	output, err = tool.Invoke(context.Background(), json.RawMessage(`{"path":"scripts/missing.py"}`))
	if err != nil || errors.Is(err, ErrToolResultPersistence) || !strings.Contains(string(output), `"status":"unavailable"`) {
		t.Fatalf("interpreter start failure: output=%s error=%v", output, err)
	}
}

func TestSkillToolIdentity(t *testing.T) {
	tool := &skillTool{kind: skillToolRun}
	identity := tool.MCPIdentity()
	if identity.ServerID == uuid.Nil || identity.OriginalName == "" || identity.EndpointFingerprint == "" {
		t.Fatalf("identity is incomplete: %+v", identity)
	}
	if tool.RiskLevel() != "high" {
		t.Fatalf("run script must be high risk, got %s", tool.RiskLevel())
	}
}

func TestSnapshotUserSkillsInjected(t *testing.T) {
	snapshot := map[string]any{
		"user_skills": []any{map[string]any{
			"id": "c437dc03-63fd-46ac-919d-436df6843098", "name": "测试技能",
			"description": "用于测试", "prompt_template": "按步骤执行。",
		}},
	}
	skills := snapshotUserSkills(snapshot)
	if len(skills) != 1 || skills[0].Name != "测试技能" || skills[0].PromptTemplate != "按步骤执行。" {
		t.Fatalf("unexpected user skills: %+v", skills)
	}
	if skillIDs(skills)[0] == uuid.Nil {
		t.Fatal("user skill id must be parsed")
	}
	var builder strings.Builder
	appendSkillSection(&builder, skills)
	if !strings.Contains(builder.String(), "测试技能") || !strings.Contains(builder.String(), "按步骤执行。") {
		t.Fatalf("user skill prompt not injected: %s", builder.String())
	}
}

func TestSnapshotLegacySkillKeys(t *testing.T) {
	snapshot := map[string]any{
		"skills": []any{map[string]any{
			"ID": "c437dc03-63fd-46ac-919d-436df6843098", "Name": "历史技能",
			"PromptTemplate": "历史模板",
		}},
	}
	skills := snapshotSkills(snapshot)
	if len(skills) != 1 || skills[0].Name != "历史技能" || skills[0].PromptTemplate != "历史模板" {
		t.Fatalf("legacy snapshot not decoded: %+v", skills)
	}
}

func TestJoinSkillRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := joinSkillRoot(root, "scripts/a.sh"); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if _, err := joinSkillRoot(root, "../a.sh"); err == nil {
		t.Fatal("traversal must be rejected")
	}
	if _, err := joinSkillRoot(filepath.Join(root, "x"), "/etc/passwd"); err == nil {
		t.Fatal("absolute path must be rejected")
	}
}

func TestSkillListSnapshotsAndResourcePrompt(t *testing.T) {
	skillID := uuid.New()
	config := SkillExecutionConfig{AllowLocalExecution: true}
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "SKILL.md", "# 说明"),
		skillFile(skillID, "scripts/run.sh", "#!/bin/sh\necho hi\n"),
	}, config)
	defer cleanup()
	if specs := registry.List(); len(specs) != 3 {
		t.Fatalf("registered tool count = %d", len(specs))
	}
	tool, ok := registry.Lookup("skill_list_files")
	if !ok {
		t.Fatal("list tool missing")
	}
	output, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(output), "scripts/run.sh") || !strings.Contains(string(output), `"executable":true`) {
		t.Fatalf("list output = %s, %v", output, err)
	}
	prompt := skillResourcePrompt(tool.(*skillTool).skills, config)
	for _, want := range []string{"测试技能", "SKILL.md", "scripts/run.sh", "skill_run_script"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("resource prompt missing %q: %s", want, prompt)
		}
	}

	snapshot := map[string]any{"published_snapshot": map[string]any{
		"skills": []any{map[string]any{"id": skillID.String(), "name": "平台技能"}},
		"user_skills": []any{map[string]any{
			"id": "c437dc03-63fd-46ac-919d-436df6843098", "name": "用户技能",
		}},
	}}
	platformSkills, userSkills := snapshotBoundSkills(snapshot)
	if len(platformSkills) != 1 || len(userSkills) != 1 || skillIDs(platformSkills)[0] != skillID {
		t.Fatalf("snapshot skills were not decoded: %#v %#v", platformSkills, userSkills)
	}
	if platformSkills, userSkills := snapshotBoundSkills(nil); platformSkills != nil || userSkills != nil {
		t.Fatal("nil snapshot must not produce skill bindings")
	}
	if decoded := decodeSnapshotSkills(func() {}); decoded != nil {
		t.Fatalf("unsupported snapshot value decoded as %#v", decoded)
	}
}

func TestCombineToolRegistries(t *testing.T) {
	skillID := uuid.New()
	registry, cleanup := newSkillTestRegistry(t, []SkillBundleFile{
		skillFile(skillID, "SKILL.md", "# 说明"),
	}, SkillExecutionConfig{})
	defer cleanup()
	if specs := combineToolRegistries(nil).List(); len(specs) != 0 {
		t.Fatalf("empty registry contains %#v", specs)
	}
	if specs := combineToolRegistries(registry).List(); len(specs) != 2 {
		t.Fatalf("single registry contains %#v", specs)
	}
	combined := combineToolRegistries(nil, registry, registry)
	if specs := combined.List(); len(specs) != 2 {
		t.Fatalf("duplicate tool names were not removed: %#v", specs)
	}
	if _, ok := combined.Lookup("skill_read_file"); !ok {
		t.Fatal("combined registry did not forward lookup")
	}
	if _, ok := combined.Lookup("missing"); ok {
		t.Fatal("unknown tool must not be found")
	}
}
