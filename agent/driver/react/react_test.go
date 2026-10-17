package react

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// scriptedPlanner 按预设序列返回 Action，可标记为流式；
// 记录每次决策收到的历史与工具定义（供断言）。
type scriptedPlanner struct {
	acts      []planner.Action
	i         int
	seen      [][]llm.Message // 每次决策收到的历史
	seenTools [][]llm.Tool    // 每次决策收到的工具定义
	stream    bool
}

func (p *scriptedPlanner) NextAction(_ context.Context, history []llm.Message, tools []llm.Tool) (planner.Action, error) {
	return p.act(history, tools)
}

func (p *scriptedPlanner) act(history []llm.Message, tools []llm.Tool) (planner.Action, error) {
	p.seen = append(p.seen, history)
	p.seenTools = append(p.seenTools, tools)
	if p.i >= len(p.acts) {
		return planner.Action{}, errors.New("剧本已耗尽")
	}
	a := p.acts[p.i]
	p.i++
	return a, nil
}

func (p *scriptedPlanner) NextActionStream(_ context.Context, history []llm.Message, tools []llm.Tool, emit func(llm.StreamChunk) error) (planner.Action, error) {
	a, err := p.act(history, tools)
	if err != nil {
		return a, err
	}
	if emit != nil && a.Content != "" {
		_ = emit(llm.StreamChunk{Delta: a.Content})
	}
	return a, nil
}

var _ planner.StreamingPlanner = (*scriptedPlanner)(nil)

func answer(text string) planner.Action {
	return planner.Action{Content: text, FinishReason: "stop", Usage: llm.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}}
}

func TestRunWritesEvents(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("北京")}}
	a := driver.New(&driver.RunContext{Planner: p}, New())

	reply, err := a.Run(context.Background(), sess, "现在几点")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "北京" {
		t.Fatalf("reply = %q", reply)
	}

	want := []struct {
		kind   session.EventKind
		reason string
	}{
		{session.EventTurnStart, ""},
		{session.EventUser, ""},
		{session.EventAssistant, ""},
		{session.EventTurnEnd, "completed"},
	}
	events := sess.Events()
	if len(events) != len(want) {
		t.Fatalf("事件数 = %d, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Kind != w.kind || events[i].Reason != w.reason {
			t.Fatalf("events[%d] = %s/%s, want %s/%s", i, events[i].Kind, events[i].Reason, w.kind, w.reason)
		}
		if events[i].Turn != 1 {
			t.Fatalf("events[%d].Turn = %d, want 1", i, events[i].Turn)
		}
	}

	// 用量与思维链落 assistant 事件
	if events[2].Usage == nil || events[2].Usage.TotalTokens != 7 {
		t.Fatalf("assistant 事件应携带用量: %+v", events[2])
	}
	// 投影回放得到两轮对话
	if msgs := sess.Messages(); len(msgs) != 2 || msgs[1].Content != "北京" {
		t.Fatalf("投影 = %+v", msgs)
	}
}

// 决策器必须看到「系统引导 + 本轮 user」的完整历史。
func TestHistoryIncludesSystemAndInput(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("好")}}
	a := driver.New(&driver.RunContext{Planner: p, SystemPrompt: "你是助手"}, New())

	if _, err := a.Run(context.Background(), sess, "你好"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hist := p.seen[0]
	if len(hist) != 2 {
		t.Fatalf("历史 = %d 条, want 2（system + user）: %+v", len(hist), hist)
	}
	if hist[0].Role != llm.RoleSystem || hist[0].Content != "你是助手" {
		t.Fatalf("首条应为系统引导: %+v", hist[0])
	}
	if hist[1].Role != llm.RoleUser || hist[1].Content != "你好" {
		t.Fatalf("末条应为本轮输入: %+v", hist[1])
	}
}

// 多次 Run 累积上下文：第二轮的历史应包含首轮问答。
func TestMultiTurnAccumulation(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("一答"), answer("二答")}}
	a := driver.New(&driver.RunContext{Planner: p}, New())
	ctx := context.Background()

	if _, err := a.Run(ctx, sess, "第一问"); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if _, err := a.Run(ctx, sess, "第二问"); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	second := p.seen[1]
	if len(second) != 3 {
		t.Fatalf("第二轮历史 = %d 条, want 3（首轮问答 + 本轮输入）: %+v", len(second), second)
	}
	if second[1].Role != llm.RoleAssistant || second[2].Content != "第二问" {
		t.Fatalf("第二轮历史内容不对: %+v", second)
	}
	// 轮次序号随日志递增
	if events := sess.Events(); events[len(events)-1].Turn != 2 {
		t.Fatalf("第二轮事件 Turn = %d, want 2", events[len(events)-1].Turn)
	}
}

// 模型请求工具但驱动未接入执行通道：fail-closed 报错，且不留孤儿 turn。
func TestToolCallsFailClosed(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{{
		Content:   "我来读文件",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}},
	}}}
	a := driver.New(&driver.RunContext{Planner: p}, New())

	_, err := a.Run(context.Background(), sess, "读 a.txt")
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("应 fail-closed 报错, got %v", err)
	}
	events := sess.Events()
	last := events[len(events)-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "error" {
		t.Fatalf("出错也要收尾（不留孤儿 turn）: %+v", events)
	}
	if last.Turn != 1 {
		t.Fatalf("收尾 Turn = %d, want 1", last.Turn)
	}
}

// 决策失败：报错并以 error 收尾。
func TestPlannerErrorClosesTurn(t *testing.T) {
	sess := session.New("t", nil)
	boom := errors.New("boom")
	p := &errPlanner{err: boom}
	a := driver.New(&driver.RunContext{Planner: p}, New())

	if _, err := a.Run(context.Background(), sess, "问"); !errors.Is(err, boom) {
		t.Fatalf("错误应冒泡, got %v", err)
	}
	events := sess.Events()
	if last := events[len(events)-1]; last.Kind != session.EventTurnEnd || last.Reason != "error" {
		t.Fatalf("出错也要收尾: %+v", events)
	}
}

type errPlanner struct{ err error }

func (e *errPlanner) NextAction(context.Context, []llm.Message, []llm.Tool) (planner.Action, error) {
	return planner.Action{}, e.err
}

func TestStreamOnChunk(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{{Content: "流式答案"}}, stream: true}
	var sb strings.Builder
	var reasoningSeen []string
	a := driver.New(&driver.RunContext{
		Planner: p, Stream: true,
		OnChunk: func(delta string, reasoning bool) {
			if reasoning {
				reasoningSeen = append(reasoningSeen, delta)
				return
			}
			sb.WriteString(delta)
		},
	}, New())

	reply, err := a.Run(context.Background(), sess, "问")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sb.String() != "流式答案" {
		t.Fatalf("增量未透出: %q", sb.String())
	}
	if reply != "流式答案" {
		t.Fatalf("reply = %q", reply)
	}
}

// Stream=false 时即便决策器支持流式也不走流式路径（不触发 OnChunk）。
func TestStreamDisabled(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("静默")}, stream: true}
	called := false
	a := driver.New(&driver.RunContext{Planner: p, OnChunk: func(string, bool) { called = true }}, New())

	if _, err := a.Run(context.Background(), sess, "问"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("Stream=false 不应触发 OnChunk")
	}
}

// fail-closed：缺 Planner 拒绝运行。
func TestMissingPlannerFailsClosed(t *testing.T) {
	sess := session.New("t", nil)
	a := driver.New(&driver.RunContext{}, New())
	if _, err := a.Run(context.Background(), sess, "问"); err == nil {
		t.Fatal("缺 Planner 应拒绝运行")
	}
	if len(sess.Events()) != 0 {
		t.Fatalf("拒绝运行不应写事件: %+v", sess.Events())
	}
}

func TestMissingSessionAndDriver(t *testing.T) {
	a := driver.New(&driver.RunContext{Planner: &scriptedPlanner{}}, New())
	if _, err := a.Run(context.Background(), nil, "问"); err == nil {
		t.Fatal("nil 会话应报错")
	}
	if _, err := driver.New(&driver.RunContext{}, nil).Run(context.Background(), session.New("t", nil), "问"); err == nil {
		t.Fatal("nil 驱动应报错")
	}
}
