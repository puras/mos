// Package httpx 提供 HTTP 客户端 capability seam（架构 v2 §3.5）。
//
// 为什么不用 http.DefaultClient：它是全局可变单例（任何被引入的依赖都能改写其
// Transport/Timeout），且默认无超时——工具抓取网页/调用搜索 API 时直接使用它
// 意味着共享未知状态与无限等待风险。本包把 *http.Client 变为与 filesystem /
// sandbox 同形的 capability：ctx 注入可换，未注入时兜底到带超时的包级 Default。
//
// 原位于各工具内散落的 http.DefaultClient 调用（v2.24 前共 3 处）；
// LLM 适配器（llm/openai 等）自建带超时的 client（http_timeout 可配），不经过本 seam。
package httpx

import (
	"context"
	"net/http"
	"time"
)

// Default 是未注入时的兜底客户端：60s 硬超时（工具自身经 ctx 设置的更短超时
// 先行生效，本值只是防呆底座）。并发安全，全进程复用一个连接池。
var Default = &http.Client{Timeout: 60 * time.Second}

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
