package mos_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/contextfile"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// writeSkillFile 写一个技能（目录 + SKILL.md）。
func writeSkillFile(t *testing.T, dir, front, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %q: %v", dir, err)
	}
	content := "---\n" + front + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写技能 %q: %v", dir, err)
	}
}

// skillCallingModel 第一轮请求 skill 工具，第二轮给最终作答。
type skillCallingModel struct {
	calls int
	seen  []llm.Message
}

func (m *skillCallingModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	m.calls++
	m.seen = append(m.seen, req.Messages...)
	if m.calls == 1 {
		return &llm.Response{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "skill", Args: `{"name":"deploy"}`}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return &llm.Response{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "已按技能执行"},
		FinishReason: "stop",
	}, nil
}

func (m *skillCallingModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *skillCallingModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }

// 装配：技能清单进系统提示词、skill 工具注册、快照写明技能。
func TestSkillsWiredIntoSystemPromptAndTool(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")

	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.RunContext().SystemPrompt
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("系统提示词应含技能清单：\n%s", got)
	}
	if !strings.Contains(got, "- deploy: 部署流程") {
		t.Fatalf("系统提示词应含技能名与描述：\n%s", got)
	}
	if _, ok := a.RunContext().Tools.Get("skill"); !ok {
		t.Fatal("应注册 skill 工具")
	}
	snap := a.Describe()
	if !strings.Contains(snap, "技能: 1 个") || !strings.Contains(snap, "deploy") {
		t.Fatalf("装配快照应写明技能：\n%s", snap)
	}
}

// 没有技能就不装 skill 工具（能力即证据）。
func TestNoSkillsNoSkillTool(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.RunContext().Tools.Get("skill"); ok {
		t.Fatal("没有技能就不该有 skill 工具")
	}
	if !strings.Contains(a.Describe(), "技能: 未装配") {
		t.Fatalf("快照应写明未装配技能：\n%s", a.Describe())
	}
}

// 清单位置：项目约定之后、工具指引之前。
func TestSkillsLandBetweenProjectContextAndToolGuidelines(t *testing.T) {
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "AGENTS.md"), []byte("项目约定正文"), 0o644); err != nil {
		t.Fatalf("写 AGENTS.md: %v", err)
	}
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")

	pb := prompt.New()
	if err := pb.Add(prompt.Section{Name: "tool:read", Order: prompt.OrderToolBase, Text: "工具指引正文"}); err != nil {
		t.Fatalf("加分节: %v", err)
	}
	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Prompt: pb,
		Context: &mos.ContextConfig{
			Dir:     ctxDir,
			Options: contextfile.Options{GlobalDirs: []string{}},
		},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.RunContext().SystemPrompt
	iCtx := strings.Index(got, "项目约定正文")
	iSkills := strings.Index(got, "<available_skills>")
	iTool := strings.Index(got, "工具指引正文")
	if iCtx < 0 || iSkills < 0 || iTool < 0 {
		t.Fatalf("三段都该在：\n%s", got)
	}
	if !(iCtx < iSkills && iSkills < iTool) {
		t.Fatalf("顺序应为 项目约定 → 技能清单 → 工具指引：\n%s", got)
	}
}

// 端到端：模型调 skill → 正文进上下文 → 事件落会话日志。
func TestSkillToolEndToEnd(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")

	m := &skillCallingModel{}
	a, err := mos.New(mos.Config{
		Model:  m,
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	reply, err := a.Run(context.Background(), sess, "帮我部署")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "已按技能执行" {
		t.Fatalf("reply = %q", reply)
	}
	// 技能正文经工具结果进模型上下文。
	found := false
	for _, msg := range m.seen {
		if strings.Contains(msg.Content, "步骤 1") {
			found = true
		}
	}
	if !found {
		t.Fatal("技能正文应进模型上下文")
	}
	// 审计留痕：一条 skill/activate 事件，只记元数据。
	var acts []session.Event
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventSkillActivate {
			acts = append(acts, ev)
		}
	}
	if len(acts) != 1 {
		t.Fatalf("skill/activate 事件 = %d 条", len(acts))
	}
	if acts[0].Skill == nil || acts[0].Skill.Name != "deploy" {
		t.Fatalf("事件载荷 = %+v", acts[0].Skill)
	}
	if acts[0].Skill.Description != "部署流程" {
		t.Fatalf("事件应带描述：%+v", acts[0].Skill)
	}
}

// 宿主显式手势：/name 由宿主调 Agent.ExpandSkill 展开（框架不自动扫文本）。
func TestExpandSkillFromAgent(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")

	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, ok := a.ExpandSkill("/deploy 生产环境")
	if !ok {
		t.Fatal("应命中手势")
	}
	if !strings.Contains(out, "步骤 1") || !strings.Contains(out, "生产环境") {
		t.Fatalf("展开结果：\n%s", out)
	}
	if _, ok := a.ExpandSkill("/nope"); ok {
		t.Fatal("未知技能不该展开")
	}
	if len(a.Skills()) != 1 || a.Skills()[0].Name != "deploy" {
		t.Fatalf("技能清单 = %+v", a.Skills())
	}
}

// 坏技能降级：装配成功，快照里写明诊断。
func TestBrokenSkillDegradesWithWarning(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "bad"), "name: Bad_Name\ndescription: 名字不合法", "正文")

	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("坏技能不该让装配失败: %v", err)
	}
	snap := a.Describe()
	if !strings.Contains(snap, "kebab-case") {
		t.Fatalf("快照应写明降级诊断：\n%s", snap)
	}
}
