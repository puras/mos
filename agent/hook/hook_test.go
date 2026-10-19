package hook

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/puras/mos/llm"
)

func ctxForTest() context.Context { return context.Background() }

// waterfall：按注册顺序逐层委托，载荷层层叠加。
func TestWaterfallOrderAndRewrite(t *testing.T) {
	hb := NewHookBus()
	var order []string
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		order = append(order, "a")
		ev.Messages = append(ev.Messages, llm.Message{Role: llm.RoleUser, Content: "注入A"})
		return next(ctxForTest(), ev)
	})
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		order = append(order, "b")
		ev.Messages = append(ev.Messages, llm.Message{Role: llm.RoleUser, Content: "注入B"})
		return next(ctxForTest(), ev)
	})

	out, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep})
	if err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if strings.Join(order, ",") != "a,b" {
		t.Fatalf("顺序 = %v", order)
	}
	if len(out.Messages) != 2 || out.Messages[0].Content != "注入A" || out.Messages[1].Content != "注入B" {
		t.Fatalf("messages = %+v", out.Messages)
	}
}

// 短路：钩子不调用 next 则下游不执行，且载荷以短路时的形态返回。
func TestWaterfallShortCircuit(t *testing.T) {
	hb := NewHookBus()
	var order []string
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, _ Next) (*HookEvent, error) {
		order = append(order, "a")
		ev.ResultText = "已定案"
		return ev, nil // 不调用 next → 短路
	})
	hb.On(HookPreStep, func(_ context.Context, _ *HookEvent, _ Next) (*HookEvent, error) {
		order = append(order, "b")
		t.Fatal("短路后下游不应执行")
		return nil, nil
	})

	out, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep})
	if err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if len(order) != 1 || out.ResultText != "已定案" {
		t.Fatalf("order=%v out=%+v", order, out)
	}
}

// 错误沿链冒泡，下游不再执行。
func TestWaterfallErrorPropagation(t *testing.T) {
	hb := NewHookBus()
	want := errors.New("hook failed")
	hb.On(HookPreStep, func(_ context.Context, _ *HookEvent, _ Next) (*HookEvent, error) {
		return nil, want
	})
	hb.On(HookPreStep, func(_ context.Context, _ *HookEvent, _ Next) (*HookEvent, error) {
		t.Fatal("上游失败后下游不应执行")
		return nil, nil
	})
	if _, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// 坏插件不许带崩整条链：返回 (nil, nil) 显式报错，不把 nil 传下去。
func TestWaterfallNilEventRejected(t *testing.T) {
	hb := NewHookBus()
	hb.On(HookToolExecute, func(_ context.Context, _ *HookEvent, _ Next) (*HookEvent, error) {
		return nil, nil
	})
	_, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookToolExecute})
	if !errors.Is(err, ErrNilHookEvent) {
		t.Fatalf("err = %v, want ErrNilHookEvent", err)
	}
	if _, err := hb.Waterfall(ctxForTest(), nil); !errors.Is(err, ErrNilHookEvent) {
		t.Fatalf("nil 载荷应报错: %v", err)
	}
}

// 不同环节互不串链：pre-step 的钩子不会在 tool/execute 时执行。
func TestKindsAreIsolated(t *testing.T) {
	hb := NewHookBus()
	hb.On(HookPreStep, func(_ context.Context, _ *HookEvent, next Next) (*HookEvent, error) {
		return next(ctxForTest(), &HookEvent{})
	})
	hb.On(HookToolExecute, func(_ context.Context, ev *HookEvent, _ Next) (*HookEvent, error) {
		ev.ResultText = "executed"
		return ev, nil
	})
	out, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookToolExecute})
	if err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if out.ResultText != "executed" {
		t.Fatalf("应只走 tool/execute 链: %+v", out)
	}
}

// 注册与移除：移除函数幂等，移除后该钩子不再参与。
func TestOnRemovalIsIdempotent(t *testing.T) {
	hb := NewHookBus()
	var n int
	remove := hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		n++
		return next(ctxForTest(), ev)
	})
	if _, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep}); err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	remove()
	remove() // 幂等
	remove()
	if _, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep}); err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if n != 1 {
		t.Fatalf("钩子执行 %d 次，want 1", n)
	}
}

// 零开销降级：nil 总线的注册与调用都是 no-op（不变式 6）。
func TestNilHookBusIsNoop(t *testing.T) {
	var hb *HookBus
	hb.On(HookPreStep, func(_ context.Context, _ *HookEvent, _ Next) (*HookEvent, error) {
		t.Fatal("nil 总线不应注册成功")
		return nil, nil
	})() // 移除函数必须是 no-op，否则 panic
	ev := &HookEvent{Kind: HookPreStep}
	out, err := hb.Waterfall(ctxForTest(), ev)
	if err != nil || out != ev {
		t.Fatalf("nil 总线应原样返回: out=%+v err=%v", out, err)
	}
}

// 注册与执行并发安全（配合 -race）：执行快照与注册互不踩踏。
func TestConcurrentOnAndWaterfall(t *testing.T) {
	hb := NewHookBus()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			remove := hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
				return next(ctxForTest(), ev)
			})
			remove()
		}()
		go func() {
			defer wg.Done()
			if _, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep}); err != nil {
				t.Errorf("Waterfall: %v", err)
			}
		}()
	}
	wg.Wait()
}
