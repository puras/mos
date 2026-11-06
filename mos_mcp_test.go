package mos_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/mcp"
)

// mcpStubClient 是 MCP 客户端的测试替身。
type mcpStubClient struct {
	mu           sync.Mutex
	tools        []mcp.ToolInfo
	instructions string
	protocol     string
}

func (c *mcpStubClient) ListTools(context.Context) ([]mcp.ToolInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mcp.ToolInfo(nil), c.tools...), nil
}

func (c *mcpStubClient) CallTool(_ context.Context, name string, _ map[string]any) (*mcp.Result, error) {
	return &mcp.Result{Content: "ok:" + name}, nil
}

func (c *mcpStubClient) Instructions() string { return c.instructions }
func (c *mcpStubClient) ProtocolVersion() string {
	return c.protocol
}
func (c *mcpStubClient) Close() error { return nil }

// mcpDialer 造一个连接工厂：按服务器名给客户端，"bad" 那台连不上。
func mcpDialer(clients map[string]*mcpStubClient) func(context.Context, mcp.ServerConfig, mcp.Options) (mcp.Client, error) {
	return func(_ context.Context, cfg mcp.ServerConfig, _ mcp.Options) (mcp.Client, error) {
		if cfg.Name == "bad" {
			return nil, errors.New("连不上")
		}
		c, ok := clients[cfg.Name]
		if !ok {
			return nil, errors.New("没有这台服务器")
		}
		return c, nil
	}
}

// MCP 工具默认 deferred：不在模型清单里，但能被 tool_search 搜到。
func TestMCPDefaultsToDeferred(t *testing.T) {
	reg := newTestRegistry(t)
	client := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "search_issues", Description: "搜 issue"}}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh"}},
			Dial:    mcpDialer(map[string]*mcpStubClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// 模型清单里没有 MCP 工具（deferred）。
	for _, tl := range reg.List() {
		if strings.HasPrefix(tl.Name, "mcp__") {
			t.Fatalf("MCP 工具默认不该进模型清单：%s", tl.Name)
		}
	}
	// 但能搜到（tool_search 的入口）。
	found := false
	for _, d := range reg.DeferredTools() {
		if d.Name == "mcp__gh__search_issues" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP 工具应能被 tool_search 搜到：%v", reg.DeferredTools())
	}
	// 有 deferred 就有 tool_search。
	if !strings.Contains(a.Describe(), "tool_search: 已装配") {
		t.Fatalf("应装上 tool_search:\n%s", a.Describe())
	}
}

// 失败隔离：一台服务器连不上，其余照常（FR-D8 验收）。
func TestMCPFailureIsolation(t *testing.T) {
	reg := newTestRegistry(t)
	good := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "a", Description: "A"}}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "bad"}, {Name: "ok"}},
			Dial:    mcpDialer(map[string]*mcpStubClient{"ok": good}),
		},
	})
	if err != nil {
		t.Fatalf("New 不该因单台服务器失败而报错: %v", err)
	}
	defer a.Close()

	if _, ok := reg.Get("mcp__ok__a"); !ok {
		t.Fatalf("好服务器的工具应注册：%v", reg.Names())
	}
	d := a.Describe()
	if !strings.Contains(d, "MCP 服务器: 1 个") {
		t.Fatalf("快照应写出连上的服务器数:\n%s", d)
	}
	if !strings.Contains(d, "⚠") || !strings.Contains(d, "bad") {
		t.Fatalf("快照应写出降级诊断:\n%s", d)
	}
}

// hidden 档：模型看不见、也搜不到，但仍可被程序化调用。
func TestMCPHiddenExposure(t *testing.T) {
	reg := newTestRegistry(t)
	client := &mcpStubClient{tools: []mcp.ToolInfo{
		{Name: "visible", Description: "看得见"},
		{Name: "secret", Description: "看不见"},
	}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{
				Name:         "gh",
				ToolExposure: map[string]mcp.Exposure{"secret": mcp.ExposureHidden},
			}},
			Dial: mcpDialer(map[string]*mcpStubClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	for _, d := range reg.DeferredTools() {
		if d.Name == "mcp__gh__secret" {
			t.Fatalf("hidden 工具不该被搜到：%v", reg.DeferredTools())
		}
	}
	// 仍可被程序化调用（隐藏 ≠ 禁止）。
	if _, ok := reg.Get("mcp__gh__secret"); !ok {
		t.Fatal("hidden 工具应仍注册着（可被程序化调用）")
	}
}

// 服务器说明：一次性注入（每个会话一次，走 user 侧）。
func TestMCPInstructionsInjectedOnce(t *testing.T) {
	reg := newTestRegistry(t)
	client := &mcpStubClient{
		tools:        []mcp.ToolInfo{{Name: "a", Description: "A"}},
		instructions: "这台服务器能查 issue",
	}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh"}},
			Dial:    mcpDialer(map[string]*mcpStubClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	sess := session.New("s1", nil)
	if _, err := a.Run(context.Background(), sess, "第一个问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 说明进了会话（user 侧）。
	injected := 0
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventUser && strings.Contains(ev.Text, "这台服务器能查 issue") {
			injected++
		}
	}
	if injected != 1 {
		t.Fatalf("说明应注入一次：%d", injected)
	}
	// 第二轮不再注入。
	if _, err := a.Run(context.Background(), sess, "第二个问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	injected = 0
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventUser && strings.Contains(ev.Text, "这台服务器能查 issue") {
			injected++
		}
	}
	if injected != 1 {
		t.Fatalf("说明不该重复注入：%d", injected)
	}
	// 新会话：说明**每个会话注入一次**（新会话该重新拿到服务器上下文）。
	sess2 := session.New("s2", nil)
	if _, err := a.Run(context.Background(), sess2, "问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	users := 0
	for _, ev := range sess2.Events() {
		if ev.Kind == session.EventUser {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("新会话应有一条说明 + 一条用户输入：%d", users)
	}
}

// 没装 MCP：能力即证据（不装就没有 MCP 相关痕迹）。
func TestMCPNotConfigured(t *testing.T) {
	reg := newTestRegistry(t)
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "MCP 服务器: 未装配") {
		t.Fatalf("快照应写明未装配 MCP:\n%s", a.Describe())
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// S8.4-A：MCP 服务器摘要进**静态**系统提示词——模型得知道有哪些服务器
// 存在（否则不搜就不知道）。
func TestMCPServersSectionInSystemPrompt(t *testing.T) {
	reg := newTestRegistry(t)
	client := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "issue", Description: "查工单"}}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh", Description: "GitHub 工单与仓库"}},
			Dial:    mcpDialer(map[string]*mcpStubClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	sp := a.RunContext().SystemPrompt
	if !strings.Contains(sp, "mcp__gh") {
		t.Fatalf("系统提示词应列出 MCP 服务器：\n%s", sp)
	}
	if !strings.Contains(sp, "GitHub 工单与仓库") {
		t.Fatalf("系统提示词应带服务器摘要：\n%s", sp)
	}
	if !strings.Contains(sp, "tool_search") {
		t.Fatalf("系统提示词应说明怎么够到：\n%s", sp)
	}
}

// 摘要节是**静态**的：两次装配逐字节一致（保 prompt cache）。
func TestMCPServersSectionIsStatic(t *testing.T) {
	build := func() string {
		reg := newTestRegistry(t)
		client := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "issue", Description: "查工单"}}}
		a, err := mos.New(mos.Config{
			Model: &stubModel{reply: "ok"},
			Tools: reg,
			MCP: &mos.MCPConfig{
				Servers: []mcp.ServerConfig{{Name: "gh", Description: "GitHub 工单"}},
				Dial:    mcpDialer(map[string]*mcpStubClient{"gh": client}),
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer a.Close()
		return a.RunContext().SystemPrompt
	}
	if build() != build() {
		t.Fatal("系统提示词两次装配应逐字节一致（静态，保 prompt cache）")
	}
}

// 只列模型够得着的服务器：direct 的不用列（模型看得见），连不上的不列。
func TestMCPServersSectionSkipsUnreachable(t *testing.T) {
	reg := newTestRegistry(t)
	direct := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "r", Description: "direct"}}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{
				{Name: "directonly", Description: "模型直接看得见", ToolExposure: map[string]mcp.Exposure{"r": mcp.ExposureDirect}},
				{Name: "down", Description: "连不上"},
			},
			Dial: func(_ context.Context, cfg mcp.ServerConfig, _ mcp.Options) (mcp.Client, error) {
				if cfg.Name == "down" {
					return nil, errors.New("连不上")
				}
				return direct, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	sp := a.RunContext().SystemPrompt
	if strings.Contains(sp, "directonly") {
		t.Fatalf("全是 direct 的服务器不该进摘要节：\n%s", sp)
	}
	if strings.Contains(sp, "down") {
		t.Fatalf("连不上的服务器不该进摘要节：\n%s", sp)
	}
}

// S8.4-B：认证失败在快照里写明「需要认证」（处置与传输层失败不同）。
func TestMCPAuthFailureSnapshotNeedsAuth(t *testing.T) {
	reg := newTestRegistry(t)
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh", Transport: mcp.TransportHTTP, URL: "http://127.0.0.1:1/mcp"}},
			Dial: func(context.Context, mcp.ServerConfig, mcp.Options) (mcp.Client, error) {
				return nil, &mcp.Error{Kind: mcp.ErrorAuth, Server: "gh", Err: errors.New("HTTP 401 Unauthorized")}
			},
		},
	})
	if err != nil {
		t.Fatalf("New 不该因单台服务器失败而报错: %v", err)
	}
	defer a.Close()

	d := a.Describe()
	if !strings.Contains(d, "需要认证") {
		t.Fatalf("快照应写明需要认证:\n%s", d)
	}
}

// newTestRegistry 造一个空注册表。
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry()
}
