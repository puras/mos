// Command assistant 是基于 MoS 的真实智能体 CLI，支持多协议 LLM 与内置工具集。
//
// 直接运行（无 API Key 时进入 mock 离线模式）：
//
//	export OPENAI_API_KEY=sk-xxx   # 或 ANTHROPIC_API_KEY
//	export MOS_MODEL=deepseek-chat # 可选
//	go run ./cmd/assistant
//
// 交互命令：/exit 退出，/reset 新开会话，/tools 查看工具，/help 帮助。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"hegq.net/mos/agent"
	"hegq.net/mos/assistant"
)

func main() {
	cfg, err := assistant.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	cfg.Observer = observer // 挂载任务循环观测（思考/工具调用过程）

	a, err := assistant.New(*cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败:", err)
		os.Exit(1)
	}

	fmt.Println("======================================")
	fmt.Println(" MoS Assistant — 智能体 CLI")
	fmt.Println("======================================")
	fmt.Printf(" Provider : %s\n", cfg.Provider)
	if cfg.Model != "" {
		fmt.Printf(" Model    : %s\n", cfg.Model)
	}
	fmt.Printf(" Tools    : %s\n", strings.Join(a.ToolNames(), ", "))
	fmt.Println(" 输入 /exit 退出，/reset 新会话，/tools 查看工具")
	fmt.Println("--------------------------------------")

	sess := a.NewSession()
	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()

	for {
		fmt.Print("\n你 > ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit", "/q":
			fmt.Println("再见！")
			return
		case "/reset", "/new":
			sess = a.NewSession()
			fmt.Println("（已开启新会话）")
			continue
		case "/tools":
			fmt.Println("可用工具:", strings.Join(a.ToolNames(), ", "))
			continue
		case "/help":
			fmt.Println("命令: /exit 退出  /reset 新会话  /tools 查看工具")
			continue
		}

		reply, err := a.Run(ctx, sess, line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\n执行出错:", err)
			continue
		}
		fmt.Printf("\n智能体 > %s\n", reply)
	}
}

// observer 输出任务循环中的中间过程（Loop/Harness Engineering 观测点）。
func observer(ev agent.Event) {
	switch ev.Type {
	case agent.EventStep:
		fmt.Printf("  ⟳ [第%d轮思考] %s\n", ev.Iteration, ev.Content)
	case agent.EventToolCall:
		fmt.Printf("  ▶ [第%d轮] 调用工具 %s(%s)\n", ev.Iteration, ev.ToolCall.Name, ev.ToolCall.Args)
	case agent.EventToolResult:
		fmt.Printf("  ◀ [第%d轮] 工具结果: %v\n", ev.Iteration, ev.ToolResult)
	}
}
