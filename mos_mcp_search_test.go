package mos_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mos "github.com/puras/mos"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool/mcp"
)

// scriptedModel 是按剧本逐次作答的假模型：每收到一次请求就记下它看到的工具
// 清单（按请求序），再执行剧本里的下一步。
//
// 用途：把「模型在某一轮看到哪些工具 → 因此调了什么」变成可断言的观测点。
// 真实模型不可控，而 deferred 激活的价值恰恰在于「清单变没变」，所以这条
// 链路必须由测试锁住。
type scriptedModel struct {
	mu    sync.Mutex
	steps []scriptStep
	seen  [][]string
}

// scriptStep 收到请求后返回这次要做的动作。
type scriptStep func(req *llm.Request) *llm.Response

func (m *scriptedModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	names := make([]string, len(req.Tools))
	for i, tl := range req.Tools {
		names[i] = tl.Name
	}
	m.seen = append(m.seen, names)
	var step scriptStep
	if len(m.steps) > 0 {
		step = m.steps[0]
		m.steps = m.steps[1:]
	}
	m.mu.Unlock()
	if step == nil {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "（剧本已用完）",
		}}, nil
	}
	return step(req), nil
}

func (m *scriptedModel) ChatStream(ctx context.Context, req *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *scriptedModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }

// seenAt 返回第 i 次请求（从 0 起）模型看到的工具名；越界返回 nil。
func (m *scriptedModel) seenAt(i int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.seen) {
		return nil
	}
	return m.seen[i]
}

// toolCallResponse 造一个「模型要求调用某工具」的响应。
func toolCallResponse(id, name, args string) *llm.Response {
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Args: args}},
	}}
}

// textResponse 造一个「模型直接作答」的响应。
func textResponse(text string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}
}

func hasToolName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// mcpCall 是一次真实到达上游的 MCP 调用（测试替身记下来的）。
type mcpCall struct {
	name string
	args map[string]any
}

// recordingMCPClient 在 mcpStubClient 之上记录真实到达上游的调用——用来证明
// 「模型调的这个工具确实打到了 MCP 服务器」，而不是被哪一层悄悄短路的。
type recordingMCPClient struct {
	mcpStubClient
	callMu sync.Mutex
	calls  []mcpCall
}

func (c *recordingMCPClient) CallTool(ctx context.Context, name string, args map[string]any) (*mcp.Result, error) {
	c.callMu.Lock()
	c.calls = append(c.calls, mcpCall{name: name, args: args})
	c.callMu.Unlock()
	return c.mcpStubClient.CallTool(ctx, name, args)
}

func (c *recordingMCPClient) recorded() []mcpCall {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	return append([]mcpCall(nil), c.calls...)
}

// recordingDialer 造一个连接工厂：按服务器名给带记录的客户端。
func recordingDialer(clients map[string]*recordingMCPClient) func(context.Context, mcp.ServerConfig, mcp.Options) (mcp.Client, error) {
	return func(_ context.Context, cfg mcp.ServerConfig, _ mcp.Options) (mcp.Client, error) {
		c, ok := clients[cfg.Name]
		if !ok {
			return nil, errors.New("没有这台服务器")
		}
		return c, nil
	}
}

// toolResult 从会话日志里取出某个 call 的回填。
func toolResult(sess *session.Session, callID string) (session.ToolResult, bool) {
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil && ev.ToolResult.CallID == callID {
			return *ev.ToolResult, true
		}
	}
	return session.ToolResult{}, false
}

// TestMCPToolActivatedViaToolSearchE2E 是 S8.2 FR-D8 的主链路端到端：
//
//	MCP 工具默认 deferred（模型看不见）→ 模型调 tool_search → 命中即激活
//	→ 激活回执回到模型手里 → 模型按回执里的名字调用 → 调用**真的打到**
//	上游 MCP 客户端 → 结果回填进会话；且下一个 turn 的清单里已有这个工具。
//
// 为什么这条链路需要 E2E：deferred 不是「注册了但不显示」那么简单——它横跨
// 注册表（DeferredTools/Activate）、BM25 检索（tool/searchtools）、驱动工具
// 清单（react 每轮取 List()）与 MCP 调用投影四层，任何一层断掉都表现为
// 「模型搜到了却用不上」。
func TestMCPToolActivatedViaToolSearchE2E(t *testing.T) {
	reg := newTestRegistry(t)
	client := &recordingMCPClient{mcpStubClient: mcpStubClient{
		tools: []mcp.ToolInfo{{
			Name:        "search_issues",
			Description: "按关键字搜索仓库的 issue 工单",
		}},
	}}

	var sawReceipt bool
	model := &scriptedModel{steps: []scriptStep{
		// ① 第一轮：清单里只有 tool_search，MCP 工具 deferred 不可见。
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c1", "tool_search", `{"query":"搜索 github issue 工单"}`)
		},
		// ② 拿到激活回执后按回执里的名字调用真 MCP 工具。
		func(req *llm.Request) *llm.Response {
			for _, msg := range req.Messages {
				if strings.Contains(msg.Content, "mcp__gh__search_issues") {
					sawReceipt = true
				}
			}
			return toolCallResponse("c2", "mcp__gh__search_issues", `{"q":"bug"}`)
		},
		// ③ 收尾作答。
		func(*llm.Request) *llm.Response { return textResponse("找到一个 issue") },
		// ④ 第二个 turn：清单里应当已经有被激活的 MCP 工具。
		func(*llm.Request) *llm.Response { return textResponse("ok") },
	}}

	a, err := mos.New(mos.Config{
		Model: model,
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh", Description: "GitHub 工单与仓库"}},
			Dial:    recordingDialer(map[string]*recordingMCPClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	sess := session.New("s-mcp-search", nil)
	reply, err := a.Run(context.Background(), sess, "帮我查一下仓库里的 bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "找到一个 issue" {
		t.Fatalf("reply = %q", reply)
	}

	// ① 第一轮模型只看见 tool_search（MCP 工具默认 deferred）。
	first := model.seenAt(0)
	if !hasToolName(first, "tool_search") {
		t.Fatalf("第一轮清单应含 tool_search：%v", first)
	}
	if hasToolName(first, "mcp__gh__search_issues") {
		t.Fatalf("deferred 的 MCP 工具不该出现在第一轮清单：%v", first)
	}

	// ①b 激活在**同一轮内**生效：激活后的下一次请求，清单里已经有它
	//（对齐 PI 的「下一轮请求声明给模型」——不必等下一个 turn）。
	afterActivate := model.seenAt(1)
	if !hasToolName(afterActivate, "mcp__gh__search_issues") {
		t.Fatalf("激活后同一轮的下一轮就该声明该 MCP 工具：%v", afterActivate)
	}

	// ② 激活回执回到了模型手里（模型据此才知道这个名字）。
	if !sawReceipt {
		t.Fatal("tool_search 的激活回执应回到模型（模型要能看见被激活的工具名）")
	}
	receipt, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺 tool_search 的回填")
	}
	if receipt.IsError {
		t.Fatalf("tool_search 不该失败：%s", receipt.Content)
	}
	if !strings.Contains(receipt.Content, "已激活 1 个工具") {
		t.Fatalf("回执应写明激活数量：%s", receipt.Content)
	}
	if !strings.Contains(receipt.Content, "mcp__gh__search_issues") {
		t.Fatalf("回执应列出被激活的工具名：%s", receipt.Content)
	}

	// ③ 调用真的打到了上游（不是被注册表短路掉的假成功）。
	calls := client.recorded()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用，实际 %d 次：%+v", len(calls), calls)
	}
	if calls[0].name != "search_issues" {
		t.Fatalf("上游应收到原始工具名 search_issues，实际 %q", calls[0].name)
	}
	if got, _ := calls[0].args["q"].(string); got != "bug" {
		t.Fatalf("参数应原样透传到上游：%+v", calls[0].args)
	}

	// ④ 结果回填成正常结果（不是错误），内容就是上游投影。
	res, ok := toolResult(sess, "c2")
	if !ok {
		t.Fatal("缺 MCP 工具的回填")
	}
	if res.IsError {
		t.Fatalf("MCP 工具调用不该失败：%s", res.Content)
	}
	if !strings.Contains(res.Content, "ok:search_issues") {
		t.Fatalf("回填应是上游投影的 JSON：%s", res.Content)
	}

	// ⑤ 激活是持久的：注册表清单里有它，deferred 集里没有它。
	if !hasToolName(toolNames(reg.List()), "mcp__gh__search_issues") {
		t.Fatalf("激活后应进模型清单：%v", reg.Names())
	}
	for _, d := range reg.DeferredTools() {
		if d.Name == "mcp__gh__search_issues" {
			t.Fatalf("激活后不该还在 deferred 集：%v", reg.DeferredTools())
		}
	}

	// ⑥ 下一个 turn：它仍在清单里（激活是持久的，不是一次性的）。
	if _, err := a.Run(context.Background(), sess, "继续"); err != nil {
		t.Fatalf("第二轮 Run: %v", err)
	}
	second := model.seenAt(3)
	if !hasToolName(second, "mcp__gh__search_issues") {
		t.Fatalf("激活后下一个 turn 的清单应含该 MCP 工具：%v", second)
	}
}

// toolNames 抽 llm.Tool 的名字（断言用）。
func toolNames(defs []llm.Tool) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}
