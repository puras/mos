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
//
// 资源能力：显式配置 resources 才有（能力即证据——没声明就报不支持）。
type mcpStubClient struct {
	mu           sync.Mutex
	tools        []mcp.ToolInfo
	instructions string
	protocol     string
	resources    []mcp.ResourceInfo
	templates    []mcp.ResourceTemplateInfo
}

func (c *mcpStubClient) ListTools(context.Context) ([]mcp.ToolInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mcp.ToolInfo(nil), c.tools...), nil
}

func (c *mcpStubClient) CallTool(_ context.Context, name string, _ map[string]any) (*mcp.Result, error) {
	return &mcp.Result{Content: "ok:" + name}, nil
}

func (c *mcpStubClient) ListResources(context.Context) ([]mcp.ResourceInfo, error) {
	if c.resources == nil {
		return nil, errors.New("不支持 resources/list")
	}
	return append([]mcp.ResourceInfo(nil), c.resources...), nil
}

func (c *mcpStubClient) ListResourceTemplates(context.Context) ([]mcp.ResourceTemplateInfo, error) {
	if c.templates == nil && c.resources == nil {
		return nil, errors.New("不支持 resources/templates/list")
	}
	return append([]mcp.ResourceTemplateInfo(nil), c.templates...), nil
}

func (c *mcpStubClient) ReadResource(_ context.Context, uri string) ([]mcp.ResourceContents, error) {
	return []mcp.ResourceContents{{URI: uri, Text: "内容 " + uri}}, nil
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

// 资源工具（S8.4-E）：有资源的服务器 → 三个共享工具直出（非 deferred），
// 无资源/hidden 的服务器不可达；hidden 服务器资源不泄露。
func TestMCPResourceToolsFromAgent(t *testing.T) {
	reg := newTestRegistry(t)
	docs := &mcpStubClient{
		tools: []mcp.ToolInfo{{Name: "search", Description: "搜"}},
		resources: []mcp.ResourceInfo{
			{URI: "file:///b.md", Name: "b"},
			{URI: "file:///a.md", Name: "a"},
		},
	}
	secret := &mcpStubClient{
		resources: []mcp.ResourceInfo{{URI: "file:///key.pem"}},
	}
	plain := &mcpStubClient{tools: []mcp.ToolInfo{{Name: "ping", Description: "拼"}}}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{
				{Name: "docs"},
				{Name: "secret", Exposure: mcp.ExposureHidden},
				{Name: "plain"},
			},
			Dial: mcpDialer(map[string]*mcpStubClient{"docs": docs, "secret": secret, "plain": plain}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// 三个共享工具直出模型清单（不是 deferred）。
	seen := map[string]bool{}
	for _, tl := range reg.List() {
		seen[tl.Name] = true
	}
	for _, name := range []string{"mcp_resource_list", "mcp_resource_templates", "mcp_resource_read"} {
		if !seen[name] {
			t.Fatalf("资源工具 %s 应直出模型清单：%v", name, seen)
		}
	}

	// docs 可达；secret（hidden）与 plain（无资源）不可达。
	list, _ := reg.Get("mcp_resource_list")
	if _, err := list.Execute(context.Background(), []byte(`{"server":"docs"}`)); err != nil {
		t.Fatalf("docs 应可达: %v", err)
	}
	for _, name := range []string{"secret", "plain", "nope"} {
		if _, err := list.Execute(context.Background(), []byte(`{"server":"`+name+`"}`)); err == nil {
			t.Fatalf("%s 不该可达", name)
		}
	}

	// 读取：文本直返 + 回显 server/uri。
	read, _ := reg.Get("mcp_resource_read")
	res, err := read.Execute(context.Background(), []byte(`{"server":"docs","uri":"file:///a.md"}`))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	out := res.(map[string]any)
	if out["content"] != "内容 file:///a.md" {
		t.Fatalf("文本应直返：%v", out["content"])
	}
	if out["server"] != "docs" || out["uri"] != "file:///a.md" {
		t.Fatalf("应回显 server/uri：%v", out)
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

// TestMCPInstructionsTruncatedAtBudget：服务器说明按字节预算**整段省略**
// （s8.2-mcp.md 的口径：超预算不截半句话——宁可少说一台）。用单台超长
// 服务器锁定不变量：整节有界、超预算内容不出现、有省略提示。
// （「预算内的说明保留」由 TestMCPInstructionsInjectedOnce 覆盖；多台场景
// 下省哪几台取决于顺序——并发连接后 Instructions() 顺序不定，不写死。）
func TestMCPInstructionsTruncatedAtBudget(t *testing.T) {
	reg := newTestRegistry(t)
	big := strings.Repeat("很长的说明", 20000) // 150KB，远超预算
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Tools: reg,
		MCP: &mos.MCPConfig{
			MaxInstructionBytes: 512,
			Servers: []mcp.ServerConfig{
				{Name: "big", Description: "说明超长的服务器"},
			},
			Dial: mcpDialer(map[string]*mcpStubClient{
				"big": {tools: []mcp.ToolInfo{{Name: "x", Description: "X"}}, instructions: big},
			}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	sess := session.New("s-budget", nil)
	if _, err := a.Run(context.Background(), sess, "你好"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var text string
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventUser && strings.Contains(ev.Text, "[MCP 服务器说明]") {
			text = ev.Text
		}
	}
	if text == "" {
		t.Fatal("说明节应注入")
	}
	// 整节受预算约束（头部两行 + 省略提示 ≈ 200 字节，加上预算 512 也远小于 768）。
	if len(text) > 768 {
		t.Fatalf("说明节应受预算约束：%d 字节", len(text))
	}
	if strings.Contains(text, "很长的说明") {
		t.Fatal("超预算服务器的说明不应出现")
	}
	if !strings.Contains(text, "其余服务器的说明超出预算") {
		t.Fatal("应有整段省略的提示")
	}
}

// newTestRegistry 造一个空注册表。
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry()
}
