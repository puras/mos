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
//	go run . -session <id> # 继续指定会话（-list-sessions 列出可选项）
//
// 退出：输入 q。
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/contextfile"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
	"github.com/puras/mos/tool/web"
)

func main() {
	fresh := flag.Bool("new", false, "从全新会话开始（默认恢复上次会话）")
	sessionFlag := flag.String("session", "", "继续指定会话（data/<id>.jsonl；不存在则新建）")
	listSessions := flag.Bool("list-sessions", false, "列出可继续的会话 id 后退出")
	denyFlag := flag.String("deny", "", "禁用指定工具（逗号分隔），演示策略拦截")
	policyFlag := flag.String("policy", "", "策略配置文件（YAML），给出后忽略 -deny")
	workdir := flag.String("workdir", ".", "文件工具的沙箱根目录（jail），越界路径会被拒绝")
	trimFlag := flag.Int("trim", 32*1024, "结果截断上限（字节），默认 32768；0 = 不截断（截断全文落盘可回读）")
	contextWindow := flag.Int("context-window", 131072, "模型上下文窗口（token，默认 128K，超过窗口-预留即触发压缩）；<=0 关闭自动压缩（溢出风险自担）")
	reserveTokens := flag.Int("reserve-tokens", 16384, "为模型响应预留的空间（token），超过窗口-预留即触发压缩")
	keepRecentTokens := flag.Int("keep-recent-tokens", 20000, "压缩后保留的近期上下文预算（token）")
	concFlag := flag.Int("concurrency", 4, "同批工具调用的并发上限（1 = 串行；仅对声明可并发的工具生效）")
	allowShell := flag.Bool("allow-shell", false, "装配 shell 工具（隔离执行；默认不装配=模型看不到这个能力）")
	allowWeb := flag.Bool("allow-web", false, "装配联网工具 web_search / web_fetch（默认不装配=模型看不到这个能力）")
	webProvider := flag.String("web-provider", "duckduckgo", "搜索后端：duckduckgo（零 Key）/ tavily / serper / brave / searxng")
	shellTimeout := flag.Int("shell-timeout", 120, "shell 命令缺省超时（秒）")
	shellMaxTimeout := flag.Int("shell-max-timeout", 300, "shell 命令超时上界（秒，模型申请更久会被夹到这里）")
	shellSpill := flag.String("shell-spill", "", "shell 输出落盘目录（截断时全文写这里；默认不落盘）")
	retryAttempts := flag.Int("retry-attempts", 3, "模型调用总尝试次数（含首次）；1 = 不重试")
	retryDelay := flag.Int("retry-delay", 1, "首次退避基数（秒），之后指数增长（带抖动）")
	approvalFlag := flag.Bool("approval", true, "装配人工审批（需确认时在终端问 y/N；关掉则一律拒绝）")
	agentsFlag := flag.Bool("agents", true, "加载上下文文件（AGENTS.md / CLAUDE.md 等，从工作目录向上递归，注入系统提示词的「项目约定」节）")
	flag.Parse()

	loadDotEnv(".env")

	model := mustModel(*retryAttempts, *retryDelay)
	if *listSessions {
		for _, id := range listSessionIDs("data") {
			fmt.Println(id)
		}
		return
	}
	sess := mustSession(*fresh, *sessionFlag)

	// 内置工具集：模型可调用的工具在此登记（未登记的工具不进 function calling 清单）
	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
		}
	}

	// 文件沙箱：文件工具的路径先经 Lexical + Realpath 双层校验，越界拒绝。
	// 宿主装配层决定 jail 根（这里是 -workdir，默认当前目录）。
	// 根集合是**活**的：会话中途用户授权新目录（模型调 grant_access、人在终端
	// 批准）后，加根对下一次工具调用立即生效——文件工具与 shell 沙箱共用
	// 这一份（装配层会把根集合接到沙箱的动态范围上，两层不会错位）。
	jailRoot, err := filepath.Abs(*workdir)
	check(err)
	roots := filesystem.NewRootSet([]string{jailRoot}, []string{jailRoot})
	// ToolsInRoots 一套给齐：文件三件套（read/write/edit）+ 搜索三件套
	//（grep/glob/ls），同一份 jail（活根集合）+ 写前观察门（没读过的文件不许覆盖）。
	//
	// WithExclude：敏感文件（.env / 私钥）既不让 read 读，也不让 grep 搜到
	//——否则“凭据不进模型上下文”的保证会被按内容搜的工具绕过去（宿主策略，
	// 框架只提供挂点）。
	fileTools, err := builtin.ToolsInRoots(roots, builtin.WithExclude(sensitivePath))
	check(err)
	for _, tl := range fileTools {
		if err := reg.Register(tl); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
		}
	}
	// 授权入口：模型只能"申请"（grant_access），批不批由人决定（hostRules 里的
	// ask 档 → 终端审批）。不注册这个工具 = 不给模型申请授权的通道。
	if err := reg.Register(builtin.NewGrantRoot(roots)); err != nil {
		check(fmt.Errorf("注册工具 grant_access: %w", err))
	}

	// 联网工具：默认**不装配**（能力即证据）。装配时：
	//   - web_search 走搜索后端（默认 DuckDuckGo，零 Key）；抓取走 web.Fetcher
	//     （带 SSRF 防御：内网地址拒绝 + 地址钉住 + 只跟同源重定向 + 体积上限）；
	//   - 抓回的内容是**外部声明**，进不了 TaskState（审计门白名单不含 web_*）。
	// 后端选错（未知名称 / 缺 Key）→ 明确报错，不静默降级。
	if *allowWeb {
		searcher, err := webSearcher(*webProvider)
		check(err)
		for _, tl := range web.Tools(searcher, web.HTTPFetcher{}) {
			if err := reg.Register(tl); err != nil {
				check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
			}
		}
	}

	// shell 工具：默认**不装配**——不给模型这个能力，比给它一个必然失败的工具
	// 干净。装配后走 sandbox 隔离执行（超时 / 输出上限 / 资源限制 / 环境裁剪）。
	// 沙箱由宿主造，装配层拿到它就注册 shell 工具（能力即证据）。
	var sb *sandbox.Local
	if *allowShell {
		sb = &sandbox.Local{
			WorkDir:    jailRoot,
			Timeout:    time.Duration(*shellTimeout) * time.Second,    // 缺省
			MaxTimeout: time.Duration(*shellMaxTimeout) * time.Second, // 上界
			SpillDir:   *shellSpill,
		}
		// 路径强制（S5f-2b）：默认范围 = 工作区（macOS 上真实生效，写外面会被
		// 内核拒绝）。被隔离的命令要写缓存/临时目录，宿主把它们指到工作区内：
		// go test 要写 GOCACHE 与临时目录，写不到宿主的缓存目录就不所谓"验证"了。
		for _, d := range []string{filepath.Join(jailRoot, ".gocache"), filepath.Join(jailRoot, ".tmp")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				check(fmt.Errorf("创建沙箱缓存目录 %q: %w", d, err))
			}
		}
		sb.Env = map[string]string{
			"GOCACHE": filepath.Join(jailRoot, ".gocache"),
			"TMPDIR":  filepath.Join(jailRoot, ".tmp"),
			"GOENV":   "off", // 别让它写宿主的 go 配置
		}
	}

	// 名字级策略：Allow/Deny 名单（-policy 文件或 -deny 快捷方式）。
	// 可见性过滤 + 守卫拒绝都由装配层按这份名单接上：被禁掉的工具**不出现在
	// 模型的工具清单里**（PI/DSH 的可见性层语义）——不给模型一个它注定调不成的
	// 工具。
	policyPath := *policyFlag
	if policyPath == "" {
		policyPath = os.Getenv("MOS_POLICY")
	}
	deny := list(*denyFlag + "," + os.Getenv("MOS_DENY_TOOLS"))
	cfg := policyConfig(policyPath, deny)

	// 系统提示词：分节装配（agent/prompt）——默认节 + 工具自带指引 + 部署事实，
	// 三类贡献方一处汇合；MOS_SYSTEM_PROMPT 仍可整段替换（Complete 语义：
	// 宿主最懂就全听宿主的）。组装一次、静态不变，利于服务商 prompt cache。
	//
	// 上下文文件（AGENTS.md 等）由**根包装配**（Config.Context）：它进
	// 「项目约定」节，位置在约定之后、工具指引之前——手工往字符串里塞就
	// 只能追加到最后，位置定不下来。
	var promptBuilder *prompt.Builder
	promptKind := ""
	customPrompt := os.Getenv("MOS_SYSTEM_PROMPT")
	if customPrompt != "" {
		promptKind = "自定义（MOS_SYSTEM_PROMPT，整段替换）"
	} else {
		promptBuilder, promptKind, err = buildSystemPrompt(reg, jailRoot, *allowShell)
		check(err)
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

	// 上下文工程（S6，对齐 PI）：
	//   - 截断：工具结果过大时就地瘦身（行/字节双限），全文落工作区内
	//     （read 可回读）；
	//   - 压缩：超过 token 预算（窗口 − 预留）时，模型生成结构化摘要
	//     落会话日志，投影从此遮蔽旧历史。窗口默认 128K；确切窗口更小的
	//     模型必须用 -context-window 显式配置，否则会在溢出后才触发。
	// 截断上限：宿主约定 0 = 不截断（框架里 0 是"默认上限"，这里翻译成负数）。
	trimBytes := *trimFlag
	if trimBytes == 0 {
		trimBytes = -1
	}

	// 装配层（mos.New）：把模型、工具、策略、安全、上下文工程一次装成可跑的
	// 智能体。宿主只提供"要什么、允许什么"，接线与默认由框架做。
	mosCfg := mos.Config{
		Model:             model,
		Tools:             reg,
		Approval:          answerer,    // 问人：宿主实现（终端弹 y/N）
		Sandbox:           sb,          // 仅 -allow-shell 时非 nil（能力即证据）
		Policy:            cfg,         // 名字级名单（宿主解析 YAML 得到）
		Rules:             hostRules(), // 参数级规则（宿主业务判断）
		Roots:             roots,       // 活根集合：文件工具与 shell 共用，运行期可加根
		WorkDir:           jailRoot,
		ContextWindow:     *contextWindow,
		ReserveTokens:     *reserveTokens,
		KeepRecentTokens:  *keepRecentTokens,
		TruncateMaxBytes:  trimBytes,
		SpillDir:          filepath.Join(jailRoot, ".mos-trunc"),
		Concurrency:       *concFlag,
		Stream:            true,
		OnChunk:           render,
		DisableCompaction: *contextWindow <= 0,
	}
	if promptBuilder != nil {
		// 分节装配：根包渲染它，并把上下文文件装进「项目约定」节。
		mosCfg.Prompt = promptBuilder
		if *agentsFlag {
			mosCfg.Context = &mos.ContextConfig{
				Dir:     jailRoot,
				Options: contextfile.Options{MaxDepth: -1}, // 向上递归到根（PI 同款）
			}
		}
	} else {
		// 宿主整段替换：上下文文件也不注入（宿主说了算，不偷偷加料）。
		mosCfg.SystemPrompt = customPrompt
	}
	a, err := mos.New(mosCfg)
	check(err)

	// 宿主自己的观测钩子：装配层之后挂——审批钩子要留在 pre-execute 的**最前**
	//（ask 决策要短路后面的钩子，被拦下的调用不该再打"即将执行"）。
	hooks := a.RunContext().Hooks
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		fmt.Fprintf(os.Stderr, "[hook] turn %d 即将执行 %s（call %s）\n", ev.Turn, ev.ToolCall.Name, ev.ToolCall.ID)
		return next(ctx, ev)
	})
	hooks.On(hook.HookToolExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		start := time.Now()
		out, err := next(ctx, ev) // ← 真正的工具调用在这里发生
		fmt.Fprintf(os.Stderr, "[hook] turn %d %s 执行完毕（%v，call %s）\n", ev.Turn, ev.ToolCall.Name, time.Since(start).Round(time.Millisecond), ev.ToolCall.ID)
		return out, err
	})

	fmt.Printf("MoS chat 已就绪（provider=%s model=%s session=%s 已有 %d 条事件 / 累计 %d tokens）\n",
		provider(), os.Getenv("MOS_MODEL"), sess.ID, len(sess.Events()), sess.TotalUsage().TotalTokens)
	// 装配快照由框架给（Describe）：装了什么、**没装什么**都写出来——宿主
	// 不再各写一份横幅，也就不会出现"横幅说有、其实没装"的情况。
	fmt.Print(a.Describe())
	fmt.Printf("  文件沙箱: %s（越界路径会被拒绝，工具不会执行）\n", jailRoot)
	if *retryAttempts > 1 {
		fmt.Printf("  重试: %d 次尝试（首次退避 %ds，指数增长带抖动；只重试瞬时错误）\n", *retryAttempts, *retryDelay)
	} else {
		fmt.Println("  重试: 未开启（1 次尝试，失败即报错）")
	}
	if *allowShell {
		fmt.Printf("  shell 超时: 缺省 %ds / 上界 %ds（写限定在工作区，越界写被内核拒绝）\n",
			*shellTimeout, *shellMaxTimeout)
	}
	if *approvalFlag {
		fmt.Println("  人工审批: 已装配（需确认时在终端问 y/N；空回车 = 拒绝）")
	} else {
		fmt.Println("  人工审批: 未装配（需确认的调用一律拒绝，fail-closed）")
	}
	// 打印的是**实际送进模型的那份**系统提示词（从运行时取，不拿本地变量
	// 充数——本地那份可能少了根包装的上下文文件）。
	if finalPrompt := a.RunContext().SystemPrompt; finalPrompt != "" {
		fmt.Printf("  系统引导: %s\n", promptKind)
		// 把最终送进模型的系统提示词原文打出来：装配结果是否合意、金样例之外
		// 的真实组合（宿主节 + 工具指引 + 项目约定）一眼可验，也方便贴 issue 对照。
		fmt.Println("  ───── 系统提示词 ─────")
		for line := range strings.SplitSeq(finalPrompt, "\n") {
			if line == "" {
				fmt.Println()
			} else {
				fmt.Printf("  %s\n", line)
			}
		}
		fmt.Println("  ─────────────────────")
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
		reply, err := a.Run(context.Background(), sess, line)
		if err != nil {
			fmt.Println()
			fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
			continue
		}
		_ = reply   // 回复已由 OnChunk 流式渲染
		flushLine() // 模型回复未必以换行结尾，先收尾再打统计，避免两行黏在一起
		u := sess.TotalUsage()
		// 区分开：输入（去缓存口径）/ 缓存读 / 缓存写 / 输出 / 共。
		// 缓存命中单列，才看得出"这次省了多少"（Anthropic 的输入本就不含
		// 缓存，OpenAI 的含——统一用去缓存口径才跨协议可比）。
		// 括号里用"，"分隔：可选的"缓存命中 xx%" + 轮次
		parts := []string{}
		if u.Details.Input.CacheRead > 0 {
			parts = append(parts, fmt.Sprintf("缓存命中 %.0f%%", u.CacheHitRatio()*100))
		}
		parts = append(parts, fmt.Sprintf("第 %d 轮", sess.NextTurn()-1))
		fmt.Printf("  [tokens] 会话累计 输入 %d / 缓存读 %d / 缓存写 %d / 输出 %d / 共 %d（%s）\n",
			u.FreshInput(), u.Details.Input.CacheRead, u.Details.Input.CacheWrite,
			u.CompletionTokens, u.TotalTokens, strings.Join(parts, "，"))
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

// webSearcher 按名字选搜索后端（宿主侧配置）。
//
// 密钥从环境变量读（框架不读环境变量，配置来源归宿主）：TAVILY_API_KEY /
// SERPER_API_KEY / BRAVE_API_KEY / SEARXNG_BASE_URL。缺了就报错——不静默
// 降级到一个“看起来能用”的后端。
func webSearcher(name string) (web.Searcher, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "duckduckgo", "ddg":
		return web.NewSearcher(web.NewDuckDuckGo()), nil
	case "tavily":
		key := os.Getenv("TAVILY_API_KEY")
		if key == "" {
			return nil, errors.New("web: tavily 需要 TAVILY_API_KEY")
		}
		return web.NewSearcher(web.NewTavily(key)), nil
	case "serper":
		key := os.Getenv("SERPER_API_KEY")
		if key == "" {
			return nil, errors.New("web: serper 需要 SERPER_API_KEY")
		}
		return web.NewSearcher(web.NewSerper(key)), nil
	case "brave":
		key := os.Getenv("BRAVE_API_KEY")
		if key == "" {
			return nil, errors.New("web: brave 需要 BRAVE_API_KEY")
		}
		return web.NewSearcher(web.NewBrave(key)), nil
	case "searxng":
		base := os.Getenv("SEARXNG_BASE_URL")
		if base == "" {
			return nil, errors.New("web: searxng 需要 SEARXNG_BASE_URL")
		}
		return web.NewSearcher(web.NewSearXNG(base)), nil
	default:
		return nil, fmt.Errorf("web: 未知搜索后端 %q（支持 duckduckgo/tavily/serper/brave/searxng）", name)
	}
}

// mustModel 经协议注册表构建模型对象，套「重试 + 调用日志」两层装饰器。
//
// 装饰器嵌套顺序即执行顺序（外层先进）：日志刻意套在重试之外——重试失败再试
// 会各打一行，重试了几次、共耗时多久一目了然；若把日志放里面，只会看到最后
// 一次成功，失败被静默吞掉。
func mustModel(retryAttempts, retryDelay int) llm.ChatModel {
	// 密钥从哪来是宿主的事；框架只公布约定的环境变量名（不读环境变量）。
	keyEnv := mos.APIKeyEnv(provider())
	if keyEnv == "" {
		fmt.Fprintf(os.Stderr, "未知协议 %q（可选：openai / anthropic / gemini）\n", provider())
		os.Exit(1)
	}
	if os.Getenv(keyEnv) == "" {
		fmt.Fprintf(os.Stderr, "缺少 %s（在 .env 或环境变量中配置）\n", keyEnv)
		os.Exit(1)
	}
	// max_tokens 是输出的硬上限：太小 → 回答半截、工具调用参数残缺
	//（anthropic 协议必填，框架默认 8192）。宿主可用环境变量调。
	extra := llm.Config{}
	if v := os.Getenv("MOS_MAX_TOKENS"); v != "" {
		extra["max_tokens"] = v
	}
	// 重试策略：**参数是宿主的，机制是框架的**（框架只重试瞬时错误：限流 /
	// 5xx / 连接失败 / 空响应；4xx、鉴权失败、配额耗尽、用户取消一律快速
	// 失败——重试确定性的错误只是浪费配额）。
	model, err := mos.NewModel(mos.ModelConfig{
		Provider: provider(),
		APIKey:   os.Getenv(keyEnv),
		BaseURL:  os.Getenv("MOS_BASE_URL"),
		Model:    os.Getenv("MOS_MODEL"),
		Extra:    extra,
		Retry:    llm.RetryPolicy{MaxAttempts: retryAttempts, BaseDelay: time.Duration(retryDelay) * time.Second},
		OnRetry: func(attempt int, delay time.Duration, err error) {
			// 重试要看得见：静默重试是查不出问题的。
			fmt.Fprintf(os.Stderr, "[llm] 第 %d 次重试（%v 后）：%v\n", attempt, delay.Round(time.Millisecond), err)
		},
		Log: func(r llm.CallResult) {
			if logLLM() {
				// 模型名等日志内容由闭包持有，装饰器只管「调用结束时回调一次」
				logCall(os.Getenv("MOS_MODEL"), r)
			}
		},
	})
	check(err)
	return model
}

// buildSystemPrompt 装配系统提示词的分节（agent/prompt Builder）：
//   - prompt.Default()  框架默认节（身份 + 简洁约定）；
//   - reg.Guidelines()  工具自带的跨调用指引（文案归工具包，tool.Guideline 接口）；
//   - shellRuntimeGuide 部署事实（工作目录、验证回路）——只有宿主知道的事。
//
// 上下文文件（AGENTS.md 等）不在这里：它由根包装配（mos.Config.Context），
// 进「项目约定」节，位置在约定之后、工具指引之前。
func buildSystemPrompt(reg *tool.Registry, jailRoot string, allowShell bool) (*prompt.Builder, string, error) {
	pb := prompt.Default()
	if err := pb.AddToolGuidelines(reg.Guidelines()); err != nil {
		return nil, "", fmt.Errorf("装配工具指引: %w", err)
	}
	kind := "分节装配（默认 + 工具指引"
	if allowShell {
		err := pb.Add(prompt.Section{
			Name:  "runtime:shell",
			Order: prompt.OrderRuntime,
			Text:  shellRuntimeGuide(jailRoot),
		})
		if err != nil {
			return nil, "", fmt.Errorf("装配 shell 部署事实: %w", err)
		}
		kind += " + shell 部署事实"
	}
	return pb, kind + "）", nil
}

// shellRuntimeGuide 是 shell 的部署事实节（runtime 位置）：只有宿主知道的事——
// 工作目录在哪、为什么要跑命令（验证回路）、练手项目在哪。工具自身的行为约定
// （退出码标记、截断逃生通道）归 builtin.Shell 的 tool.Guideline，不在这里重复。
func shellRuntimeGuide(workdir string) string {
	return fmt.Sprintf(`shell 在隔离环境执行：工作目录固定为 %s，不要 cd 到别处；`+
		`命令有时限（超会被杀）、输出有上限（超了会截断并给全文路径）、环境变量是白名单。

正确做法是「改代码 → 跑命令 → 看真实输出 → 再改」，不要靠猜代码对不对。`, workdir)
}

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
		// tokens 三元组 = 输入(去缓存) / 输出 / 共；缓存读另列（命中才是省下来的部分）
		fmt.Fprintf(&b, " finish=%s tokens=%d/%d/%d",
			finish, u.FreshInput(), u.CompletionTokens, u.TotalTokens)
		// 缓存命中与推理 token：非零才打印，避免噪声
		if d := u.Details; d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 || d.Output.Reasoning > 0 {
			if d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 {
				fmt.Fprintf(&b, " cache=%d/%d", d.Input.CacheRead, d.Input.CacheWrite)
			}
			// 命中率是自算的：cache_read / (输入 + cache_read)
			if d.Input.CacheRead > 0 {
				fmt.Fprintf(&b, " hit=%.0f%%", u.CacheHitRatio()*100)
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
//
// 三种选择：
//   - -new：新建时间戳会话（chat-<unix>），不接旧上下文；
//   - -session <id>：**继续指定的那个**（不存在则新建同名空会话）；
//   - 都不给：继续默认会话 chat（示例里的"恢复上次会话"）。
func mustSession(fresh bool, wantID string) *session.Session {
	id := "chat"
	switch {
	case fresh && wantID != "":
		// 一个要全新、一个要指定——矛盾，不猜哪个赢。
		check(errors.New("-new 与 -session 不能同时给：一个要全新会话，一个要指定会话"))
	case fresh:
		id = fmt.Sprintf("chat-%d", time.Now().Unix())
	case wantID != "":
		id = wantID
	}
	// 建或恢复一行（落盘 data/<id>.jsonl；不存在则新建）。
	sess, err := session.Open("data", id)
	check(err)
	return sess
}

// listSessionIDs 列出 data/ 下可继续的会话 id（文件名去掉 .jsonl）。
func listSessionIDs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	sort.Strings(ids)
	return ids
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
