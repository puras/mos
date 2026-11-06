package mcp

import (
	"context"
	"fmt"
	"net/http"
)

// TokenProvider 返回当前可用的访问凭据（Bearer token）。
//
// 契约（**边界清晰，不越界**）：
//   - 框架**每次请求取一次**，**不缓存、不刷新**——凭据生命周期归宿主
//     （宿主自己决定什么时候刷新、怎么存、怎么过期）。框架拿到 token
//     只往请求头一放，不留副本。
//   - 返回**空字符串** = 不注入 Authorization 头（这台服务器不需要凭据，
//     或宿主想让它自己 401）。
//   - 返回 **error** = 该请求失败，归**认证类**错误（连接期表现为「这台
//     服务器不可用：取凭据失败」）。
//   - 只对 **streamable-http** 生效：stdio 是本地子进程，不走 HTTP。
//
// 参照：PI 做完整的 OAuth（发现 + 动态注册 + 回调服务器），
// DSH 只支持宿主注入静态 headers。MoS 取两者之间的边界形态——
// **框架只提供「取凭据」的缝，不做认证流程**（治理规则 5：框架不绑外部
// 系统；凭据与浏览器交互都归宿主）。
type TokenProvider func(ctx context.Context) (string, error)

// authTransport 给 HTTP 请求加凭据，并把 401/403 归为认证错误。
//
// 为什么自己包一层 RoundTripper（而不是解析错误文本）：
//   - 官方 SDK 在 401/403 时只给出含 "Unauthorized"/"Forbidden" 的**文本**
//     错误，没有结构化状态码——按文本匹配既脆又会误伤；
//   - MoS 要注入 Bearer token，本来就得自定义 Transport——顺手把状态码
//     变成结构化分类，零额外成本。
//
// 401/403 直接**转成错误**（不把响应交出去）：语义上「这台服务器现在
// 不可用（缺凭据）」，不该让上层把它当协议错误继续往下走。
type authTransport struct {
	base     http.RoundTripper
	headers  map[string]string
	provider TokenProvider
	server   string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 || t.provider != nil {
		// 克隆请求：不改调用方的 req（Transport 契约要求不改原请求）。
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		if t.provider != nil {
			token, err := t.provider(req.Context())
			if err != nil {
				return nil, &Error{
					Kind:   ErrorAuth,
					Server: t.server,
					Err:    fmt.Errorf("取访问凭据失败: %w", err),
				}
			}
			if token != "" {
				// provider 的 token 覆盖静态 Headers 里的同名头（更新鲜）。
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, &Error{
			Kind:   ErrorAuth,
			Server: t.server,
			Err:    fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		}
	}
	return resp, nil
}

// withAuth 给 HTTP 客户端套上凭据注入（克隆一份，不改宿主传进来的）。
//
// **无条件套上**（哪怕宿主没配凭据）：它的第二个职责是把 401/403 归成认证类
// 错误——那与“有没有配凭据”无关，只与“服务器怎么回”有关。
//
// 克隆是必须的：httpx.FromContext 返回的 client 可能是宿主共享的
// （连接池、超时设置都是它的）——就地改 Transport 会污染宿主自己的请求。
func withAuth(hc *http.Client, cfg ServerConfig) *http.Client {
	clone := *hc
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &authTransport{
		base:     base,
		headers:  cfg.Headers,
		provider: cfg.TokenProvider,
		server:   cfg.Name,
	}
	return &clone
}
