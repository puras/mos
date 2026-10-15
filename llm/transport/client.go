// Package transport 提供 LLM 适配器共享的 HTTP envelope 与用量归一化。
//
// 三个协议适配器（llm/anthropic / llm/openai / llm/gemini）的请求发送骨架
// （POST + JSON body + 协议鉴权头 + 非 200 错误体解析）与 moproto 用量映射在此收编，
// 适配器只保留真正的协议差异：请求/响应结构转换与 SSE 事件语义。
//
// 客户端解析复用顶层 httpx capability seam：未显式配置 http_timeout 时，
// 每次请求经 httpx.FromContextStream(ctx) 解析——平台 WithClient 一处注入
// （统一代理 / 重试 Transport / 指标采集），工具层与 LLM 调用两端生效；
// 显式配置 http_timeout 时使用构造期私有客户端（显式配置优先于注入，且
// 运行时改共享 client 的 Timeout 属数据竞态，只能构造期私有化）。
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/puras/mos/httpx"
	"github.com/puras/mos/llm"
)

// Config 是构造 Client 的协议侧参数，由各适配器提供。
type Config struct {
	// Endpoint 返回完整请求 URL（含路径）；stream 为 true 时返回流式端点
	// （如 Gemini 的 streamGenerateContent?alt=sse）。
	Endpoint func(stream bool) string
	// AuthHeaders 在每次请求时写入 req.Header；值为空字符串的键跳过
	// （如 openai 本地 Ollama 无 apiKey 时不设 Authorization）。
	AuthHeaders map[string]string
	// ParseError 在非 200 响应时解析错误体（协议结构差异大，由适配器提供）；
	// 返回 ok=false 时 ParseStatusError 退化为通用兜底。可为 nil。
	ParseError func(statusCode int, body []byte) (msg string, ok bool)
}

// Client 是共享的 HTTP envelope 客户端。并发安全（无可变状态）。
type Client struct {
	cfg     Config
	private *http.Client // http_timeout 显式配置时的私有客户端；nil 表示走 seam
}

// New 创建 Client。llmCfg 用于解析 http_timeout 配置键：
// 为空时不设超时（默认，每次请求经 httpx seam 解析客户端，长流式靠 ctx 取消）；
// 非空但非法的 duration 在此 fail-fast。
func New(cfg Config, llmCfg llm.Config) (*Client, error) {
	if cfg.Endpoint == nil {
		return nil, errors.New("transport: Config.Endpoint is required")
	}
	c := &Client{cfg: cfg}
	if v := strings.TrimSpace(llmCfg["http_timeout"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("transport: invalid http_timeout %q: %w", v, err)
		}
		c.private = &http.Client{Timeout: d}
	}
	return c, nil
}

// Do 发送 POST + JSON body 请求，等待响应头返回。
// 返回的 *http.Response 的 Body 归调用方所有：成功路径由调用方 defer Close；
// 非 200 路径调用 ParseStatusError（其内部读完并关闭 Body）。
func (c *Client) Do(ctx context.Context, stream bool, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint(stream), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("transport: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.cfg.AuthHeaders {
		if v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient(ctx).Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: do request: %w", err)
	}
	return resp, nil
}

// httpClient 解析本次请求使用的客户端：显式 http_timeout → 构造期私有客户端；
// 否则走 httpx capability seam（未注入回退无硬超时的 DefaultStream）。
func (c *Client) httpClient(ctx context.Context) *http.Client {
	if c.private != nil {
		return c.private
	}
	return httpx.FromContextStream(ctx)
}

// ParseStatusError 将非 200 响应转为带 provider 前缀的错误。
// 内部读完并 Close resp.Body（Close 幂等，调用方既有 defer 二次关闭安全），
// 因此必须在确认不再读取 Body 后调用。
// parse 返回 ok=true 时错误为 "<provider>: <msg>"，否则兜底为
// "<provider>: unexpected status <code>: <body>"。parse 可为 nil。
func ParseStatusError(provider string, resp *http.Response, parse func(statusCode int, body []byte) (msg string, ok bool)) error {
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if parse != nil {
		if msg, ok := parse(resp.StatusCode, data); ok {
			return fmt.Errorf("%s: %s", provider, msg)
		}
	}
	return fmt.Errorf("%s: unexpected status %d: %s", provider, resp.StatusCode, string(data))
}
