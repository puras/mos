// Package mos 是 MoS 的装配层：把「模型 + 工具 + 策略 + 安全 + 上下文工程」
// 一次性装成一个可跑的智能体。
//
// 分工一句话：**框架负责接线与默认，宿主负责要什么、允许什么**——策略
// （Allow/Deny、参数级规则）、能力（审批问人、shell 沙箱、工具集）与密钥来源
// 都在宿主手里。框架不碰文件系统与网络，也不绑定配置格式（YAML/JSON 解析归
// 宿主，policy 层已是这样做的）。
//
// 四条不变式（详见 docs/assembly.md）：
//
//   - 能力即证据：能力字段是接口/对象，nil = **不提供该能力**（模型看不到，
//     需要时一律拒绝）。不用「开关 + 参数」表达「有没有」；
//   - 缺能力 = fail-closed：没有模型就跑不起来；没有审批通道，需人工确认的
//     调用一律拒绝；没给沙箱就不注册 shell；
//   - 配置 ≠ 装配：Config 不含文件路径、配置格式、密钥来源；
//   - 装配快照是事实：Describe() 只描述装了什么，**没装什么也写出来**。
//
// 用法：
//
//	a, err := mos.New(mos.Config{
//		Model:    model,
//		Tools:    reg,
//		Approval: answerer, // 问人：宿主实现
//		Policy:   &policy.Config{Allow: []string{"read"}},
//		Rules:    hostRules, // 参数级规则：deny→守卫 / ask→审批
//	})
//	reply, err := a.Run(ctx, sess, "读一下 README")
package mos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/contextfile"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

// Config 是装配层配置：全字段可选，零值 = 框架默认（见 defaults.go）。
//
// 字段分四组：必填、能力、策略、运行参数。**能力即证据**——能力字段是
// 接口/对象，nil 表示不提供该能力，框架不会"顺手装一个"。
type Config struct {
	// ── 必填 ──
	//
	// Model 是对话模型。nil 时 New 直接报错：没有模型就装配不出能跑的
	// 智能体（fail-closed，不降级成空转）。
	//
	// **单模型模式**：与 Models 二选一（都给 = 不知道听谁的，装配报错）。
	Model llm.ChatModel
	// Models 是模型目录（**多模型模式**）：会话里可切换的候选，运行期能换
	// （a.SetModel，下一轮生效，不用重建智能体）。与 Model 二选一。
	//
	// 目录项：ID（稳定标识，同一目录内唯一）+ Label（展示名）+ Chat（模型）。
	Models []ModelOption
	// DefaultModelID 是默认模型 ID（空 = 目录第一项）。不在目录里 → 装配报错。
	DefaultModelID string

	// ── 能力（nil = 不提供该能力）──
	//
	// Tools 是工具注册表。nil = 空注册表（模型看不到任何工具）。宿主自己
	// 注册工具（含文件工具、shell 等），装配层不替宿主选工具。
	Tools *tool.Registry
	// Approval 是人工审批的应答方（宿主实现"问人"）。nil = 未装配审批通道，
	// 需要人工确认的调用一律拒绝（fail-closed）。
	Approval approval.Answerer
	// Sandbox 是 shell 的隔离执行器。非 nil 时装配层注册 shell 工具
	// （注册表里已有 shell 则不重复注册）。nil = 不提供 shell 能力。
	Sandbox *sandbox.Local
	// Compactor 是上下文压缩器。nil = 按下面的窗口参数建默认的模型压缩器；
	// 宿主自备则用宿主的（想换摘要提示词时）。
	Compactor agent.Compactor
	// DisableCompaction 为 true 时**不装**压缩器（溢出风险自担）。
	//
	// 语义分两类，别混：压缩/截断是**机制**（框架造得出、参数是量）→ 默认装、
	// 显式关；审批/沙箱是**能力**（只有宿主有）→ 给了才有，nil 就没有。
	DisableCompaction bool
	// Truncator 是工具结果截断器。nil = 按 TruncateMaxBytes 建默认的
	// （全文落盘，read 可回读）。
	Truncator agent.ResultTruncator
	// Persistence 是会话落盘后端。nil = 内存会话（不落盘）。
	Persistence session.Persistence

	// Roots 是**运行期可变的根集合**（可选）：声明文件 jail 与 shell 沙箱
	// 共享的读写根。
	//
	// 给了它就能在运行期增删根（a.AddRoot(dir)）——用户在会话中途授权另一个
	// 目录时不用重建智能体。宿主应把**同一个集合**接给文件工具
	// （filesystem.NewSandboxedWithRoots(roots) / builtin.FileToolsInRoots）
	// 与沙箱（sandbox.ScopeFunc）；装配层会把它接到 cfg.Sandbox 上（宿主没
	// 自己接 Scope 时），避免"工具能读、命令写不了"的边界错位。
	//
	// nil = 不提供这个能力（a.AddRoot 报错，不静默无效）。
	Roots *filesystem.RootSet

	// ── 策略（宿主决定允许什么）──
	//
	// Policy 是名字级名单（工具 allow/deny + 模型可见性过滤）。nil = 不收紧
	// （全部已注册工具都可见、可调）。
	Policy *policy.Config
	// Rules 是参数级规则：deny 档进守卫（单调拒绝，不可被钩子洗白），
	// ask 档进 pre-execute 钩子（转人工确认）。nil = 无参数级规则。
	Rules []policy.Check

	// ── 运行参数（零值 = 框架默认）──
	//
	// SystemPrompt 是静态系统引导词（作为历史首条消息注入）。空 = 不加。
	//
	// 与 Prompt **二选一**：两个都给 = 装配错误（不静默挑一个——不知道听谁的）。
	SystemPrompt string
	// Prompt 是系统提示词的**分节装配器**（agent/prompt）：宿主用它把身份、
	// 约定、项目上下文、工具指引、部署事实装配成一段。
	//
	// nil = 不分节，直接用 SystemPrompt 字符串。给了它，根包就用它渲染
	// （装配期渲染一次，之后静态不变——利于 prompt cache）。
	Prompt *prompt.Builder
	// Context 装配**上下文文件**（AGENTS.md / CLAUDE.md 等）：项目自己的规矩
	// 写在文件里，模型据此知道这个项目的门规。
	//
	// nil = 不装配。装配后作为「项目约定」进系统提示词：给了 Prompt 就进分节
	// （位置在约定之后、工具指引之前）；没给 Prompt 就追加在 SystemPrompt
	// 之后（根包的 SystemPrompt 是字符串，位置只能到末尾——想要精确位置就用
	// Prompt）。
	Context *ContextConfig
	// WorkDir 是工作目录：文件工具的沙箱根、截断落盘目录的父目录。空 = 当前目录。
	WorkDir string
	// ContextWindow 是模型上下文窗口（token）。0 = 默认 128K；确切窗口更小的
	// 模型必须显式配置，否则会在溢出后才触发压缩。
	ContextWindow int
	// ReserveTokens 是为模型响应预留的空间。0 = 默认 16K。
	ReserveTokens int
	// KeepRecentTokens 是压缩后保留的近期上下文预算。0 = 默认 20K。
	KeepRecentTokens int
	// TruncateMaxBytes 是单次工具结果截断上限。0 = 默认 32KB；**负数 = 不截断**。
	// 宿主自备 Truncator 时本项忽略。
	TruncateMaxBytes int
	// SpillDir 是截断全文的落盘目录。空 = <WorkDir>/.mos-trunc（在工作区内，
	// 逃生通道才真的可达）。
	SpillDir string
	// Concurrency 是同一批工具调用的并发上限。<=0 = 默认 4；1 = 串行。
	Concurrency int
	// MaxDecisionRounds 是单轮内决策次数上限（含最终作答）。<=0 = 不限
	// （上下文增长由压缩接管；保险丝留给无人值守场景按需设置）。
	MaxDecisionRounds int
	// Stream 为 true 时走决策器的流式接口；OnChunk 是流式增量回调
	//（reasoning=true 表示这是思维链增量，应用层可据此区别渲染）。
	// 零值 = 非流式（与框架默认一致）。
	Stream  bool
	OnChunk func(delta string, reasoning bool)
}

// Agent 是装配产物：模型 + 工具 + 策略 + 安全 + 上下文工程。
//
// 它**无状态**：会话由调用方持有（a.Run(ctx, sess, msg)），一个 Agent 可服务
// 多个会话；运行期旋钮是 agent 级的（并行会话共用同一份设置）。
type Agent struct {
	rt     *driver.RunContext
	driver driver.LoopDriver

	tools *tool.Registry
	// policyHolder 持有当前的名字级策略与其守卫移除函数（热替换用）。
	policyMu     sync.Mutex
	policy       *policy.Policy
	removeGuard  func()
	rules        *rulesHolder
	compactor    agent.Compactor
	truncator    agent.ResultTruncator
	spillDir     string
	workDir      string
	truncateMax  int
	shellEnabled bool
	// roots 是运行期可变的根集合（宿主给的活边界；nil = 没有这个能力）。
	roots *filesystem.RootSet

	// 上下文文件（装配快照用；contextWired = 宿主有没有要求装这个能力）。
	contextWired   bool
	contextFiles   []string
	contextBytes   int
	contextWarning []string

	// catalog 是模型目录（多模型模式；nil = 单模型模式）。
	catalog *modelCatalog

	// 运行期旋钮（原子；下一轮生效）。
	concurrency atomic.Int64
	maxRounds   atomic.Int64
}

// ContextConfig 是上下文文件的装配参数。
//
// 零值可用：Dir 空 = 用 Config.WorkDir；Options 零值 = 默认候选名、32KB 预算、
// **不向上递归**（只看 Dir 本身）。
type ContextConfig struct {
	// Dir 是要扫的目录（空 = Config.WorkDir）。扫的是目录**本身**：每个目录里
	// 取第一个命中的候选文件（AGENTS.override.md > AGENTS.md > AGENTS.MD >
	// CLAUDE.md > CLAUDE.MD）。
	Dir string
	// Options 是发现参数（候选名 / 全局目录 / 字节预算 / 递归层数 / Reader）。
	//
	// GlobalDirs 为空 = 用 MoS 的全局目录（~/.mos）——全局约定是「所有项目
	// 共享的那部分」，默认该有；填了就以宿主填的为准（不追加）。
	Options contextfile.Options
}

// New 装配智能体。缺 Model 或参数非法直接报错（fail-closed）。
//
// 装配出来的 Agent 无状态：会话由调用方持有并传入 Run。
func New(cfg Config) (*Agent, error) {
	// 模型：单模型模式（Config.Model）或目录模式（Config.Models），**二选一**。
	// 都没给 = 没装配模型（fail-closed）；都给 = 不知道听谁的（报错，不静默挑）。
	var catalog *modelCatalog
	var model llm.ChatModel
	switch {
	case len(cfg.Models) > 0 && cfg.Model != nil:
		return nil, errors.New("mos: Model 与 Models 二选一（两个都给，不知道听谁的）")
	case len(cfg.Models) > 0:
		cat, err := newModelCatalog(cfg.Models, cfg.DefaultModelID)
		if err != nil {
			return nil, err
		}
		catalog = cat
		model = cat.shell // 转发壳：运行期换模型只换壳里的目标
	case cfg.Model != nil:
		model = cfg.Model
	default:
		return nil, errors.New("mos: 未装配模型（Config.Model 或 Config.Models 必填其一）")
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	// 系统提示词：分节装配器优先（宿主用 agent/prompt 拼好），否则用字符串。
	// 两个都给 = 装配错误：不知道听谁的，不静默挑一个。
	if cfg.Prompt != nil && cfg.SystemPrompt != "" {
		return nil, errors.New("mos: SystemPrompt 与 Prompt 二选一（两个都给，不知道听谁的）")
	}
	systemPrompt := cfg.SystemPrompt
	pb := cfg.Prompt

	// 上下文文件（AGENTS.md 等）：装配后作为「项目约定」进系统提示词。
	//
	// 位置：有分节装配器就进分节（约定之后、工具指引之前）；没有就追加在
	// 字符串末尾（根包的 SystemPrompt 是字符串，位置只能到末尾）。
	var contextFiles []string
	contextBytes := 0
	var contextWarning []string
	if cfg.Context != nil {
		dir := cfg.Context.Dir
		if dir == "" {
			dir = cfg.WorkDir
		}
		opts := cfg.Context.Options
		if len(opts.GlobalDirs) == 0 {
			// 宿主没显式给全局目录 = 用 MoS 自己的（~/.mos）：全局约定是
			// 「所有项目共享的那部分」，默认该有。
			opts.GlobalDirs = []string{contextfile.DefaultGlobalDir()}
		}
		res, err := contextfile.NewLoader(opts).Load(dir)
		if err != nil {
			return nil, fmt.Errorf("mos: 加载上下文文件: %w", err)
		}
		contextWarning = res.Warnings
		contextBytes = len(res.Text)
		for _, f := range res.Files {
			contextFiles = append(contextFiles, f.Path)
		}
		if res.Text != "" {
			if pb != nil {
				if err := pb.AddProjectContext(res.Text); err != nil {
					return nil, fmt.Errorf("mos: 装配项目约定: %w", err)
				}
			} else {
				if systemPrompt != "" {
					systemPrompt += "\n\n"
				}
				systemPrompt += res.Text
			}
		}
	}
	if pb != nil {
		systemPrompt = pb.Render()
	}

	// 工具注册表：宿主自备优先，nil = 空注册表。
	reg := cfg.Tools
	if reg == nil {
		reg = tool.NewRegistry()
	}

	// 根集合（运行期可变）：宿主给的活边界。同时把它接到沙箱的动态范围上
	//（宿主没自己接 Scope 时）——文件 jail 与 shell 沙箱共用一份根，
	// 用户授权新目录时两层同时放宽，不会错位。
	if roots := cfg.Roots; roots != nil && cfg.Sandbox != nil && cfg.Sandbox.Scope == nil {
		cfg.Sandbox.Scope = sandbox.ScopeFunc(
			func() []string { return roots.RootsFor(filesystem.OpRead) },
			func() []string { return roots.RootsFor(filesystem.OpWrite) },
		)
	}

	// shell：给了沙箱就注册 shell 工具（能力即证据）；已有则不重复注册。
	shellEnabled := false
	if cfg.Sandbox != nil {
		if _, ok := reg.Get("shell"); !ok {
			if err := reg.Register(builtin.NewShell(cfg.Sandbox)); err != nil {
				return nil, fmt.Errorf("mos: 注册 shell 工具: %w", err)
			}
		}
		shellEnabled = true
	}

	// 守卫总线（单调裁决）与钩子总线（可改写环节）：两条轨道，互不绕过。
	guards := hook.NewGuardBus()
	hooks := hook.NewHookBus()

	// 名字级策略：既管"能不能调"，也管"模型看不看得见"（不给模型一个它
	// 注定调不成的工具）。
	a := &Agent{
		rt:           &driver.RunContext{},
		driver:       react.New(),
		tools:        reg,
		rules:        newRulesHolder(),
		compactor:    cfg.Compactor,
		truncator:    cfg.Truncator,
		spillDir:     cfg.SpillDir,
		workDir:      cfg.WorkDir,
		truncateMax:  cfg.TruncateMaxBytes,
		shellEnabled: shellEnabled,
		roots:        cfg.Roots,

		contextWired:   cfg.Context != nil,
		contextFiles:   contextFiles,
		contextBytes:   contextBytes,
		contextWarning: contextWarning,

		catalog: catalog,
	}
	a.SetPolicy(cfg.Policy) // 装初始策略（挂守卫 + 设可见性）
	a.rules.set(cfg.Rules)
	// 参数级规则的两个门：deny 档进守卫（单调拒绝），ask 档进 pre-execute
	// 钩子（转人工确认）。守卫与钩子都通过 holder 读**当前**规则——所以
	// 热替换规则时不用重挂钩子，审批钩子的"最前"位置也不会丢。
	guards.Add(func(ctx context.Context, ev *hook.HookEvent) error {
		g := a.rules.guard()
		if g == nil {
			return nil
		}
		return g(ctx, ev)
	})
	hooks.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ap := a.rules.approval()
		if ap == nil {
			return next(ctx, ev)
		}
		return ap(ctx, ev, next)
	})

	// 上下文压缩：宿主自备优先，否则按窗口参数建默认的（模型生成摘要）；
	// 显式关掉就不装（机制类能力的"显式关闭"）。
	compactor := cfg.Compactor
	if cfg.DisableCompaction {
		compactor = nil
	}
	if compactor == nil && !cfg.DisableCompaction {
		compactor = agent.NewLLMCompactor(model, agent.CompactorConfig{
			ContextWindow:    cfg.ContextWindow,
			ReserveTokens:    cfg.ReserveTokens,
			KeepRecentTokens: cfg.KeepRecentTokens,
		})
	}
	a.compactor = compactor

	// 工具结果截断：宿主自备优先，否则按上限建默认的（全文落盘可回读）。
	truncator := cfg.Truncator
	if truncator == nil && cfg.TruncateMaxBytes > 0 {
		dir := cfg.SpillDir
		if dir == "" {
			dir = filepath.Join(cfg.WorkDir, DefaultSpillDirName)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mos: 创建截断落盘目录 %q: %w", dir, err)
		}
		a.spillDir = dir
		truncator = agent.NewDefaultTruncator(agent.TruncatorConfig{
			MaxBytes: cfg.TruncateMaxBytes,
			Dir:      dir,
		})
	}
	a.truncator = truncator
	if truncator != nil {
		hooks.On(hook.HookToolPostExecute, agent.TruncateHook(truncator))
	}

	a.rt = &driver.RunContext{
		Planner:           planner.NewReAct(model),
		Tools:             reg,
		Hooks:             hooks,
		Guards:            guards,
		Concurrency:       cfg.Concurrency,
		MaxDecisionRounds: cfg.MaxDecisionRounds,
		SystemPrompt:      systemPrompt,
		Stream:            cfg.Stream,
		OnChunk:           cfg.OnChunk,
		Answerer:          cfg.Approval,
		Compactor:         compactor,
	}
	a.concurrency.Store(int64(cfg.Concurrency))
	a.maxRounds.Store(int64(cfg.MaxDecisionRounds))
	return a, nil
}

// Run 执行一次任务（驱动自行决定流式/非流式路径）。会话由调用方持有——
// 多会话与并行场景不被单会话 API 卡住。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
	if a == nil || a.driver == nil {
		return "", errors.New("mos: 智能体未装配")
	}
	if sess == nil {
		return "", errors.New("mos: 会话未设置")
	}
	// 每轮用运行上下文的**浅拷贝**：旋钮值写进副本，驱动只读副本——
	// 并行会话之间没有共享可变状态（钩子/守卫/注册表是共享的，那是有意）。
	rt := *a.rt
	rt.Concurrency = int(a.concurrency.Load())
	rt.MaxDecisionRounds = int(a.maxRounds.Load())
	return a.driver.Run(ctx, &rt, sess, input)
}

// RunContext 返回底层运行上下文的**模板**（只读用）。
//
// 模板的字段改动只在没有运行中的轮次时才有意义：每轮跑的是模板的浅拷贝。
func (a *Agent) RunContext() *driver.RunContext { return a.rt }

// SetModel 运行期换模型（**下一轮生效**，正在跑的那一轮不受影响——它已经
// 取过目标）。目录里没有这个 ID 直接报错（不静默无效）。
//
// 单模型模式没有目录可换 → 报错（宿主要用换模型，就该装目录）。
func (a *Agent) SetModel(id string) error {
	if a.catalog == nil {
		return errors.New("mos: 未装配模型目录（单模型模式不能换模型）")
	}
	return a.catalog.set(id)
}

// ModelID 返回当前模型 ID（单模型模式 = 空串）。
func (a *Agent) ModelID() string {
	if a.catalog == nil {
		return ""
	}
	return a.catalog.currentID()
}

// Models 返回模型目录（保序；单模型模式 = nil）。
func (a *Agent) Models() []ModelOption {
	if a.catalog == nil {
		return nil
	}
	return a.catalog.list()
}

// Compactor 返回上下文压缩器（nil = 没装这个能力）。
//
// 宿主要主动压缩（“立刻压一次”）时用它；框架不替宿主决定什么时候压。
func (a *Agent) Compactor() agent.Compactor { return a.compactor }

// SetConcurrency 设置并发上限（运行期旋钮）。**下一轮生效**，不打断当前轮。
func (a *Agent) SetConcurrency(n int) {
	if n <= 0 {
		n = DefaultConcurrency
	}
	a.concurrency.Store(int64(n))
}

// SetMaxDecisionRounds 设置单轮决策次数上限（运行期旋钮）。
// **下一轮生效**；n <= 0 = 不限。
func (a *Agent) SetMaxDecisionRounds(n int) { a.maxRounds.Store(int64(n)) }

// SetPolicy 热替换名字级策略：旧守卫撤下、新守卫挂上、工具可见性同步更新。
// **下一轮生效**（一轮里工具载荷已冻结、守卫已判过）。
//
// 宿主重载配置文件后调用它，实现"策略热更新"而不必重建智能体。
func (a *Agent) SetPolicy(p *policy.Config) {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	if a.removeGuard != nil {
		a.removeGuard()
		a.removeGuard = nil
	}
	if p == nil {
		a.policy = nil
		a.tools.SetVisibility(nil)
		return
	}
	pol := policy.New(*p)
	a.policy = pol
	a.removeGuard = a.rt.Guards.Add(pol.Check)
	a.tools.SetVisibility(pol.Visible)
}

// SetRules 热替换参数级规则（运行期旋钮）。**下一轮生效**。
//
// 规则是宿主的业务判断（什么算危险由宿主定），宿主重载配置后调用它即可。
func (a *Agent) SetRules(rules []policy.Check) { a.rules.set(rules) }

// AddRoot 运行期加根（读写都加）：智能体从"只有 A"变成"A + B"。
//
// 生效时机：边界是**每次调用现判**的 → 下一次工具调用即生效（同一轮内也
// 生效），不需要重建智能体，也不用等下一轮。
//
// 这是**宿主的权力**：模型不能自己扩边界（模型参数里的路径范围只能收窄）。
// 加根是安全事件——变更记在根集合的历史里（a.Roots().History()），宿主可
// 落盘审计。
//
// 作用域是 **agent 级**：该 Agent 的所有会话共享这份边界（与其它运行期旋钮
// 同理）。要按会话隔离，就给每个会话配一个 Agent。
//
// 未装配根集合（Config.Roots == nil）→ 报错，不静默无效。
func (a *Agent) AddRoot(dir string) error {
	rs, err := a.rootSet()
	if err != nil {
		return err
	}
	rs.Add(dir)
	return nil
}

// AddReadRoot 运行期加**读**根（不授予写权限）。
func (a *Agent) AddReadRoot(dir string) error {
	rs, err := a.rootSet()
	if err != nil {
		return err
	}
	rs.AddRead(dir)
	return nil
}

// AddWriteRoot 运行期加**写**根（不授予读权限）。
func (a *Agent) AddWriteRoot(dir string) error {
	rs, err := a.rootSet()
	if err != nil {
		return err
	}
	rs.AddWrite(dir)
	return nil
}

// RemoveRoot 运行期**移除**根（读写都移除）：收窄边界，随时可做。
func (a *Agent) RemoveRoot(dir string) error {
	rs, err := a.rootSet()
	if err != nil {
		return err
	}
	rs.Remove(dir)
	return nil
}

// Roots 返回底层的根集合（nil = 未装配这个能力）。
//
// 宿主用它把**同一份集合**接给文件工具（filesystem.NewSandboxedWithRoots）
// 与沙箱（sandbox.ScopeFunc）——否则两层边界会错位（工具能读、命令写不了）。
func (a *Agent) Roots() *filesystem.RootSet {
	if a == nil {
		return nil
	}
	return a.roots
}

// rootSet 取根集合；未装配时报错（fail-closed：能力缺失要说清楚，不静默无效）。
func (a *Agent) rootSet() (*filesystem.RootSet, error) {
	if a == nil || a.roots == nil {
		return nil, errors.New("mos: 未装配根集合（Config.Roots 为 nil）——运行期改边界不可用")
	}
	return a.roots, nil
}

// Describe 返回装配快照（多行文本）：装了什么、**没装什么**也写出来。
//
// 宿主直接打印它，不必各写一份横幅——框架说的是事实（"未装配" / "一律拒绝"
// 都明说），不会用沉默代替事实。
func (a *Agent) Describe() string {
	if a == nil {
		return "mos: 智能体未装配"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  已注册工具: %s\n", strings.Join(a.tools.Names(), ", "))
	fmt.Fprintf(&b, "  模型可见工具: %s\n", strings.Join(a.visibleNames(), ", "))
	fmt.Fprintf(&b, "  并发上限: %d（仅对声明可并发的工具生效）\n", a.concurrency.Load())
	if r := a.maxRounds.Load(); r > 0 {
		fmt.Fprintf(&b, "  决策轮次上限: %d\n", r)
	} else {
		b.WriteString("  决策轮次上限: 不限（上下文增长由压缩接管）\n")
	}
	fmt.Fprintf(&b, "  工作目录: %s\n", a.workDir)
	if a.catalog != nil {
		fmt.Fprintf(&b, "  模型: %s（目录 %d 个，可运行期切换）\n", a.catalog.currentID(), len(a.catalog.list()))
	} else {
		b.WriteString("  模型: 单模型（未装目录，运行期不能换）\n")
	}
	if a.roots != nil {
		snap := a.roots.Snapshot()
		fmt.Fprintf(&b, "  文件根（运行期可变）: 读 %d 个 / 写 %d 个\n", len(snap.ReadRoots), len(snap.WriteRoots))
	} else {
		b.WriteString("  文件根（运行期可变）: 未装配（运行期加根不可用）\n")
	}
	if a.shellEnabled {
		b.WriteString("  shell: 已装配（隔离执行 + 路径强制：写限定在工作区）\n")
	} else {
		b.WriteString("  shell: 未装配（模型看不到这个能力）\n")
	}
	if a.compactor != nil {
		b.WriteString("  上下文压缩: 已装配（超预算时模型生成结构化摘要落日志）\n")
	} else {
		b.WriteString("  上下文压缩: 未装配\n")
	}
	if a.truncator != nil {
		fmt.Fprintf(&b, "  结果截断: %d 字节（全文落 %s，read 可回读）\n", a.truncateMax, a.spillDir)
	} else {
		b.WriteString("  结果截断: 未开启\n")
	}
	if a.rules.count() > 0 {
		fmt.Fprintf(&b, "  参数级规则: %d 个检查器（deny→守卫 / ask→人工确认）\n", a.rules.count())
	} else {
		b.WriteString("  参数级规则: 无\n")
	}
	if a.policy != nil {
		b.WriteString("  名字级策略: 已装配\n")
	} else {
		b.WriteString("  名字级策略: 未装配（名字级名单不收紧）\n")
	}
	switch {
	case !a.contextWired:
		b.WriteString("  上下文文件: 未装配（宿主没要求）\n")
	case len(a.contextFiles) == 0:
		b.WriteString("  上下文文件: 已装配，但没找到文件（目录里没有 AGENTS.md / CLAUDE.md 等）\n")
	default:
		fmt.Fprintf(&b, "  上下文文件: %d 个（%d 字节）: %s\n",
			len(a.contextFiles), a.contextBytes, strings.Join(a.contextFiles, ", "))
		for _, w := range a.contextWarning {
			fmt.Fprintf(&b, "    ⚠ %s\n", w)
		}
	}
	return b.String()
}

// visibleNames 返回模型可见的工具名（受可见性过滤）。
func (a *Agent) visibleNames() []string {
	out := make([]string, 0, len(a.tools.Names()))
	for _, t := range a.tools.List() {
		out = append(out, t.Name)
	}
	return out
}

// rulesHolder 持有当前参数级规则的两个门（守卫 + 审批钩子）。
//
// 用 holder 而不是把两个闭包直接挂进总线：热替换规则时不用重挂钩子——
// 审批钩子必须待在 pre-execute 的**最前**（ask 决策要短路后面的钩子），
// 重挂会把它挪到链尾。
type rulesHolder struct {
	mu       sync.RWMutex
	guardFn  hook.Guard
	approvFn hook.Hook
	n        int
}

func newRulesHolder() *rulesHolder { return &rulesHolder{} }

// set 换掉当前规则（空规则 = 无门）。
func (h *rulesHolder) set(rules []policy.Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(rules) == 0 {
		h.guardFn, h.approvFn, h.n = nil, nil, 0
		return
	}
	pp := policy.NewParamPolicy("参数级规则", rules...)
	h.guardFn, h.approvFn, h.n = pp.Guard(), pp.Approval(), len(rules)
}

func (h *rulesHolder) guard() hook.Guard {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.guardFn
}

func (h *rulesHolder) approval() hook.Hook {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.approvFn
}

func (h *rulesHolder) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.n
}
