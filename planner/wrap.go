package planner

import (
	"context"
	"time"

	"hegq.net/mos/llm"
)

// 标准包装器（Standard Wrappers）。
// 与 llm.ChatModel 包装器同约定：签名 WithXxx(p Planner, ...) Planner，组合顺序即执行顺序。

// LogResult 描述一次规划决策，供观测包装器回调。
type LogResult struct {
	History []llm.Message
	Tools   []llm.Tool
	Action  Action
	Err     error
	Elapsed time.Duration
}

// WithLogging 返回一个每次决策结束后回调 logf 的规划器包装器。
func WithLogging(p Planner, logf func(LogResult)) Planner {
	return &loggingPlanner{p: p, logf: logf}
}

type loggingPlanner struct {
	p    Planner
	logf func(LogResult)
}

func (w *loggingPlanner) NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error) {
	start := time.Now()
	action, err := w.p.NextAction(ctx, history, tools)
	if w.logf != nil {
		w.logf(LogResult{History: history, Tools: tools, Action: action, Err: err, Elapsed: time.Since(start)})
	}
	return action, err
}

// WithRetry 返回一个在决策失败时按固定间隔重试的规划器包装器。
// attempts 为总尝试次数（<=0 视为 1）。
func WithRetry(p Planner, attempts int, delay time.Duration) Planner {
	return &retryPlanner{p: p, attempts: attempts, delay: delay}
}

type retryPlanner struct {
	p        Planner
	attempts int
	delay    time.Duration
}

func (w *retryPlanner) NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error) {
	attempts := w.attempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		action, err := w.p.NextAction(ctx, history, tools)
		if err == nil {
			return action, nil
		}
		lastErr = err
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return Action{}, ctx.Err()
			case <-time.After(w.delay):
			}
		}
	}
	return Action{}, lastErr
}
