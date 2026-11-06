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
	"time"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/contextfile"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/agent/skill"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
	"github.com/puras/mos/tool/mcp"
	"github.com/puras/mos/tool/searchtools"
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
	// Skills 是技能能力配置（nil = 不提供该能力）。
	//
	// 技能 = 目录里的 SKILL.md + frontmatter：清单（name / description）进
	// 系统提示词（装配期渲染一次 → 静态，保 prompt cache），正文由 skill
	// 工具按需加载。技能集**装配期固定**——不做运行期热加载。
	//
	// 给了目录但没装出技能（目录不存在 / 全被跳过）不算装配失败：降级说明
	// 进 Describe() 快照，不静默丢。
	Skills *SkillConfig
	// MCP 是 MCP（Model Context Protocol）服务器配置（nil = 不提供这个能力）。
	//
	// **一开即装**：装配期连上服务器、发现工具、按 exposure 注册进工具注册表。
	// 单台服务器连不上不影响其余（失败隔离），降级说明进 Describe() 快照。
	//
	// MCP 工具默认 deferred（模型经 tool_search 搜到才进清单）——服务器工具面
	// 通常很大，全量声明会撑爆 prompt（FR-C3）。
	//
	// 连接是**长连接**（stdio 是子进程）：宿主用完请调 a.Close() 收掉。
	MCP *MCPConfig
	// WorkDir 是工作目录：文件工具的沙箱根、截断落盘目录的父目录。空 = 当前目录。
	WorkDir string
	// ContextWindow 是模型上下文窗口（token）。0 = 默认 128K；确切窗口更小的
	// 模型必须显式配置，否则会在溢出后才触发压缩。
	ContextWindow int
	// ReserveTokens 是为模型响应预留的空间。0 = 默认 16K。
	ReserveTokens int
	// KeepRecentTokens 是压缩后保留的近期上下文预算。0 = 默认 20K。
	KeepRecentTokens int
	// SummaryTimeout 是**压缩摘要调用**自己的超时。0 = 默认 3m；
	// **负数 = 不限**（宿主自担挂住的风险）。
	//
	// 摘要是整轮里的阻塞点：宿主的轮次 ctx 往往只有 cancel 没有 deadline，
	// 网关半开时若不设超时，这一轮会永远挂着（宿主拿不到 turn/end）。
	// 超时后这一轮按失败收尾，错误文案写明「摘要调用超时」，宿主一眼能
	// 分辨是上游挂了还是用户点了停止。
	//
	// 只影响摘要这一次调用：主对话的流式回答与宿主显式配置的 http_timeout
	// 都不受它影响（宿主自备 Compactor 时本项忽略）。
	SummaryTimeout time.Duration
	// CompactionFailurePolicy 决定"压缩失败"时本轮怎么办。
	// 零值 = driver.CompactionFailClosed（终止本轮，与默认行为一致）。
	//
	// 选 driver.CompactionSkipAndContinue：摘要失败 → 记一条提示、跳过压缩、
	// 本轮继续（上下文可能已超预算，溢出风险由宿主自担）。
	CompactionFailurePolicy driver.CompactionFailurePolicy
	// OnCompactionFailure 是"压缩失败但本轮继续"时的宿主通知（可选，
	// nil = 只记会话日志）。参数是压缩失败的原因，宿主据此在界面上提示。
	OnCompactionFailure func(error)
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
	// searchWired 标记装配时注册了 tool_search（deferred 集非空；Describe 快照用）。
	searchWired bool
	// roots 是运行期可变的根集合（宿主给的活边界；nil = 没有这个能力）。
	roots *filesystem.RootSet

	// 上下文文件（装配快照用；contextWired = 宿主有没有要求装这个能力）。
	contextWired   bool
	contextFiles   []string
	contextBytes   int
	contextWarning []string

	// skills 是装配期发现的技能清单（nil = 没装技能这个能力）。
	skills *skill.Catalog

	// mcp 是 MCP 编排器（nil = 没装 MCP 这个能力）。
	mcp *mcp.Manager
	// mcpInstructions 是各服务器的说明（连接建立时注入一次）。
	mcpInstructions []mcp.Instruction
	// mcpInstructionMax 是说明注入的字节上限（0 = 默认）。
	mcpInstructionMax int

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

// SkillConfig 是技能装配参数。
//
// 零值可用：Dirs 空 = 没有技能目录（不装技能）；Reader 空 = 读本地文件。
type SkillConfig struct {
	// Dirs 是技能目录列表（按优先级从高到低）。
	//
	// 顺序即优先级：同名技能**先到先得**（后来的被遮蔽并出诊断，PI 同款）。
	// 每个目录下：目录自己含 SKILL.md 就是技能根，否则它的每个直接子目录
	// （含 SKILL.md）是一个技能。
	//
	// 路径**原样**传给 Reader：注入自定义 Reader 时，路径是宿主命名空间里的
	// 键（未必是文件系统路径），框架不擅自展开 "~"。
	Dirs []string
	// Reader 是读技能文件与列目录的能力（nil = 读本地文件）。
	//
	// 框架不碰文件系统：宿主要 jail 语义（只许读声明过的目录）就注入自己的
	// Reader。**技能目录列表本身就是宿主声明的授权范围**，框架不另行校验根
	// 集合——框架看不到注入的 Reader 内部边界，越界判断只能是宿主的事。
	Reader skill.Reader
	// MaxBodyBytes 是单个技能正文的字节上限（≤0 = 64KB）。超限的技能跳过
	// 并出诊断（截断的指令是坏指令）。
	MaxBodyBytes int
	// MaxCatalogBytes 是清单渲染的字节上限（≤0 = 8KB）。超预算时先丢描述、
	// 再丢条目，丢了多少如实写进末尾提示。
	MaxCatalogBytes int
}

// MCPConfig 是 MCP（Model Context Protocol）装配参数。
//
// 零值可用：Servers 空 = 不连任何服务器；超时与说明上限取默认值。
type MCPConfig struct {
	// Servers 是要连接的服务器列表（stdio / streamable-http）。
	Servers []mcp.ServerConfig
	// DefaultExposure 是服务器未单独指定时的默认暴露档（空 = deferred）。
	//
	// 三档：deferred（默认，搜到才进清单）/ direct（直接进清单）/
	// hidden（模型看不见也不搜得到，仍可被程序化调用）。
	DefaultExposure mcp.Exposure
	// ConnectTimeout 是单台服务器的连接超时（≤0 = 15s）。
	ConnectTimeout time.Duration
	// CallTimeout 是单次工具调用超时（≤0 = 60s）。
	CallTimeout time.Duration
	// MaxInstructionBytes 是服务器说明注入的字节上限（≤0 = 32KB）。
	MaxInstructionBytes int
	// Dial 是连接服务器的工厂（nil = 用 tool/mcp 的默认实现）。
	//
	// 宿主可以换掉连接方式（自己的网关代理、注入凭据）；测试用替身顶掉真连接。
	Dial func(ctx context.Context, cfg mcp.ServerConfig, opts mcp.Options) (mcp.Client, error)
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
	// 技能（Skills）：清单进系统提示词（装配期渲染一次 → 静态，保 prompt
	// cache），正文由 skill 工具按需加载。
	//
	// 装配期就把技能读进来（而不是调用时再读）：技能集是静态的，坏掉的技能
	// 文件应该在装配期就暴露成诊断，而不是跑到一半才失败。
	var skillCatalog *skill.Catalog
	if cfg.Skills != nil {
		skillCatalog = skill.Load(skill.Options{
			Dirs:            cfg.Skills.Dirs,
			Reader:          cfg.Skills.Reader,
			MaxBodyBytes:    cfg.Skills.MaxBodyBytes,
			MaxCatalogBytes: cfg.Skills.MaxCatalogBytes,
		})
		if text := skillCatalog.Render(); text != "" {
			if pb != nil {
				if err := pb.AddSkills(text); err != nil {
					return nil, fmt.Errorf("mos: 装配技能清单: %w", err)
				}
			} else {
				if systemPrompt != "" {
					systemPrompt += "\n\n"
				}
				systemPrompt += text
			}
		}
	}
	// 提示词渲染**故意挪后**：MCP 服务器摘要节要进系统提示词，而 MCP 装配
	// 在注册表之后（工具注册序不能变——变了工具清单顺序就变，prompt cache
	// 与模型行为都跟着抖）。所以：先装 MCP，最后统一渲染。
	//
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

	// MCP：一开即装（连服务器 → 发现工具 → 按 exposure 注册）。
	//
	// 位置：在 tool_search 之前——MCP 工具默认 deferred，注册完才知道 deferred
	// 集非空（tool_search 是「有 deferred 才有」的能力即证据）。
	var mcpMgr *mcp.Manager
	var mcpInstructions []mcp.Instruction
	mcpInstructionMax := 0
	if cfg.MCP != nil && len(cfg.MCP.Servers) > 0 {
		servers := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers))
		for _, s := range cfg.MCP.Servers {
			if s.Exposure == "" && cfg.MCP.DefaultExposure != "" {
				s.Exposure = cfg.MCP.DefaultExposure
			}
			servers = append(servers, s)
		}
		mcpMgr = mcp.NewManager(mcp.Options{
			Servers:        servers,
			ClientName:     "mos",
			ClientVersion:  Version,
			ConnectTimeout: cfg.MCP.ConnectTimeout,
			CallTimeout:    cfg.MCP.CallTimeout,
			Dial:           cfg.MCP.Dial,
		}, reg)
		// 装配期连接：失败隔离在 Manager 里（单台连不上不影响其余）。
		mcpMgr.Start(context.Background()) // 并发连接，返回即全部就绪
		mcpInstructions = mcpMgr.Instructions()
		mcpInstructionMax = cfg.MCP.MaxInstructionBytes
		// hidden 档：可见性层（与 policy 那层 AND 叠加——谁也不覆盖谁）。
		reg.SetVisibilitySlot(mcp.HiddenSlot, mcpMgr.HiddenFilter())
		// 服务器摘要节：模型看不见的服务器（deferred 工具）得让它知道存在。
		//
		// 进**静态**系统提示词：摘要取宿主配置的 Description（静态字符串），
		// 不取连接的 instructions（那个走 user 侧一次性注入，会变）——静态
		// 内容进静态提示词不破 prompt cache。
		if text := mcp.RenderServersSection(mcpMgr.Summaries(), 0); text != "" {
			if pb != nil {
				if err := pb.AddMCPServers(text); err != nil {
					return nil, fmt.Errorf("mos: 装配 MCP 服务器摘要: %w", err)
				}
			} else {
				if systemPrompt != "" {
					systemPrompt += "\n\n"
				}
				systemPrompt += text
			}
		}
		// 资源工具（S8.4-E）：任一台可见服务器有资源才注册（工具面恒为 0 或 3）。
		if err := mcpMgr.RegisterResourceTools(); err != nil {
			return nil, fmt.Errorf("mos: 注册 MCP 资源工具: %w", err)
		}
	}

	// tool_search：deferred 集非空才注册（能力即证据——没有懒加载工具就不给
	// 发现工具，小工具集行为与从前完全一致）。机制在框架、策略在宿主：
	// 要不要 deferred 是宿主的装配决定（S7.3，docs/ARCHITECTURE.md §3.15）。
	searchWired := false
	if len(reg.DeferredTools()) > 0 {
		if _, ok := reg.Get("tool_search"); !ok {
			if err := reg.Register(searchtools.NewToolSearch(searchtools.Options{Registry: reg})); err != nil {
				return nil, fmt.Errorf("mos: 注册 tool_search 工具: %w", err)
			}
		}
		searchWired = true
	}

	// skill 工具：只有存在**模型可见**技能时才注册（能力即证据——没有可
	// 加载的技能就不给这个工具；仅用户可调用的技能由宿主的展开器处理）。
	if skillCatalog != nil && len(skillCatalog.ModelVisible()) > 0 {
		if _, ok := reg.Get(skill.ToolName); !ok {
			if err := reg.Register(skill.NewTool(skillCatalog)); err != nil {
				return nil, fmt.Errorf("mos: 注册 skill 工具: %w", err)
			}
		}
	}
	// allowed-tools 求交诊断的探针（S8.4-D）：注册表已就位（可见性层会在
	// 装配后继续叠加——探针每次激活时现查，自然跟上 policy / MCP 换代）。
	if skillCatalog != nil {
		skillCatalog.SetToolProbe(reg.Visible)
	}

	// 分节装配器到此才渲染：所有要进提示词的分节（项目约定、技能清单、
	// MCP 服务器摘要）都已就位。
	if pb != nil {
		systemPrompt = pb.Render()
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
		searchWired:  searchWired,
		roots:        cfg.Roots,

		contextWired:   cfg.Context != nil,
		contextFiles:   contextFiles,
		contextBytes:   contextBytes,
		contextWarning: contextWarning,

		skills: skillCatalog,

		mcp:               mcpMgr,
		mcpInstructions:   mcpInstructions,
		mcpInstructionMax: mcpInstructionMax,

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
			SummaryTimeout:   cfg.SummaryTimeout,
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
		Planner:                 planner.NewReAct(model),
		Tools:                   reg,
		Hooks:                   hooks,
		Guards:                  guards,
		Concurrency:             cfg.Concurrency,
		MaxDecisionRounds:       cfg.MaxDecisionRounds,
		SystemPrompt:            systemPrompt,
		Stream:                  cfg.Stream,
		OnChunk:                 cfg.OnChunk,
		Answerer:                cfg.Approval,
		Compactor:               compactor,
		CompactionFailurePolicy: cfg.CompactionFailurePolicy,
		OnCompactionFailure:     cfg.OnCompactionFailure,
	}
	a.concurrency.Store(int64(cfg.Concurrency))
	a.maxRounds.Store(int64(cfg.MaxDecisionRounds))
	return a, nil
}

// Run 执行一次任务（驱动自行决定流式/非流式路径）。会话由调用方持有——
// 多会话与并行场景不被单会话 API 卡住。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
	return a.run(ctx, sess, input)
}

// RunWithCause 与 Run 相同，但多带一个**取消原因**：
//
//	ctx 被取消、且没有更具体的原因（宿主没用 context.WithCancelCause）时，
//	用 cause 解释——turn/end 的错误详情与返回错误都会写上它，
//	宿主一眼能分清"用户点了停止"与"超时中止"。
//
// 典型用法（宿主有"停止"按钮）：
//
//	a.RunWithCause(ctx, sess, input, errors.New("用户点了停止"))
//
// 等价写法（宿主自己管 cancel 函数时更直接）：
//
//	ctx, cancel := context.WithCancelCause(parent)
//	stop → cancel(errors.New("用户点了停止"))
//	a.Run(ctx, sess, input)
func (a *Agent) RunWithCause(ctx context.Context, sess *session.Session, input string, cause error) (string, error) {
	return a.run(driver.WithCancelCause(ctx, cause), sess, input)
}

// run 是 Run / RunWithCause 的共同实现。
func (a *Agent) run(ctx context.Context, sess *session.Session, input string) (string, error) {
	if a == nil || a.driver == nil {
		return "", errors.New("mos: 智能体未装配")
	}
	if sess == nil {
		return "", errors.New("mos: 会话未设置")
	}
	// 技能：每轮一份激活状态（ctx 携带）+ 事件记录器（接会话）。
	//
	// 状态用**指针**语义：宿主若自己注入了一份，Run 结束后能读到这轮加载过
	// 哪些技能；没注入就由框架建一份（用完即弃，不跨轮共享）。
	if a.skills != nil {
		if skill.StateFrom(ctx) == nil {
			ctx = skill.WithState(ctx, skill.NewState())
		}
		ctx = skill.WithRecorder(ctx, skillRecorder{sess: sess})
	}
	// 服务器说明：一次性注入（每个会话一次）。
	//
	// 走 **user 侧**而不是系统提示词：系统提示词是装配期渲染一次的静态内容
	//（保 prompt cache），而 MCP 连接可能掉线重连、说明可能变——写进静态
	// 提示词会失真。注入一次后不再重复（避免刷屏）。
	if len(a.mcpInstructions) > 0 && sess.Claim(mcpInstructionClaim) {
		text := renderMCPInstructions(a.mcpInstructions, a.mcpInstructionMax)
		if text != "" {
			if _, err := sess.Append(session.Event{Kind: session.EventUser, Text: text}); err != nil {
				return "", err
			}
		}
	}
	// 每轮用运行上下文的**浅拷贝**：旋钮值写进副本，驱动只读副本——
	// 并行会话之间没有共享可变状态（钩子/守卫/注册表是共享的，那是有意）。
	rt := *a.rt
	rt.Concurrency = int(a.concurrency.Load())
	rt.MaxDecisionRounds = int(a.maxRounds.Load())
	return a.driver.Run(ctx, &rt, sess, input)
}

// mcpInstructionClaim 是「本会话已注入过服务器说明」的标记（会话级一次性）。
const mcpInstructionClaim = "mcp/instructions"

// renderMCPInstructions 渲染服务器说明（一次性注入给模型）。
//
// 格式带服务器归属（DSH 语义）：模型看得懂这段话是谁说的。
// 超预算时按服务器顺序**整段**截断（半句话比没有更坏——宁可少说一台）。
func renderMCPInstructions(list []mcp.Instruction, maxBytes int) string {
	if len(list) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = mcp.DefaultMaxInstructionBytes
	}
	var b strings.Builder
	b.WriteString("[MCP 服务器说明]\n")
	b.WriteString("以下说明来自已连接的 MCP 服务器（连接建立时注入一次）：\n")
	for _, ins := range list {
		part := fmt.Sprintf("- %s: %s\n", ins.Server, ins.Text)
		if b.Len()+len(part) > maxBytes {
			b.WriteString("- （其余服务器的说明超出预算，已省略）\n")
			break
		}
		b.WriteString(part)
	}
	return b.String()
}

// ExpandSkill 展开用户文本里的技能手势（/name）。
//
// 框架**不自动扫**用户文本（误触发风险，且 SDK 没有用户命令面）：宿主拿到
// 用户输入时自己调一次即可。命中就把手势换成技能正文块，其后的文字作为参数
// 追加在正文之后（PI 的 args 追加语义）。
//
// 没装技能、或不是已知技能 → 原样返回 + false（不是错误）。
//
// 手势命中但正文读不出来 → 返回错误（不拿半截正文糊弄用户）。
//
// 仅用户可调用的技能（disable-model-invocation）在这里**能**展开——用户可以，
// 模型不行。
func (a *Agent) ExpandSkill(text string) (string, bool, error) {
	if a == nil || a.skills == nil {
		return text, false, nil
	}
	return a.skills.ExpandInvocation(text)
}

// ReloadSkills 重扫技能目录（显式、幂等、全量）。
//
// 什么时候用：宿主改了技能目录（新增 / 删除 / 改名 / 改 frontmatter）之后，
// 自己调一次。想自动盯文件，宿主自己盯（框架不提供 watcher 接口——
// 治理规则 5，框架不绑文件系统）。
//
// **幂等**：目录摘要没变就不换快照（Changed=false、Notice 空）——可以无脑调。
//
// **正文不受影响**：正文在调用时现读，改了 SKILL.md 不必 reload。
//
// **静态系统提示词不重渲染**（保 prompt cache）：清单变化只走 Notice
// ——宿主把它作为 user 侧消息追加到会话里（DSH 的「替换目录」语义）。
func (a *Agent) ReloadSkills() skill.ReloadResult {
	if a == nil || a.skills == nil {
		return skill.ReloadResult{}
	}
	return a.skills.Reload()
}

// Skills 返回装配期发现的技能清单（含仅用户可调用的那些——宿主展示什么由
// 宿主决定）。
func (a *Agent) Skills() []skill.Skill {
	if a == nil || a.skills == nil {
		return nil
	}
	return a.skills.All()
}

// skillRecorder 把技能事件写进会话（装配层接线）。
//
// 只记元数据（名字 / 描述 / 目录），不记正文——正文已经在 tool/result 里。
type skillRecorder struct {
	sess *session.Session
}

// RecordActivate 实现 skill.Recorder：记一条 skill/activate 事件。
//
// 会话写失败即报错：技能正文会改变模型行为，没有留痕的加载等于没发生过的
// 加载（审计链断在这里，事后查不到这轮加载了什么）。
func (r skillRecorder) RecordActivate(name, description, dir string) error {
	if r.sess == nil {
		return nil
	}
	_, err := r.sess.Append(session.Event{
		Kind: session.EventSkillActivate,
		Skill: &session.SkillEvent{
			Name:        name,
			Description: description,
			Dir:         dir,
		},
	})
	return err
}

// RecordAllowedToolsMismatch 实现 skill.Recorder：记一条 allowed-tools 求交
// 诊断（S8.4-D）。
//
// 这是**诊断**不是激活：写失败不拦加载（工具照样能调/不能调由策略定，
// 缺了这条诊断只损失可观测性，不损失安全性）——与 RecordActivate 的
// fail-fast 不同。
func (r skillRecorder) RecordAllowedToolsMismatch(name string, missing []string) error {
	if r.sess == nil {
		return nil
	}
	_, err := r.sess.Append(session.Event{
		Kind: session.EventSkillToolMismatch,
		Skill: &session.SkillEvent{
			Name:           name,
			MissingAllowed: append([]string(nil), missing...),
		},
	})
	return err
}

// Close 释放装配期建立的长连接（目前是 MCP 服务器连接：stdio 子进程等）。
//
// 宿主用完智能体应调它——不调的话 stdio 子进程会一直活着。
// 没装 MCP 时是空操作。
func (a *Agent) Close() error {
	if a == nil || a.mcp == nil {
		return nil
	}
	return a.mcp.Close()
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

// policySlot 是名字级策略在可见性过滤器栈里的槽名。
//
// 用具名槽而不是整栈替换：policy 热替换只动自己那层，MCP 的 hidden 层
// 不受影响（整栈替换会把对方的层一起清掉）。
const policySlot = "policy"

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
		a.tools.SetVisibilitySlot(policySlot, nil)
		return
	}
	pol := policy.New(*p)
	a.policy = pol
	a.removeGuard = a.rt.Guards.Add(pol.Check)
	a.tools.SetVisibilitySlot(policySlot, pol.Visible)
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
	fmt.Fprintf(&b, "  MoS 版本: %s\n", Version)
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
	switch {
	case !a.searchWired:
		b.WriteString("  tool_search: 未装配（没有 deferred 工具）\n")
	default:
		b.WriteString("  tool_search: 已装配（deferred 工具经搜索激活，下一轮生效）\n")
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
	switch {
	case a.skills == nil:
		b.WriteString("  技能: 未装配\n")
	case a.skills.Len() == 0:
		b.WriteString("  技能: 已装配，但没找到技能\n")
	default:
		fmt.Fprintf(&b, "  技能: %d 个（模型可见 %d 个）: %s\n",
			a.skills.Len(), len(a.skills.ModelVisible()), strings.Join(a.skills.ModelVisibleNames(), ", "))
	}
	// 降级诊断照实写（技能没装上也是一条诊断）。
	if a.skills != nil {
		for _, w := range a.skills.Warnings() {
			fmt.Fprintf(&b, "    ⚠ %s\n", w)
		}
		// allowed-tools 求交诊断（S8.4-D）：声明了该字段的技能 × 当前可见集，
		// 缺什么如实写（激活时也会记 skill/allowed-tools-mismatch 事件）。
		for _, s := range a.skills.All() {
			if len(s.AllowedTools) == 0 {
				continue
			}
			missing, notice := a.skills.CheckAllowedTools(s)
			if len(missing) == 0 {
				continue
			}
			fmt.Fprintf(&b, "    ⚠ 技能 %q: %s\n", s.Name, notice)
		}
	}
	// MCP：装了什么 + 没装什么（连不上的服务器、换代失败都要写出来）。
	switch {
	case a.mcp == nil:
		b.WriteString("  MCP 服务器: 未装配\n")
	default:
		deferred, direct, hidden := a.mcp.ToolCount()
		fmt.Fprintf(&b, "  MCP 服务器: %d 个（工具 %d 个：deferred %d / direct %d / hidden %d）\n",
			len(a.mcp.Servers()), deferred+direct+hidden, deferred, direct, hidden)
		for _, s := range a.mcp.Servers() {
			fmt.Fprintf(&b, "    - %s: 协议 %s，工具 %d 个\n",
				s.Name(), s.ProtocolVersion(), len(s.Tools()))
		}
		for _, name := range a.mcp.Skipped() {
			fmt.Fprintf(&b, "    - %s: 未连接（配置为禁用）\n", name)
		}
		for _, w := range a.mcp.Warnings() {
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
