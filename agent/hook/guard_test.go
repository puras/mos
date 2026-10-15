package hook

import (
	"context"
	"errors"
	"testing"
)

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