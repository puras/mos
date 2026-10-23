package llm

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"time"
)

// RetryPolicy 是重试策略：**机制由框架实现，参数由宿主给**。
//
// 分工的理由：重试是模型调用层的事，不该散落到业务各处各写一遍（策略会
// 不一致，细节总会漏）；但"重试几次、退避多久、哪些错误值得重试"是产品
// 决策——交互式 CLI 与后台批处理的合理重试完全不同——框架不该发明默认
// 策略。所以这里是**一个不带默认值的结构体**：不装配就没有重试
// （默认 fail-fast，把问题早暴露）。
//
// 对标：PI 的 packages/ai/src/utils/retry.ts、DSH 的
// packages/llm/llm/src/retry-policy.ts。
type RetryPolicy struct {
	// MaxAttempts 是总尝试次数（**含首次调用**）；<=1 表示不重试。
	// 默认 1（不重试）。
	MaxAttempts int
	// BaseDelay 是首次退避的基数；<=0 用默认 500ms。
	BaseDelay time.Duration
	// MaxDelay 是单次退避的上限；<=0 用默认 30s。
	MaxDelay time.Duration
	// Jitter 是抖动比例（0..1）：实际延迟 = 计算值 × (1 ± jitter)。
	// 抖动是为了避开惊群——多个实例同时失败、同时重试、同时再失败。
	// <0 显式关闭；0 用默认 0.1；>0 用该值。
	Jitter float64
	// Retryable 覆盖默认的可重试判定；nil 用 DefaultRetryable。
	Retryable func(error) bool
	// OnScheduled 在每次重试的退避等待**之前**调用（attempt 从 1 开始）。
	// 宿主可在这里记日志/打指标——静默重试是查不出问题的。
	OnScheduled func(attempt int, delay time.Duration, err error)
	// OnFinished 在重试循环结束时调用一次（成功也给；err 为最终错误或 nil）。
	OnFinished func(attempts int, err error)
}

const (
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 30 * time.Second
	defaultRetryJitter    = 0.1
)

// WithRetry 返回一个按策略重试的包装器（Chat 与 ChatStream 均生效）。
//
// 行为：
//   - 只重试**瞬时**错误（默认判定见 DefaultRetryable）：限流、5xx、
//     连接层故障、空响应。4xx 参数/鉴权错误、配额耗尽、用户取消
//     **快速失败**——重试确定性的错误只是浪费配额、拖长失败；
//   - 退避是**有界指数 + 抖动**：delay = min(BaseDelay × 2^(n-1), MaxDelay)，
//     再乘 (1 ± Jitter)；
//   - **取消是终止性的**：ctx 结束（取消/超时）立即停止，不再重试；
//   - 流式只在**还没吐出任何内容**时重试——增量已下发，重试会让用户
//     看到重复内容。
func WithRetry(m ChatModel, p RetryPolicy) ChatModel {
	return &retryWrapper{m: m, p: p.normalized()}
}

type retryWrapper struct {
	m ChatModel
	p RetryPolicy
}

// normalized 补齐缺省值（策略里没填的用内置默认）。
func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultRetryBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultRetryMaxDelay
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	switch {
	case p.Jitter < 0:
		p.Jitter = 0 // 显式关闭抖动
	case p.Jitter == 0:
		p.Jitter = defaultRetryJitter
	}
	if p.Retryable == nil {
		p.Retryable = DefaultRetryable
	}
	return p
}

// scheduled 通知宿主：本次要重试（回调可为 nil）。
func (p RetryPolicy) scheduled(attempt int, delay time.Duration, err error) {
	if p.OnScheduled != nil {
		p.OnScheduled(attempt, delay, err)
	}
}

// finished 通知宿主：重试循环结束（回调可为 nil）。
func (p RetryPolicy) finished(attempts int, err error) {
	if p.OnFinished != nil {
		p.OnFinished(attempts, err)
	}
}

func (w *retryWrapper) Capabilities() Capability { return w.m.Capabilities() }

func (w *retryWrapper) Chat(ctx context.Context, req *Request) (*Response, error) {
	var lastErr error
	made := 0
	for attempt := 1; attempt <= w.p.MaxAttempts; attempt++ {
		made = attempt
		resp, err := w.m.Chat(ctx, req)
		if err == nil {
			w.p.finished(made, nil)
			return resp, nil
		}
		lastErr = err
		if !w.shouldRetry(ctx, err) || attempt == w.p.MaxAttempts {
			break
		}
		delay := w.delay(attempt)
		w.p.scheduled(attempt, delay, err)
		if !sleepCtx(ctx, delay) {
			// ctx 结束：终止性错误优先于原始错误（取消就是取消）。
			w.p.finished(made, ctx.Err())
			return nil, ctx.Err()
		}
	}
	w.p.finished(made, lastErr)
	return nil, lastErr
}

func (w *retryWrapper) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	// 还没吐出任何内容才可重试：增量已下发的话，重试会让用户看到重复内容。
	emitted := false
	guarded := func(c StreamChunk) error {
		if emit == nil {
			return nil
		}
		emitted = true
		return emit(c)
	}

	var lastErr error
	made := 0
	for attempt := 1; attempt <= w.p.MaxAttempts; attempt++ {
		made = attempt
		resp, err := w.m.ChatStream(ctx, req, guarded)
		if err == nil {
			w.p.finished(made, nil)
			return resp, nil
		}
		lastErr = err
		if emitted || !w.shouldRetry(ctx, err) || attempt == w.p.MaxAttempts {
			break
		}
		delay := w.delay(attempt)
		w.p.scheduled(attempt, delay, err)
		if !sleepCtx(ctx, delay) {
			w.p.finished(made, ctx.Err())
			return nil, ctx.Err()
		}
	}
	w.p.finished(made, lastErr)
	return nil, lastErr
}

// shouldRetry 判定这次失败是否值得重试。
func (w *retryWrapper) shouldRetry(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// ctx 已经结束（取消/超时）：没有预算了，重试没有意义。
	if ctx.Err() != nil {
		return false
	}
	// 用户取消是终止性的，永远不重试。
	if errors.Is(err, context.Canceled) {
		return false
	}
	return w.p.Retryable(err)
}

// delay 计算第 attempt 次重试前的等待时长：有界指数 + 抖动。
func (w *retryWrapper) delay(attempt int) time.Duration {
	d := w.p.BaseDelay
	for i := 1; i < attempt && d < w.p.MaxDelay; i++ {
		d *= 2
	}
	if d > w.p.MaxDelay {
		d = w.p.MaxDelay
	}
	// 抖动：均匀分布于 ±Jitter，避免惊群。
	if w.p.Jitter > 0 {
		delta := float64(d) * w.p.Jitter
		d = time.Duration(float64(d) + (rand.Float64()*2-1)*delta)
		if d < 0 {
			d = 0
		}
	}
	return d
}

// sleepCtx 等待 d；ctx 先结束则返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// DefaultRetryable 是默认可重试判定：**只重试瞬时错误**。
//
// 依据 PI（packages/ai/src/utils/retry.ts）与 DSH
// （packages/llm/llm/src/retry-policy.ts）的共识：
//
//	可重试：429 限流、408 超时、5xx 服务端故障、连接层失败、空响应；
//	不可重试：4xx 参数/鉴权错误、配额与计费耗尽、用户取消、TLS 证书问题。
//
// 不可重试的**快速失败**比"无脑重试"重要：重试确定性的错误只是浪费配额、
// 拖长失败，还会在日志里放大成一片红。宿主可用 RetryPolicy.Retryable 覆盖。
func DefaultRetryable(err error) bool {
	if err == nil {
		return false
	}
	// 用户取消：终止性。
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 空响应通常是瞬时的（网关抖动）。
	if errors.Is(err, ErrEmptyResponse) {
		return true
	}
	// 有状态码：只重试明确的瞬时码。
	var se *StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	// 超时（DSH 的 TIMEOUT）：值得重试——但外层 ctx 若已结束，循环里会先拦下。
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// 连接层失败：OpError 覆盖 refused/reset/unreachable；net.Error 覆盖读超时。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// 响应中途断开。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}
