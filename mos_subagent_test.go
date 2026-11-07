package mos_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// writeAgentFile 写一个子代理定义（目录 + AGENT.md）。
func writeAgentFile(t *testing.T, dir, front, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %q: %v", dir, err)
	}
	content := "---\n" + front + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写子代理 %q: %v", dir, err)
	}
}

// stubRegTool 是只登记名字的工具替身（父注册表用）。
type stubRegTool struct{ name string }

func (s stubRegTool) Name() string        { return s.name }
func (s stubRegTool) Description() string { return "工具 " + s.name }
func (s stubRegTool) Parameters() any     { return &struct{}{} }
func (s stubRegTool) Execute(context.Context, json.RawMessage) (any, error) {
	return "ok", nil
}

// subagentSeen 是一次请求的快照（模型看到了什么）。
type subagentSeen struct {
	Tools  []string
	Msgs   []llm.Message
	System string
}

// subagentModel 是按剧本作答的模型替身：记录每次请求看到的工具与消息。
type subagentModel struct {
	mu    sync.Mutex
	steps []func(req *llm.Request) *llm.Response
	seen  []subagentSeen
}

func (m *subagentModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	system := ""
	if len(req.Messages) > 0 {
		system = req.Messages[0].Content
	}
	m.seen = append(m.seen, subagentSeen{Tools: names, Msgs: req.Messages, System: system})
	var step func(req *llm.Request) *llm.Response
	if len(m.steps) > 0 {
		step = m.steps[0]
		m.steps = m.steps[1:]
	}
	m.mu.Unlock()
	if step == nil {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "（剧本已用完）",
		}}, nil
	}
	return step(req), nil
}

func (m *subagentModel) ChatStream(ctx context.Context, req *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *subagentModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }

// seenAt 返回第 i 次请求的快照（从 0 起）；越界返回零值。
func (m *subagentModel) seenAt(i int) subagentSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.seen) {
		return subagentSeen{}
	}
	return m.seen[i]
}

// count 返回请求次数。
func (m *subagentModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}

// 装配：子代理清单进静态系统提示词、工具注册、快照写明子代理。
func TestSubAgentsWiredIntoPromptAndTool(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 只读代码审查", "你是审查者。")

	a, err := mos.New(mos.Config{
		Model:     &stubModel{reply: "ok"},
		SubAgents: &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.RunContext().SystemPrompt
	if !strings.Contains(got, "<available_subagents>") {
		t.Fatalf("系统提示词应含子代理清单：\n%s", got)
	}
	if !strings.Contains(got, "- reviewer: 只读代码审查") {
		t.Fatalf("清单应含名字与描述：\n%s", got)
	}
	if _, ok := a.RunContext().Tools.Get("subagent__reviewer"); !ok {
		t.Fatal("应注册 subagent__reviewer 工具")
	}
	snap := a.Describe()
	if !strings.Contains(snap, "子代理: 1 个") || !strings.Contains(snap, "reviewer") {
		t.Fatalf("装配快照应写明子代理：\n%s", snap)
	}
}

// 没装子代理 = 零行为变化（没有工具、提示词没多、快照写明未装配）。
func TestNoSubAgentsNoBehaviorChange(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range a.RunContext().Tools.Names() {
		if strings.HasPrefix(name, "subagent__") {
			t.Fatalf("没装子代理就不该有子代理工具：%s", name)
		}
	}
	if strings.Contains(a.RunContext().SystemPrompt, "available_subagents") {
		t.Fatalf("提示词不该有子代理清单：\n%s", a.RunContext().SystemPrompt)
	}
	if !strings.Contains(a.Describe(), "子代理: 未装配") {
		t.Fatalf("快照应写明未装配子代理：\n%s", a.Describe())
	}
}

// 端到端：父模型派子代理 → 子代理用受限工具面干活 → 结构化回执回填父上下文。
func TestSubAgentEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"),
		"name: reviewer\ndescription: 只读代码审查\ntools: [read]", "你是严格的代码审查者。")

	reg := tool.NewRegistry()
	for _, n := range []string{"read", "shell"} {
		if err := reg.Register(stubRegTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}

	m := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		// 父：第一轮点名派子代理。
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent__reviewer", Args: `{"task":"看看 main.go 有没有问题"}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		// 子：给固定结构的回执。
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: `{"summary":"看完了","findings":[{"ref":"main.go:12","summary":"有空指针风险"}]}`}}
		},
		// 父：收尾。
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "已收到子代理结论"}}
		},
	}}

	a, err := mos.New(mos.Config{
		Model:     m,
		Tools:     reg,
		SubAgents: &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	reply, err := a.Run(context.Background(), sess, "帮我审查 main.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "已收到子代理结论" {
		t.Fatalf("reply = %q", reply)
	}
	if m.count() != 3 {
		t.Fatalf("请求次数 = %d（父 2 次 + 子 1 次）", m.count())
	}

	// 子代理的工具面：只有白名单里的 read（父还有 shell，子拿不到）。
	child := m.seenAt(1)
	if len(child.Tools) != 1 || child.Tools[0] != "read" {
		t.Fatalf("子代理的工具面应只剩白名单：%v", child.Tools)
	}
	// 子代理拿到的是自己的提示词（人设 + 回执格式），不是父的。
	if !strings.Contains(child.System, "你是严格的代码审查者。") {
		t.Fatalf("子代理提示词应含人设：\n%s", child.System)
	}
	if !strings.Contains(child.System, "findings") {
		t.Fatalf("子代理提示词应写明回执格式：\n%s", child.System)
	}
	if strings.Contains(child.System, "available_subagents") {
		t.Fatalf("子代理不该拿到父的子代理清单：\n%s", child.System)
	}

	// 回执进父上下文（父的第二轮请求里能看到 findings）。
	parent := m.seenAt(2)
	found := false
	for _, msg := range parent.Msgs {
		if strings.Contains(msg.Content, "有空指针风险") && strings.Contains(msg.Content, "main.go:12") {
			found = true
		}
	}
	if !found {
		t.Fatalf("子代理回执应进父上下文：\n%+v", parent.Msgs)
	}
}

// 父会话里只有子代理这一条工具调用记录（子代理内部的事留在子会话）。
func TestSubAgentCallStaysInParentSession(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 审查", "人设")

	m := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent__reviewer", Args: `{"task":"干活"}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: `{"summary":"好了"}`}}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "收到"}}
		},
	}}
	a, err := mos.New(mos.Config{
		Model:     m,
		SubAgents: &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "干活"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var calls, results int
	for _, ev := range sess.Events() {
		switch ev.Kind {
		case session.EventToolCall:
			calls++
			if ev.ToolCall == nil || ev.ToolCall.Name != "subagent__reviewer" {
				t.Fatalf("父会话里的工具调用应是子代理工具：%+v", ev.ToolCall)
			}
		case session.EventToolResult:
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("父会话应只有 1 次工具调用与 1 条结果：%d / %d", calls, results)
	}
}

// 子代理调不到白名单外的工具（工具压根不在它的注册表里 → fail-closed）。
func TestSubAgentCannotUseToolsOutsideWhitelist(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 审查\ntools: [read]", "人设")

	reg := tool.NewRegistry()
	for _, n := range []string{"read", "shell"} {
		if err := reg.Register(stubRegTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	m := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent__reviewer", Args: `{"task":"干活"}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		// 子代理越界调 shell（不在它的工具面里）。
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c2", Name: "shell", Args: `{}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "子代理改用别的方式完成了"}}
		},
	}}
	a, err := mos.New(mos.Config{
		Model:     m,
		Tools:     reg,
		SubAgents: &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "干活"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 子代理调不到白名单外的工具：驱动把它当工具错误回填（fail-closed）。
	var denied bool
	for i := 0; i < m.count(); i++ {
		for _, msg := range m.seenAt(i).Msgs {
			if strings.Contains(msg.Content, "未知工具") && strings.Contains(msg.Content, "shell") {
				denied = true
			}
		}
	}
	if !denied {
		t.Fatal("子代理调白名单外的工具应被挡掉")
	}
	// 父拿到的仍是一段正常回执（父轮次没失败）。
	parent := m.seenAt(m.count() - 1)
	joined := ""
	for _, msg := range parent.Msgs {
		joined += msg.Content
	}
	if !strings.Contains(joined, "status") || !strings.Contains(joined, "summary") {
		t.Fatalf("父应看到固定结构的回执：\n%s", joined)
	}
}

// 模型覆盖：声明了宿主目录里没有的模型 → 装配期报错（假能力，fail-closed）。
func TestSubAgentUnknownModelFailsAssembly(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 审查\nmodel: fast", "人设")

	_, err := mos.New(mos.Config{
		Model:     &stubModel{reply: "ok"},
		SubAgents: &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err == nil {
		t.Fatal("声明了没装的模型应当装配失败")
	}
	if !strings.Contains(err.Error(), "fast") {
		t.Fatalf("报错应点名模型：%v", err)
	}
}

// 模型覆盖：目录里有就用目录里那个（子代理走别的模型）。
func TestSubAgentModelOverrideUsesCatalogModel(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 审查\nmodel: fast", "人设")

	// 目录模式：父跑当前模型（base），子代理跑声明的 fast。
	base := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent__reviewer", Args: `{"task":"干活"}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "收到"}}
		},
	}}
	fast := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: `{"summary":"快模型答的"}`}}
		},
	}}

	a, err := mos.New(mos.Config{
		Models: []mos.ModelOption{
			{ID: "base", Chat: base},
			{ID: "fast", Chat: fast},
		},
		DefaultModelID: "base",
		SubAgents:      &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "干活"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fast.count() != 1 {
		t.Fatalf("子代理该走 fast 模型（fast 被调 %d 次）", fast.count())
	}
	if base.count() != 2 {
		t.Fatalf("父该走 base 模型 2 次，实际 %d", base.count())
	}
}

// 子会话独立：宿主查得到子会话日志（子代理的中间过程不进父会话）。
func TestSubAgentUsesIndependentSession(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, filepath.Join(dir, "reviewer"), "name: reviewer\ndescription: 审查", "人设")

	logDir := t.TempDir()
	persist, err := session.NewJSONL(logDir)
	if err != nil {
		t.Fatal(err)
	}
	m := &subagentModel{steps: []func(*llm.Request) *llm.Response{
		func(*llm.Request) *llm.Response {
			return &llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent__reviewer", Args: `{"task":"干活"}`,
				}}},
				FinishReason: "tool_calls",
			}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: `{"summary":"好了"}`}}
		},
		func(*llm.Request) *llm.Response {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "收到"}}
		},
	}}
	a, err := mos.New(mos.Config{
		Model:       m,
		Persistence: persist,
		SubAgents:   &mos.SubAgentConfig{Dirs: []string{dir}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("parent-1", persist)
	if _, err := a.Run(context.Background(), sess, "干活"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	found := false
	for _, n := range names {
		if strings.HasPrefix(n, "parent-1-sub-reviewer-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有独立的子会话日志：%v", names)
	}
}
