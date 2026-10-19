package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// stubTool 是可控的测试工具：记录收到的参数，按预设返回或报错。
type stubTool struct {
	name string
	args json.RawMessage
	err  error
	ret  any
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "测试工具" }
func (s *stubTool) Parameters() any     { return &struct{}{} }
func (s *stubTool) Execute(_ context.Context, args json.RawMessage) (any, error) {
	s.args = append(json.RawMessage(nil), args...)
	if s.err != nil {
		return nil, s.err
	}
	if s.ret != nil {
		return s.ret, nil
	}
	return map[string]any{"tool": s.name}, nil
}

func registryOf(tools ...tool.Tool) *tool.Registry {
	r := tool.NewRegistry()
	for _, tl := range tools {
		if err := r.Register(tl); err != nil {
			panic(err)
		}
	}
	return r
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Args: args}
}

// 结果按模型给定的调用顺序返回，与执行完成顺序无关（回填顺序即调用顺序）。
func TestOutcomesFollowCallOrder(t *testing.T) {
	a := &stubTool{name: "alpha"}
	b := &stubTool{name: "beta"}
	reg := registryOf(a, b)

	got := ExecuteBatch(context.Background(), reg, []llm.ToolCall{
		call("c1", "alpha", `{"a":1}`),
		call("c2", "beta", `{"b":2}`),
	}, Options{})

	if len(got) != 2 {
		t.Fatalf("结果数 = %d, want 2", len(got))
	}
	if got[0].Call.ID != "c1" || got[1].Call.ID != "c2" {
		t.Fatalf("结果未按调用顺序: %s, %s", got[0].Call.ID, got[1].Call.ID)
	}
	if got[0].Call.Name != "alpha" || got[1].Call.Name != "beta" {
		t.Fatalf("结果未对回原调用: %+v", got)
	}
	if string(a.args) != `{"a":1}` || string(b.args) != `{"b":2}` {
		t.Fatalf("参数未按调用分发: alpha=%s beta=%s", a.args, b.args)
	}
	if got[0].IsError || got[1].IsError {
		t.Fatalf("成功调用不应判失败: %+v %+v", got[0], got[1])
	}
	if !strings.Contains(got[0].Content, `"tool":"alpha"`) {
		t.Fatalf("内容应为返回值 JSON: %q", got[0].Content)
	}
}

// 串行执行：批内一次只跑一个工具（S5e 改并发时此测试随之改写）。
func TestExecutionIsSerial(t *testing.T) {
	var order []string
	reg := registryOf(recordTool{name: "a", order: &order}, recordTool{name: "b", order: &order})
	ExecuteBatch(context.Background(), reg, []llm.ToolCall{
		call("c1", "a", `{}`), call("c2", "b", `{}`), call("c3", "a", `{}`),
	}, Options{})
	if strings.Join(order, ",") != "a,b,a" {
		t.Fatalf("执行顺序 = %v", order)
	}
}

type recordTool struct {
	name  string
	order *[]string
}

func (r recordTool) Name() string        { return r.name }
func (r recordTool) Description() string { return "" }
func (r recordTool) Parameters() any     { return &struct{}{} }
func (r recordTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	*r.order = append(*r.order, r.name)
	return nil, nil
}

// 工具报错是结果不是故障：转成 IsError 供模型自我纠正，不返回 error。
func TestToolErrorBecomesOutcome(t *testing.T) {
	boom := errors.New("磁盘写入失败")
	reg := registryOf(&stubTool{name: "flaky", err: boom})
	got := ExecuteBatch(context.Background(), reg, []llm.ToolCall{call("c1", "flaky", `{}`)}, Options{})

	if len(got) != 1 || !got[0].IsError || got[0].Content != boom.Error() {
		t.Fatalf("工具报错应以 IsError 回填: %+v", got)
	}
}

// 未注册的工具（模型幻觉）同样以失败回填，且不触碰任何实现。
func TestUnknownToolBecomesOutcome(t *testing.T) {
	real := &stubTool{name: "real"}
	got := ExecuteBatch(context.Background(), registryOf(real), []llm.ToolCall{
		call("c1", "ghost", `{}`), call("c2", "real", `{}`),
	}, Options{})
	if !got[0].IsError || !strings.Contains(got[0].Content, "ghost") {
		t.Fatalf("未注册工具应判失败并点名: %+v", got[0])
	}
	if got[1].IsError {
		t.Fatalf("同批正常工具不应受影响: %+v", got[1])
	}
}

// 参数非法同样落在结果里（模型自造参数是常态，不是系统故障）。
func TestBadArgsBecomeOutcome(t *testing.T) {
	reg := registryOf(&stubTool{name: "p"})
	got := ExecuteBatch(context.Background(), reg, []llm.ToolCall{call("c1", "p", `{"broken`)}, Options{})
	if len(got) != 1 {
		t.Fatalf("结果数 = %d", len(got))
	}
	if strings.Contains(got[0].Content, "解析参数") && got[0].IsError == false {
		t.Fatalf("参数解析失败应判 IsError: %+v", got[0])
	}
}

// 返回值不可序列化时退化为 Go 文本，且仍属成功。
func TestUnserializableResultDegrades(t *testing.T) {
	// NaN 是 JSON 不支持的值（json.Marshal 报错），但 %v 渲染稳定
	reg := registryOf(&stubTool{name: "nan", ret: math.NaN()})
	got := ExecuteBatch(context.Background(), reg, []llm.ToolCall{call("c1", "nan", `{}`)}, Options{})
	if got[0].IsError {
		t.Fatalf("不可序列化不等于失败: %+v", got[0])
	}
	if got[0].Content != "NaN" {
		t.Fatalf("应退化为 Go 文本: %q", got[0].Content)
	}
}

// 注册表为 nil 时全部判失败：fail-closed 兜底，不 panic。
func TestNilRegistryFailsClosed(t *testing.T) {
	got := ExecuteBatch(context.Background(), nil, []llm.ToolCall{call("c1", "x", `{}`)}, Options{})
	if len(got) != 1 || !got[0].IsError {
		t.Fatalf("nil 注册表应判失败: %+v", got)
	}
}

// 空批次（模型这一轮没要工具）不应 panic，返回空结果。
func TestEmptyBatch(t *testing.T) {
	if got := ExecuteBatch(context.Background(), registryOf(), nil, Options{}); len(got) != 0 {
		t.Fatalf("空批次 = %+v", got)
	}
}

// denyGuard 返回一个只放行白名单内工具的守卫（Policy 的最小形状）。
func denyGuard(allowed ...string) *hook.GuardBus {
	gb := hook.NewGuardBus()
	gb.Add(func(_ context.Context, ev *hook.HookEvent) error {
		for _, a := range allowed {
			if ev.ToolCall.Name == a {
				return nil
			}
		}
		return errors.New("不在白名单: " + ev.ToolCall.Name)
	})
	return gb
}

// 守卫拒绝的工具**不执行**，以拒绝理由回填；同批其他调用照常执行。
func TestGuardDenialSkipsExecution(t *testing.T) {
	danger := &stubTool{name: "danger"}
	safe := &stubTool{name: "safe"}
	reg := registryOf(danger, safe)

	got := ExecuteBatch(context.Background(), reg, []llm.ToolCall{
		call("c1", "danger", `{}`), call("c2", "safe", `{}`),
	}, Options{Guards: denyGuard("safe"), Turn: 3})

	if got[0].Call.ID != "c1" || !got[0].IsError {
		t.Fatalf("被拒调用应判失败且对回原 CallID: %+v", got[0])
	}
	if !strings.Contains(got[0].Content, "安全策略拒绝") || !strings.Contains(got[0].Content, "不在白名单") {
		t.Fatalf("回填应含拒绝理由: %q", got[0].Content)
	}
	if danger.args != nil {
		t.Fatal("被拒的工具不应被执行（守卫必须先于执行）")
	}
	if got[1].IsError || safe.args == nil {
		t.Fatalf("同批放行的调用应正常执行: %+v", got[1])
	}
}

// 守卫拿到的是原始载荷与轮次（turn 用于跨轮判定，如 S6 的重复调用检测）。
func TestGuardSeesCallAndTurn(t *testing.T) {
	gb := hook.NewGuardBus()
	var seen []hook.HookEvent
	gb.Add(func(_ context.Context, ev *hook.HookEvent) error {
		seen = append(seen, *ev)
		return nil
	})
	ExecuteBatch(context.Background(), registryOf(&stubTool{name: "t"}), []llm.ToolCall{
		call("c1", "t", `{"x":1}`),
	}, Options{Guards: gb, Turn: 7})

	if len(seen) != 1 {
		t.Fatalf("守卫被调 %d 次", len(seen))
	}
	if seen[0].Kind != hook.HookToolPreExecute || seen[0].Turn != 7 {
		t.Fatalf("载荷 = %+v", seen[0])
	}
	if seen[0].ToolCall.ID != "c1" || seen[0].ToolCall.Name != "t" || string(seen[0].ToolCall.Args) != `{"x":1}` {
		t.Fatalf("守卫应看到原始调用: %+v", seen[0].ToolCall)
	}
}

// 零开销降级：不装守卫时行为与未注入完全一致（不变式 6）。
func TestNilGuardsAllowEverything(t *testing.T) {
	tool := &stubTool{name: "t"}
	got := ExecuteBatch(context.Background(), registryOf(tool), []llm.ToolCall{
		call("c1", "t", `{}`),
	}, Options{}) // Guards 为 nil
	if got[0].IsError || tool.args == nil {
		t.Fatalf("无守卫时不应拦截: %+v", got[0])
	}
}
