package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/puras/mos/tool"
)

// 401 → 认证类（不是传输、不是协议）。
func TestAuthTransportClassifies401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{Name: "s"})
	if err == nil {
		t.Fatal("应报错")
	}
	if !IsAuthError(err) {
		t.Fatalf("401 应归认证类：%v (kind=%v)", err, errKind(err))
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误文本应带状态码：%v", err)
	}
}

// 403 → 认证类。
func TestAuthTransportClassifies403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{Name: "s"})
	if !IsAuthError(err) {
		t.Fatalf("403 应归认证类：%v (kind=%v)", err, errKind(err))
	}
}

// TokenProvider 的 token 进 Authorization 头。
func TestTokenProviderInjectsBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{
		Name: "s",
		TokenProvider: func(context.Context) (string, error) {
			return "tok-123", nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got != "Bearer tok-123" {
		t.Fatalf("Authorization 头不对：%q", got)
	}
}

// 静态 Headers 真的生效（此前配置项存在但没接线）。
func TestStaticHeadersApplied(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tenant")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{
		Name:    "s",
		Headers: map[string]string{"X-Tenant": "acme"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got != "acme" {
		t.Fatalf("静态头未生效：%q", got)
	}
}

// provider 返回空串 → 不注入 Authorization 头（服务器自己判断）。
func TestTokenProviderEmptyMeansNoHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{
		Name: "s",
		TokenProvider: func(context.Context) (string, error) {
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got != "" {
		t.Fatalf("空 token 不该注入头：%q", got)
	}
}

// provider 报错 → 认证类失败（请求根本不该发出去）。
func TestTokenProviderErrorIsAuth(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := doRequest(t, srv.URL, ServerConfig{
		Name: "s",
		TokenProvider: func(context.Context) (string, error) {
			return "", errors.New("凭据服务挂了")
		},
	})
	if !IsAuthError(err) {
		t.Fatalf("取凭据失败应归认证类：%v (kind=%v)", err, errKind(err))
	}
	if called {
		t.Fatal("取凭据失败时不该发出请求")
	}
}

// provider 每次请求都调（框架不缓存）。
func TestTokenProviderCalledPerRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ServerConfig{
		Name: "s",
		TokenProvider: func(context.Context) (string, error) {
			calls++
			return "tok", nil
		},
	}
	for i := 0; i < 3; i++ {
		if err := doRequest(t, srv.URL, cfg); err != nil {
			t.Fatalf("请求失败: %v", err)
		}
	}
	if calls != 3 {
		t.Fatalf("provider 应每次请求都调，实际 %d 次", calls)
	}
}

// 宿主传进来的 http.Client 不被就地修改（连接池是共享的）。
func TestWithAuthDoesNotMutateHostClient(t *testing.T) {
	host := &http.Client{}
	got := withAuth(host, ServerConfig{Name: "s", Headers: map[string]string{"X": "1"}})
	if host.Transport != nil {
		t.Fatal("不该改宿主 client 的 Transport")
	}
	if got.Transport == nil {
		t.Fatal("克隆体的 Transport 应已装上")
	}
	if got == host {
		t.Fatal("应返回克隆体，不是原 client")
	}
}

// 认证失败 → 快照写「需要认证」（处置与传输层失败不同：重试没用）。
func TestManagerAuthFailureNeedsAuth(t *testing.T) {
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "gh"}},
		Dial: func(context.Context, ServerConfig, Options) (Client, error) {
			return nil, &Error{Kind: ErrorAuth, Server: "gh", Err: errors.New("HTTP 401 Unauthorized")}
		},
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	warns := mgr.Warnings()
	if len(warns) == 0 {
		t.Fatal("应有降级诊断")
	}
	if !strings.Contains(warns[0], "需要认证") {
		t.Fatalf("快照应写明需要认证：%v", warns)
	}
}

// wrapError 保留内层的认证分类（SDK 会层层包装错误，不能一律当传输层）。
func TestWrapErrorPreservesAuthKind(t *testing.T) {
	inner := &Error{Kind: ErrorAuth, Server: "gh", Err: errors.New("401")}
	wrapped := wrapError("gh", inner)
	if !IsAuthError(wrapped) {
		t.Fatalf("应保留认证分类：%v", wrapped)
	}
	// 非认证错误 → 走 classify。
	other := wrapError("gh", errors.New("boom"))
	if IsAuthError(other) {
		t.Fatalf("普通错误不该是认证类：%v", other)
	}
	if errKind(other) != ErrorProtocol {
		t.Fatalf("普通错误应归协议类：%v", errKind(other))
	}
}

// doRequest 用给定配置打一发真实 HTTP 请求（走 withAuth 的那套 transport）。
func doRequest(t *testing.T, url string, cfg ServerConfig) error {
	t.Helper()
	hc := withAuth(&http.Client{}, cfg)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// errKind 取错误的分类（测试辅助）。
func errKind(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
