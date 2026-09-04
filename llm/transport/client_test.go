package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/puras/mos/httpx"
	"github.com/puras/mos/llm"
)

func newTestClient(t *testing.T, llmCfg llm.Config, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{
		Endpoint: func(stream bool) string {
			if stream {
				return srv.URL + "/stream"
			}
			return srv.URL + "/chat"
		},
		AuthHeaders: map[string]string{"x-test-key": "k-1", "x-empty": ""},
	}, llmCfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestNewRequiresEndpoint(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil || !strings.Contains(err.Error(), "Endpoint") {
		t.Fatalf("缺 Endpoint 应报错，got %v", err)
	}
}

func TestNewHTTPTimeoutPrivate(t *testing.T) {
	c, err := New(Config{Endpoint: func(bool) string { return "http://x" }}, llm.Config{"http_timeout": "90s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.private == nil || c.private.Timeout.String() != "1m30s" {
		t.Fatalf("http_timeout=90s 应构造私有客户端，got %+v", c.private)
	}
	// 未配置时走 seam（private == nil）。
	c2, err := New(Config{Endpoint: func(bool) string { return "http://x" }}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c2.private != nil {
		t.Fatalf("未配置 http_timeout 不应构造私有客户端，got %+v", c2.private)
	}
}

func TestNewInvalidHTTPTimeout(t *testing.T) {
	_, err := New(Config{Endpoint: func(bool) string { return "http://x" }}, llm.Config{"http_timeout": "abc"})
	if err == nil || !strings.Contains(err.Error(), "http_timeout") {
		t.Fatalf("非法 duration 应 fail-fast，got %v", err)
	}
}

func TestDoSuccess(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotKey, gotEmpty, gotBody string
	c, _ := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotKey, gotEmpty = r.Header.Get("x-test-key"), r.Header.Get("x-empty")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"ok":true}`))
	})
	resp, err := c.Do(context.Background(), false, []byte(`{"q":1}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if gotMethod != http.MethodPost || gotPath != "/chat" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	if gotKey != "k-1" {
		t.Fatalf("x-test-key 应透传，got %q", gotKey)
	}
	if gotEmpty != "" {
		t.Fatalf("空值头应跳过，got %q", gotEmpty)
	}
	if gotBody != `{"q":1}` {
		t.Fatalf("body = %q", gotBody)
	}
	// stream=true 走流式端点。
	resp2, err := c.Do(context.Background(), true, []byte(`{}`))
	if err != nil {
		t.Fatalf("Do(stream): %v", err)
	}
	defer resp2.Body.Close()
}

func TestDoStreamEndpointSwitch(t *testing.T) {
	var paths []string
	c, _ := newTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`{}`))
	})
	for _, stream := range []bool{false, true, false} {
		resp, err := c.Do(context.Background(), stream, []byte(`{}`))
		if err != nil {
			t.Fatalf("Do(%v): %v", stream, err)
		}
		resp.Body.Close()
	}
	want := []string{"/chat", "/stream", "/chat"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("endpoint 序列 = %v, want %v", paths, want)
	}
}

func TestDoNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭制造网络错误
	c, err := New(Config{Endpoint: func(bool) string { return url }}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(context.Background(), false, []byte(`{}`)); err == nil {
		t.Fatal("已关闭服务应返回网络错误")
	}
}

func TestDoUsesSeamInjectedClient(t *testing.T) {
	// 注入自定义 client（自定义 Transport 直接返回响应），Do 应经 seam 使用它
	// 而非发起真实网络请求。
	used := false
	injected := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})}
	c, err := New(Config{Endpoint: func(bool) string { return "http://unused.invalid" }}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := httpx.WithClient(context.Background(), injected)
	resp, err := c.Do(ctx, false, []byte(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if !used {
		t.Fatal("未配置 http_timeout 时 Do 应使用 seam 注入的客户端")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseStatusErrorCustom(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"auth","message":"bad key"}}`)),
	}
	err := ParseStatusError("anthropic", resp, func(code int, body []byte) (string, bool) {
		return "auth: bad key", true
	})
	if err == nil || err.Error() != "anthropic: auth: bad key" {
		t.Fatalf("自定义解析错误 = %v", err)
	}
}

func TestParseStatusErrorFallback(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("oops")),
	}
	err := ParseStatusError("gemini", resp, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 502") || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("兜底错误 = %v", err)
	}
}

func TestParseStatusErrorClosesBody(t *testing.T) {
	body := io.NopCloser(strings.NewReader("oops"))
	resp := &http.Response{StatusCode: 500, Header: http.Header{}, Body: body}
	_ = ParseStatusError("openai", resp, nil)
	if err := body.Close(); err == nil {
		// NopCloser 的 Close 恒返回 nil，无法直接断言；用自定义 body 验证。
	}
	closed := false
	resp2 := &http.Response{
		StatusCode: 500,
		Header:     http.Header{},
		Body:       closeTrackingBody{io.NopCloser(strings.NewReader("x")), &closed},
	}
	_ = ParseStatusError("openai", resp2, nil)
	if !closed {
		t.Fatal("ParseStatusError 内部应读完并关闭 Body（不依赖调用方 defer）")
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *bool
}

func (b closeTrackingBody) Close() error { *b.closed = true; return nil }
