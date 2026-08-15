package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
)

const skillDoc = `---
name: web-debug
description: 排查网页抓取失败问题，仅在 web_fetch 报错时触发
---

# Web Debug

步骤化正文（懒加载，触发时才经 read_file 读取）。
`

func TestLoadSkills(t *testing.T) {
	root := t.TempDir()
	// 根目录自身 + 一级子目录各一份 SKILL.md
	writeFile(t, filepath.Join(root, "SKILL.md"), skillDoc)
	writeFile(t, filepath.Join(root, "code-review", "SKILL.md"), `---
name: review
description: 代码审查
---
正文`)

	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %d, want 2: %+v", len(skills), skills)
	}

	web := skills[0]
	if web.Name != "web-debug" || !strings.Contains(web.Description, "web_fetch 报错时触发") {
		t.Fatalf("skill = %+v", web)
	}
	if !strings.Contains(web.Dir, root) {
		t.Fatalf("skill dir = %q, want under %q", web.Dir, root)
	}
}

func TestLoadSkillsNoFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "SKILL.md"), "# 没有 frontmatter 的技能")

	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %+v, want empty (no name in frontmatter)", skills)
	}
}

func TestLoadSkillsMissingDir(t *testing.T) {
	skills, err := LoadSkills(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %+v, want empty", skills)
	}
}

func TestLoadSkillsMissingName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "SKILL.md"), "---\ndescription: 无名字的技能\n---\n正文")
	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skill without name should be skipped: %+v", skills)
	}
}

func TestRenderSkills(t *testing.T) {
	got := renderSkills([]Skill{
		{Name: "web-debug", Description: "排查抓取失败"},
		{Name: "review"},
	})
	if !strings.Contains(got, "web-debug") || !strings.Contains(got, "排查抓取失败") {
		t.Fatalf("render = %q", got)
	}
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "懒加载") {
		t.Fatalf("render should mention lazy loading: %q", got)
	}
	if !strings.Contains(got, "- review") {
		t.Fatalf("render = %q", got)
	}
}

// 组装器集成：配置 SkillsDir 后，技能清单槽位注入懒加载目录。
func TestSystemAssemblerSkills(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "web-debug", "SKILL.md"), skillDoc)

	asm := NewDefaultSystemAssembler(SystemAssemblerConfig{
		Constraints: "你是助手",
		SkillsDir:   root,
	})
	sections, err := asm.Assemble(t.Context(), session.NewSession("s1"), []llm.Tool{{Name: "grep", Description: "文本检索"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	text := RenderSystem(sections)
	if !strings.Contains(text, "【技能清单】") || !strings.Contains(text, "web-debug") {
		t.Fatalf("system = %q, want skills slot", text)
	}
	// 技能正文不注入（懒加载），清单只含 name + description
	if strings.Contains(text, "步骤化正文") {
		t.Fatalf("skill body should not be injected: %q", text)
	}
	// 技能清单位于工具签名之后
	if strings.Index(text, "【技能清单】") < strings.Index(text, "【工具签名】") {
		t.Fatalf("skills slot should follow tools: %q", text)
	}
}
