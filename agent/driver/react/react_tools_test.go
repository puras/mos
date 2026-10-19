package react

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// fakeTool 记录调用次数与收到的参数；可预设错误。
type fakeTool struct {
	name  string
	err   error
	calls int
	last  json.RawMessage
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "测试工具" }
func (f *fakeTool) Parameters() any {
	return &struct {
		What string `json:"what" mos:"desc=内容,required=true"`
	}{}
}
func (f *fakeTool) Execute(_ context.Context, args json.RawMessage) (any, error) {
	f.calls++
	f.last = append(json.RawMessage(nil), args...)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"echo": "ok"}, nil
}

func registryOf(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	for _, tl := range tools {
		if err := r.Register(tl); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return r
}

func toolCallAction(names ...string) planner.Action {
	var calls []llm.ToolCall
	for i, n := range names {
		calls = append(calls, llm.ToolCall{ID: "c" + string(rune('1'+i)), Name: n, Args: `{"what":"x"}`})
	}
	return planner.Action{Content: "调用中", ToolCalls: calls, FinishReason: "tool_calls"}
}

func kinds(evs []session.Event) string {
	var sb []string
	for _, e := range evs {
		sb = append(sb, string(e.Kind))
	}
	return strings.Join(sb, ",")
}

// 单轮内：决策请求工具 → 执行 → 回填 → 再决策 → 作答收尾。
func TestToolExecutionLoop(t *testing.T) {
	ft := &fakeTool{name: "probe"}
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("probe"),
		answer("查到了"),
	}}
	a := driver.New(&driver.RunContext{Planner: p, Tools: registryOf(t, ft)}, New())

	reply, err := a.Run(context.Background(), sess, "探测一下")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "查到了" {
		t.Fatalf("reply = %q", reply)
	}
	if ft.calls != 1 {
		t.Fatalf("工具应被执行一次, got %d", ft.calls)
	}

	want := "turn/start,user,assistant,tool/call,tool/result,assistant,turn/end"
	if got := kinds(sess.Events()); got != want {
		t.Fatalf("事件序列 = %s\nwant %s", got, want)
	}

	evs := sess.Events()
	// tool/call 按模型顺序先全部落日志，tool/result 按序回填，Seq 连续
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("events[%d].Seq = %d, want %d（Seq 连续可重放）", i, ev.Seq, i+1)
		}
	}
	if evs[3].ToolCall == nil || evs[3].ToolCall.Name != "probe" {
		t.Fatalf("tool/call 应携带调用: %+v", evs[3])
	}
	if evs[4].ToolResult == nil || evs[4].ToolResult.CallID != "c1" || evs[4].ToolResult.IsError {
		t.Fatalf("tool/result 应回填成功: %+v", evs[4])
	}
	if !strings.Contains(evs[4].ToolResult.Content, `"echo":"ok"`) {
		t.Fatalf("tool/result 应是工具返回值的 JSON 文本: %q", evs[4].ToolResult.Content)
	}

	// 工具结果要进第二次决策的历史（投影）
	second := p.seen[1]
	if len(second) != 3 {
		t.Fatalf("第二次决策历史 = %+v, want [user assistant tool]", second)
	}
	if second[1].Role != llm.RoleAssistant || len(second[1].ToolCalls) != 1 {
		t.Fatalf("历史缺 assistant.tool_calls: %+v", second)
	}
	if second[2].Role != llm.RoleTool || second[2].ToolCallID != "c1" {
		t.Fatalf("历史缺 tool 消息（CallID 不匹配）: %+v", second[2])
	}
}

// 装配的工具定义要交给模型：每次决策都收到同一份清单。
func TestToolDefinitionsPassedToPlanner(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("好")}}
	reg := registryOf(t, &fakeTool{name: "alpha"}, &fakeTool{name: "beta"})
	d := driver.New(&driver.RunContext{Planner: p, Tools: reg}, New())

	if _, err := d.Run(context.Background(), sess, "问"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seenTools) != 1 {
		t.Fatalf("决策次数 = %d", len(p.seenTools))
	}
	got := p.seenTools[0]
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("工具定义 = %+v（按注册顺序）", got)
	}
	if got[0].Parameters == nil {
		t.Fatal("工具定义应携带 schema")
	}

	// 未装注册表时模型收不到任何工具定义
	p2 := &scriptedPlanner{acts: []planner.Action{answer("好")}}
	d2 := driver.New(&driver.RunContext{Planner: p2}, New())
	if _, err := d2.Run(context.Background(), session.New("t", nil), "问"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p2.seenTools[0]) != 0 {
		t.Fatalf("未装配时不应下发工具定义: %+v", p2.seenTools[0])
	}
}

// 工具报错不是系统故障：回填 IsError 给模型，循环继续，不中断 turn。
func TestToolErrorIsBackfilledNotAborted(t *testing.T) {
	boom := errors.New("磁盘写入失败")
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("flaky"),
		answer("我换个办法"),
	}}
	d := driver.New(&driver.RunContext{Planner: p, Tools: registryOf(t, &fakeTool{name: "flaky", err: boom})}, New())

	reply, err := d.Run(context.Background(), sess, "重试")
	if err != nil {
		t.Fatalf("工具报错不应中断 turn: %v", err)
	}
	if reply != "我换个办法" {
		t.Fatalf("reply = %q", reply)
	}
	evs := sess.Events()
	if got := kinds(evs); !strings.HasSuffix(got, "tool/result,assistant,turn/end") {
		t.Fatalf("事件序列 = %s", got)
	}
	res := evs[4].ToolResult
	if res == nil || !res.IsError || res.Content != boom.Error() {
		t.Fatalf("失败应以 IsError 回填错误文本: %+v", res)
	}
	// 失败也必须是 tool/result（历史保持 assistant.tool_calls → tool 配对）
	if res.CallID != "c1" {
		t.Fatalf("CallID = %q", res.CallID)
	}
}

// 模型幻觉出未注册工具名：同样回填失败，让模型自我纠正，不 panic 不中断。
func TestUnknownToolBackfilled(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("ghost_tool"),
		answer("换一个"),
	}}
	d := driver.New(&driver.RunContext{Planner: p, Tools: registryOf(t, &fakeTool{name: "real"})}, New())

	if _, err := d.Run(context.Background(), sess, "用那个工具"); err != nil {
		t.Fatalf("未知工具应回填而非中断: %v", err)
	}
	evs := sess.Events()
	if res := evs[4].ToolResult; res == nil || !res.IsError || !strings.Contains(res.Content, "ghost_tool") {
		t.Fatalf("未知工具失败回填: %+v", res)
	}
}

// 未装注册表却要求工具：fail-closed，且不写 assistant——
// 否则日志留下带 ToolCalls 却无 tool/result 的非法历史。
func TestToolsWithoutRegistryFailsClosedCleanly(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{toolCallAction("probe")}}
	d := driver.New(&driver.RunContext{Planner: p}, New())

	_, err := d.Run(context.Background(), sess, "问")
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("应 fail-closed, got %v", err)
	}
	if got := kinds(sess.Events()); got != "turn/start,user,turn/end" {
		t.Fatalf("不应写 assistant 事件, 事件序列 = %s", got)
	}
	if last := sess.Events()[2]; last.Reason != "error" {
		t.Fatalf("收尾 = %+v", last)
	}
	// 投影不得出现缺 tool 配对的 assistant.tool_calls
	for _, m := range sess.Messages() {
		if len(m.ToolCalls) > 0 && m.Role == llm.RoleAssistant {
			t.Fatalf("投影出现无配对的 ToolCalls: %+v", m)
		}
	}
}

// 模型一直请求工具不收口：耗尽上限报错，不留孤儿 turn。
func TestMaxDecisionRounds(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{
		acts: []planner.Action{
			toolCallAction("probe"), toolCallAction("probe"), toolCallAction("probe"),
			toolCallAction("probe"), toolCallAction("probe"), toolCallAction("probe"),
			toolCallAction("probe"), toolCallAction("probe"), toolCallAction("probe"),
			toolCallAction("probe"), toolCallAction("probe"), toolCallAction("probe"),
			toolCallAction("probe"), toolCallAction("probe"), toolCallAction("probe"),
			toolCallAction("probe"), // 第 17 次：上限 16 次决策后拒绝
		},
	}
	ft := &fakeTool{name: "probe"}
	d := driver.New(&driver.RunContext{Planner: p, Tools: registryOf(t, ft)}, New())

	_, err := d.Run(context.Background(), sess, "干活")
	if !errors.Is(err, ErrMaxDecisionRounds) {
		t.Fatalf("应报决策上限, got %v", err)
	}
	if ft.calls != maxDecisionRounds {
		t.Fatalf("工具执行 %d 次, want %d", ft.calls, maxDecisionRounds)
	}
	evs := sess.Events()
	last := evs[len(evs)-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "error" {
		t.Fatalf("耗尽上限也要收尾: %+v", last)
	}
	if last.Turn != 1 {
		t.Fatalf("收尾 Turn = %d", last.Turn)
	}
}

// 决策器出错：不写 assistant，以 error 收尾。
func TestDecisionErrorAborts(t *testing.T) {
	boom := errors.New("决策炸了")
	sess := session.New("t", nil)
	d := driver.New(&driver.RunContext{Planner: &errPlanner{err: boom}, Tools: registryOf(t, &fakeTool{name: "x"})}, New())

	if _, err := d.Run(context.Background(), sess, "问"); !errors.Is(err, boom) {
		t.Fatalf("错误应冒泡, got %v", err)
	}
	if got := kinds(sess.Events()); got != "turn/start,user,turn/end" {
		t.Fatalf("事件序列 = %s", got)
	}
}

// 两个独立 turn 各自完整收尾，轮次序号递增。
func TestTwoTurnsWithTools(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("probe"), answer("一"),
		toolCallAction("probe"), answer("二"),
	}}
	d := driver.New(&driver.RunContext{Planner: p, Tools: registryOf(t, &fakeTool{name: "probe"})}, New())
	ctx := context.Background()

	if _, err := d.Run(ctx, sess, "第一问"); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if _, err := d.Run(ctx, sess, "第二问"); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	evs := sess.Events()
	starts, ends := 0, 0
	for _, e := range evs {
		switch e.Kind {
		case session.EventTurnStart:
			starts++
		case session.EventTurnEnd:
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("turn/start=%d turn/end=%d, want 2/2", starts, ends)
	}
	if ends != starts {
		t.Fatal("每个 turn/start 必须有配对的 turn/end")
	}
}

// 端到端：装配 GuardBus 后，被拒的工具不执行，但事件序列与历史合法性不受影响——
// tool/call 照常留痕（审计：被拦截也要留痕），tool/result 回填拒绝理由，
// 模型下一轮看得见被拒原因并可自我纠正。
func TestGuardDeniesThroughDriver(t *testing.T) {
	denied := &fakeTool{name: "danger"}
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("danger"),
		answer("知道了，不调它"),
	}}
	guards := hook.NewGuardBus()
	guards.Add(func(_ context.Context, ev *hook.HookEvent) error {
		if ev.ToolCall.Name == "danger" {
			return errors.New("危险工具已禁用")
		}
		return nil
	})
	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   registryOf(t, denied),
		Guards:  guards,
	}, New())

	reply, err := d.Run(context.Background(), sess, "去调用它")
	if err != nil {
		t.Fatalf("拒绝不应中断 turn: %v", err)
	}
	if reply != "知道了，不调它" {
		t.Fatalf("reply = %q", reply)
	}
	if denied.calls != 0 {
		t.Fatalf("被拒工具不应被执行, got %d", denied.calls)
	}

	// 事件序列与无守卫时完全一致——拒绝只改结果内容，不改日志形状
	evs := sess.Events()
	if got := kinds(evs); got != "turn/start,user,assistant,tool/call,tool/result,assistant,turn/end" {
		t.Fatalf("事件序列 = %s", got)
	}
	if evs[3].ToolCall == nil || evs[3].ToolCall.Name != "danger" {
		t.Fatalf("被拒调用仍应有 tool/call 审计留痕: %+v", evs[3])
	}
	res := evs[4].ToolResult
	if res == nil || !res.IsError || !strings.Contains(res.Content, "危险工具已禁用") {
		t.Fatalf("应回填拒绝理由: %+v", res)
	}
	if res.CallID != "c1" {
		t.Fatalf("CallID = %q（结果须对回原调用）", res.CallID)
	}
	// 历史仍合法：assistant.tool_calls 与 tool 消息按 CallID 成对
	msgs := sess.Messages()
	if len(msgs) != 4 {
		t.Fatalf("投影历史 = %+v", msgs)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].Role != llm.RoleAssistant {
		t.Fatalf("投影缺 assistant.tool_calls: %+v", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolCallID != "c1" {
		t.Fatalf("投影缺配对的 tool 消息: %+v", msgs[2])
	}
}
