// harness.go 提供七站共享的骨架：模型构造（双协议）、内置工具集、
// RunContext 基座与 REPL 循环。各 stage 文件只写"本站新增的装配差异"。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	fs "github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

// defaultSystemPrompt 是各站共用的简短系统引导词（刻意精简，
// 槽位式组装的完整形态见 stage4 的 SystemAssembler）。
const defaultSystemPrompt = `你是 MoS 学习 CLI 的智能体，通过调用工具解决用户问题。
回答使用与用户相同的语言，简洁、准确；能用工具确认的事实不要凭空猜测。`

// harness 是单个站点的运行载体：一个装配好的 agent + 会话 + 命令表。
type harness struct {
	protocol string // openai | anthropic（仅横幅展示用）
	stream   bool   // -stream：RunStream 打字机输出

	agent *driver.Agent      // 装配好的智能体（driver.Agent 或句柄内agent）
	rt    *driver.RunContext // 共享 RunContext（各站往里加自己的设施）
	sess  *session.Session   // 当前会话

	commands map[string]func(arg string) // 斜杠命令表（/xxx → 处理函数）
	// handleInput 非空时接管默认输入（如 stage5 的挂起-恢复流程）；
	// nil 时走默认 Run / RunStream。
	handleInput func(ctx context.Context, input string) (string, error)
	extraBanner []string // 启动横幅的站点附加说明
}

// baseRunContext 构造各站共享的 RunContext 基座：内置工具集 + ReAct 规划器 +
// 沙箱化文件能力（jail 到 temp/）+ 事件总线。各站在此基础上追加自己的字段。
// 第二个返回值是工具注册表（stage4 等需要追加注册工具的站点使用）。
func baseRunContext(model llm.ChatModel) (*driver.RunContext, *tool.Registry) {
	reg := tool.NewRegistry()
	for _, t := range builtin.Default() {
		check(reg.Register(t))
	}
	rt := &driver.RunContext{
		Tools:             reg,
		Planner:           planner.NewReAct(model),
		FS:                &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: workspaceRoot()}},
		SystemPrompt:      defaultSystemPrompt,
		EventBus:          event.NewEventBus(),
		MaxTurns:          flagMaxTurns,
		DoomLoopMaxRepeat: flagDoom,
	}
	return rt, reg
}

// repl 是共享 REPL 主循环：斜杠命令分发 + 默认输入走 handleInput / Run。
func (h *harness) repl() {
	// 流式回显：站点未设 Observer 时安装一个只打印增量的默认观察器。
	if h.stream && h.rt.Observer == nil {
		h.rt.Observer = func(ev agent.Event) {
			if ev.Type != agent.EventStream {
				return
			}
			if ev.Reasoning != "" {
				printDim(ev.Reasoning)
			}
			fmt.Print(ev.Delta)
		}
	}
	for _, line := range h.extraBanner {
		fmt.Printf("  %s\n", line)
	}
	fmt.Println("输入任务开始；q 退出。")

	ctx := context.Background()
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())
		switch {
		case line == "":
			continue
		case line == "q", line == "quit":
			return
		case strings.HasPrefix(line, "/"):
			name, arg := line, ""
			if i := strings.IndexByte(line, ' '); i >= 0 {
				name, arg = line[:i], strings.TrimSpace(line[i+1:])
			}
			if fn, ok := h.commands[name]; ok {
				fn(arg)
				continue
			}
			fmt.Printf("（未知命令 %s；可用：%v）\n", name, h.commandNames())
			continue
		}
		out, err := h.input(ctx, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run error: %v\n", err)
			continue
		}
		// 流式路径增量已实时打印，不再重复输出全文。
		if !h.stream {
			fmt.Println(out)
		}
	}
}

// input 处理一次默认输入：站点接管（handleInput）优先，否则 Run / RunStream。
func (h *harness) input(ctx context.Context, line string) (string, error) {
	if h.handleInput != nil {
		return h.handleInput(ctx, line)
	}
	if h.stream {
		return h.agent.RunStream(ctx, h.sess, line)
	}
	return h.agent.Run(ctx, h.sess, line)
}

// on 注册一个斜杠命令。
func (h *harness) on(name string, fn func(arg string)) {
	if h.commands == nil {
		h.commands = make(map[string]func(string))
	}
	h.commands[name] = fn
}

func (h *harness) commandNames() []string {
	names := make([]string, 0, len(h.commands))
	for n := range h.commands {
		names = append(names, n)
	}
	return names
}

// newSession 创建挂 UUID 的空会话。
func newSession() *session.Session {
	return session.NewSession(uuid.NewString())
}

// workspaceRoot 返回文件工具的 jail 根目录（examples/agentcli/temp，自动创建）。
func workspaceRoot() string {
	root, err := filepath.Abs("temp")
	check(err)
	check(os.MkdirAll(root, 0o755))
	return root
}

// printObserver 返回打印实时事件流的 Observer（stage2 起各站复用；
// 流式增量交给 harness 的默认回显器，这里不再打印，避免重复）。
func printObserver() agent.Observer {
	return func(ev agent.Event) {
		switch ev.Type {
		case agent.EventStream:
			// 交给 repl 安装的流式回显器处理。
		case agent.EventStep:
			fmt.Printf("%s[思考 t%d] %s\n", dim, ev.Turn, short(ev.Content, 160))
		case agent.EventToolCall:
			args := ""
			if ev.ToolCall != nil {
				args = short(ev.ToolCall.Args, 80)
			}
			fmt.Printf("%s[工具调用 t%d] %s(%s)\n", dim, ev.Turn, eventToolName(ev), args)
		case agent.EventToolResult:
			fmt.Printf("%s[工具结果 t%d] %s\n", dim, ev.Turn, short(fmt.Sprintf("%v", ev.ToolResult), 120))
		case agent.EventDone:
			fmt.Printf("%s[完成]\n", dim)
		case agent.EventFact:
			if ev.Fact != nil {
				fmt.Printf("%s[事实] %s\n", dim, ev.Fact.Summary)
			}
		case agent.EventCompacted:
			fmt.Printf("%s[压缩] %s\n", dim, short(ev.Content, 120))
		case agent.EventContextUsage:
			fmt.Printf("%s[上下文] %s\n", dim, ev.Content)
		case agent.EventPlan:
			fmt.Printf("%s[计划]\n%s\n", dim, ev.Content)
		case agent.EventReflection:
			fmt.Printf("%s[评审] %s\n", dim, short(ev.Content, 160))
		case agent.EventScript:
			fmt.Printf("%s[脚本]\n%s\n", dim, ev.Content)
		case agent.EventResume:
			fmt.Printf("%s[恢复] %s\n", dim, ev.Content)
		}
	}
}

// eventToolName 从事件里取工具名（无工具调用时返回 "?"）。
func eventToolName(e agent.Event) string {
	if e.ToolCall != nil {
		return e.ToolCall.Name
	}
	return "?"
}

// ---- 输出小工具 ----

const dim = "\033[2m"

// short 截断长文本用于打印。
func short(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// printDim 以暗色输出文本。
func printDim(s string) {
	fmt.Print(dim + s + "\033[0m")
}

// ---- 模型与 .env（照抄 protocli 的双协议约定）----

// newModel 按协议构造 llm.ChatModel（openai 兼容 / anthropic 原生）。
func newModel(protocol string) (llm.ChatModel, error) {
	switch protocol {
	case "openai":
		return openai.New(llm.Config{
			"api_key":  os.Getenv("OPENAI_API_KEY"),
			"base_url": os.Getenv("OPENAI_BASE_URL"),
			"model":    os.Getenv("OPENAI_MODEL"),
		})
	case "anthropic":
		return anthropic.New(llm.Config{
			"api_key":  os.Getenv("ANTHROPIC_API_KEY"),
			"base_url": os.Getenv("ANTHROPIC_BASE_URL"),
			"model":    os.Getenv("ANTHROPIC_MODEL"),
		})
	default:
		return nil, fmt.Errorf("未知协议 %q（支持 openai | anthropic）", protocol)
	}
}

// loadDotEnv 极简 .env 加载器：不引第三方依赖。
// 读 path；忽略空行与 # 注释；解析 KEY=VALUE 注入 os.Setenv（已存在则不覆盖）。
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
