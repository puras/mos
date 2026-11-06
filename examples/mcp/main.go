// MoS MCP 示例：接一台 MCP 服务器，让它的工具经 tool_search 被搜到、被调用。
//
// 装配只有四行关键代码（MCP.Servers + Name/Command/Args），其余都是宿主的活：
//   - 装配期连上服务器、发现工具，按 exposure 注册进同一张工具注册表 —— MCP 工具
//     和平常工具没有区别，策略、审批、守卫、并发、截断全都照常管它们；
//   - 默认 **deferred**：服务器工具面通常很大，全量声明会撑爆提示词。模型先
//     `tool_search` 搜到它，下一次请求才在清单里看见；
//   - 服务器说明（instructions）按预算注入一次；超预算整段省略，`Describe()`
//     里能看见省了谁；
//   - 单台服务器连不上**不影响其余**，也不炸装配：降级说明进 `Describe()`。
//
// 跑法（示例不读 .env，环境变量自己导出——配置来源归宿主）：
//
//	cd examples/mcp
//	export MOS_PROVIDER=openai ANTHROPIC_API_KEY=sk-...  # 按协议取对应的那把
//	export MOS_MODEL=gpt-4o-mini
//	go run .                       # 默认起 npx 的参考服务器（需要 Node）
//	go run . -- npx -y @modelcontextprotocol/server-everything
//	go run . -- /path/to/your-mcp-server --flag
//
// 这一步需要 Node（npx）。没装也能跑：连不上只会让这例子没有 MCP 工具，
// 装配照样成功，你会看到快照里的那条降级说明。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/puras/mos"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
	"github.com/puras/mos/tool/mcp"
)

func main() {
	provider := env("MOS_PROVIDER", "openai")
	keyEnv := mos.APIKeyEnv(provider)
	if keyEnv == "" {
		fmt.Fprintf(os.Stderr, "未知协议 %q\n", provider)
		os.Exit(1)
	}
	model, err := mos.NewModel(mos.ModelConfig{
		Provider: provider,
		APIKey:   os.Getenv(keyEnv),
		BaseURL:  env("MOS_BASE_URL", ""),
		Model:    env("MOS_MODEL", "gpt-4o-mini"),
		Retry:    llm.RetryPolicy{MaxAttempts: 3},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	a, err := mos.New(mos.Config{
		Model: model,
		Tools: reg,
		MCP: &mos.MCPConfig{
			Servers: []mcp.ServerConfig{mcpServerFromArgs()},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = a.Close() }() // 长连接（stdio 是子进程）：用完必须收掉

	// Describe() 是装配快照：连上了哪台、装了多少工具、有谁降级了。
	fmt.Print(a.Describe())

	// deferred 集 = 「注册了但还没声明给模型」的工具。MCP 工具默认都在这里，
	// 模型搜到之前不进模型清单。
	deferred := a.RunContext().Tools.DeferredTools()
	if len(deferred) > 0 {
		fmt.Printf("已注册但未声明的工具（模型要先 tool_search 才会看见）：%d 个\n", len(deferred))
		for _, n := range deferred {
			fmt.Printf("  - %s\n", n)
		}
	} else {
		fmt.Println("没有 deferred 工具：这台服务器可能没连上，看上面快照里的降级说明。")
	}

	q := strings.Join(os.Args[1:], " ")
	if q == "" || strings.HasPrefix(q, "--") {
		q = "先用 tool_search 搜一下你能用的 MCP 工具，然后调用其中最简单的一个试试，最后用一句话汇报结果。"
	}

	sess, err := session.Open("data", "mcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	reply, err := a.Run(context.Background(), sess, q)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(reply)
	fmt.Printf("[tokens] 输入 %d / 输出 %d\n",
		sess.TotalUsage().FreshInput(), sess.TotalUsage().CompletionTokens)
}

// mcpServerFromArgs 让命令行第一个非 flag 参数及其后内容成为服务器命令：
//
//	go run . -- npx -y @modelcontextprotocol/server-everything
//
// 不给就用参考服务器（everything：工具、资源、提示词都有，适合试手）。
func mcpServerFromArgs() mcp.ServerConfig {
	var cmd []string
	if i := indexOf(os.Args, "--"); i >= 0 {
		cmd = os.Args[i+1:]
	}
	if len(cmd) == 0 {
		cmd = []string{"npx", "-y", "@modelcontextprotocol/server-everything"}
	}
	return mcp.ServerConfig{
		Name:        "demo",
		Description: "示例 MCP 服务器（工具经 tool_search 搜到才进清单）",
		Command:     cmd[0],
		Args:        cmd[1:],
		// Exposure 留空即 deferred。改成 mcp.ExposureDirect 就是全量声明
		// （工具面小时简单，工具面大时会撑爆提示词）；hidden 是"模型既看不见
		// 也搜不到，程序还能调"。
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// env 读环境变量，空则用默认值。
func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
