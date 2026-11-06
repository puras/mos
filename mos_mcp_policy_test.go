package mos_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mos "github.com/puras/mos"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool/mcp"
)

// newMCPPolicyAgent 造一台 **direct 档** 的 MCP 服务器接进智能体：
//
//	mcp__gh__search_issues  只读，应当照常可用
//	mcp__gh__delete_repo    危险，应当被策略/规则挡住
//
// 为什么用 direct 档：deferred 的工具本来就不在模型清单里，分不清它是被策略
// 过滤还是还没激活——可见性这一层要可观测，就得让工具本来在清单里。
func newMCPPolicyAgent(t *testing.T, cfg *policy.Config, rules []policy.Check, steps ...scriptStep) (*mos.Agent, *scriptedModel, *recordingMCPClient) {
	t.Helper()
	client := &recordingMCPClient{mcpStubClient: mcpStubClient{tools: []mcp.ToolInfo{
		{Name: "search_issues", Description: "按关键字搜索仓库的 issue 工单"},
		{Name: "delete_repo", Description: "删除一个仓库"},
	}}}
	model := &scriptedModel{steps: steps}
	a, err := mos.New(mos.Config{
		Model:  model,
		Tools:  newTestRegistry(t),
		Policy: cfg,
		Rules:  rules,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh", Exposure: mcp.ExposureDirect}},
			Dial:    recordingDialer(map[string]*recordingMCPClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, model, client
}

// TestMCPToolDeniedByPolicyInvisible：策略拒绝名单同时管「能不能调」与
// 「模型看不看得见」（policy 包的核心承诺：两份语义读同一份名单）。
//
// 这里断言可见性这半边：拒绝名单里的 MCP 工具不进模型清单，其余照常；工具
// 本身**仍注册着**（策略是收窄层，不是能力清单——注销归注册表）。
func TestMCPToolDeniedByPolicyInvisible(t *testing.T) {
	a, model, _ := newMCPPolicyAgent(t,
		&policy.Config{Deny: []string{"mcp__gh__delete_repo"}},
		nil,
		func(*llm.Request) *llm.Response { return textResponse("我看不到删仓库") },
	)

	sess := session.New("s-policy-visible", nil)
	if _, err := a.Run(context.Background(), sess, "你能做哪些事"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	names := model.seenAt(0)
	if !hasToolName(names, "mcp__gh__search_issues") {
		t.Fatalf("未被拒的 MCP 工具应当可见：%v", names)
	}
	if hasToolName(names, "mcp__gh__delete_repo") {
		t.Fatalf("拒绝名单里的 MCP 工具不该进模型清单：%v", names)
	}

	// 策略只收窄可见性，不注销：工具还在注册表里（程序化路径仍然存在，
	// 那条路径由运行时守卫兜着，见下一个用例）。
	registered := a.RunContext().Tools.Names()
	if !hasToolName(registered, "mcp__gh__delete_repo") {
		t.Fatalf("策略拒绝不该把工具从注册表摘掉：%v", registered)
	}
}

// TestMCPToolBlockedByPolicyBeforeExecution：被策略拒绝的 MCP 工具即使被模型
// 点名调用，也在执行通道的**守卫**这道门 fail-closed 拦下——上游 MCP 客户端
// 一次都不会被调用；同批次里未被拒的 MCP 工具照常执行（逐条独立裁决）。
func TestMCPToolBlockedByPolicyBeforeExecution(t *testing.T) {
	a, _, client := newMCPPolicyAgent(t,
		&policy.Config{Deny: []string{"mcp__gh__delete_repo"}},
		nil,
		// ① 模型点名去删仓库（例如从旧会话里学来的名字）
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c1", "mcp__gh__delete_repo", `{"repo":"prod"}`)
		},
		// ② 换成只读工具
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c2", "mcp__gh__search_issues", `{"q":"bug"}`)
		},
		// ③ 收尾
		func(*llm.Request) *llm.Response { return textResponse("删不了，但能查") },
	)

	sess := session.New("s-policy-guard", nil)
	if _, err := a.Run(context.Background(), sess, "把仓库删了"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// ① 被拒的那次调用：错误回填、理由给到模型。
	denied, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺被拒调用的回填")
	}
	if !denied.IsError {
		t.Fatalf("被策略拒绝的调用应标成失败：%s", denied.Content)
	}
	if !strings.Contains(denied.Content, "安全策略拒绝") {
		t.Fatalf("回填应说明是策略拒绝：%s", denied.Content)
	}
	if !strings.Contains(denied.Content, "mcp__gh__delete_repo") {
		t.Fatalf("回填应点名是哪个工具被拒：%s", denied.Content)
	}

	// ② 上游只被放行的那次碰过（fail-closed 发生在 preflight，早于派发）。
	calls := client.recorded()
	if len(calls) != 1 {
		t.Fatalf("只有放行的那次该打到上游，实际 %d 次：%+v", len(calls), calls)
	}
	if calls[0].name != "search_issues" {
		t.Fatalf("打到上游的应是放行的工具，实际 %q", calls[0].name)
	}

	// ③ 放行的那次正常回填。
	allowed, ok := toolResult(sess, "c2")
	if !ok {
		t.Fatal("缺放行调用的回填")
	}
	if allowed.IsError {
		t.Fatalf("放行的调用不该失败：%s", allowed.Content)
	}
}

// TestMCPToolBlockedByParamRule：参数级规则（deny 档）对 MCP 工具同样生效，
// 走**守卫**这道门（单调、不可被钩子洗白），同样在执行前拦下。
func TestMCPToolBlockedByParamRule(t *testing.T) {
	denyDangerous := func(_ context.Context, call policy.Call) (policy.Verdict, string) {
		if call.Tool == "mcp__gh__delete_repo" {
			return policy.VerdictDeny, "删仓库属于危险操作"
		}
		return policy.VerdictAllow, ""
	}
	a, _, client := newMCPPolicyAgent(t, nil, []policy.Check{denyDangerous},
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c1", "mcp__gh__delete_repo", `{"repo":"prod"}`)
		},
		func(*llm.Request) *llm.Response { return textResponse("不删") },
	)

	sess := session.New("s-param-deny", nil)
	if _, err := a.Run(context.Background(), sess, "删仓库"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	res, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺回填")
	}
	if !res.IsError {
		t.Fatalf("参数级 deny 应拦下调用：%s", res.Content)
	}
	if !strings.Contains(res.Content, "参数级规则") || !strings.Contains(res.Content, "删仓库属于危险操作") {
		t.Fatalf("回填应带规则名前缀与理由：%s", res.Content)
	}
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("被规则拒绝的调用不该打到上游：%+v", calls)
	}
}

// TestMCPToolAskWithoutApprovalFailsClosed：参数级规则给「需人工确认」而宿主
// 没装审批通道时，MCP 调用同样 fail-closed（既不悄悄放行，也不静默失败）。
func TestMCPToolAskWithoutApprovalFailsClosed(t *testing.T) {
	askDangerous := func(_ context.Context, call policy.Call) (policy.Verdict, string) {
		if call.Tool == "mcp__gh__delete_repo" {
			return policy.VerdictAsk, "删仓库要先问人"
		}
		return policy.VerdictAllow, ""
	}
	a, _, client := newMCPPolicyAgent(t, nil, []policy.Check{askDangerous},
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c1", "mcp__gh__delete_repo", `{"repo":"prod"}`)
		},
		func(*llm.Request) *llm.Response { return textResponse("那算了") },
	)

	sess := session.New("s-param-ask", nil)
	if _, err := a.Run(context.Background(), sess, "删仓库"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	res, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺回填")
	}
	if !res.IsError {
		t.Fatalf("无审批通道时 ask 应 fail-closed：%s", res.Content)
	}
	if !strings.Contains(res.Content, "未装配审批通道") {
		t.Fatalf("回填应说明没有审批通道：%s", res.Content)
	}
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("未确认的调用不该打到上游：%+v", calls)
	}
}

// TestMCPDeniedDeferredToolNotSearchable：被策略拒绝的 **deferred** 工具连
// 搜索结果都不该出现——「看不见」与「搜不到」一致。否则模型会拿到一条走不
// 通的路（搜到了、激活了，调用时才被守卫拦）。
func TestMCPDeniedDeferredToolNotSearchable(t *testing.T) {
	client := &recordingMCPClient{mcpStubClient: mcpStubClient{tools: []mcp.ToolInfo{
		{Name: "search_issues", Description: "按关键字搜索仓库的 issue 工单"},
		{Name: "delete_repo", Description: "删除一个仓库"},
	}}}
	model := &scriptedModel{steps: []scriptStep{
		func(*llm.Request) *llm.Response { return textResponse("ok") },
	}}
	a, err := mos.New(mos.Config{
		Model:  model,
		Tools:  newTestRegistry(t),
		Policy: &policy.Config{Deny: []string{"mcp__gh__delete_repo"}},
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{{Name: "gh", Exposure: mcp.ExposureDeferred}},
			Dial:    recordingDialer(map[string]*recordingMCPClient{"gh": client}),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	reg := a.RunContext().Tools
	// deferred 集里不该有被拒的工具。
	for _, d := range reg.DeferredTools() {
		if d.Name == "mcp__gh__delete_repo" {
			t.Fatalf("被拒的 deferred 工具不该留在 deferred 集：%+v", reg.DeferredTools())
		}
	}
	// tool_search 搜“删除仓库”也搜不到它（只搜得到没被拒的那个）。
	ts, ok := reg.Get("tool_search")
	if !ok {
		t.Fatal("有 deferred 工具时该装 tool_search")
	}
	out, err := ts.Execute(context.Background(), json.RawMessage(`{"query":"删除仓库"}`))
	if err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	text := searchResultText(t, out)
	if strings.Contains(text, "mcp__gh__delete_repo") {
		t.Fatalf("被拒的工具不该被搜到：%s", text)
	}
	if !strings.Contains(text, "mcp__gh__search_issues") {
		t.Fatalf("未被拒的工具应当能被搜到：%s", text)
	}
}

// searchResultText 从 tool_search 的返回值里取文本（它返回 map）。
func searchResultText(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("tool_search 返回值形态不对：%T", v)
	}
	s, _ := m["text"].(string)
	return s
}

// TestMCPToolNotInPolicyWhitelist：放行名单（Allow 非空 = 白名单）同样收窄
// MCP 工具面——不在名单里的工具既不进清单，也调不动。
func TestMCPToolNotInPolicyWhitelist(t *testing.T) {
	a, model, client := newMCPPolicyAgent(t,
		&policy.Config{Allow: []string{"some_other_tool"}},
		nil,
		func(*llm.Request) *llm.Response {
			return toolCallResponse("c1", "mcp__gh__search_issues", `{"q":"bug"}`)
		},
		func(*llm.Request) *llm.Response { return textResponse("调不动") },
	)

	sess := session.New("s-whitelist", nil)
	if _, err := a.Run(context.Background(), sess, "查 bug"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, n := range model.seenAt(0) {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("白名单外的 MCP 工具不该进模型清单：%v", n)
		}
	}
	res, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺回填")
	}
	if !res.IsError || !strings.Contains(res.Content, "不在策略放行名单中") {
		t.Fatalf("白名单外的调用应被守卫拦下：%s", res.Content)
	}
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("白名单外的调用不该打到上游：%+v", calls)
	}
}
