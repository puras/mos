// MoS chat Demo（S4b）：驱动层 ReAct 循环 + 直连 LLM 多轮对话。
//
// 装配：llm 协议注册表建模型（套 WithRetry / WithLogging 装饰器）
// → planner.ReAct 决策器 → driver.RunContext（含 tool.Registry）
// → react 驱动。每一轮对话的 turn/user/assistant/tool/turn-end 事件落会话 JSONL，
// 多轮上下文由事件投影累积，重启自动恢复续聊。
//
// 跑法：
//
//	cd examples/chat
//	cp .env.example .env   # 填入 API Key 与模型名
//	go run .               # 默认恢复上次会话；-new 开新会话
//
// 退出：输入 q。
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/gemini"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

func main() {
	fresh := flag.Bool("new", false, "从全新会话开始（默认恢复上次会话）")
	denyFlag := flag.String("deny", "", "禁用指定工具（逗号分隔），演示策略拦截")
	policyFlag := flag.String("policy", "", "策略配置文件（YAML），给出后忽略 -deny")
	workdir := flag.String("workdir", ".", "文件工具的沙箱根目录（jail），越界路径会被拒绝")
	trimFlag := flag.Int("trim", 0, "结果裁剪上限（字节），0 = 不裁剪（post-execute 钩子演示）")
	concFlag := flag.Int("concurrency", 4, "同批工具调用的并发上限（1 = 串行；仅对声明可并发的工具生效）")
	allowShell := flag.Bool("allow-shell", false, "装配 shell 工具（隔离执行；默认不装配=模型看不到这个能力）")
	shellTimeout := flag.Int("shell-timeout", 120, "shell 命令缺省超时（秒）")
	shellMaxTimeout := flag.Int("shell-max-timeout", 300, "shell 命令超时上界（秒，模型申请更久会被夹到这里）")
	shellSpill := flag.String("shell-spill", "", "shell 输出落盘目录（截断时全文写这里；默认不落盘）")
	retryAttempts := flag.Int("retry-attempts", 3, "模型调用总尝试次数（含首次）；1 = 不重试")
	retryDelay := flag.Int("retry-delay", 1, "首次退避基数（秒），之后指数增长（带抖动）")
	approvalFlag := flag.Bool("approval", true, "装配人工审批（需确认时在终端问 y/N；关掉则一律拒绝）")
	flag.Parse()

	loadDotEnv(".env")

	model := mustModel(*retryAttempts, *retryDelay)
	jl, sess := mustSession(*fresh)
	defer jl.Close()

	// 内置工具集：模型可调用的工具在此登记（未登记的工具不进 function calling 清单）
	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
		}
	}

	// 文件沙箱：文件工具的路径先经 Lexical + Realpath 双层校验，越界拒绝。
	// 宿主装配层决定 jail 根（这里是 -workdir，默认当前目录）。
	jailRoot, err := filepath.Abs(*workdir)
	check(err)
	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{jailRoot},
		WriteRoots: []string{jailRoot},
	})
	for _, tl := range builtin.FileTools(fsys) {
		if err := reg.Register(tl); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
		}
	}

	// shell 工具：默认**不装配**——不给模型这个能力，比给它一个必然失败的工具
	// 干净。装配后走 sandbox 隔离执行（超时 / 输出上限 / 资源限制 / 环境裁剪）。
	if *allowShell {
		sb := &sandbox.Local{
			WorkDir:    jailRoot,
			Timeout:    time.Duration(*shellTimeout) * time.Second,    // 缺省
			MaxTimeout: time.Duration(*shellMaxTimeout) * time.Second, // 上界
			SpillDir:   *shellSpill,
		}
		if err := reg.Register(builtin.NewShell(sb)); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", "shell", err))
		}
	}

	// 单调守卫：可改写环节走钩子，安全约束走守卫，两者不可互相绕过。
	// 策略默认放行本示例已注册的内置工具，再叠加 -deny 拒绝名单。
	policyPath := *policyFlag
	if policyPath == "" {
		policyPath = os.Getenv("MOS_POLICY")
	}
	deny := list(*denyFlag + "," + os.Getenv("MOS_DENY_TOOLS"))
	cfg := policyConfig(policyPath, deny)
	guards := hook.NewGuardBus()
	if cfg != nil {
		p := policy.New(*cfg)
		// 可见性过滤：被策略禁掉的工具**不出现在模型的工具清单里**（PI/DSH 的
		// 可见性层语义）——不给模型一个它注定调不成的工具。
		reg.SetVisibility(p.Visible)
		guards.Add(nameLevelGuard(*cfg))
	}

	// 参数级检查（宿主规则）：两个门都要装——
	//   - 守卫门：deny 档直接拒绝（单调，不可被钩子洗白）；
	//   - 钩子门：ask 档转成「需人工确认」（当前没有审批通道 → fail-closed 拒绝）。
	hp := hostPolicy()
	guards.Add(hp.Guard())
	hooks := mustHooks(*trimFlag, hp)

	// 系统引导词：自定义优先；开了 shell 而没自定义时给一份默认引导——
	// 工具在清单里不等于模型会主动用它，得告诉它有这回事、怎么用。
	sysPrompt := os.Getenv("MOS_SYSTEM_PROMPT")
	promptKind := "自定义（MOS_SYSTEM_PROMPT）"
	if sysPrompt == "" && *allowShell {
		sysPrompt = shellSystemPrompt(jailRoot)
		promptKind = "默认（shell 验证指引）"
	}

	// 输入读取器：主循环与人工审批共用（审批发生在驱动阻塞期间，同一
	// goroutine，所以直接读下一行即可）。
	in := bufio.NewScanner(os.Stdin)

	// 人工审批（宿主侧"问人"的实现）：pre-execute 的「需人工确认」会在这里
	// 弹提示。未装配时 ask 降级为拒绝（fail-closed）。
	var answerer approval.Answerer
	if *approvalFlag {
		answerer = terminalAnswerer{in: in, out: os.Stderr}
	}

	rt := &driver.RunContext{
		Planner:      planner.NewReAct(model),
		Tools:        reg,
		Hooks:        hooks,
		Guards:       guards,
		Concurrency:  *concFlag,
		SystemPrompt: sysPrompt,
		Stream:       true,
		OnChunk:      render,
		Answerer:     answerer,
	}
	agent := driver.New(rt, react.New())

	fmt.Printf("MoS chat 已就绪（provider=%s model=%s session=%s 已有 %d 条事件 / 累计 %d tokens）\n",
		provider(), os.Getenv("MOS_MODEL"), sess.ID, len(sess.Events()), sess.TotalUsage().TotalTokens)
	fmt.Printf("  已注册工具: %s\n", strings.Join(reg.Names(), ", "))
	fmt.Printf("  模型可见工具: %s\n", strings.Join(visibleNames(reg), ", "))
	fmt.Printf("  文件沙箱: %s（越界路径会被拒绝，工具不会执行）\n", jailRoot)
	fmt.Printf("  并发上限: %d（仅对声明可并发的工具生效；read_file 可并发，write_file 独占）\n", *concFlag)
	if *retryAttempts > 1 {
		fmt.Printf("  重试: %d 次尝试（首次退避 %ds，指数增长带抖动；只重试瞬时错误）\n", *retryAttempts, *retryDelay)
	} else {
		fmt.Println("  重试: 未开启（1 次尝试，失败即报错）")
	}
	if *allowShell {
		fmt.Printf("  shell: 已装配（隔离执行，超时缺省 %ds / 上界 %ds；工作目录 %s）\n",
			*shellTimeout, *shellMaxTimeout, jailRoot)
	} else {
		fmt.Println("  shell: 未装配（模型看不到这个能力；加 -allow-shell 开启）")
	}
	if sysPrompt != "" {
		fmt.Printf("  系统引导: %s\n", promptKind)
	}
	// 名字级策略（Allow/Deny 名单）装没装，看 cfg——守卫总线本身总是非 nil
	//（它还挂着参数级的宿主规则）。
	fmt.Print(policyBanner(cfg, policyPath))
	fmt.Println("  参数级规则: 宿主规则已装配（敏感文件拒绝 / 递归删除需人工确认）")
	if *approvalFlag {
		fmt.Println("  人工审批: 已装配（需确认时在终端问 y/N；空回车 = 拒绝）")
	} else {
		fmt.Println("  人工审批: 未装配（需确认的调用一律拒绝，fail-closed）")
	}
	fmt.Println("输入 q 退出。")

	for {
		fmt.Print("\n你 > ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if line == "q" || line == "quit" || line == "exit" {
			break
		}
		reply, err := agent.Run(context.Background(), sess, line)
		if err != nil {
			fmt.Println()
			fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
			continue
		}
		// 回复已由 OnChunk 流式渲染（决策器回退非流式时也会一次性 emit 全文）；
		// 仅在关闭流式时才在此补打印，避免同一段回复显示两遍。
		if !rt.Stream {
			fmt.Println(reply)
		}
		flushLine() // 模型回复未必以换行结尾，先收尾再打统计，避免两行黏在一起
		u := sess.TotalUsage()
		fmt.Printf("  [tokens] 会话累计 输入 %d / 输出 %d / 共 %d（第 %d 轮）\n",
			u.PromptTokens, u.CompletionTokens, u.TotalTokens, sess.NextTurn()-1)
	}
}

// render 渲染流式增量：思维链用暗色，正文原样（打字机效果）。
func render(delta string, reasoning bool) {
	if reasoning {
		fmt.Printf("\x1b[2m%s\x1b[0m", delta)
	} else {
		fmt.Print(delta)
	}
	if delta != "" {
		pendingLine.Store(true)
	}
}

// pendingLine 记录终端上是否还有没换行的半截输出（模型回复不以换行结尾）。
// 随后的 token 统计与模型调用日志都靠 flushLine 另起一行，避免插进回复中间。
var pendingLine atomic.Bool

func flushLine() {
	if pendingLine.Swap(false) {
		fmt.Println()
	}
}

// mustModel 经协议注册表构建模型对象，套「重试 + 调用日志」两层装饰器。
//
// 装饰器嵌套顺序即执行顺序（外层先进）：日志刻意套在重试之外——重试失败再试
// 会各打一行，重试了几次、共耗时多久一目了然；若把日志放里面，只会看到最后
// 一次成功，失败被静默吞掉。
func mustModel(retryAttempts, retryDelay int) llm.ChatModel {
	reg := llm.NewRegistry()
	reg.Register("openai", openai.New)
	reg.Register("anthropic", anthropic.New)
	reg.Register("gemini", gemini.New)

	keyEnv := map[string]string{
		"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY",
	}[provider()]
	if os.Getenv(keyEnv) == "" {
		fmt.Fprintf(os.Stderr, "缺少 %s（在 .env 或环境变量中配置）\n", keyEnv)
		os.Exit(1)
	}
	model, err := reg.Create(provider(), llm.Config{
		"api_key":  os.Getenv(keyEnv),
		"base_url": os.Getenv("MOS_BASE_URL"),
		"model":    os.Getenv("MOS_MODEL"),
	})
	check(err)

	// 重试策略：**参数是宿主的，机制是框架的**（llm.WithRetry）。
	// 只重试瞬时错误（限流 / 5xx / 连接失败 / 空响应）；4xx、鉴权失败、
	// 配额耗尽、用户取消一律快速失败——重试确定性的错误只是浪费配额。
	m := llm.WithRetry(model, llm.RetryPolicy{
		MaxAttempts: retryAttempts,
		BaseDelay:   time.Duration(retryDelay) * time.Second,
		// 重试要看得见：静默重试是查不出问题的。
		OnScheduled: func(attempt int, delay time.Duration, err error) {
			fmt.Fprintf(os.Stderr, "[llm] 第 %d 次重试（%v 后）：%v\n", attempt, delay.Round(time.Millisecond), err)
		},
	})
	if logLLM() {
		// 模型名等日志内容由闭包持有，装饰器只管「调用结束时回调一次」
		name := os.Getenv("MOS_MODEL")
		m = llm.WithLogging(m, func(r llm.CallResult) { logCall(name, r) })
	}
	return m
}

// shellSystemPrompt 是开启 shell 时的默认系统引导词。
//
// 工具在清单里 ≠ 模型会主动用它：得说清三件事——有什么能力、工作目录在哪、
// 为什么要用（验证回路）。没有这段，模型多半仍然只写代码不验证。
func shellSystemPrompt(workdir string) string {
	return fmt.Sprintf(`你可以通过 shell 工具执行命令来验证改动（go build / go test / git diff 等）。

- 工作目录固定为 %s，命令在那里执行（不要 cd 到别处）。
- 命令是隔离执行的：有时限（超会被杀）、输出有上限（超了会截断并给全文路径）、
  环境变量是白名单（不是全部主机的环境）。
- 正确做法是「改代码 → 跑命令 → 看真实输出 → 再改」，不要靠猜代码对不对。
- 命令失败不是故障：退出码非零、输出原样回给你，照着输出改就是。
- 练手项目在 lab/ 子目录（独立的 go module）：修好那里的测试并跑通。`, workdir)
}

// visibleNames 列出**模型真正看到**的工具（可见性过滤之后）。
func visibleNames(reg *tool.Registry) []string {
	var names []string
	for _, tl := range reg.List() {
		names = append(names, tl.Name)
	}
	return names
}

// mustGuards 构建单调守卫链：安全策略（Policy）作为第一道关卡。
//
// 拒绝是「结果」不是「故障」：工具不执行，拒绝理由经 tool/result 回填给
// 模型，让它自行纠正。
//
// 这里在 Policy 外包一层只为演示可见性（打印被拒的工具名与轮次）——裁决
// 本身全部委托 Policy.Check，示例不自己重写规则。GuardBus 尚无观测通道，
// 正经日志是 S5c 之后的事。
//
// policyBanner 拼出策略横幅。
//
// 单独立函数是为了能测：cfg 为 nil（没装名字级名单）是**正常路径**，而守卫
// 总线此时并非 nil（它还挂着参数级的宿主规则）——曾经在这里判错分支，
// 直接解引用了 nil 的 cfg。
func policyBanner(cfg *policy.Config, policyPath string) string {
	if cfg == nil {
		return "  策略: 未装配（名字级名单不收紧；参数级宿主规则仍然生效）\n"
	}
	src := "默认（放行已注册工具）+ -deny"
	if policyPath != "" {
		src = "文件 " + policyPath
	}
	allow := "不限制"
	if len(cfg.Allow) > 0 {
		allow = "[" + strings.Join(cfg.Allow, ", ") + "]"
	}
	deny := "无"
	if len(cfg.Deny) > 0 {
		deny = "[" + strings.Join(cfg.Deny, ", ") + "]"
	}
	// 语义：Allow 未配置 = 不限制（未列名的工具放行）；Allow 非空 = 只放行名单内。
	unlisted := "未列名的工具默认放行"
	if len(cfg.Allow) > 0 {
		unlisted = "未列名的工具一律拒绝"
	}
	return fmt.Sprintf("  策略[%s]: 放行 %s 拒绝 %s（%s）\n", src, allow, deny, unlisted)
}

// nameLevelGuard 构建**工具名级**的守卫（Policy 的 Allow/Deny 名单）。
//
// 名字级策略管「能不能调这个工具」；参数级规则（hostPolicy）管「这次调用的
// 参数危不危险」——同一套守卫链上，先名字级后参数级。
func nameLevelGuard(cfg policy.Config) hook.Guard {
	p := policy.New(cfg)
	return func(ctx context.Context, ev *hook.HookEvent) error {
		err := p.Check(ctx, ev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[guard] 第 %d 轮：拒绝 %s——工具不会执行（%v）\n",
				ev.Turn, ev.ToolCall.Name, err)
		}
		return err
	}
}

// mustHooks 构建可改写环节的钩子链（HookBus）——与守卫是**两条不同的轨道**：
// 守卫单调裁决（拒绝/放行），钩子可改写（裁剪结果、环绕派发、裁决调用）。
//
// 这里演示三件事：
//  1. pre-execute：只观测即将执行的调用（不改载荷——载荷对钩子冻结）；
//  2. tool/execute：环绕派发，打印耗时（真实执行发生在钩子之间）；
//  3. post-execute：把过长的结果裁剪（钩子最典型的用途）。
//
// 载荷冻结是刻意的：钩子**改不了**工具名/参数/CallID，所以它无法绕过策略；
// 要修正入参请在工具内部做（工具自己 normalize）。
func mustHooks(trim int, pp *policy.ParamPolicy) *hook.HookBus {
	hb := hook.NewHookBus()

	// 参数级审批钩子**放最前**：ask 决策会短路后面的钩子——被拦下的调用
	// 不该再打印「即将执行」（它不会真的执行）。
	if pp != nil {
		hb.On(hook.HookToolPreExecute, pp.Approval())
	}

	hb.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		// 打 turn + callID：同一 turn 里的多次调用都只打「第 N 轮」分不清，
		// 带上 callID 才看得出是两次不同的调用。
		fmt.Fprintf(os.Stderr, "[hook] turn %d 即将执行 %s（call %s）\n", ev.Turn, ev.ToolCall.Name, ev.ToolCall.ID)
		return next(ctx, ev)
	})

	hb.On(hook.HookToolExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		start := time.Now()
		out, err := next(ctx, ev) // ← 真正的工具调用在这里发生
		fmt.Fprintf(os.Stderr, "[hook] turn %d %s 执行完毕（%v，call %s）\n", ev.Turn, ev.ToolCall.Name, time.Since(start).Round(time.Millisecond), ev.ToolCall.ID)
		return out, err
	})

	if trim > 0 {
		hb.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
			out, err := next(ctx, ev)
			if len(out.ResultText) > trim {
				orig := len(out.ResultText)
				out.ResultText = out.ResultText[:trim] + fmt.Sprintf("…（已裁剪，原 %d 字节）", orig)
			}
			return out, err
		})
	}
	return hb
}

// policyConfig 决定生效的策略：
//   - 指定 -policy 文件：完全以该 YAML 为准（平台纯配置装配，G3）；
//   - 否则：只有 -deny / MOS_DENY_TOOLS 的拒绝名单（allow 未配置 = 不限制）。
//
// 语义对齐 PI / DSH：两份名单都可选，都不给 = 不限制。策略层是**收窄层**——
// fail-closed 的那道门是注册表（没注册的工具模型看不见也调不动）。
func policyConfig(policyPath string, deny []string) *policy.Config {
	if policyPath != "" {
		cfg, err := loadPolicy(policyPath)
		check(err)
		return &cfg
	}
	if len(deny) == 0 {
		return nil // 不装策略 = 不收紧
	}
	return &policy.Config{Deny: deny}
}

// appPolicy 是**应用层**的策略文件格式：键名由应用定，与框架解耦。
//
// 框架的 policy.Config 不带格式 tag，也不提供 YAML 解析——读文件与认格式都是
// 应用层的活（框架不碰文件系统、不绑定配置格式）。用独立 DTO 的好处：框架字段
// 改名不破坏既有配置文件。
type appPolicy struct {
	// Allow 可选：未配置（nil）= 不限制；配成空列表 = 全禁。
	// 区分两者需要指针（yaml 的 `allow: []` 与不写 allow，语义不同）。
	Allow *[]string `yaml:"allow"`
	Deny  []string  `yaml:"deny"`
}

// loadPolicy 读策略文件并解析成框架的 policy.Config。
//
// 严格解析（KnownFields(true)）：字段名拼错直接报错，不静默忽略——被忽略的
// 拼写错误会让用户以为规则生效了，实际上什么都没拦。
func loadPolicy(path string) (policy.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Config{}, fmt.Errorf("读取策略文件 %q: %w", path, err)
	}
	var ap appPolicy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&ap); err != nil {
		return policy.Config{}, fmt.Errorf("解析策略文件 %q: %w", path, err)
	}
	cfg := policy.Config{Deny: ap.Deny}
	if ap.Allow != nil {
		cfg.Allow = *ap.Allow
	}
	return cfg, nil
}

// list 按逗号切分并去空白（命令行与 .env 共用）。
func list(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// logLLM 是模型调用观测日志的开关：默认开启，MOS_LOG_LLM=0 关闭。
func logLLM() bool { return os.Getenv("MOS_LOG_LLM") != "0" }

// llmCalls 是模型调用计数，仅用于给日志行编号，便于与会话事件对号入座。
var llmCalls atomic.Int64

// logCall 打印一次模型调用的观测行：模式 / 耗时 / 请求规模 / 停止原因 / token。
//
// 只观测不改写——要改写模型输入或拦下某次调用，属 S5 的 HookBus（可短路可改写），
// 那是另一套机制。本 Demo 把日志写 stderr：stdout 是流式渲染的地盘。
func logCall(model string, r llm.CallResult) {
	flushLine()
	mode := "chat  "
	if r.Stream {
		mode = "stream"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[llm #%d] %s %6.2fs model=%s", llmCalls.Add(1), mode, r.Elapsed.Seconds(), model)
	if r.Req != nil {
		fmt.Fprintf(&b, " msgs=%d", len(r.Req.Messages))
		if n := len(r.Req.Tools); n > 0 {
			fmt.Fprintf(&b, " tools=%d", n)
		}
	}
	switch {
	case r.Err != nil:
		fmt.Fprintf(&b, " 错误=%v", r.Err)
	case r.Resp == nil:
		b.WriteString(" 空响应")
	default:
		finish := r.Resp.FinishReason
		if finish == "" {
			finish = "-"
		}
		u := r.Resp.Usage
		fmt.Fprintf(&b, " finish=%s tokens=%d/%d/%d",
			finish, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		// 缓存命中与推理 token：非零才打印，避免噪声
		if d := u.Details; d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 || d.Output.Reasoning > 0 {
			if d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 {
				fmt.Fprintf(&b, " cache=%d/%d", d.Input.CacheRead, d.Input.CacheWrite)
			}
			if d.Output.Reasoning > 0 {
				fmt.Fprintf(&b, " reasoning=%d", d.Output.Reasoning)
			}
		}
		if calls := r.Resp.Message.ToolCalls; len(calls) > 0 {
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				names = append(names, c.Name)
			}
			fmt.Fprintf(&b, " → 请求调用 %s", strings.Join(names, ", "))
		}
	}
	fmt.Fprintln(os.Stderr, b.String())
}

// mustSession 打开或恢复会话（JSONL 落 data/；中断的 turn 由 Restore 合成收尾）。
// 返回后端与会话：后端由调用方负责 Close。
func mustSession(fresh bool) (*session.JSONL, *session.Session) {
	jl, err := session.NewJSONL("data")
	check(err)
	id := "chat"
	if fresh {
		id = fmt.Sprintf("chat-%d", time.Now().Unix())
	}
	_ = jl.SaveHeader(id, session.Header{ID: id, CreatedAt: time.Now()})
	sess, err := session.Restore(id, jl)
	check(err)
	return jl, sess
}

func provider() string {
	if p := os.Getenv("MOS_PROVIDER"); p != "" {
		return p
	}
	return "openai"
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadDotEnv 极简 .env 加载器：忽略空行与 # 注释，KEY=VALUE 注入环境（已存在则不覆盖）。
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
