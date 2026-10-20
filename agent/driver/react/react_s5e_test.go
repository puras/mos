package react

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// 并发安全的假工具：只读，可与同批其他调用并发。
type parTool struct {
	mu      sync.Mutex
	started []time.Time
	release chan struct{}
	calls   int
}

func (p *parTool) Name() string        { return "par" }
func (p *parTool) Description() string { return "并发测试工具" }
func (p *parTool) Parameters() any     { return &struct{}{} }
func (p *parTool) Execute(ctx context.Context, _ json.RawMessage) (any, error) {
	p.mu.Lock()
	p.calls++
	p.started = append(p.started, time.Now())
	p.mu.Unlock()
	if p.release != nil {
		<-p.release
	}
	return map[string]any{"ok": true}, nil
}

// ConcurrencySafe 声明可并发。
func (p *parTool) ConcurrencySafe(json.RawMessage) bool { return true }

// 端到端：post-execute 钩子真的改写了回填给模型的文本。
func TestPostExecuteHookRewritesResultThroughDriver(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("x"),
		answer("好了"),
	}}
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		out.ResultText = "已裁剪"
		return out, err
	})
	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   registryOf(t, &fakeTool{name: "x"}),
		Hooks:   hooks,
	}, New())

	if _, err := d.Run(context.Background(), sess, "去调用"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult {
			found = true
			if ev.ToolResult.Content != "已裁剪" {
				t.Fatalf("回填内容应被钩子改写: %+v", ev.ToolResult)
			}
		}
	}
	if !found {
		t.Fatal("没有 tool/result 事件")
	}
}

// 端到端：pre-execute 钩子拒绝 → 工具不执行，理由回填，事件形状不变。
func TestPreExecuteDenyThroughDriver(t *testing.T) {
	denied := &fakeTool{name: "danger"}
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("danger"),
		answer("不调了"),
	}}
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionDeny, Reason: "扩展点拒绝"}
		return ev, nil
	})
	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   registryOf(t, denied),
		Hooks:   hooks,
	}, New())

	reply, err := d.Run(context.Background(), sess, "去调用它")
	if err != nil {
		t.Fatalf("拒绝不应中断 turn: %v", err)
	}
	if reply != "不调了" {
		t.Fatalf("reply = %q", reply)
	}
	if denied.calls != 0 {
		t.Fatal("被拒工具不应被执行")
	}
	evs := sess.Events()
	if got := kinds(evs); got != "turn/start,user,assistant,tool/call,tool/result,assistant,turn/end" {
		t.Fatalf("事件序列 = %s", got)
	}
	res := evs[4].ToolResult
	if res == nil || !res.IsError || !strings.Contains(res.Content, "扩展点拒绝") {
		t.Fatalf("应回填拒绝理由: %+v", res)
	}
}

// 端到端：终止决策 → 本轮收尾（turn/end reason=terminated），不再回到决策器。
func TestCancelDecisionEndsTurn(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		toolCallAction("x"),
		answer("不该到这里"),
	}}
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionCancel, Reason: "到此为止"}
		return ev, nil
	})
	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   registryOf(t, &fakeTool{name: "x"}),
		Hooks:   hooks,
	}, New())

	if _, err := d.Run(context.Background(), sess, "去调用"); err != nil {
		t.Fatal(err)
	}
	evs := sess.Events()
	last := evs[len(evs)-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "terminated" {
		t.Fatalf("应以 terminated 收尾: %+v", last)
	}
	// 决策器不应被再次调用（只有一次决策）
	if got := kinds(evs); got != "turn/start,user,assistant,tool/call,tool/result,turn/end" {
		t.Fatalf("事件序列 = %s", got)
	}
}

// 端到端：并发执行的结果仍按调用序回填。
func TestConcurrentResultsKeepCallOrderThroughDriver(t *testing.T) {
	pt := &parTool{release: make(chan struct{})}
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		{Content: "并发读", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "par", Args: `{}`},
			{ID: "c2", Name: "par", Args: `{}`},
		}, FinishReason: "tool_calls"},
		answer("读完了"),
	}}
	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   registryOf(t, pt),
	}, New())

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(pt.release)
	}()

	if _, err := d.Run(context.Background(), sess, "去读"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult {
			ids = append(ids, ev.ToolResult.CallID)
		}
	}
	if len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Fatalf("结果应按调用序回填: %v", ids)
	}
}

var _ = tool.Tool(&parTool{})
