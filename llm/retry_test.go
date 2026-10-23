package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// 默认可重试判定：只重试瞬时错误。
func TestDefaultRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 限流", &StatusError{Provider: "openai", StatusCode: 429, Message: "rate limited"}, true},
		{"503 服务不可用", &StatusError{Provider: "openai", StatusCode: 503, Message: "unavailable"}, true},
		{"500", &StatusError{Provider: "openai", StatusCode: 500, Message: "boom"}, true},
		{"400 参数错", &StatusError{Provider: "openai", StatusCode: 400, Message: "bad request"}, false},
		{"401 鉴权失败", &StatusError{Provider: "openai", StatusCode: 401, Message: "bad key"}, false},
		{"404", &StatusError{Provider: "openai", StatusCode: 404, Message: "no such model"}, false},
		{"配额耗尽", &StatusError{Provider: "openai", StatusCode: 429, Message: "insufficient_quota"}, true},
		{"用户取消", context.Canceled, false},
		{"空响应", fmt.Errorf("openai: %w", ErrEmptyResponse), true},
		{"连接被拒", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"EOF", io.EOF, true},
		{"普通错误", errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DefaultRetryable(c.err); got != c.want {
				t.Fatalf("DefaultRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// 状态错误被包装后仍能取出状态码（errors.As 穿透）。
func TestStatusCodeOf(t *testing.T) {
	err := fmt.Errorf("planner: %w", &StatusError{Provider: "openai", StatusCode: 429, Message: "限流"})
	if got := StatusCodeOf(err); got != 429 {
		t.Fatalf("StatusCodeOf = %d, want 429", got)
	}
	if got := StatusCodeOf(errors.New("x")); got != 0 {
		t.Fatalf("非状态错误应返回 0, got %d", got)
	}
}

// 状态错误的文本格式与旧的 fmt.Errorf 版本一致（既有日志不必改）。
func TestStatusErrorText(t *testing.T) {
	e := &StatusError{Provider: "openai", StatusCode: 429, Message: "rate limited"}
	if got := e.Error(); got != "openai: rate limited" {
		t.Fatalf("Error() = %q", got)
	}
}

// 退避：有界指数 + 上限。
func TestRetryDelayExponentialAndCapped(t *testing.T) {
	w := &retryWrapper{p: RetryPolicy{
		MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 350 * time.Millisecond, Jitter: -1,
	}.normalized()}
	if got := w.delay(1); got != 100*time.Millisecond {
		t.Fatalf("第 1 次退避 = %v, want 100ms", got)
	}
	if got := w.delay(2); got != 200*time.Millisecond {
		t.Fatalf("第 2 次退避 = %v, want 200ms", got)
	}
	if got := w.delay(3); got != 350*time.Millisecond {
		t.Fatalf("第 3 次退避应被上界夹住 = %v, want 350ms", got)
	}
	if got := w.delay(9); got != 350*time.Millisecond {
		t.Fatalf("第 9 次退避应仍被上界夹住 = %v", got)
	}
}

// 抖动在 ±Jitter 范围内（避免惊群，但不能跑到负数或越界）。
func TestRetryDelayJitter(t *testing.T) {
	w := &retryWrapper{p: RetryPolicy{
		MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second, Jitter: 0.2,
	}.normalized()}
	for i := 0; i < 50; i++ {
		got := w.delay(1)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("抖动超出范围: %v", got)
		}
	}
}

// ctx 取消是终止性的：不重试。
func TestRetryStopsOnCancel(t *testing.T) {
	base := &flakyModel{failTimes: 99, resp: &Response{}}
	m := WithRetry(base, RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Chat(ctx, &Request{}); err == nil {
		t.Fatal("应当报错")
	}
	if base.calls != 1 {
		t.Fatalf("calls = %d, want 1（取消不重试）", base.calls)
	}
}

// 重试过程要看得见：OnScheduled 逐次上报，OnFinished 收尾。
func TestRetryObservability(t *testing.T) {
	base := &fakeModel{resp: &Response{}}
	var scheduled []int
	var finishedAttempts int
	var finishedErr error
	m := WithRetry(base, RetryPolicy{
		MaxAttempts: 3, BaseDelay: time.Millisecond,
		OnScheduled: func(attempt int, delay time.Duration, err error) {
			scheduled = append(scheduled, attempt)
		},
		OnFinished: func(attempts int, err error) {
			finishedAttempts, finishedErr = attempts, err
		},
	})
	base.err = &StatusError{Provider: "openai", StatusCode: 503, Message: "unavailable"}
	if _, err := m.Chat(context.Background(), &Request{}); err == nil {
		t.Fatal("应当报错")
	}
	if len(scheduled) != 2 || scheduled[0] != 1 || scheduled[1] != 2 {
		t.Fatalf("OnScheduled = %v, want [1 2]", scheduled)
	}
	if finishedAttempts != 3 {
		t.Fatalf("OnFinished attempts = %d, want 3", finishedAttempts)
	}
	if finishedErr == nil {
		t.Fatal("OnFinished 应带上最终错误")
	}
}

// 恢复后成功：重试应把瞬时故障吃掉。
func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	flaky := &flakyModel{failTimes: 1, resp: &Response{}}
	m := WithRetry(flaky, RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond})
	resp, err := m.Chat(context.Background(), &Request{})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if resp == nil {
		t.Fatal("resp = nil")
	}
	if flaky.calls != 2 {
		t.Fatalf("calls = %d, want 2（失败一次后重试成功）", flaky.calls)
	}
}

// flakyModel 前 failTimes 次失败，之后成功。
type flakyModel struct {
	failTimes int
	calls     int
	resp      *Response
}

func (m *flakyModel) Capabilities() Capability { return CapFunctionCalling }

func (m *flakyModel) Chat(context.Context, *Request) (*Response, error) {
	m.calls++
	if m.calls <= m.failTimes {
		return nil, &StatusError{Provider: "openai", StatusCode: 503, Message: "unavailable"}
	}
	return m.resp, nil
}

func (m *flakyModel) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	return m.Chat(ctx, req)
}
