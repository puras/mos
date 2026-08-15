package event

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestEventBusEmit(t *testing.T) {
	b := NewEventBus()
	var calls []string
	_ = b.On("ev", ModeEmit, func(_ context.Context, p any) (any, error) {
		calls = append(calls, p.(string))
		return nil, nil
	})
	_ = b.Emit(context.Background(), "ev", "a")
	if len(calls) != 1 || calls[0] != "a" {
		t.Fatalf("calls = %v", calls)
	}
	// 错误中断
	wantErr := errors.New("boom")
	_ = b.On("ev", ModeEmit, func(_ context.Context, _ any) (any, error) {
		return nil, wantErr
	})
	_ = b.On("ev", ModeEmit, func(_ context.Context, _ any) (any, error) {
		t.Fatal("后续监听器不应执行")
		return nil, nil
	})
	if err := b.Emit(context.Background(), "ev", "b"); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestEventBusWaterfall(t *testing.T) {
	b := NewEventBus()
	_ = b.On("wf", ModeWaterfall, func(_ context.Context, p any) (any, error) {
		return p.(string) + "1", nil
	})
	_ = b.On("wf", ModeWaterfall, func(_ context.Context, p any) (any, error) {
		return p.(string) + "2", nil
	})
	out, err := b.Waterfall(context.Background(), "wf", "")
	if err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if out != "12" {
		t.Fatalf("out = %v, want 12", out)
	}
}

func TestEventBusSerial(t *testing.T) {
	b := NewEventBus()
	_ = b.On("s", ModeSerial, func(_ context.Context, _ any) (any, error) { return true, nil })
	ok, err := b.Serial(context.Background(), "s", nil)
	if err != nil || !ok {
		t.Fatalf("serial all-pass: ok=%v err=%v", ok, err)
	}
	_ = b.On("s", ModeSerial, func(_ context.Context, _ any) (any, error) { return false, nil })
	ok, err = b.Serial(context.Background(), "s", nil)
	if err != nil || ok {
		t.Fatalf("serial short-circuit: ok=%v err=%v, want false", ok, err)
	}
}

func TestEventBusParallel(t *testing.T) {
	b := NewEventBus()
	var n atomic.Int32
	for i := 0; i < 4; i++ {
		_ = b.On("p", ModeParallel, func(_ context.Context, _ any) (any, error) {
			n.Add(1)
			return nil, nil
		})
	}
	if err := b.Parallel(context.Background(), "p", nil); err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	if n.Load() != 4 {
		t.Fatalf("parallel calls = %d, want 4", n.Load())
	}
}

func TestEventBusUnregister(t *testing.T) {
	b := NewEventBus()
	remove := b.On("ev", ModeEmit, func(_ context.Context, _ any) (any, error) { return nil, nil })
	remove()
	remove() // 幂等
	if err := b.Emit(context.Background(), "ev", nil); err != nil {
		t.Fatalf("Emit after unregister: %v", err)
	}
	// 重新注册后生效
	called := false
	_ = b.On("ev", ModeEmit, func(_ context.Context, _ any) (any, error) { called = true; return nil, nil })
	_ = b.Emit(context.Background(), "ev", nil)
	if !called {
		t.Fatal("listener re-registered should be called")
	}
}
