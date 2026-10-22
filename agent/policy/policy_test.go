package policy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/agent/execpath"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

func event(name string) *hook.HookEvent {
	return &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: llm.ToolCall{Name: name}}
}

// 列入 allow 的放行。
func TestAllowPasses(t *testing.T) {
	p := New(Config{Allow: []string{"current_time", "web_search"}})
	if err := p.Check(context.Background(), event("current_time")); err != nil {
		t.Fatalf("放行名单内的工具应通过: %v", err)
	}
}

// Allow 非空时，名单外的工具仍被拒绝（策略层是收窄层）。
func TestToolOutsideAllowDenied(t *testing.T) {
	p := New(Config{Allow: []string{"current_time"}})
	err := p.Check(context.Background(), event("shell"))
	if err == nil {
		t.Fatal("不在 allow 名单的工具应被拒绝")
	}
	if !strings.Contains(err.Error(), "shell") || !strings.Contains(err.Error(), "放行名单") {
		t.Fatalf("拒绝理由应点名工具并说明原因: %v", err)
	}
}

// Allow 未配置 = 不限制（对齐 PI / DSH）：策略层不收紧，收敛由注册表负责。
func TestUnconfiguredConfigDoesNotRestrict(t *testing.T) {
	p := New(Config{})
	for _, name := range []string{"current_time", "shell"} {
		if err := p.Check(context.Background(), event(name)); err != nil {
			t.Fatalf("Allow 未配置时不应拒绝 %q: %v", name, err)
		}
	}
}

// 名单归一：空白、去重、前后空格都不影响判定。
func TestNameNormalization(t *testing.T) {
	p := New(Config{Allow: []string{" current_time ", "", "current_time", "\tweb_search\n"}})
	if err := p.Check(context.Background(), event("current_time")); err != nil {
		t.Fatalf("去空白后应放行: %v", err)
	}
	if err := p.Check(context.Background(), event("web_search")); err != nil {
		t.Fatalf("tab 包裹应放行: %v", err)
	}
}

// nil 接收者与残缺载荷：拒绝而非 panic——fail-closed 是行为不只是默认值。
func TestNilAndMalformedRejected(t *testing.T) {
	var p *Policy
	if err := p.Check(context.Background(), event("current_time")); err == nil {
		t.Fatal("nil 策略应拒绝")
	}
	real := New(Config{Allow: []string{"current_time"}})
	if err := real.Check(context.Background(), &hook.HookEvent{}); err == nil {
		t.Fatal("缺工具名应拒绝")
	}
	if err := real.Check(context.Background(), nil); err == nil {
		t.Fatal("nil 载荷应拒绝")
	}
}

// 组合验收：policy 装进守卫链后，经执行通道真的拦住工具（不执行）。
// 这是 S5c 的落地证明——策略不是摆设，而是执行路径上的 fail-closed 关卡。
type echoTool struct {
	name  string
	calls int
}

// readTool 是策略未提及的工具（收窄层语义下应放行并真的执行）。
type readTool struct{ echoTool }

func (e *echoTool) Name() string        { return e.name }
func (e *echoTool) Description() string { return "" }
func (e *echoTool) Parameters() any     { return &struct{}{} }
func (e *echoTool) Execute(context.Context, json.RawMessage) (any, error) {
	e.calls++
	return map[string]any{"ok": true}, nil
}

func TestPolicyBlocksToolThroughExecChannel(t *testing.T) {
	ok, bad, other := &echoTool{name: "current_time"}, &echoTool{name: "shell"}, &readTool{echoTool{name: "read_file"}}
	reg := tool.NewRegistry()
	for _, tl := range []tool.Tool{ok, bad, other} {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}

	gb := hook.NewGuardBus()
	gb.Add(New(Config{Deny: []string{"shell"}}).Check)

	out := execpath.ExecuteBatch(context.Background(), reg, []llm.ToolCall{
		{ID: "c1", Name: "current_time", Args: `{}`},
		{ID: "c2", Name: "shell", Args: `{}`},
		{ID: "c3", Name: "read_file", Args: `{}`}, // 策略未提及 → 放行（收窄层语义）
	}, execpath.Options{Turn: 1, Guards: gb})

	if len(out) != 3 {
		t.Fatalf("结果数 = %d", len(out))
	}
	if out[0].IsError || !strings.Contains(out[0].Content, `"ok":true`) {
		t.Fatalf("放行的工具应正常执行: %+v", out[0])
	}
	if !out[1].IsError || !strings.Contains(out[1].Content, "拒绝名单") {
		t.Fatalf("deny 命中的工具应回填拒绝理由: %+v", out[1])
	}
	if out[2].IsError {
		t.Fatalf("策略未提及的工具应放行（收窄层语义）: %+v", out[2])
	}
	// 结果仍按调用顺序对回 CallID（执行通道的不变式）
	for i, id := range []string{"c1", "c2", "c3"} {
		if out[i].Call.ID != id {
			t.Fatalf("out[%d].Call.ID = %q, want %q", i, out[i].Call.ID, id)
		}
	}
	if ok.calls != 1 || bad.calls != 0 || other.calls != 1 {
		t.Fatalf("执行次数 ok=%d bad=%d other=%d, want 1/0/1", ok.calls, bad.calls, other.calls)
	}
}
