package tool

import (
	"context"
	"encoding/json"
	"time"
)

// 标准包装器（Standard Wrappers）。
// 与 llm.ChatModel 包装器同约定：签名 WithXxx(t Tool, ...) Tool，组合顺序即执行顺序。

// Result 描述一次工具执行的结果，供观测包装器回调。
type Result struct {
	Name    string
	Args    json.RawMessage
	Out     any
	Err     error
	Elapsed time.Duration
}

// WithTimeout 返回一个为 Execute 设置超时的工具包装器。
func WithTimeout(t Tool, timeout time.Duration) Tool {
	return &timeoutTool{t: t, timeout: timeout}
}

type timeoutTool struct {
	t       Tool
	timeout time.Duration
}

func (w *timeoutTool) Name() string        { return w.t.Name() }
func (w *timeoutTool) Description() string { return w.t.Description() }
func (w *timeoutTool) Parameters() any     { return w.t.Parameters() }

func (w *timeoutTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.t.Execute(ctx, args)
}

// WithLogging 返回一个每次执行结束后回调 logf 的工具包装器。
func WithLogging(t Tool, logf func(Result)) Tool {
	return &loggingTool{t: t, logf: logf}
}

type loggingTool struct {
	t    Tool
	logf func(Result)
}

func (w *loggingTool) Name() string        { return w.t.Name() }
func (w *loggingTool) Description() string { return w.t.Description() }
func (w *loggingTool) Parameters() any     { return w.t.Parameters() }

func (w *loggingTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	start := time.Now()
	out, err := w.t.Execute(ctx, args)
	if w.logf != nil {
		w.logf(Result{Name: w.t.Name(), Args: args, Out: out, Err: err, Elapsed: time.Since(start)})
	}
	return out, err
}
