package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// stubCompactor 是可编程的压缩器桩：按预设返回切割方案与摘要，
// 并记录收到的视图与消息（供断言）。
type stubCompactor struct {
	planOK       bool
	thresholdOff bool // true = 阈值门报告未超预算（模拟小上下文）
	cut          int
	previous     string
	text         string
	sumErr       error

	gotView   []session.SeqMessage
	gotTokens int
	gotMsgs   []llm.Message
	gotPrev   string
}

func (c *stubCompactor) ShouldCompact(contextTokens int) bool {
	// 模拟 LLMCompactor 的公式：超过窗口-预留才触发；窗口未配置永不触发。
	if c.thresholdOff {
		return false
	}
	return true
}

func (c *stubCompactor) Plan(view []session.SeqMessage, contextTokens int) (agent.CutPlan, bool) {
	c.gotView = view
	c.gotTokens = contextTokens
	if !c.planOK {
		return agent.CutPlan{}, false
	}
	return agent.CutPlan{Cut: c.cut, Previous: c.previous}, true
}

func (c *stubCompactor) Summarize(_ context.Context, msgs []llm.Message, previous string) (string, llm.Usage, error) {
	c.gotMsgs = msgs
	c.gotPrev = previous
	if c.sumErr != nil {
		return "", llm.Usage{}, c.sumErr
	}
	return c.text, llm.Usage{TotalTokens: 42}, nil
}

// prefillSessions 预先落多轮历史：每轮 user + assistant(带工具调用) + tool/call + tool/result + 收尾。
func prefillSessions(t *testing.T, sess *session.Session, turns int) {
	t.Helper()
	for turn := 1; turn <= turns; turn++ {
		for _, ev := range []session.Event{
			{Kind: session.EventTurnStart, Turn: turn},
			{Kind: session.EventUser, Turn: turn, Text: fmt.Sprintf("第 %d 个问题", turn)},
			{Kind: session.EventAssistant, Turn: turn, ToolCalls: []llm.ToolCall{
				{ID: fmt.Sprintf("call-%d", turn), Name: "read", Args: `{}`},
			}},
			{Kind: session.EventToolCall, Turn: turn, ToolCall: &llm.ToolCall{ID: fmt.Sprintf("call-%d", turn), Name: "read", Args: `{}`}},
			{Kind: session.EventToolResult, Turn: turn, ToolResult: &session.ToolResult{
				CallID: fmt.Sprintf("call-%d", turn), Content: fmt.Sprintf("环境证据 %d", turn),
			}},
			{Kind: session.EventTurnEnd, Turn: turn, Reason: "completed"},
		} {
			if _, err := sess.Append(ev); err != nil {
				t.Fatalf("预填事件: %v", err)
			}
		}
	}
}

// 压缩接线（PI 同款时序：工具结果追加之后、下一次模型响应之前）：
// 超预算 → 定切割点 → 摘要落 compaction 事件 → 后续投影即压缩后视图。
func TestCompactionWiredIntoDecide(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 5)

	// 切割点定在视图第 2 条（真实算法保证它不会落在 tool 结果上，
	// 桩里直接给定）。
	comp := &stubCompactor{planOK: true, cut: 2, text: "## Goal\n测试"}
	p := &scriptedPlanner{acts: []planner.Action{answer("done")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp, SystemPrompt: "sys"}
	a := driver.New(rt, New())
	if _, err := a.Run(context.Background(), sess, "新问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 压缩器收到的视图与上下文估算
	if len(comp.gotView) == 0 || comp.gotTokens <= 0 {
		t.Fatalf("应收到视图与 token 估算: view=%d tokens=%d", len(comp.gotView), comp.gotTokens)
	}

	// compaction 事件落日志：FirstKeptSeq 指向切割下标的 Seq，摘要调用的用量一并记
	var compaction *session.Event
	for i, ev := range sess.Events() {
		if ev.Kind == session.EventCompaction {
			compaction = &sess.Events()[i]
		}
	}
	if compaction == nil {
		t.Fatal("应落 compaction 事件")
	}
	wantSeq := comp.gotView[comp.cut].Seq
	if compaction.Compaction.FirstKeptSeq != wantSeq {
		t.Fatalf("FirstKeptSeq = %d, want %d", compaction.Compaction.FirstKeptSeq, wantSeq)
	}
	if compaction.Compaction.Summary != "## Goal\n测试" {
		t.Fatalf("Summary = %q", compaction.Compaction.Summary)
	}
	if compaction.Usage == nil || compaction.Usage.TotalTokens != 42 {
		t.Fatalf("摘要调用用量应随事件落库: %+v", compaction.Usage)
	}

	// 投影从此是压缩后视图：决策器看到 [system, 摘要, 保留段..., 新输入]
	hist := p.seen[0]
	if len(hist) == 0 || !strings.HasPrefix(hist[1].Content, session.CompactionSummaryPrefix) {
		t.Fatalf("决策器应看到压缩后的历史: %+v", hist)
	}
	if hist[0].Content != "sys" {
		t.Fatalf("系统引导仍应在首位: %+v", hist[0])
	}
	if hist[len(hist)-1].Content != "新问题" {
		t.Fatalf("本轮输入应在末尾: %+v", hist[len(hist)-1])
	}
	// 摘要调用收到的消息不含旧摘要占位（无旧摘要时为空）
	if comp.gotPrev != "" {
		t.Fatalf("首轮压缩无旧摘要: %q", comp.gotPrev)
	}
}

// 迭代压缩：被遮蔽视图里的旧摘要作为 previous-summary 传给摘要器，且不进序列化输入。
func TestIterativeCompactionCarriesPreviousSummary(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 3)
	// 预置一次压缩：旧摘要遮蔽前两轮
	view := sess.View()
	if _, err := sess.Append(session.Event{Kind: session.EventCompaction, Compaction: &session.Compaction{
		Summary: "旧摘要内容", FirstKeptSeq: view[3].Seq,
	}}); err != nil {
		t.Fatalf("预置压缩: %v", err)
	}

	comp := &stubCompactor{planOK: true, cut: 2, previous: "旧摘要内容"}
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.gotPrev != "旧摘要内容" {
		t.Fatalf("previous-summary 应传给摘要器: %q", comp.gotPrev)
	}
	for _, m := range comp.gotMsgs {
		if strings.HasPrefix(m.Content, session.CompactionSummaryPrefix) {
			t.Fatal("旧摘要不该进序列化输入")
		}
	}
}

// 阈值门：ShouldCompact 报告未超预算时，即使切割方案可用也不压缩
// （PI 的 shouldCompact → findCutPoint 两道门，第一道不過第二道不跑）。
func TestCompactionThresholdGate(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 3)
	comp := &stubCompactor{planOK: true, cut: 2, thresholdOff: true}
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventCompaction {
			t.Fatal("阈值门未过不该有压缩事件")
		}
	}
}

// 未超预算（Plan 说不需要）：不落压缩事件，行为与未装配一致。
func TestCompactionPlanNoOp(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 2)
	before := len(sess.Events())
	comp := &stubCompactor{planOK: false}
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventCompaction {
			t.Fatal("不该有压缩事件")
		}
	}
	_ = before
}

// 压缩失败以错误终止本轮（fail-fast）：静默超预算会在下次调用溢出。
func TestCompactionFailureAbortsTurn(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 2)
	comp := &stubCompactor{planOK: true, cut: 1, sumErr: errors.New("模型不可用")}
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp}
	_, err := driver.New(rt, New()).Run(context.Background(), sess, "q")
	if err == nil || !strings.Contains(err.Error(), "压缩失败") {
		t.Fatalf("压缩失败应终止本轮: %v", err)
	}
	// 收尾事件落了 error 详情
	last := sess.Events()[len(sess.Events())-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "error" {
		t.Fatalf("应以 turn/end{error} 收尾: %+v", last)
	}
}

// pre-step 瀑布在压缩之后、决策之前跑：宿主钩子能改写模型输入。
func TestPreStepHookRewritesInput(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	hooks := hook.NewHookBus()
	hooks.On(hook.HookPreStep, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Messages = append(ev.Messages, llm.Message{Role: llm.RoleUser, Content: "[注入的检索结果]"})
		return next(ctx, ev)
	})
	rt := &driver.RunContext{Planner: p, Hooks: hooks}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := p.seen[0][len(p.seen[0])-1]
	if last.Content != "[注入的检索结果]" {
		t.Fatalf("pre-step 注入应到达决策器: %+v", p.seen[0])
	}
}

// S6 起默认不限决策轮次：不设 MaxDecisionRounds 时长任务照常完成。
func TestUnlimitedRoundsByDefault(t *testing.T) {
	sess := session.New("t", nil)
	acts := []planner.Action{}
	for i := 0; i < 60; i++ {
		acts = append(acts, toolCallAction("read"))
	}
	acts = append(acts, answer("完成"))
	p := &scriptedPlanner{acts: acts}
	ft := &fakeTool{name: "read"}
	rt := &driver.RunContext{Planner: p, Tools: registryOf(t, ft)}
	reply, err := driver.New(rt, New()).Run(context.Background(), sess, "慢慢来")
	if err != nil {
		t.Fatalf("默认不限轮次不应熔断: %v", err)
	}
	if reply != "完成" || ft.calls != 60 {
		t.Fatalf("reply=%q calls=%d", reply, ft.calls)
	}
}

// 显式设了上限而耗尽：照旧熔断（ErrMaxDecisionRounds），不假装完成。
func TestExplicitRoundLimitStillFuse(t *testing.T) {
	sess := session.New("t", nil)
	acts := []planner.Action{}
	for i := 0; i < 10; i++ {
		acts = append(acts, toolCallAction("read"))
	}
	p := &scriptedPlanner{acts: acts}
	rt := &driver.RunContext{Planner: p, Tools: registryOf(t, &fakeTool{name: "read"}), MaxDecisionRounds: 3}
	_, err := driver.New(rt, New()).Run(context.Background(), sess, "q")
	if err == nil || !strings.Contains(err.Error(), "决策次数超出上限") {
		t.Fatalf("应报 ErrMaxDecisionRounds: %v", err)
	}
}

// bigTool 返回巨大的结果（一个长字符串），用于验证截断钩子。
type bigTool struct{ body string }

func (b *bigTool) Name() string        { return "spill" }
func (b *bigTool) Description() string { return "测试工具（大输出）" }
func (b *bigTool) Parameters() any     { return &struct{}{} }
func (b *bigTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return map[string]any{"out": b.body}, nil
}

// 截断钩子接进执行通道后，过长的工具结果在回填给模型之前就被瘦身。
func TestTruncateHookShrinksToolResult(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{toolCallAction("spill"), answer("看了")}}
	body := strings.Repeat("证据", 3000) // 18000 字节，远超限制
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPostExecute, agent.TruncateHook(agent.NewDefaultTruncator(
		agent.TruncatorConfig{MaxBytes: 200, MaxLines: 3})))
	rt := &driver.RunContext{Planner: p, Tools: registryOf(t, &bigTool{body: body}), Hooks: hooks}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 找到 tool/result 事件，确认它已被截断并带说明
	var found bool
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil && ev.ToolResult.CallID == "c1" {
			found = true
			if len(ev.ToolResult.Content) >= len(body) {
				t.Fatalf("回填应被截短: %d 字节", len(ev.ToolResult.Content))
			}
			if !strings.Contains(ev.ToolResult.Content, "已截断") {
				t.Fatalf("回填应带截断说明: %.80s", ev.ToolResult.Content)
			}
		}
	}
	if !found {
		t.Fatal("找不到 tool/result 事件")
	}
}
