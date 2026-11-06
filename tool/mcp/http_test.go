package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/puras/mos/tool"
)

// httpTestServer 是一台**真的走 HTTP** 的 MCP 服务器：官方 SDK 的
// streamable-http handler 挂在 httptest 上。与 stdio 那些用例的差别是线协议、
// 会话头（Mcp-Session-Id）、事件流全都真的过一遍，而不是在进程内传球。
type httpTestServer struct {
	url     string // 端点（已含 /mcp 路径）
	calls   atomic.Int64
	authHdr string // 非空 = 要求 Authorization 等于它，否则 401
}

// newHTTPTestServer 起一台带 echo 工具的 streamable-http 服务器。
func newHTTPTestServer(t *testing.T, authHdr string) *httpTestServer {
	t.Helper()
	ts := &httpTestServer{authHdr: authHdr}

	s := sdk.NewServer(&sdk.Implementation{Name: "http-test", Version: "0.0.1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "回显"}, func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
		ts.calls.Add(1)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo:" + in.Text}},
		}, nil, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", guardAuth(authHdr, handler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ts.url = srv.URL + "/mcp"
	return ts
}

// guardAuth 是可选的鉴权门（模拟真实网关）：不匹配就 401，连协议层都进不去。
func guardAuth(want string, next http.Handler) http.Handler {
	if want == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestHTTPClientRealServer：Dial 走真实 HTTP 完成初始化、tools/list、tools/call
// 与关闭——这条链路此前只有 transport 构造与错误边界的单测，没有真服务端。
func TestHTTPClientRealServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := newHTTPTestServer(t, "")
	client, err := Dial(ctx, ServerConfig{
		Name:      "http",
		Transport: TransportHTTP,
		URL:       srv.url,
	}, Options{ConnectTimeout: 15 * time.Second, CallTimeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	if v := client.ProtocolVersion(); v == "" {
		t.Fatal("应协商出协议版本")
	}
	infos, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "echo" {
		t.Fatalf("工具清单不对：%+v", infos)
	}
	res, err := client.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.Content != "echo:hi" {
		t.Fatalf("调用结果不对：%+v", res)
	}
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("服务端应被调用 1 次，实际 %d", n)
	}
	// 关闭后不能再调（会话已收）。
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", map[string]any{"text": "again"}); err == nil {
		t.Fatal("关闭后调用应失败")
	}
}

// TestHTTPClientStaticHeaders：静态 Headers 每个请求都带上——鉴权门通过，
// 整条链路可用（S8.4-B 的静态头路径在真 HTTP 上的验收）。
func TestHTTPClientStaticHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := newHTTPTestServer(t, "Bearer static-token")
	client, err := Dial(ctx, ServerConfig{
		Name:      "http",
		Transport: TransportHTTP,
		URL:       srv.url,
		Headers:   map[string]string{"Authorization": "Bearer static-token"},
	}, Options{ConnectTimeout: 15 * time.Second, CallTimeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("带凭据应当连上：%v", err)
	}
	defer func() { _ = client.Close() }()

	res, err := client.CallTool(ctx, "echo", map[string]any{"text": "ok"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Content != "echo:ok" {
		t.Fatalf("调用结果不对：%+v", res)
	}
}

// TestHTTPClientTokenProviderEachRequest：TokenProvider **每个请求取一次**、
// 不缓存——所以「换 token」下一请求就能生效，而 provider 的调用次数与请求数同阶。
func TestHTTPClientTokenProviderEachRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := newHTTPTestServer(t, "Bearer rotating")
	var fetch atomic.Int64
	client, err := Dial(ctx, ServerConfig{
		Name:      "http",
		Transport: TransportHTTP,
		URL:       srv.url,
		TokenProvider: func(context.Context) (string, error) {
			fetch.Add(1)
			return "rotating", nil
		},
	}, Options{ConnectTimeout: 15 * time.Second, CallTimeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("带 provider 应当连上：%v", err)
	}
	defer func() { _ = client.Close() }()

	before := fetch.Load()
	if before == 0 {
		t.Fatal("连接期至少取过一次凭据")
	}
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", map[string]any{"text": "hi"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if after := fetch.Load(); after <= before {
		t.Fatalf("每次调用都该重新取凭据：连接后 %d 次，两次调用后 %d 次", before, after)
	}
}

// TestHTTPClientAuthFailureClassified：凭据被服务端拒 → 认证类错误
// （宿主据此提示「这台服务器要凭据」，而不是当成协议错误糊过去）。
func TestHTTPClientAuthFailureClassified(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := newHTTPTestServer(t, "Bearer right")
	_, err := Dial(ctx, ServerConfig{
		Name:      "http",
		Transport: TransportHTTP,
		URL:       srv.url,
		Headers:   map[string]string{"Authorization": "Bearer wrong"},
	}, Options{ConnectTimeout: 15 * time.Second})
	if err == nil {
		t.Fatal("凭据不对应当连不上")
	}
	if !IsAuthError(err) {
		t.Fatalf("401 应归认证类：%v", err)
	}
	if n := srv.calls.Load(); n != 0 {
		t.Fatalf("鉴权门就拦下了，工具不该被调用：%d 次", n)
	}
}

// TestManagerRealHTTP：装配路径（Manager.Start → 注册表 → 工具执行）在真 HTTP
// 服务器上跑通；Close 收掉会话。这条覆盖的是宿主实际用法，不只是 Dial。
func TestManagerRealHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := newHTTPTestServer(t, "")
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:      "http",
			Transport: TransportHTTP,
			URL:       srv.url,
		}},
		ConnectTimeout: 15 * time.Second,
		CallTimeout:    15 * time.Second,
	}, reg)
	mgr.Start(ctx)
	defer mgr.Close()

	tl, ok := reg.Get("mcp__http__echo")
	if !ok {
		t.Fatalf("工具应注册：%v（warnings=%v）", reg.Names(), mgr.Warnings())
	}
	out, err := tl.Execute(ctx, json.RawMessage(`{"text":"via-manager"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := textOf(t, out)
	if !strings.Contains(text, "echo:via-manager") {
		t.Fatalf("结果不对：%s", text)
	}
}

// textOf 从工具返回值里取出文本（工具结果可能是字符串或结构化投影）。
func textOf(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case *Result:
		return x.Content
	default:
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatalf("投影结果不可序列化：%v", err)
		}
		return string(b)
	}
}
