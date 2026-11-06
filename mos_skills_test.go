package mos_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// S8.4-D：allowed-tools × 可见工具集求交诊断（只诊断不收窄）——缺失写进
// skill/allowed-tools-mismatch 事件与 Describe() 快照；不进模型可见返回值。
func TestAllowedToolsMismatchDiagnostic(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"),
		"name: deploy\ndescription: 部署流程\nallowed-tools:\n  - gone_tool", "步骤 1")

	m := &skillCallingModel{}
	a, err := mos.New(mos.Config{
		Model:  m,
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "帮我部署"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 缺失写进事件。
	var mismatches []session.Event
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventSkillToolMismatch {
			mismatches = append(mismatches, ev)
		}
	}
	if len(mismatches) != 1 {
		t.Fatalf("mismatch 事件 = %d 条", len(mismatches))
	}
	if mismatches[0].Skill == nil || mismatches[0].Skill.Name != "deploy" ||
		!reflect.DeepEqual(mismatches[0].Skill.MissingAllowed, []string{"gone_tool"}) {
		t.Fatalf("事件载荷 = %+v", mismatches[0].Skill)
	}
	// 缺失写进快照（Describe）。
	if !strings.Contains(a.Describe(), "gone_tool") || !strings.Contains(a.Describe(), "不收窄") {
		t.Fatalf("Describe 应写明缺失：\n%s", a.Describe())
	}
	// 工具面**不**被收窄（诊断不改行为）：gone_tool 从未注册。
	if _, ok := a.RunContext().Tools.Get("gone_tool"); ok {
		t.Fatal("声明了没注册的工具，不该被自动注册")
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
	out, ok, err := a.ExpandSkill("/deploy 生产环境")
	if err != nil {
		t.Fatalf("ExpandSkill: %v", err)
	}
	if !ok {
		t.Fatal("应命中手势")
	}
	if !strings.Contains(out, "步骤 1") || !strings.Contains(out, "生产环境") {
		t.Fatalf("展开结果：\n%s", out)
	}
	if _, ok, _ := a.ExpandSkill("/nope"); ok {
		t.Fatal("未知技能不该展开")
	}
	if len(a.Skills()) != 1 || a.Skills()[0].Name != "deploy" {
		t.Fatalf("技能清单 = %+v", a.Skills())
	}
}

// S8.4-C：宿主显式重扫技能目录——幂等、差异通知，且**静态提示词不变**
// （保 prompt cache；清单变化走 user 侧消息）。
func TestReloadSkillsFromAgent(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")

	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := a.RunContext().SystemPrompt

	// 幂等：没改目录 → 无变化、无通知。
	if res := a.ReloadSkills(); res.Changed || res.Notice != "" {
		t.Fatalf("目录没变不该有变化：%+v", res)
	}

	// 新增一个技能 → 有变化 + 通知。
	writeSkillFile(t, filepath.Join(skillDir, "fresh"), "name: fresh\ndescription: 新技能", "新正文")
	res := a.ReloadSkills()
	if !res.Changed {
		t.Fatalf("新增技能应算变化：%+v", res)
	}
	if len(res.Added) != 1 || res.Added[0] != "fresh" {
		t.Fatalf("新增 = %v", res.Added)
	}
	if !strings.Contains(res.Notice, "fresh") {
		t.Fatalf("通知应带新清单：\n%s", res.Notice)
	}
	// 静态提示词**不重渲染**（保 prompt cache）。
	if a.RunContext().SystemPrompt != before {
		t.Fatalf("静态提示词不该因 reload 而变：\n%s", a.RunContext().SystemPrompt)
	}
	// 新技能可加载（工具读的是当前快照）。
	if len(a.Skills()) != 2 {
		t.Fatalf("技能清单 = %+v", a.Skills())
	}
}

// S8.4-C：正文调用时现读——改了 SKILL.md 不重扫也生效。
func TestSkillBodyReadAtCallTimeFromAgent(t *testing.T) {
	skillDir := t.TempDir()
	dir := filepath.Join(skillDir, "deploy")
	writeSkillFile(t, dir, "name: deploy\ndescription: 部署流程", "旧步骤")

	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{Dirs: []string{skillDir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeSkillFile(t, dir, "name: deploy\ndescription: 部署流程", "新步骤")

	out, ok, err := a.ExpandSkill("/deploy")
	if err != nil || !ok {
		t.Fatalf("ExpandSkill: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out, "新步骤") {
		t.Fatalf("应读到改后的正文：\n%s", out)
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

// MaxCatalogBytes 接线：宿主配置的字节上限要真的生效（默认 8KB）。
func TestSkillMaxCatalogBytesWired(t *testing.T) {
	skillDir := t.TempDir()
	writeSkillFile(t, filepath.Join(skillDir, "deploy"), "name: deploy\ndescription: 部署流程", "步骤")

	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Skills: &mos.SkillConfig{
			Dirs:            []string{skillDir},
			MaxCatalogBytes: 64, // 小到装不下完整清单 → 触发降级说明
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prompt := a.RunContext().SystemPrompt
	// 64 字节预算装不下完整清单 → 描述被降级丢掉（默认 8KB 时不会）。
	if strings.Contains(prompt, "部署流程") {
		t.Fatalf("预算应生效（描述应被丢掉）：\n%s", prompt)
	}
}
