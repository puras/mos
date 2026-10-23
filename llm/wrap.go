package llm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

// 标准包装器（Standard Wrappers）。
//
// 统一协议（ChatModel 接口）使横切逻辑可以包装为可插拔的标准装饰器：
// 任何包装器都实现 ChatModel 接口并组合底层 ChatModel，调用方按需嵌套组合。
// 约定：签名统一为 WithXxx(m ChatModel, ...) ChatModel；组合顺序即执行顺序（外层先执行）。
//
// 与"通用中间件链"的区别：这里没有隐式顺序与全局注册表，
// 每个包装器职责单一、嵌套关系由调用方显式书写，可读性与可测试性更好。

// CallResult 描述一次模型调用的结果，供观测包装器回调。
type CallResult struct {
	Req     *Request
	Resp    *Response
	Err     error
	Elapsed time.Duration
	Stream  bool // 是否为流式调用（ChatStream）
}

// WithLogging 返回一个每次调用结束后回调 logf 的包装器（发起/结果/失败统一在结束时上报）。
// 日志内容（模型名/地址等）由调用方闭包持有，本包装器只负责时机与上下文。
func WithLogging(m ChatModel, logf func(CallResult)) ChatModel {
	return &loggingWrapper{m: m, logf: logf}
}

type loggingWrapper struct {
	m    ChatModel
	logf func(CallResult)
}

func (w *loggingWrapper) Capabilities() Capability { return w.m.Capabilities() }

func (w *loggingWrapper) Chat(ctx context.Context, req *Request) (*Response, error) {
	start := time.Now()
	resp, err := w.m.Chat(ctx, req)
	w.logf(CallResult{Req: req, Resp: resp, Err: err, Elapsed: time.Since(start)})
	return resp, err
}

func (w *loggingWrapper) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	start := time.Now()
	resp, err := w.m.ChatStream(ctx, req, emit)
	w.logf(CallResult{Req: req, Resp: resp, Err: err, Elapsed: time.Since(start), Stream: true})
	return resp, err
}

// WithTimeout 返回一个为每次调用设置超时的包装器（Chat 与 ChatStream 均生效）。
func WithTimeout(m ChatModel, timeout time.Duration) ChatModel {
	return &timeoutWrapper{m: m, timeout: timeout}
}

type timeoutWrapper struct {
	m       ChatModel
	timeout time.Duration
}

func (w *timeoutWrapper) Capabilities() Capability { return w.m.Capabilities() }

func (w *timeoutWrapper) Chat(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.m.Chat(ctx, req)
}

func (w *timeoutWrapper) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	return w.m.ChatStream(ctx, req, emit)
}

// WithFallback 返回一个主模型失败时自动切换备用模型的包装器（Chat 与 ChatStream 均生效）。
// backup 为 nil 时等价于直接使用主模型。
func WithFallback(m ChatModel, backup ChatModel) ChatModel {
	return &fallbackWrapper{m: m, backup: backup}
}

type fallbackWrapper struct {
	m      ChatModel
	backup ChatModel
}

func (w *fallbackWrapper) Capabilities() Capability {
	if w.backup == nil {
		return w.m.Capabilities()
	}
	return w.m.Capabilities() | w.backup.Capabilities()
}

func (w *fallbackWrapper) Chat(ctx context.Context, req *Request) (*Response, error) {
	resp, err := w.m.Chat(ctx, req)
	if err == nil || w.backup == nil {
		return resp, err
	}
	return w.backup.Chat(ctx, req)
}

func (w *fallbackWrapper) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	resp, err := w.m.ChatStream(ctx, req, emit)
	if err == nil || w.backup == nil {
		return resp, err
	}
	return w.backup.ChatStream(ctx, req, emit)
}

// WithCache 返回一个按请求内容缓存非流式结果的包装器（ChatStream 不缓存，直接透传）。
// ttl <= 0 表示不过期。缓存的响应为共享只读对象，调用方不应修改。
func WithCache(m ChatModel, ttl time.Duration) ChatModel {
	return &cacheWrapper{m: m, ttl: ttl, entries: make(map[string]cacheEntry)}
}

type cacheEntry struct {
	resp   *Response
	expiry time.Time
}

type cacheWrapper struct {
	m       ChatModel
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func (w *cacheWrapper) Capabilities() Capability { return w.m.Capabilities() }

func (w *cacheWrapper) Chat(ctx context.Context, req *Request) (*Response, error) {
	key := cacheKey(req)
	if resp, ok := w.lookup(key); ok {
		return resp, nil
	}
	resp, err := w.m.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.entries[key] = cacheEntry{resp: resp, expiry: time.Now().Add(w.ttl)}
	w.mu.Unlock()
	return resp, nil
}

func (w *cacheWrapper) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	return w.m.ChatStream(ctx, req, emit)
}

func (w *cacheWrapper) lookup(key string) (*Response, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[key]
	if !ok {
		return nil, false
	}
	if w.ttl > 0 && time.Now().After(e.expiry) {
		delete(w.entries, key)
		return nil, false
	}
	return e.resp, true
}

// cacheKey 以请求的规范化 JSON 为缓存键。
func cacheKey(req *Request) string {
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return string(sum[:])
}
