package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// fakeModel 记录收到的请求，返回预设文本（供压缩器测试）。
type fakeModel struct {
	req   *llm.Request
	text  string
	calls []llm.ToolCall // 非空时模拟模型试图调用工具
	err   error
}

func (m *fakeModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.req = req
	if m.err != nil {
		return nil, m.err
	}
	return &llm.Response{
		Message: llm.Message{Content: m.text, ToolCalls: m.calls},
		Usage:   llm.Usage{TotalTokens: 42},
	}, nil
}

func (m *fakeModel) ChatStream(ctx context.Context, req *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}
func (m *fakeModel) Capabilities() llm.Capability { return 0 }

func u(content string) llm.Message { return llm.Message{Role: llm.RoleUser, Content: content} }
func a(content string) llm.Message { return llm.Message{Role: llm.RoleAssistant, Content: content} }
func toolMsg(content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: "c1"}
}

func sm(seq int64, m llm.Message) session.SeqMessage { return session.SeqMessage{Seq: seq, Message: m} }

func newCompactor(t *testing.T, m llm.ChatModel, window int) *LLMCompactor {
	t.Helper()
	return NewLLMCompactor(m, CompactorConfig{ContextWindow: window})
}

// 触发公式与 PI 一致：contextTokens > contextWindow - reserveTokens。
func TestShouldCompact(t *testing.T) {
	c := newCompactor(t, &fakeModel{}, 100000)
	if c.ShouldCompact(100000 - 16384) {
		t.Fatal("未超预算不该触发")
	}
	if !c.ShouldCompact(100000 - 16384 + 1) {
		t.Fatal("超预算应触发")
	}
	// 未配置窗口 = 默认 128K（宁早勿溢出；更小的模型必须显式配置）
	def := NewLLMCompactor(&fakeModel{}, CompactorConfig{})
	if def.cfg.ContextWindow != DefaultContextWindow || DefaultContextWindow != 128*1024 {
		t.Fatalf("默认窗口应为 128K: %d", def.cfg.ContextWindow)
	}
	if !def.ShouldCompact(128*1024-16384+1) || def.ShouldCompact(1000) {
		t.Fatal("默认窗口的阈值不对")
	}
}

// 切割点算法（PI 的 findCutPoint）：从尾部按估算累计，超预算处取其后最近
// 的合法切割点；合法切割点 = user / assistant，绝不是 tool 结果。
func TestPlanCutNeverAtToolResult(t *testing.T) {
	c := newCompactor(t, &fakeModel{}, 1<<30)
	// 长历史 + 尾部若干短消息；预算设小让切割落进中段
	small := NewLLMCompactor(&fakeModel{}, CompactorConfig{ContextWindow: 1 << 30, KeepRecentTokens: 10})
	view := []session.SeqMessage{
		sm(1, u(strings.Repeat("长", 400))),
		sm(2, a(strings.Repeat("复", 400))),
		sm(3, u("q2")),
		sm(4, a("r2 with tool call")),
		sm(5, toolMsg("tool result")),
		sm(6, u("q3")),
	}
	plan, ok := small.Plan(view, 1<<30)
	if !ok {
		t.Fatal("应产生切割方案")
	}
	if view[plan.Cut].Message.Role == llm.RoleTool {
		t.Fatalf("切割点落在了 tool 结果上: cut=%d", plan.Cut)
	}
	// 预算只有 10 token：保留的应是尾部少数几条（q2 起）
	if plan.Cut < 2 {
		t.Fatalf("切割点应贴近尾部: cut=%d", plan.Cut)
	}
	_ = c
}

// 预算内不压缩；切到开头（无可摘要段）也不压缩。
func TestPlanNoOp(t *testing.T) {
	c := NewLLMCompactor(&fakeModel{}, CompactorConfig{ContextWindow: 1 << 30, KeepRecentTokens: 1 << 20})
	view := []session.SeqMessage{sm(1, u("a")), sm(2, a("b")), sm(3, u("c"))}
	if _, ok := c.Plan(view, 1<<30); ok {
		t.Fatal("预算远大于视图不该压缩")
	}
	// 视图只有一条消息：没有合法切割点
	tiny := NewLLMCompactor(&fakeModel{}, CompactorConfig{ContextWindow: 1 << 30, KeepRecentTokens: 1})
	if _, ok := tiny.Plan(view[:1], 1<<30); ok {
		t.Fatal("无可摘要段不该压缩")
	}
}

// Summarize 走 PI 的提示词结构：<conversation> 包裹 + 系统提示禁止续写；
// 有旧摘要时附 <previous-summary> 并换用更新提示词；工具结果被截断；
// 模型试图调用工具时直接报错。
func TestSummarizePromptShape(t *testing.T) {
	m := &fakeModel{text: "## Goal\nx"}
	c := NewLLMCompactor(m, CompactorConfig{ContextWindow: 1000, MaxToolResultChars: 50})
	msgs := []llm.Message{
		u("问题"),
		a("回答"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "read_file", Args: `{"path":"a.go"}`}}},
		toolMsg(strings.Repeat("结果", 100)),
	}
	text, usage, err := c.Summarize(context.Background(), msgs, "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if text != "## Goal\nx" || usage.TotalTokens != 42 {
		t.Fatalf("返回值不对: %q %+v", text, usage)
	}

	req := m.req
	if len(req.Messages) != 2 || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("请求应为 [system, user]: %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "Do NOT continue the conversation") {
		t.Fatalf("系统提示应禁止续写: %s", req.Messages[0].Content)
	}
	body := req.Messages[1].Content
	for _, want := range []string{
		"<conversation>", "[User]: 问题", "[Assistant]: 回答",
		"[Assistant tool calls]: read_file(", "[Tool result]: 结果",
		"chars truncated]", "## Goal",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("提示词缺 %q:\n%.400s", want, body)
		}
	}
	if strings.Contains(body, "<previous-summary>") {
		t.Fatal("无旧摘要时不应带 previous-summary")
	}
	// 工具结果截到 50 rune + 标注
	if strings.Count(body, "结果") > 60 {
		t.Fatal("工具结果未被截断")
	}

	// 迭代路径：previous 非空 → <previous-summary> + 更新提示词
	if _, _, err := c.Summarize(context.Background(), msgs, "旧摘要"); err != nil {
		t.Fatalf("Summarize(previous): %v", err)
	}
	body = m.req.Messages[1].Content
	if !strings.Contains(body, "<previous-summary>\n旧摘要\n</previous-summary>") {
		t.Fatalf("应带 previous-summary:\n%.300s", body)
	}
	if !strings.Contains(body, "Update the existing summary") {
		t.Fatal("应换用更新提示词")
	}
	// 旧摘要不再作为对话序列化
	if strings.Contains(body, "[User]: [历史摘要]") {
		t.Fatal("旧摘要不该被序列化进对话")
	}
}

// 模型试图调用工具：摘要是纯文本产出，直接报错（PI 同款护栏）。
func TestSummarizeRejectsToolCalls(t *testing.T) {
	m := &fakeModel{calls: []llm.ToolCall{{Name: "shell"}}}
	c := newCompactor(t, m, 1000)
	if _, _, err := c.Summarize(context.Background(), []llm.Message{u("x")}, ""); err == nil {
		t.Fatal("应拒绝试图调用工具的摘要响应")
	}
}

// nil 模型 fail-closed。
func TestSummarizeNilModel(t *testing.T) {
	c := NewLLMCompactor(nil, CompactorConfig{ContextWindow: 1000})
	if _, _, err := c.Summarize(context.Background(), nil, ""); err == nil {
		t.Fatal("nil 模型应报错")
	}
}

// 序列化：思维链省略；空内容跳过。
func TestSerializeConversation(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "q"},
		{Role: llm.RoleAssistant, Content: "", ReasoningContent: "思考过程", ToolCalls: []llm.ToolCall{{Name: "t", Args: `{}`}}},
		toolMsg("res"),
	}
	out := serializeConversation(msgs, 100)
	if strings.Contains(out, "思考过程") {
		t.Fatal("思维链应省略（PI：摘要请求不应大于被总结的上下文）")
	}
	if strings.Contains(out, "[Assistant]:") {
		t.Fatalf("空文本 assistant 只应出 tool calls 行:\n%s", out)
	}
	for _, want := range []string{"[User]: q", "[Assistant tool calls]: t({})", "[Tool result]: res"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q:\n%s", want, out)
		}
	}
}
