package memory

import (
	"context"
	"time"

	"hegq.net/mos/llm"
)

// 标准包装器（Standard Wrappers）。
// 与 llm.ChatModel 包装器同约定：签名 WithXxx(m Memory, ...) Memory，组合顺序即执行顺序。

// Op 描述一次记忆操作，供观测包装器回调。
type Op struct {
	// Kind 是操作名：load | append | clear。
	Kind      string
	SessionID string
	Err       error
	Elapsed   time.Duration
}

// WithLogging 返回一个每次操作结束后回调 logf 的记忆包装器。
func WithLogging(m Memory, logf func(Op)) Memory {
	return &loggingMemory{m: m, logf: logf}
}

type loggingMemory struct {
	m    Memory
	logf func(Op)
}

func (w *loggingMemory) Load(ctx context.Context, sessionID string) ([]llm.Message, error) {
	start := time.Now()
	msgs, err := w.m.Load(ctx, sessionID)
	w.log("load", sessionID, err, time.Since(start))
	return msgs, err
}

func (w *loggingMemory) Append(ctx context.Context, sessionID string, msg llm.Message) error {
	start := time.Now()
	err := w.m.Append(ctx, sessionID, msg)
	w.log("append", sessionID, err, time.Since(start))
	return err
}

func (w *loggingMemory) Clear(ctx context.Context, sessionID string) error {
	start := time.Now()
	err := w.m.Clear(ctx, sessionID)
	w.log("clear", sessionID, err, time.Since(start))
	return err
}

func (w *loggingMemory) log(kind, sessionID string, err error, elapsed time.Duration) {
	if w.logf != nil {
		w.logf(Op{Kind: kind, SessionID: sessionID, Err: err, Elapsed: elapsed})
	}
}

// WithTimeout 返回一个为每次操作设置超时的记忆包装器。
func WithTimeout(m Memory, timeout time.Duration) Memory {
	return &timeoutMemory{m: m, timeout: timeout}
}

type timeoutMemory struct {
	m       Memory
	timeout time.Duration
}

func (w *timeoutMemory) Load(ctx context.Context, sessionID string) ([]llm.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.m.Load(ctx, sessionID)
}

func (w *timeoutMemory) Append(ctx context.Context, sessionID string, msg llm.Message) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.m.Append(ctx, sessionID, msg)
}

func (w *timeoutMemory) Clear(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.m.Clear(ctx, sessionID)
}
