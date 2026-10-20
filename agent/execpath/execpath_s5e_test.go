package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// testTool 是可控的测试工具。
type testTool struct {
	name string
	// safe 声明本次调用可并发。
	safe bool
	// safePanic 让并发分类器 panic（验证 fail-closed）。
	safePanic bool
	// gate 非 nil 时，执行前等待它放行（用于构造并发重叠）。
	gate func()
	// started 记录开始执行的时刻（用于验证重叠）。
	started *[]time.Time
	mu      *sync.Mutex
	calls   atomic.Int32
}

func (t *testTool) Name() string        { return t.name }
func (t *testTool) Description() string { return "测试工具" }
func (t *testTool) Parameters() any     { return &struct{}{} }

func (t *testTool) Execute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.calls.Add(1)
	if t.started != nil {
		t.mu.Lock()
		*t.started = append(*t.started, time.Now())
		t.mu.Unlock()
	}
	if t.gate != nil {
		t.gate()
	}
	return map[string]any{"tool": t.name}, nil
}

// ConcurrencySafe 实现可选接口（仅当 safe/safePanic 被设置时才有意义）。
func (t *testTool) ConcurrencySafe(json.RawMessage) bool {
	if t.safePanic {
		panic("分类器炸了")
	}
	return t.safe
}

func regOf(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	for _, tl := range tools {
		if err := r.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func callsOf(names ...string) []llm.ToolCall {
	out := make([]llm.ToolCall, len(names))
	for i, n := range names {
		out[i] = llm.ToolCall{ID: "c" + string(rune('1'+i)), Name: n, Args: `{}`}
	}
	return out
}

// ── 决策：pre-execute 钩子 ───────────────────────────────────────────

// pre-execute 钩子拒绝 → 工具不执行，理由回填。
func TestPreExecuteDenyBlocksTool(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionDeny, Reason: "此工具今天不许用"}
		return ev, nil // 不调用 next = 短路
	})

	out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if len(out) != 1 {
		t.Fatalf("结果数 = %d", len(out))
	}
	if !out[0].IsError || !strings.Contains(out[0].Content, "此工具今天不许用") {
		t.Fatalf("应回填拒绝理由: %+v", out[0])
	}
	if tl.calls.Load() != 0 {
		t.Fatal("被拒的工具不应执行")
	}
}

// 请求确认但无审批通道 → fail-closed 拒绝（对齐 DSH 的降级）。
func TestPreExecuteAskFailsClosed(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionAsk, Reason: "需要主管点头"}
		return ev, nil
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if !out[0].IsError || !strings.Contains(out[0].Content, "未装配审批通道") {
		t.Fatalf("ask 应 fail-closed 拒绝: %+v", out[0])
	}
	if tl.calls.Load() != 0 {
		t.Fatal("被拒的工具不应执行")
	}
}

// 终止本轮 → 该调用拒绝 + 同批后续调用跳过 + Outcome.Terminate。
func TestPreExecuteCancelSkipsRest(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionCancel, Reason: "到此为止"}
		return ev, nil
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("x", "x", "x"), Options{Hooks: hooks})
	if len(out) != 3 {
		t.Fatalf("结果数 = %d", len(out))
	}
	if !out[0].Terminate {
		t.Fatal("首个调用应标记 Terminate")
	}
	if !out[1].IsError || !out[2].IsError {
		t.Fatal("同批后续调用应被跳过（合成失败结果）")
	}
	if tl.calls.Load() != 0 {
		t.Fatalf("工具不应执行, got %d", tl.calls.Load())
	}
}

// ── 载荷冻结 ─────────────────────────────────────────────────────────

// 钩子改写载荷（参数/工具名/CallID）→ 违约，拒绝执行。
func TestHookCannotRewritePayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(ev *hook.HookEvent)
	}{
		{"改参数", func(ev *hook.HookEvent) { ev.ToolCall.Args = `{"path":"/etc/passwd"}` }},
		{"改工具名", func(ev *hook.HookEvent) { ev.ToolCall.Name = "other" }},
		{"改 CallID", func(ev *hook.HookEvent) { ev.ToolCall.ID = "zzz" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := &testTool{name: "x"}
			reg := regOf(t, tl)
			hooks := hook.NewHookBus()
			hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
				tc.mutate(ev)
				return next(ctx, ev)
			})
			out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
			if !out[0].IsError || !strings.Contains(out[0].Content, "载荷冻结") {
				t.Fatalf("改写载荷应被判违约: %+v", out[0])
			}
			if tl.calls.Load() != 0 {
				t.Fatal("违约的调用不应执行")
			}
		})
	}
}

// ── 环绕派发与结果改写 ───────────────────────────────────────────────

// execute 钩子不调用 next → 工具不执行（短路）。
func TestExecuteHookCanShortCircuit(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Result = map[string]any{"cached": true}
		return ev, nil
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if tl.calls.Load() != 0 {
		t.Fatal("短路的调用不应执行工具体")
	}
	if !strings.Contains(out[0].Content, "cached") {
		t.Fatalf("应回填钩子给的结果: %+v", out[0])
	}
}

// execute 钩子环绕计时：能看到真实执行发生。
func TestExecuteHookWrapsDispatch(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	var wrapped bool
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		wrapped = out != nil && out.Result != nil
		return out, err
	})
	ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if !wrapped {
		t.Fatal("execute 钩子应看到真实派发结果")
	}
}

// post-execute 裁剪结果文本。
func TestPostExecuteTruncates(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		out.ResultText = "裁剪后"
		return out, err
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if out[0].Content != "裁剪后" {
		t.Fatalf("结果文本应被改写: %+v", out[0])
	}
	if out[0].IsError {
		t.Fatal("裁剪不应改变错误标记")
	}
}

// post-execute 可把成功改成失败（加重）。
func TestPostExecuteCanEscalateToError(t *testing.T) {
	tl := &testTool{name: "x"}
	reg := regOf(t, tl)
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		out.Err = errors.New("结果不合规")
		return out, err
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("x"), Options{Hooks: hooks})
	if !out[0].IsError || !strings.Contains(out[0].Content, "结果不合规") {
		t.Fatalf("成功应被加重为失败: %+v", out[0])
	}
}

// post-execute 不能把失败洗成成功（错误是事实）。
func TestPostExecuteCannotWashError(t *testing.T) {
	reg := tool.NewRegistry() // 空注册表 → 工具未注册 → 失败
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		out.Err = nil // 试图洗白
		out.ResultText = "看起来成功"
		return out, err
	})
	out := ExecuteBatch(context.Background(), reg, callsOf("nope"), Options{Hooks: hooks})
	if !out[0].IsError {
		t.Fatalf("失败不应被洗成成功: %+v", out[0])
	}
}

// ── 并发 ─────────────────────────────────────────────────────────────

// 可并发工具在同批内重叠执行；结果仍按调用序回填。
func TestConcurrentToolsOverlapButKeepOrder(t *testing.T) {
	var started []time.Time
	var mu sync.Mutex
	release := make(chan struct{})
	first := &testTool{name: "a", safe: true, started: &started, mu: &mu,
		gate: func() { <-release }}
	second := &testTool{name: "b", safe: true, started: &started, mu: &mu}
	reg := regOf(t, first, second)

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()

	out := ExecuteBatch(context.Background(), reg, callsOf("a", "b"), Options{Concurrency: 4})
	if len(out) != 2 {
		t.Fatalf("结果数 = %d", len(out))
	}
	// 结果按调用序：第一个是 a
	if !strings.Contains(out[0].Content, `"tool":"a"`) || !strings.Contains(out[1].Content, `"tool":"b"`) {
		t.Fatalf("结果顺序应与调用序一致: %+v", out)
	}
	// 重叠证明：a 阻塞期间 b 已开始
	mu.Lock()
	n := len(started)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("两个调用都应执行, got %d", n)
	}
}

// 未声明可并发 → 独占（串行）：第二个调用在第一个结束前不得开始。
func TestUndeclaredToolIsExclusive(t *testing.T) {
	var started []time.Time
	var mu sync.Mutex
	release := make(chan struct{})
	first := &testTool{name: "a", started: &started, mu: &mu, gate: func() { <-release }}
	second := &testTool{name: "b", started: &started, mu: &mu}
	reg := regOf(t, first, second)

	done := make(chan []Outcome, 1)
	go func() {
		done <- ExecuteBatch(context.Background(), reg, callsOf("a", "b"), Options{Concurrency: 4})
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(started)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("未声明可并发的工具应串行, started=%d", n)
	}
	close(release)
	<-done
}

// 分类器 panic → fail-closed（独占）。
func TestConcurrencyClassifierPanicIsExclusive(t *testing.T) {
	var started []time.Time
	var mu sync.Mutex
	release := make(chan struct{})
	first := &testTool{name: "a", safePanic: true, started: &started, mu: &mu, gate: func() { <-release }}
	second := &testTool{name: "b", safePanic: true, started: &started, mu: &mu}
	reg := regOf(t, first, second)

	done := make(chan []Outcome, 1)
	go func() {
		done <- ExecuteBatch(context.Background(), reg, callsOf("a", "b"), Options{Concurrency: 4})
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(started)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("分类器 panic 应按独占处理, started=%d", n)
	}
	close(release)
	<-done
}

// 独占调用形成屏障：其后的可并发调用要等它结束。
func TestExclusiveCallFormsBarrier(t *testing.T) {
	var order []string
	var mu sync.Mutex
	mark := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	release := make(chan struct{})
	writer := &testTool{name: "w", gate: func() { mark("w-start"); <-release; mark("w-end") }}
	reader := &testTool{name: "r", safe: true, gate: func() { mark("r-start") }}
	reg := regOf(t, writer, reader)

	done := make(chan []Outcome, 1)
	go func() {
		done <- ExecuteBatch(context.Background(), reg, callsOf("r", "w", "r"), Options{Concurrency: 4})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(order, ",")
	// 第一个 r 与 w 之间是屏障：w 必须在第一个 r 之后开始
	if !strings.Contains(joined, "r-start,w-start") {
		t.Fatalf("独占调用应形成屏障: %s", joined)
	}
	// w 结束后第二个 r 才能开始
	if !strings.Contains(joined, "w-end,r-start") && strings.Count(joined, "r-start") == 2 {
		t.Fatalf("屏障后的调用应在独占调用结束后开始: %s", joined)
	}
}

// ── 取消 ─────────────────────────────────────────────────────────────

// 上下文取消：未派发的调用补记合成失败结果（call/result 配对不破）。
func TestCancelledContextStillProducesResults(t *testing.T) {
	tl := &testTool{name: "a", safe: true}
	reg := regOf(t, tl)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := ExecuteBatch(ctx, reg, callsOf("a", "b", "c"), Options{Concurrency: 4})
	if len(out) != 3 {
		t.Fatalf("取消后仍应每个调用一个结果: %d", len(out))
	}
	for i, o := range out {
		if !o.IsError {
			t.Fatalf("result[%d] 应为失败: %+v", i, o)
		}
	}
}
