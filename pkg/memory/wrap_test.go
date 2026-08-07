package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"hegq.net/mos/pkg/llm"
)

// fakeMemory 是测试用的可编程记忆实现。
type fakeMemory struct {
	err   error
	slow  time.Duration
	calls int
}

func (m *fakeMemory) Load(ctx context.Context, _ string) ([]llm.Message, error) {
	m.calls++
	return nil, m.block(ctx)
}

func (m *fakeMemory) Append(ctx context.Context, _ string, _ llm.Message) error {
	m.calls++
	return m.block(ctx)
}

func (m *fakeMemory) Clear(ctx context.Context, _ string) error {
	m.calls++
	return m.block(ctx)
}

func (m *fakeMemory) block(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.slow):
	}
	return m.err
}

func TestMemoryWithLogging(t *testing.T) {
	base := &fakeMemory{err: errors.New("disk full")}
	var got []Op
	m := WithLogging(base, func(op Op) { got = append(got, op) })
	if err := m.Append(context.Background(), "s1", llm.Message{}); err == nil {
		t.Fatal("Append should fail")
	}
	base.err = nil
	if err := m.Clear(context.Background(), "s1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(got) != 2 || got[0].Kind != "append" || got[0].Err == nil || got[0].SessionID != "s1" {
		t.Fatalf("ops = %+v", got)
	}
	if got[1].Kind != "clear" || got[1].Err != nil {
		t.Fatalf("ops = %+v", got)
	}
}

func TestMemoryWithTimeout(t *testing.T) {
	base := &fakeMemory{slow: 50 * time.Millisecond}
	m := WithTimeout(base, time.Millisecond)
	if _, err := m.Load(context.Background(), "s1"); err == nil {
		t.Fatal("Load should exceed timeout")
	}
}
