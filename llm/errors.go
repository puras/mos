package llm

import (
	"errors"
	"fmt"
)

// StatusError 是带 HTTP 状态码的模型调用错误。
//
// 为什么要有它：错误文本是拍平的字符串——"openai: Rate limited" 与
// "openai: bad request" 分不出该重试还是该放弃。分类信息一旦在适配器层
// 丢了，上层只能瞎重试（浪费配额、放大故障）或瞎放弃（一次瞬时抖动直接
// 变成整轮失败）。结构化字段让重试策略、指标、审计都拿到**事实**，
// 而不是去 parse 字符串。
type StatusError struct {
	// Provider 是协议提供方（"openai" / "anthropic" / "gemini" …）。
	Provider string
	// StatusCode 是 HTTP 状态码（429、503 等；0 = 无状态码）。
	StatusCode int
	// Message 是服务端返回的错误信息（未解析时是响应体摘要）。
	Message string
	// Err 是底层错误（可 nil）：如响应体读取失败。
	Err error
}

// Error 实现 error。文本格式与旧的 fmt.Errorf 版本保持一致
// （"<provider>: <message>"），这样既有日志与测试不必改。
func (e *StatusError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	return e.Provider + ": " + msg
}

// Unwrap 返回底层错误（可 nil）。
func (e *StatusError) Unwrap() error { return e.Err }

// StatusCodeOf 取出错误链里的 HTTP 状态码；不是状态错误时返回 0。
//
// 用法：if code := llm.StatusCodeOf(err); code == 429 { ... }
func StatusCodeOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}

// ErrEmptyResponse 表示模型返回了空响应（没有可用内容）。
//
// 单列一个哨兵：空响应通常是**瞬时**的（网关抖动、上游回了空体），
// 与"参数写错了"不同——重试有意义。DSH 的 EMPTY_RESPONSE 是同一位置。
var ErrEmptyResponse = errors.New("模型返回空响应")

// ErrStreamTruncated 表示流式响应在收到结束信号之前就中断了
// （连接被断开 / 服务端异常终止）。
//
// 适配器遇到这种流必须返回本错误，而不是把半截聚合当正常响应返回——
// 半截消息 + 残缺工具调用会误导下游（2026-10 事故：空参数的 write
// 进了执行通道）。注意：S2b 的重试包装对**已吐出内容**的流不会重试
// （增量已下发，重试会重复输出），本错误主要供驱动层记入 turn/end 详情、
// 供指标分类。
var ErrStreamTruncated = errors.New("流式响应意外终止（未收到结束信号）")

// NewStatusError 构造一个带状态码的错误（适配器层用）。
func NewStatusError(provider string, statusCode int, message string) *StatusError {
	return &StatusError{Provider: provider, StatusCode: statusCode, Message: message}
}

// StatusErrorf 构造一个带状态码与格式化消息的错误。
func StatusErrorf(provider string, statusCode int, format string, args ...any) *StatusError {
	return &StatusError{Provider: provider, StatusCode: statusCode, Message: fmt.Sprintf(format, args...)}
}
