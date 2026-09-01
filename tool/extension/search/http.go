package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"hegq.net/mos/httpx"
)

const (
	// searchMaxBytes 是最大响应体（2MB），防止异常响应撑爆内存。
	searchMaxBytes = 2 << 20
	// searchTimeout 是单次搜索的超时时间。
	searchTimeout = 30 * time.Second
	// searchUserAgent 是请求携带的 User-Agent。
	searchUserAgent = "MoS-Agent/0.1"
)

// newRequest 构造携带 UA 的 GET/POST 请求；超时由调用方通过 ctx 控制。
func newRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", searchUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doJSON 发起请求并将 JSON 响应解码到 out；非 200 状态码报错。
// 客户端经 httpx seam 解析（请求构建时的 ctx 已随 req 携带，注入链路见 execpath.ExecuteHook）。
func doJSON(req *http.Request, out any) error {
	resp, err := httpx.FromContext(req.Context()).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, searchMaxBytes)).Decode(out)
}
