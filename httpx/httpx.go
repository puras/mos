// Package httpx 提供 HTTP 客户端 capability seam（架构 v2 §3.5）。
//
// 为什么不用 http.DefaultClient：它是全局可变单例（任何被引入的依赖都能改写其
// Transport/Timeout），且默认无超时——工具抓取网页/调用搜索 API 时直接使用它
// 意味着共享未知状态与无限等待风险。本包把 *http.Client 变为与 filesystem /
// sandbox 同形的 capability：ctx 注入可换，未注入时兜底到带超时的包级 Default。
//
// 原位于各工具内散落的 http.DefaultClient 调用（v2.24 前共 3 处）已收编到本 seam；
// LLM 适配器（llm/*，经 llm/transport）同样经本 seam 解析客户端——流式路径用
// FromContextStream（无硬超时兜底），非流式等价复用同一注入 key：
// 平台 WithClient 一处注入，工具与 LLM 两端生效。注意：注入的客户端若自带硬超时
// （Timeout 覆盖整个响应体读取），会截断超过该时长的流式输出，注入方需自行保证流式安全。
package httpx

import (
	"context"
	"net/http"
	"time"
)

// Default 是未注入时的兜底客户端：60s 硬超时（工具自身经 ctx 设置的更短超时
// 先行生效，本值只是防呆底座）。并发安全，全进程复用一个连接池。
var Default = &http.Client{Timeout: 60 * time.Second}

// DefaultStream 是流式/长连接场景的兜底客户端：无硬超时。
// http.Client.Timeout 覆盖整个请求周期（含响应体读取），LLM 流式输出常超过
// 60s，故流式路径不能用 Default；生命周期由调用方 ctx 控制。
// 并发安全，全进程复用一个连接池。
var DefaultStream = &http.Client{}

type ctxKey struct{}

// WithClient 把 *http.Client 注入 ctx（capability seam）。
// 平台可注入共享的自定义客户端：统一代理 / 重试 Transport / 指标采集。
func WithClient(ctx context.Context, c *http.Client) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext 从 ctx 取出客户端；未注入返回 Default（带超时兜底），永远不返回 nil。
func FromContext(ctx context.Context) *http.Client {
	if c, _ := ctx.Value(ctxKey{}).(*http.Client); c != nil {
		return c
	}
	return Default
}

// FromContextStream 从 ctx 取出流式/长连接场景的客户端；未注入返回 DefaultStream
// （无硬超时兜底，靠调用方 ctx 取消），永远不返回 nil。与 FromContext 共用同一
// 注入 key：WithClient 一处注入，工具层（FromContext）与 LLM 流式（本函数）同时生效。
// 注意：若注入的客户端自带硬超时，超过该时长的流式输出会被截断。
func FromContextStream(ctx context.Context) *http.Client {
	if c, _ := ctx.Value(ctxKey{}).(*http.Client); c != nil {
		return c
	}
	return DefaultStream
}
