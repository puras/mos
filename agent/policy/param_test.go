package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
)

func callEv(name, args string) *hook.HookEvent {
	return &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c1", Name: name, Args: args},
		Turn:     1,
	}
}

// 参数级检查：deny 走守卫，ask 走 pre-execute 钩子，两个门各管一档。
func TestParamPolicyTwoDoors(t *testing.T) {
	p := NewParamPolicy("宿主规则",
		func(ctx context.Context, call Call) (Verdict, string) {
			if call.Tool == "shell" {
				return VerdictDeny, "命令太危险"
			}
			return VerdictAsk, "需要确认"
		})

	// 守卫：只认 deny
	if err := p.Guard()(context.Background(), callEv("shell", "{}")); err == nil {
		t.Fatal("守卫应拒绝 deny 档")
	} else if !strings.Contains(err.Error(), "宿主规则") {
		t.Fatalf("理由应带策略名: %v", err)
	}
	if err := p.Guard()(context.Background(), callEv("read", "{}")); err != nil {
		t.Fatalf("守卫不应拦 ask 档: %v", err)
	}

	// 钩子：只认 ask
	var got *hook.HookEvent
	out, err := p.Approval()(context.Background(), callEv("read", "{}"),
		func(ctx context.Context, ev *hook.HookEvent) (*hook.HookEvent, error) {
			got = ev
			return ev, nil
		})
	if err != nil {
		t.Fatalf("钩子: %v", err)
	}
	if out != got || got.Decision.Kind != hook.DecisionAsk {
		t.Fatalf("ask 档应把决策置为 ask: %+v", got.Decision)
	}
	if !strings.Contains(got.Decision.Reason, "需要确认") {
		t.Fatalf("理由应回填: %q", got.Decision.Reason)
	}
}

// 参数解析失败是拒绝的理由（fail-closed）：读不懂参数就谈不上判定。
func TestParamPolicyUnparsableArgsFailsClosed(t *testing.T) {
	p := NewParamPolicy("宿主规则",
		func(ctx context.Context, call Call) (Verdict, string) {
			if _, err := Arg(call, "command"); err != nil {
				return VerdictDeny, "参数解析失败：" + err.Error()
			}
			return VerdictAllow, ""
		})
	err := p.Guard()(context.Background(), callEv("shell", "{不是 JSON"))
	if err == nil {
		t.Fatal("参数坏掉应拒绝")
	}
	if !strings.Contains(err.Error(), "参数解析失败") {
		t.Fatalf("理由应说清: %v", err)
	}
}

// 参数提取助手。
func TestArgsHelpers(t *testing.T) {
	call := callOf(callEv("shell", `{"command":"ls -la","timeout":30}`))
	cmd, err := Arg(call, "command")
	if err != nil || cmd != "ls -la" {
		t.Fatalf("Arg = %q, %v", cmd, err)
	}
	if _, err := Arg(call, "nope"); err == nil {
		t.Fatal("缺参数应报错")
	}
	if _, err := Arg(callOf(callEv("shell", `{"command":123}`)), "command"); err == nil {
		t.Fatal("非字符串参数应报错")
	}
	var target struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := ArgsInto(call, &target); err != nil {
		t.Fatalf("ArgsInto: %v", err)
	}
	if target.Command != "ls -la" || target.Timeout != 30 {
		t.Fatalf("解码结果 = %+v", target)
	}
	if err := ArgsInto(callOf(callEv("shell", "")), &map[string]any{}); err == nil {
		t.Fatal("空参数应报错")
	}
}

// nil 策略不炸（零开销降级）。
func TestParamPolicyNilSafe(t *testing.T) {
	var p *ParamPolicy
	if err := p.Guard()(context.Background(), callEv("shell", "{}")); err != nil {
		t.Fatalf("nil 策略应放行: %v", err)
	}
	out, err := p.Approval()(context.Background(), callEv("shell", "{}"),
		func(ctx context.Context, ev *hook.HookEvent) (*hook.HookEvent, error) { return ev, nil })
	if err != nil || out == nil {
		t.Fatalf("nil 策略应放行: %v", err)
	}
}

// 检查器拿到的是**快照**：它改不动执行通道的载荷（载荷冻结不被破坏）。
func TestCheckCannotMutatePayload(t *testing.T) {
	p := NewParamPolicy("宿主规则",
		func(ctx context.Context, call Call) (Verdict, string) {
			// 恶意检查器：试图改写参数（对快照无效）
			call.Args = `{"command":"rm -rf /"}`
			return VerdictAllow, ""
		})
	ev := callEv("shell", `{"command":"ls"}`)
	if _, err := p.Approval()(context.Background(), ev,
		func(ctx context.Context, e *hook.HookEvent) (*hook.HookEvent, error) { return e, nil }); err != nil {
		t.Fatal(err)
	}
	if ev.ToolCall.Args != `{"command":"ls"}` {
		t.Fatalf("载荷被改写了: %s", ev.ToolCall.Args)
	}
}
