package react

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// blockingPlanner 等 ctx 结束才返回（模拟"模型调用被取消"）。
type blockingPlanner struct{}

func (p *blockingPlanner) NextAction(ctx context.Context, _ []llm.Message, _ []llm.Tool) (planner.Action, error) {
	<-ctx.Done()
	return planner.Action{}, ctx.Err()
}

// turnEndError 取最后一条 turn/end 的错误详情。
func turnEndError(t *testing.T, sess *session.Session) string {
	t.Helper()
	evs := sess.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == session.EventTurnEnd {
			return evs[i].Error
		}
	}
	t.Fatal("没有 turn/end 事件")
	return ""
}

// 取消原因要能分辨：宿主给了原因（WithCancelCause）→ turn/end 与返回错误
// 都写明原因；不再是一个分不清的裸 context canceled。
func TestTurnEndExplainsCancelCause(t *testing.T) {
	sess := session.New("t", nil)
	rt := &driver.RunContext{Planner: &blockingPlanner{}}
	a := driver.New(rt, New())

	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("用户点了停止")
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(cause)
	}()

	_, err := a.Run(ctx, sess, "问题")
	if err == nil {
		t.Fatal("取消应以错误收尾")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("返回错误应认得出取消原因: %v", err)
	}
	detail := turnEndError(t, sess)
	if !strings.Contains(detail, "用户点了停止") {
		t.Fatalf("turn/end 应写明取消原因: %q", detail)
	}
}

// 普通取消（宿主没给原因）保持原样：还是 context canceled，不编造原因。
func TestTurnEndPlainCancelUnchanged(t *testing.T) {
	sess := session.New("t", nil)
	rt := &driver.RunContext{Planner: &blockingPlanner{}}
	a := driver.New(rt, New())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := a.Run(ctx, sess, "问题")
	if err == nil {
		t.Fatal("取消应以错误收尾")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误链应保留 context.Canceled: %v", err)
	}
	detail := turnEndError(t, sess)
	if !strings.Contains(detail, "context canceled") {
		t.Fatalf("turn/end 应保留取消信息: %q", detail)
	}
}

// RunWithCause 挂的兜底原因：普通取消时用它解释（宿主不想自己管 cancel 函数）。
func TestTurnEndUsesFallbackCancelCause(t *testing.T) {
	sess := session.New("t", nil)
	rt := &driver.RunContext{Planner: &blockingPlanner{}}
	a := driver.New(rt, New())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	cause := errors.New("用户点了停止")
	_, err := a.RunWithCause(ctx, sess, "问题", cause)
	if err == nil {
		t.Fatal("取消应以错误收尾")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("返回错误应认得出取消原因: %v", err)
	}
	if detail := turnEndError(t, sess); !strings.Contains(detail, "用户点了停止") {
		t.Fatalf("turn/end 应写明兜底原因: %q", detail)
	}
}
