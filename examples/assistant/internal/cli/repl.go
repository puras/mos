// Package cli 实现 MoS 完整智能体的交互式命令行（REPL），
// 仅内部使用：依赖 pkg 基础库，被 cmd/assistant 薄入口调用。
package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"hegq.net/mos/agent"
	"hegq.net/mos/assistant"
	"hegq.net/mos/examples/assistant/internal/config"
	"hegq.net/mos/llm"
)

// Run 启动完整智能体 CLI：加载配置 → 组装智能体 → 交互式问答。
func Run() {
	modelName, model, err := config.LoadModel()
	if err != nil {
		fmt.Fprintln(os.Stderr, red("模型加载失败: "+err.Error()))
		os.Exit(1)
	}
	display := modelName
	if m := os.Getenv("MOS_MODEL"); m != "" {
		display += "/" + m
	}
	model = llm.WithLogging(model, logModelCall(display, os.Getenv("MOS_BASE_URL"))) // 挂载模型调用观测（标准包装器：过程/用量/耗时）
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, red("配置错误: "+err.Error()))
		os.Exit(1)
	}
	cfg.Observer = observer // 挂载任务循环观测（思考/工具调用过程）
	attachConfirmer(cfg, isTerminal(os.Stdin) && isTerminal(os.Stdout))

	a, err := assistant.New(model, loadTools(), *cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, red("初始化失败: "+err.Error()))
		os.Exit(1)
	}

	// 展示层策略（应用层能力）：stdin/stdout 均为终端时启用全屏 TUI，
	// 否则（管道/重定向/非交互环境）回退为纯文本 REPL，避免控制码污染输出。
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		if err := runTUI(a, display); err != nil {
			fmt.Fprintln(os.Stderr, red("TUI 退出异常: "+err.Error()))
		}
		return
	}

	fmt.Println(banner("MoS Assistant — 智能体 CLI"))
	fmt.Printf(" %s: %s\n", dim("Protocol"), modelName)
	if m := os.Getenv("MOS_MODEL"); m != "" {
		fmt.Printf(" %s: %s\n", dim("Model"), m)
	}
	fmt.Printf(" %s: %s\n", dim("Tools"), strings.Join(a.ToolNames(), ", "))
	fmt.Println(dim(" 输入 /exit 退出，/reset 新会话，/tools 查看工具"))

	sess := a.NewSession()
	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()

	for {
		fmt.Print("\n" + green(bold("你 > ")))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit", "/q":
			fmt.Println(green("再见！"))
			return
		case "/reset", "/new":
			sess = a.NewSession()
			fmt.Println(yellow("（已开启新会话）"))
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
			fmt.Fprintln(os.Stderr, "\n"+red("执行出错: "+err.Error()))
			continue
		}
		fmt.Printf("\n%s%s\n", cyan(bold("智能体 > ")), reply)
	}
}

// pendingThink 缓存上一条思考，用于应用层去重展示：
// 最终作答轮的思考（EventStep）与最终回复（EventDone）内容相同，只展示一次。
var pendingThink struct {
	turn    int
	content string
}

// attachConfirmer 按展示模式决定是否挂载交互确认器（应用层策略）：
// 纯文本 REPL（非全屏 TUI）挂载，危险操作执行前向用户确认；
// 全屏 TUI 不挂载——stdin 被界面接管，受限危险操作（rm/sudo 等）按 fail-closed 拒绝。
func attachConfirmer(cfg *assistant.Config, tui bool) {
	if tui {
		return
	}
	if dp, ok := cfg.Policy.(*assistant.DefaultPolicy); ok {
		dp.Confirm = cliConfirm{}
	}
}

// eventSink 任务循环观测事件的输出口：TUI 模式下由界面接管（通道转发），
// 否则回退到下方默认的纯文本打印。路由属于应用层展示策略。
var eventSink func(agent.Event)

// observer 输出任务循环中的中间过程（Loop/Harness Engineering 观测点）。
// 展示策略属于应用层：思考默认逐轮打印，最终轮与回复重复的思考不展示。
func observer(ev agent.Event) {
	if eventSink != nil {
		eventSink(ev)
		return
	}
	switch ev.Type {
	case agent.EventStep:
		pendingThink.turn, pendingThink.content = ev.Turn, ev.Content
	case agent.EventToolCall:
		flushThink()
		fmt.Printf("  %s %s %s\n", dim("▶"), cyan(fmt.Sprintf("[第%d轮]", ev.Turn)), green(fmt.Sprintf("调用工具 %s", ev.ToolCall.Name))+dim("("+ev.ToolCall.Args+")"))
	case agent.EventToolResult:
		flushThink()
		fmt.Printf("  %s %s %s\n", dim("◀"), cyan(fmt.Sprintf("[第%d轮]", ev.Turn)), green("工具结果: ")+yellow(fmt.Sprint(ev.ToolResult)))
	case agent.EventDone:
		// 最终回复由主流程打印；若上一条思考与之内容相同则跳过
		if pendingThink.content != "" && pendingThink.content != ev.Content {
			flushThink()
		}
		pendingThink = struct {
			turn    int
			content string
		}{}
	}
}

// flushThink 输出缓存的思考行（工具事件前调用）。
func flushThink() {
	if pendingThink.content == "" {
		return
	}
	fmt.Printf("  %s %s %s\n", dim("⟳"), yellow(fmt.Sprintf("[第%d轮思考]", pendingThink.turn)), pendingThink.content)
	pendingThink = struct {
		turn    int
		content string
	}{}
}

// isTerminal 判断文件描述符是否指向交互终端（全屏 TUI 需要 stdin/stdout 均为终端）。
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd())
}
