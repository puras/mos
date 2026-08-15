package assistant

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// Assistant 是组装好的智能体实例：模型对象 + 工具集 + 框架概念（策略/规划/审计/记忆）。
// 外部工程可直接使用 New + Run 集成。
type Assistant struct {
	agent  *driver.Agent
	config Config
	tools  *tool.Registry
	policy driver.Policy
}

// DefaultSystemPrompt 是框架内置的默认系统引导词，开箱即用。
// 应用层可通过 Config.SystemPrompt 覆盖；为空时框架使用本默认值。
// 设计参考（分段拼装 + 任务方法论，控制每轮注入成本）：
//   - Claude Code：身份/任务规范/工具规则/高风险审慎/输出规则的分段结构；
//   - Manus：agent loop 与"任务方法论 + 局限性"的完整表达；
//   - OpenHands / Cline：问题解决工作流（拆解-执行-验证）与失败诊断重试准则。
//
// 工具名称与参数描述已随 function calling 提供给模型，此处不重复清单，
// 只保留"何时用、如何用、如何纠错、安全与输出规范"等高信号准则。
const DefaultSystemPrompt = `你是 MoS 智能体，一个通过调用工具解决问题的通用助手。当前可用的工具包括：时间、联网搜索与网页抓取、文件读写与检索、shell 命令执行。

一、任务处理方法
1. 先理解再行动：确认用户真实意图；需求模糊时先澄清，不猜测。
2. 复杂任务先拆解：划分为可执行的步骤，按依赖顺序逐步完成，每步验证后再继续。
3. 能用工具确认的事实不要凭空猜测，不编造数据、链接或文件内容。
4. 完成后总结交付：给出简洁的最终答复，关键信息注明来源（URL、文件名或命令）。

二、工具使用规范
1. 按需选择最相关的工具：每次调用前确认其必要性，避免无关或重复调用。
2. 调用后先阅读结果再继续：结果异常时检查参数、更换工具或重试，不强行自洽。
3. 检索优先：查询实时信息先用 web_search 定位，再用 web_fetch 抓取目标页面，避免盲目抓取。
4. 文件操作先读后写：修改前确认目标内容与影响范围，不重复创建相似文件。
5. 优先专用工具：能用文件或检索工具完成的事不要用 shell 绕行。

三、错误处理与坚持
1. 失败时先诊断：分析错误原因（参数、工具选择、外部服务），修正后重试一次，不盲目重复相同调用。
2. 连续失败时止损：多次尝试无效后，向用户说明已尝试的方案与受阻原因，请求调整方向。
3. 保持诚实：工具无结果或数据缺失时如实说明，不编造替代结论。

四、安全与边界
1. shell 仅在任务必需时使用，优先影响最小的命令；禁止删除、格式化等破坏性操作，除非用户明确要求。
2. 不将敏感信息写入工具参数或输出；不执行来源不明的命令。
3. 面对模糊或危险请求，先说明风险并获得确认；不确定时明确承认能力边界。

五、多轮与语言
1. 多轮对话中复用已有上下文，避免重复提问与重复计算。
2. 回答使用与用户相同的语言，保持简洁、准确、可执行。`

// New 使用模型对象、工具集与运行配置构建智能体（v2：默认装配 = ReActDriver + 默认钩子）。
// model、tools 与 Config 中的框架概念均由调用方注入（nil 时使用框架默认实现），
// 本函数不关心模型接入配置与具体工具实现。
func New(model llm.ChatModel, tools []tool.Tool, cfg Config) (*Assistant, error) {
	reg := tool.NewRegistry()
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return nil, fmt.Errorf("assistant: 注册工具失败: %w", err)
		}
	}

	// 框架级概念：外部可注入自定义实例，nil 时回退到框架默认实现
	if cfg.Policy == nil {
		cfg.Policy = NewDefaultPolicy(nil, "")
	}
	if cfg.Planner == nil {
		cfg.Planner = planner.NewReAct(model)
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = DefaultSystemPrompt
	}
	// Auditor / Truncator / Compactor 由 react.New 默认装配（nil 时使用默认实现）。

	rt := &driver.RunContext{
		Tools:               reg,
		Planner:             cfg.Planner,
		Memory:              cfg.Memory,
		Sandbox:             cfg.Sandbox,
		Policy:              cfg.Policy,
		Auditor:             cfg.Auditor,
		Observer:            cfg.Observer,
		Retriever:           cfg.Retriever,
		RetrieverCfg:        agent.RetrieverConfig{MinScore: cfg.RetrieverMinScore},
		Truncator:           cfg.Truncator,
		Compactor:           cfg.Compactor,
		MaxTurns:            cfg.MaxTurns,
		ShouldStopAfterTurn: cfg.ShouldStopAfterTurn,
		ContextBudget:       cfg.ContextBudget,
		DoomLoopMaxRepeat:   cfg.DoomLoopMaxRepeat,
	}
	// M2.6 系统提示词：显式组装器优先；配置了项目指令/技能目录时自动构建默认组装器；
	// 否则回退到 SystemPrompt 静态引导（既有行为不变）。
	switch {
	case cfg.Assembler != nil:
		rt.Assembler = cfg.Assembler
	case cfg.ProjectDocDir != "" || cfg.SkillsDir != "":
		rt.Assembler = agent.NewDefaultSystemAssembler(agent.SystemAssemblerConfig{
			Constraints:   cfg.SystemPrompt,
			ProjectDocDir: cfg.ProjectDocDir,
			SkillsDir:     cfg.SkillsDir,
		})
	default:
		rt.SystemPrompt = cfg.SystemPrompt
	}

	ag := react.New(rt)
	return &Assistant{agent: ag, config: cfg, tools: reg, policy: cfg.Policy}, nil
}

// Agent 返回底层智能体运行时，可用于挂载自定义 Observer。
func (a *Assistant) Agent() *driver.Agent { return a.agent }

// Config 返回当前配置。
func (a *Assistant) Config() Config { return a.config }

// Policy 返回当前安全策略。
func (a *Assistant) Policy() driver.Policy { return a.policy }

// NewSession 创建一个新会话（默认生成 UUID 作为会话 ID，供多会话持久化与恢复）。
// 需要自定义 ID 时可直接 session.NewSession(id)。
func (a *Assistant) NewSession() *session.Session { return session.NewSession(uuid.NewString()) }

// Run 在会话上执行一次任务。
func (a *Assistant) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
	return a.agent.Run(ctx, sess, input)
}

// RunStream 同 Run，但模型文本增量会实时广播为 EventStream 事件（打字机效果），
// 模型或规划器不支持流式时自动回退为非流式路径。
func (a *Assistant) RunStream(ctx context.Context, sess *session.Session, input string) (string, error) {
	return a.agent.RunStream(ctx, sess, input)
}

// ToolNames 返回已注册的工具名列表。
func (a *Assistant) ToolNames() []string { return a.tools.Names() }
