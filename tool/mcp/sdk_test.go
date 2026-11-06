package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/puras/mos/tool"
)

// 测试服务器的开关环境变量：测试进程自己再跑一遍，扮演 MCP 服务器。
const testServerEnv = "MOS_MCP_TEST_SERVER"

// TestMain：带开关环境变量时当服务器跑，否则正常跑测试。
func TestMain(m *testing.M) {
	if os.Getenv(testServerEnv) == "1" {
		runTestServer()
		return
	}
	os.Exit(m.Run())
}

// echoIn 是测试工具的参数。
type echoIn struct {
	Text string `json:"text"`
}

// runTestServer 扮演一个 stdio MCP 服务器。
func runTestServer() {
	ctx := context.Background()
	s := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "回显"}, func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo:" + in.Text}},
		}, nil, nil
	})
	sdk.AddTool(s, &sdk.Tool{Name: "boom", Description: "总是失败"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: "炸了"}},
		}, nil, nil
	})
	// 动态加工具 → 触发 tools/list_changed（换代路径）。
	sdk.AddTool(s, &sdk.Tool{Name: "add_late", Description: "加一个工具"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		sdk.AddTool(s, &sdk.Tool{Name: "late", Description: "后加的工具"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "late"}}}, nil, nil
		})
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "added"}}}, nil, nil
	})
	session, err := s.Connect(ctx, &sdk.StdioTransport{}, nil)
	if err != nil {
		os.Exit(1)
	}
	_ = session.Wait()
}

// testServerCommand 造出启动测试服务器的子进程命令。
func testServerCommand() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(), testServerEnv+"=1")
	return cmd
}

// TestStdioClient 通过真实 stdio 子进程走一遍协议。
func TestStdioClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport, err := buildTransport(ctx, ServerConfig{
		Name:      "test",
		Transport: TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMain"},
		Env:       map[string]string{testServerEnv: "1"},
	})
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "mos", Version: "dev"},
		&sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	c := &sdkClient{server: "test", session: session, callTimeout: 10 * time.Second}
	if init := session.InitializeResult(); init != nil {
		c.protocol = init.ProtocolVersion
	}
	infos, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, info := range infos {
		names[info.Name] = true
	}
	if !names["echo"] || !names["boom"] {
		t.Fatalf("应列出服务器的工具：%v", names)
	}
	if c.ProtocolVersion() == "" {
		t.Fatal("应协商出协议版本")
	}
	// 调用：文本投影。
	res, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Content != "echo:hi" {
		t.Fatalf("文本投影不对：%q", res.Content)
	}
	if res.IsError {
		t.Fatal("成功调用不该是错误")
	}
	// 调用：上游报错 → isError。
	res, err = c.CallTool(ctx, "boom", nil)
	if err != nil {
		t.Fatalf("CallTool boom: %v", err)
	}
	if !res.IsError || res.Content != "炸了" {
		t.Fatalf("上游错误应投影成 isError + 内容：%+v", res)
	}
}

// TestManagerRealStdioRefresh 用真实 stdio 服务器验证换代：
// 服务器动态加工具 → 客户端收到 list_changed → 注册表换代。
func TestManagerRealStdioRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:      "test",
			Transport: TransportStdio,
			Command:   os.Args[0],
			Args:      []string{"-test.run=TestMain"},
			Env:       map[string]string{testServerEnv: "1"},
		}},
		ConnectTimeout: 15 * time.Second,
		CallTimeout:    15 * time.Second,
	}, reg)
	mgr.Start(ctx)
	defer mgr.Close()

	if _, ok := reg.Get("mcp__test__echo"); !ok {
		t.Fatalf("服务器工具应注册：%v (warnings=%v)", reg.Names(), mgr.Warnings())
	}
	// 触发服务器加工具。
	tl, ok := reg.Get("mcp__test__add_late")
	if !ok {
		t.Fatalf("add_late 工具应注册：%v", reg.Names())
	}
	if _, err := tl.Execute(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("调 add_late: %v", err)
	}
	// 等换代落地（轮询：新工具进注册表）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("mcp__test__late"); ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("换代后新工具应进注册表：%v (warnings=%v)", reg.Names(), mgr.Warnings())
}

// TestManagerRealStdioProtocolVersion 记录协商到的协议版本（快照用）。
func TestManagerRealStdioProtocolVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:      "test",
			Transport: TransportStdio,
			Command:   os.Args[0],
			Args:      []string{"-test.run=TestMain"},
			Env:       map[string]string{testServerEnv: "1"},
		}},
		ConnectTimeout: 15 * time.Second,
	}, reg)
	mgr.Start(ctx)
	defer mgr.Close()

	if len(mgr.Servers()) != 1 {
		t.Fatalf("应连上 1 台服务器：%v", mgr.Warnings())
	}
	v := mgr.Servers()[0].ProtocolVersion()
	if v == "" {
		t.Fatal("协议版本不该为空")
	}
	if !strings.HasPrefix(v, "20") {
		t.Fatalf("协议版本格式不对：%q", v)
	}
	t.Logf("协商到的协议版本：%s", v)
}
