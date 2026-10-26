package react

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// recordingCompactor 记录每次收到的历史与状态快照，可配置是否压缩。
type recordingCompactor struct {
	keep         int // <=0 = 不压缩
	seenHistory  [][]llm.Message
	seenStates   []*session.TaskState
	summaryGiven string
}

func (c *recordingCompactor) Compact(history []llm.Message, state *session.TaskState) ([]llm.Message, bool) {
	c.seenHistory = append(c.seenHistory, history)
	c.seenStates = append(c.seenStates, state)
	if c.keep <= 0 {
		return history, false
	}
	// 极简压缩：只留 system + 摘要 + 最后 keep 条（成对由测试保证，这里不重复实现）
	start := 0
	if len(history) > 0 && history[0].Role == llm.RoleSystem {
		start = 1
	}
	msgs := history[start:]
	if len(msgs) <= c.keep {
		return history, false
	}
	cut := len(msgs) - c.keep
	for cut > 0 && msgs[cut].Role == llm.RoleTool {
		cut--
	}
	if cut == 0 {
		return history, false
	}
	summary := llm.Message{Role: llm.RoleUser, Content: c.summaryGiven}
	out := []llm.Message{}
	if start > 0 {
		out = append(out, history[0])
	}
	out = append(out, summary)
	out = append(out, msgs[cut:]...)
	return out, true
}

// prefillSessions 预先落多轮历史：每轮 user + assistant(带工具调用) + tool/result。
// trusted 工具的结果会经审计门进 TaskState。
func prefillSessions(t *testing.T, sess *session.Session, turns int) {
	t.Helper()
	for turn := 1; turn <= turns; turn++ {
		for _, ev := range []session.Event{
			{Kind: session.EventTurnStart, Turn: turn},
			{Kind: session.EventUser, Turn: turn, Text: "第 " + string(rune('0'+turn)) + " 个问题"},
			{Kind: session.EventAssistant, Turn: turn, ToolCalls: []llm.ToolCall{
				{ID: callID(turn), Name: "read_file", Args: `{}`},
			}},
			{Kind: session.EventToolCall, Turn: turn, ToolCall: &llm.ToolCall{ID: callID(turn), Name: "read_file", Args: `{}`}},
			{Kind: session.EventToolResult, Turn: turn, ToolResult: &session.ToolResult{
				CallID: callID(turn), Content: "环境证据 " + string(rune('0'+turn)),
			}},
			{Kind: session.EventTurnEnd, Turn: turn, Reason: "completed"},
		} {
			if _, err := sess.Append(ev); err != nil {
				t.Fatalf("预填事件: %v", err)
			}
		}
	}
}

func callID(turn int) string { return "call-" + string(rune('0'+turn)) }

// 压缩要接进决策路径：决策器收到的历史被替换成 [system, 摘要, 保留区]，
// 且摘要内容来自 TaskState 快照（审计门信任的工具输出）。
func TestCompactionWiredIntoDecide(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 5)

	comp := &recordingCompactor{keep: 2, summaryGiven: "[压缩摘要]"}
	p := &scriptedPlanner{acts: []planner.Action{answer("done")}}
	rt := &driver.RunContext{
		Planner:   p,
		Compactor: comp,
		Auditor:   agent.NewDefaultAuditor(), // read_file 在白名单 → 有事实
	}
	a := driver.New(rt, New())
	if _, err := a.Run(context.Background(), sess, "新问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	hist := p.seen[0]
	// system 未设 → 首条应为压缩摘要；之后是保留区 + 本轮新输入
	if hist[0].Content != "[压缩摘要]" {
		t.Fatalf("决策器应收到压缩后的历史: %+v", hist[0])
	}
	if len(hist) != 1+2+1 { // 摘要 + 保留 2 条 + 本轮新 user
		t.Fatalf("历史应被压缩: %d 条: %+v", len(hist), hist)
	}
	// 压缩器收到的状态快照应带审计门验证过的事实
	st := comp.seenStates[0]
	if st == nil || len(st.Facts) == 0 || st.Facts[0].Tool != "read_file" {
		t.Fatalf("TaskState 应含 read_file 的已验证事实: %+v", st)
	}
	if !strings.Contains(st.Facts[0].Evidence, "环境证据") {
		t.Fatalf("事实证据应来自工具结果: %+v", st.Facts[0])
	}
}

// 没装配审计门 = 全不信任：快照里只有目标与状态，没有任何事实（fail-closed）。
func TestNilAuditorNoFactsInSnapshot(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 3)

	comp := &recordingCompactor{keep: 0} // 只记录、不压缩
	p := &scriptedPlanner{acts: []planner.Action{answer("ok")}}
	rt := &driver.RunContext{Planner: p, Compactor: comp}
	if _, err := driver.New(rt, New()).Run(context.Background(), sess, "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := comp.seenStates[0]
	if len(st.Facts) != 0 {
		t.Fatalf("nil 审计门不该有事实进快照: %+v", st.Facts)
	}
	if st.Goal != "第 1 个问题" {
		t.Fatalf("Goal 仍应派生: %q", st.Goal)
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
	// 60 轮工具调用后收尾（旧默认上限 128 内，但足以证明 <=0 不会立刻熔断；
	// 真正的"不限"由 MaxDecisionRounds<=0 语义保证，这里锁定不回归）。
	acts := []planner.Action{}
	for i := 0; i < 60; i++ {
		acts = append(acts, toolCallAction("read_file"))
	}
	acts = append(acts, answer("完成"))
	p := &scriptedPlanner{acts: acts}
	ft := &fakeTool{name: "read_file"}
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
		acts = append(acts, toolCallAction("read_file"))
	}
	p := &scriptedPlanner{acts: acts}
	rt := &driver.RunContext{Planner: p, Tools: registryOf(t, &fakeTool{name: "read_file"}), MaxDecisionRounds: 3}
	_, err := driver.New(rt, New()).Run(context.Background(), sess, "q")
	if err == nil || !strings.Contains(err.Error(), "决策次数超出上限") {
		t.Fatalf("应报 ErrMaxDecisionRounds: %v", err)
	}
}

// bigTool 返回巨大的结果（一个长字符串），用于验证截断钩子。
type bigTool struct{ body string }

func (b *bigTool) Name() string        { return "spill" }
func (b *bigTool) Description() string { return "测试工具（大输出）" }
func (b *bigTool) Parameters() any {
	return &struct{}{}
}
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
