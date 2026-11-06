package mcp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 传输层故障与协议语义错误要分得开：快照与日志的处置不同。
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"超时", context.DeadlineExceeded, ErrorTransport},
		{"取消", context.Canceled, ErrorTransport},
		{"连接断开", io.EOF, ErrorTransport},
		{"意外 EOF", io.ErrUnexpectedEOF, ErrorTransport},
		{"管道关闭", net.ErrClosed, ErrorTransport},
		{"文件已关闭", os.ErrClosed, ErrorTransport},
		{"协议错误", errors.New("invalid params"), ErrorProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// transport 构造：缺关键配置要**早失败**（不等到连不上才报错）。
func TestBuildTransportRequiresConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := buildTransport(ctx, ServerConfig{Transport: TransportStdio}); err == nil {
		t.Fatal("stdio 缺 Command 应报错")
	}
	if _, err := buildTransport(ctx, ServerConfig{Transport: TransportHTTP}); err == nil {
		t.Fatal("streamable-http 缺 URL 应报错")
	}
	if _, err := buildTransport(ctx, ServerConfig{Transport: "carrier-pigeon"}); err == nil {
		t.Fatal("未知 transport 应报错")
	}
}

// streamable-http：端点与 HTTP client 交给 SDK（宿主可注入自己的 client）。
func TestBuildTransportHTTP(t *testing.T) {
	tr, err := buildTransport(context.Background(), ServerConfig{
		Transport: TransportHTTP,
		URL:       "https://example.test/mcp",
	})
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	ht, ok := tr.(*sdk.StreamableClientTransport)
	if !ok {
		t.Fatalf("应是 StreamableClientTransport：%T", tr)
	}
	if ht.Endpoint != "https://example.test/mcp" {
		t.Fatalf("端点不对：%q", ht.Endpoint)
	}
	if ht.HTTPClient == nil {
		t.Fatal("HTTP client 应来自 httpx（宿主可注入）")
	}
}

// stdio 的环境变量：同名**覆盖**（不是追加两份）。
func TestBuildTransportStdioEnvOverrides(t *testing.T) {
	const key = "MOS_MCP_TEST_ENV"
	tr, err := buildTransport(context.Background(), ServerConfig{
		Transport: TransportStdio,
		Command:   "/bin/sh",
		Args:      []string{"-c", "echo $" + key},
		Env:       map[string]string{key: "覆盖值"},
	})
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	ct, ok := tr.(*sdk.CommandTransport)
	if !ok {
		t.Fatalf("应是 CommandTransport：%T", tr)
	}
	out, err := ct.Command.Output()
	if err != nil {
		t.Fatalf("跑子进程: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "覆盖值" {
		t.Fatalf("环境变量应被覆盖：%q", got)
	}
	// 宿主环境其余部分继承（不因为给了 Env 就丢掉 PATH）。
	if !containsEnv(ct.Command.Env, "PATH=") {
		t.Fatalf("其余环境变量应继承：%v", ct.Command.Env)
	}
}

// containsEnv 看环境变量列表里有没有某个前缀。
func containsEnv(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// stdio 子进程的 stderr 接宿主 stderr（不接管道）：没人读的管道会写满卡死子进程。
func TestStdioStderrNotPiped(t *testing.T) {
	tr, err := buildTransport(context.Background(), ServerConfig{
		Transport: TransportStdio,
		Command:   "/bin/sh",
		Args:      []string{"-c", "echo oops >&2"},
	})
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	ct := tr.(*sdk.CommandTransport)
	if ct.Command.Stderr == nil {
		t.Fatal("stderr 不该是 nil（nil = 丢弃）")
	}
	if _, ok := ct.Command.Stderr.(*os.File); !ok {
		t.Fatalf("stderr 应是文件（宿主 stderr），实际 %T", ct.Command.Stderr)
	}
}
