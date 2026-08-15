package agent

import (
	"context"
	"errors"
	"testing"

	"hegq.net/mos/agent/session"
)

func TestHookBusWaterfallOrder(t *testing.T) {
	hb := NewHookBus()
	var order []string
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		order = append(order, "a")
		ev.Messages = append(ev.Messages, session.UserMsg("注入A"))
		return next(ctxForTest(), ev)
	})
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		order = append(order, "b")
		ev.Messages = append(ev.Messages, session.UserMsg("注入B"))
		return next(ctxForTest(), ev)
	})
	out, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep})
	if err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v", order)
	}
	if len(out.Messages) != 2 || out.Messages[0].Content != "注入A" || out.Messages[1].Content != "注入B" {
		t.Fatalf("messages = %+v", out.Messages)
	}
}

func TestHookBusShortCircuit(t *testing.T) {
	hb := NewHookBus()
	var order []string
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		order = append(order, "a")
		ev.Messages = append(ev.Messages, session.UserMsg("改写"))
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
	if len(order) != 1 || len(out.Messages) != 1 {
		t.Fatalf("order=%v messages=%+v", order, out.Messages)
	}
}

func TestHookBusErrorPropagation(t *testing.T) {
	hb := NewHookBus()
	wantErr := errors.New("hook failed")
	hb.On(HookPreStep, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		return nil, wantErr
	})
	if _, err := hb.Waterfall(ctxForTest(), &HookEvent{Kind: HookPreStep}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestGuardBusSerialDeny(t *testing.T) {
	gb := NewGuardBus()
	gb.Add(func(_ context.Context, _ *HookEvent) error { return nil })
	denied := errors.New("denied")
	gb.Add(func(_ context.Context, _ *HookEvent) error { return denied })
	if err := gb.Allow(ctxForTest(), &HookEvent{}); !errors.Is(err, denied) {
		t.Fatalf("Allow = %v, want %v", err, denied)
	}
}

func TestGuardBusRemove(t *testing.T) {
	gb := NewGuardBus()
	remove := gb.Add(func(_ context.Context, _ *HookEvent) error { return errors.New("deny") })
	remove()
	if err := gb.Allow(ctxForTest(), &HookEvent{}); err != nil {
		t.Fatalf("Allow after remove: %v", err)
	}
}

func ctxForTest() context.Context { return context.Background() }
