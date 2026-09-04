package agent

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

const skillDoc = `---
name: web-debug
description: 排查网页抓取失败问题，仅在 web_fetch 报错时触发
---

# Web Debug

步骤化正文（懒加载，触发时才经 read_file 读取）。
`

const skillDocFull = `---
name: git-commit-helper
description: 辅助提交 Git commit
schema_version: "1"
when_to_use: 用户请求提交时
allowed-tools:
  - git
  - read_file
references:
  - references/commit-types.md
source: local
user-invocable: true
---

# Body markdown
`

func TestParseSkillFile_Full(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SKILL.md"), skillDocFull)

	s, ok := parseSkillFile(filepath.Join(dir, "SKILL.md"))
	if !ok {
		t.Fatal("parseSkillFile returned !ok")
	}
	if s.Name != "git-commit-helper" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q", s.SchemaVersion)
	}
	if s.WhenToUse != "用户请求提交时" {
		t.Errorf("WhenToUse = %q", s.WhenToUse)
	}
	if !reflect.DeepEqual(s.AllowedTools, []string{"git", "read_file"}) {
		t.Errorf("AllowedTools = %v", s.AllowedTools)
	}
	if !reflect.DeepEqual(s.References, []string{"references/commit-types.md"}) {
		t.Errorf("References = %v", s.References)
	}
	if s.Source != "local" {
		t.Errorf("Source = %q", s.Source)
	}
	if !s.UserInvocable {
		t.Errorf("UserInvocable = false")
	}
}

func TestParseSkillFile_Minimal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SKILL.md"), skillDoc) // 旧 fixture

	s, ok := parseSkillFile(filepath.Join(dir, "SKILL.md"))
	if !ok {
		t.Fatal("parseSkillFile returned !ok for legacy SKILL.md")
	}
	if s.Name != "web-debug" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Description == "" {
		t.Errorf("Description empty")
	}
	if s.SchemaVersion != "" || len(s.AllowedTools) != 0 {
		t.Errorf("unexpected fields populated: %+v", s)
	}
}

func TestParseSkillFile_BadYAML(t *testing.T) {
	dir := t.TempDir()
	// 缺 closing ---
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: x\nno-close\n")

	_, ok := parseSkillFile(filepath.Join(dir, "SKILL.md"))
	if ok {
		t.Error("expected !ok for malformed YAML")
	}
}

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
	// 单段清单，引导用 activate_skill 懒加载
	if !strings.Contains(got, "activate_skill") {
		t.Fatalf("render should mention activate_skill: %q", got)
	}
	if !strings.Contains(got, "- review") {
		t.Fatalf("render = %q", got)
	}
}

// v2.21：paths 字段已移除（清单过滤属业务层），renderSkills 为单段清单。
func TestRenderSkills_SingleList(t *testing.T) {
	s1 := Skill{Name: "git", Description: "git helper"}
	s2 := Skill{Name: "deploy", Description: "deploy helper"}
	out := renderSkills([]Skill{s1, s2})

	if strings.Contains(out, "未命中路径") || strings.Contains(out, "可用技能") {
		t.Errorf("section headers should be gone, got: %s", out)
	}
	for _, want := range []string{"- git", "- deploy", "activate_skill"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in render, got: %s", want, out)
		}
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
