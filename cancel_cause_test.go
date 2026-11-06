package mos_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// blockingModel 等 ctx 结束才返回（模拟"模型调用被取消"）。
type blockingModel struct{}

func (m *blockingModel) Chat(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *blockingModel) Capabilities() llm.Capability { return llm.CapStreaming }

// 取消原因要能分辨：宿主用 context.WithCancelCause 取消时，turn/end 的
// 错误详情与返回错误都写明原因（"用户点了停止"），不是一个分不清的裸
// context canceled。
func TestRunWithCancelCauseIsDistinguishable(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &blockingModel{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)

	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("用户点了停止")
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(cause)
	}()

	_, err = a.Run(ctx, sess, "问题")
	if err == nil {
		t.Fatal("取消应以错误收尾")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("返回错误应认得出取消原因: %v", err)
	}

	evs := sess.Events()
	var detail string
	found := false
	for _, ev := range evs {
		if ev.Kind == session.EventTurnEnd {
			detail = ev.Error
			found = true
		}
	}
	if !found {
		t.Fatal("没有 turn/end 事件")
	}
	if !strings.Contains(detail, "用户点了停止") {
		t.Fatalf("turn/end 应写明取消原因: %q", detail)
	}
}

// RunWithCause：宿主不想自己管 cancel 函数时，兜底原因同样进 turn/end。
func TestRunWithCauseFallbackIsDistinguishable(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &blockingModel{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	cause := errors.New("用户点了停止")
	if _, err := a.RunWithCause(ctx, sess, "问题", cause); err == nil {
		t.Fatal("取消应以错误收尾")
	} else if !errors.Is(err, cause) {
		t.Fatalf("返回错误应认得出取消原因: %v", err)
	}

	for _, ev := range sess.Events() {
		if ev.Kind == session.EventTurnEnd && !strings.Contains(ev.Error, "用户点了停止") {
			t.Fatalf("turn/end 应写明兜底原因: %q", ev.Error)
		}
	}
}
