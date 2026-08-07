package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// fakeTool 是测试用的可编程工具。
type fakeTool struct {
	name  string
	calls int
	err   error
	out   any
	slow  time.Duration
}

func (t *fakeTool) Name() string        { return t.name }
func (t *fakeTool) Description() string { return "fake" }
func (t *fakeTool) Parameters() any     { return struct{}{} }

func (t *fakeTool) Execute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.calls++
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(t.slow):
	}
	if t.err != nil {
		return nil, t.err
	}
	return t.out, nil
}

func TestToolWithTimeout(t *testing.T) {
	base := &fakeTool{name: "slow", slow: 50 * time.Millisecond}
	w := WithTimeout(base, time.Millisecond)
	if _, err := w.Execute(context.Background(), nil); err == nil {
		t.Fatal("Execute should exceed timeout")
	}
}

func TestToolWithLogging(t *testing.T) {
	base := &fakeTool{name: "calc", out: 42}
	var got []Result
	w := WithLogging(base, func(r Result) { got = append(got, r) })
	args := json.RawMessage(`{"a":1}`)
	out, err := w.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != 42 {
		t.Fatalf("out = %v, want 42", out)
	}
	if len(got) != 1 || got[0].Name != "calc" || string(got[0].Args) != `{"a":1}` {
		t.Fatalf("result = %+v", got)
	}
}

func TestToolWrapperKeepsIdentity(t *testing.T) {
	base := &fakeTool{name: "x"}
	w := WithTimeout(WithLogging(base, nil), time.Second)
	if w.Name() != "x" || w.Description() != "fake" {
		t.Fatalf("identity lost: %s / %s", w.Name(), w.Description())
	}
}
