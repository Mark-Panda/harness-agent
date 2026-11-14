package skillpkg

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// buildZip 构造测试用 zip；name 为文件路径，mode 用于构造符号链接。
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buffer.Bytes()
}

const sampleSkill = `---
name: 测试技能
description: 用于验证解析
---

# 测试技能

按步骤执行。
`

func TestParseZipValid(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"SKILL.md":            sampleSkill,
		"scripts/hello.py":    "print('hi')\n",
		"references/guide.md": "# 指南\n",
	})
	pkg, err := ParseZip(archive, "fallback")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pkg.Name != "测试技能" || pkg.Description != "用于验证解析" {
		t.Fatalf("unexpected metadata: %+v", pkg)
	}
	if pkg.Entrypoint != "SKILL.md" || len(pkg.Files) != 3 {
		t.Fatalf("unexpected files: %+v", pkg.Files)
	}
	if !pkg.HasScripts || pkg.TotalBytes == 0 || pkg.Digest == "" {
		t.Fatalf("unexpected bundle stats: %+v", pkg)
	}
	if strings.Contains(pkg.PromptTemplate, "name:") {
		t.Fatalf("frontmatter leaked into prompt: %s", pkg.PromptTemplate)
	}
	for _, file := range pkg.Files {
		if file.Path == "scripts/hello.py" && (!file.IsScript || file.MIME == "") {
			t.Fatalf("script metadata missing: %+v", file)
		}
	}
}

func TestParseZipStripsSingleRoot(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"my-skill/SKILL.md":         sampleSkill,
		"my-skill/scripts/run.sh":   "#!/bin/sh\necho ok\n",
		"my-skill/references/a.txt": "a",
	})
	pkg, err := ParseZip(archive, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pkg.Entrypoint != "SKILL.md" {
		t.Fatalf("root not stripped: %+v", pkg.Files)
	}
	for _, file := range pkg.Files {
		if strings.HasPrefix(file.Path, "my-skill/") {
			t.Fatalf("root prefix remains: %s", file.Path)
		}
	}
}

func TestParseZipRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]string
	}{
		{name: "missing skill", entries: map[string]string{"README.md": "x"}},
		{name: "traversal", entries: map[string]string{"SKILL.md": sampleSkill, "../evil.sh": "rm -rf /"}},
		{name: "absolute", entries: map[string]string{"SKILL.md": sampleSkill, "/etc/passwd": "x"}},
		{name: "backslash", entries: map[string]string{"SKILL.md": sampleSkill, `scripts\run.sh`: "x"}},
		{name: "empty body", entries: map[string]string{"SKILL.md": "---\nname: x\n---\n"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseZip(buildZip(t, testCase.entries), ""); !errors.Is(err, ErrInvalidPackage) {
				t.Fatalf("expected invalid package, got %v", err)
			}
		})
	}
}

func TestParseZipRejectsOversizedScript(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"SKILL.md":       sampleSkill,
		"scripts/big.py": strings.Repeat("a", MaxScriptBytes+1),
	})
	if _, err := ParseZip(archive, ""); !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("expected invalid package, got %v", err)
	}
}

func TestInterpreterAndTextMIME(t *testing.T) {
	if Interpreter("scripts/a.py") != "python3" || Interpreter("a.txt") != "" {
		t.Fatal("interpreter mapping is incorrect")
	}
	if !IsTextMIME("text/markdown; charset=utf-8") || IsTextMIME("image/png") {
		t.Fatal("text mime detection is incorrect")
	}
}
