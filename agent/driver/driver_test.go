package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// stubPlanner 是最小决策器（RunContext 的必填能力）。
type stubPlanner struct{}

func (p *stubPlanner) NextAction(context.Context, []llm.Message, []llm.Tool) (planner.Action, error) {
	return planner.Action{Content: "ok"}, nil
}

// stubDriver 记录收到的 ctx（验证 RunWithCause 把取消原因挂进了 ctx）。
type stubDriver struct{ ctx context.Context }

func (d *stubDriver) Run(ctx context.Context, _ *RunContext, _ *session.Session, _ string) (string, error) {
	d.ctx = ctx
	return "done", nil
}

// 取消原因的解析：没取消 → nil；普通取消 → context.Canceled；宿主自己给了
// 原因 → 那个原因；RunWithCause 挂的兜底原因只在没有更具体原因时用；
// ctx 超时 → DeadlineExceeded（"超时中止"可分辨）。
func TestCancelCauseResolution(t *testing.T) {
	if got := CancelCause(context.Background()); got != nil {
		t.Fatalf("没被取消时不该有取消原因: %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := CancelCause(ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("普通取消应是 context.Canceled: %v", got)
	}

	// 宿主自己用 WithCancelCause 取消 → 优先用它给的原因
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	want := errors.New("用户点了停止")
	cancel2(want)
	if got := CancelCause(ctx2); !errors.Is(got, want) {
		t.Fatalf("应取宿主给的原因: %v", got)
	}

	// RunWithCause 挂的兜底原因（普通取消时用）
	ctx3, cancel3 := context.WithCancel(WithCancelCause(context.Background(), want))
	cancel3()
	if got := CancelCause(ctx3); !errors.Is(got, want) {
		t.Fatalf("应取兜底原因: %v", got)
	}

	// ctx 超时 → DeadlineExceeded
	ctx4, cancel4 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel4()
	<-ctx4.Done()
	if got := CancelCause(ctx4); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("超时应是 DeadlineExceeded: %v", got)
	}
}

// RunWithCause 把取消原因挂进 ctx（驱动据此解释 turn/end 的错误）。
func TestAgentRunWithCauseAttachesCause(t *testing.T) {
	d := &stubDriver{}
	a := New(&RunContext{Planner: &stubPlanner{}}, d)
	want := errors.New("用户点了停止")
	if _, err := a.RunWithCause(context.Background(), session.New("t", nil), "问题", want); err != nil {
		t.Fatalf("RunWithCause: %v", err)
	}
	got, _ := d.ctx.Value(cancelCauseKey{}).(error)
	if !errors.Is(got, want) {
		t.Fatalf("取消原因应挂进 ctx: %v", got)
	}
}

// 没给原因时 RunWithCause 不改变任何行为（不挂兜底原因）。
func TestAgentRunWithCauseWithoutCause(t *testing.T) {
	d := &stubDriver{}
	a := New(&RunContext{Planner: &stubPlanner{}}, d)
	if _, err := a.RunWithCause(context.Background(), session.New("t", nil), "问题", nil); err != nil {
		t.Fatalf("RunWithCause: %v", err)
	}
	if got, _ := d.ctx.Value(cancelCauseKey{}).(error); got != nil {
		t.Fatalf("没给原因就不该挂: %v", got)
	}
}
