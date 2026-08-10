package agent

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
)

func TestRenderSystem(t *testing.T) {
	sections := []SystemSection{
		{Title: "系统约束", Content: "你是助手"},
		{Title: "", Content: "无标题段"},
		{Title: "空段", Content: "   "}, // 空内容被跳过
	}
	got := RenderSystem(sections)
	wantSub := []string{"【系统约束】", "你是助手", "无标题段"}
	for _, s := range wantSub {
		if !strings.Contains(got, s) {
			t.Fatalf("RenderSystem = %q, want contains %q", got, s)
		}
	}
	if strings.Contains(got, "空段") {
		t.Fatalf("empty section should be skipped: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Fatalf("RenderSystem should trim edges: %q", got)
	}
}

func TestDefaultSystemAssemblerSections(t *testing.T) {
	sess := NewSession("s1")
	sess.State.Goal = "统计日志错误"
	sess.State.AddFact(Fact{Tool: "grep", Summary: "找到 3 处 ERROR", Evidence: "log"})

	asm := NewDefaultSystemAssembler(SystemAssemblerConfig{
		Constraints: "你是 MoS 智能体",
	})
	sections, err := asm.Assemble(context.Background(), sess, []llm.Tool{{Name: "grep", Description: "文本检索"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	// 默认槽位：约束 / 任务 / 状态 / 工具签名，顺序固定
	var titles []string
	for _, s := range sections {
		titles = append(titles, s.Title)
	}
	want := []string{"系统约束", "任务定义", "当前状态", "工具签名"}
	if strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	if sections[0].Content != "你是 MoS 智能体" {
		t.Fatalf("constraints = %q", sections[0].Content)
	}
	if sections[1].Content != "统计日志错误" {
		t.Fatalf("task = %q", sections[1].Content)
	}
	// 当前状态槽位 = TaskState 快照（仅已验证事实）
	if !strings.Contains(sections[2].Content, "找到 3 处 ERROR") {
		t.Fatalf("state = %q", sections[2].Content)
	}
	if !strings.Contains(sections[3].Content, "- grep: 文本检索") {
		t.Fatalf("tools = %q", sections[3].Content)
	}
	// 历史摘要槽位默认不启用：Compactor 已注入消息历史，系统槽位重复注入会冗余
	if strings.Contains(strings.Join(titles, ","), "历史摘要") {
		t.Fatal("summary slot should be off by default")
	}
}

// 动态组装：同一组装器、不同进度的任务状态，当前状态槽位随已验证事实刷新。
func TestSystemAssemblerStateRefresh(t *testing.T) {
	asm := NewDefaultSystemAssembler(SystemAssemblerConfig{Constraints: "约束"})

	sess := NewSession("s1")
	sess.State.Goal = "x"
	before, err := asm.Assemble(context.Background(), sess, nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	beforeState := RenderSystem(before)

	sess.State.AddFact(Fact{Tool: "grep", Summary: "找到 2 处", Evidence: "e"})
	after, err := asm.Assemble(context.Background(), sess, nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	afterState := RenderSystem(after)

	if !strings.Contains(afterState, "找到 2 处") {
		t.Fatalf("after state should contain verified fact: %q", afterState)
	}
	if beforeState == afterState {
		t.Fatalf("state slot should refresh with progress:\nbefore: %q\nafter:  %q", beforeState, afterState)
	}
}

func TestSystemAssemblerCustomSlots(t *testing.T) {
	// 只启用任务槽位：其余槽位不产出
	asm := NewDefaultSystemAssembler(SystemAssemblerConfig{
		Constraints: "约束",
		EnableSlots: []SystemSlot{SlotTask},
	})
	sess := NewSession("s1")
	sess.State.Goal = "目标"
	sections, err := asm.Assemble(context.Background(), sess, []llm.Tool{{Name: "x"}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(sections) != 1 || sections[0].Title != "任务定义" {
		t.Fatalf("sections = %+v, want only 任务定义", sections)
	}
}

// 运行时集成：每轮模型调用前系统提示词随 TaskState 进度动态刷新，
// 且 system 引导始终只有一条（替换而非累积）。
func TestAgentSystemAssemblerPerTurn(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	rec := &recordingModel{}
	a := New(rec, reg,
		WithSystemAssembler(NewDefaultSystemAssembler(SystemAssemblerConfig{
			Constraints: "你是助手",
			EnableSlots: []SystemSlot{SlotConstraints, SlotTask, SlotState, SlotTools},
		})),
	)

	sess := NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "1+2=3?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(rec.requests))
	}

	// 每轮恰好一条 system，且位于首位
	for i, req := range rec.requests {
		var sysCount int
		for _, m := range req.Messages {
			if m.Role == llm.RoleSystem {
				sysCount++
			}
		}
		if sysCount != 1 {
			t.Fatalf("turn %d: system count = %d, want 1", i+1, sysCount)
		}
		if req.Messages[0].Role != llm.RoleSystem {
			t.Fatalf("turn %d: first message not system", i+1)
		}
		// 工具签名槽位：calculator 描述
		if !strings.Contains(req.Messages[0].Content, "- calculator: 计算两个整数之和") {
			t.Fatalf("turn %d: tools slot missing: %q", i+1, req.Messages[0].Content)
		}
	}

	// 第二轮（工具已执行）：当前状态槽位应包含审计写入的已验证事实
	turn2 := rec.requests[1].Messages[0].Content
	if !strings.Contains(turn2, "已验证事实") {
		t.Fatalf("turn 2 state slot should include verified fact: %q", turn2)
	}
	turn1 := rec.requests[0].Messages[0].Content
	if strings.Contains(turn1, "已验证事实") {
		t.Fatalf("turn 1 should have no facts yet: %q", turn1)
	}
}

// recordingModel 记录每次 Chat 请求，首轮返回工具调用、次轮返回最终答复。
type recordingModel struct {
	requests []*llm.Request
}

func (m *recordingModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *recordingModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.requests = append(m.requests, req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "结果是 " + last.Content}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "我来调用工具",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}
func (m *recordingModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}
