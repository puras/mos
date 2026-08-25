package assistant

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/planexecute"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/driver/reflection"
	"hegq.net/mos/agent/driver/scripted"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/handle"
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

	model       llm.ChatModel // 供新会话重建 RunContext（Handle.Create / Registry）
	persistence session.Persistence
	checkpoints checkpoint.Store
	bus         *event.EventBus
	registry    *handle.Registry // 会话句柄注册表（Handle 入口）
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
	// 同步确认器经 ConfirmerAdapter 桥接为异步 Approval（保留 CLI y/N 体验）。
	if cfg.Approval == nil && cfg.Confirm != nil {
		cfg.Approval = approval.NewConfirmerAdapter(confirmBridge{inner: cfg.Confirm})
	}
	// 配置了审批通道时自动注册 request_human_input（人工介入工具，M2.8）：
	// 模型可主动请求人类输入/确认/授权（挂起 → 平台作答 → Resume 续跑）。
	if cfg.Approval != nil {
		if err := reg.Register(RequestHumanInput{}); err != nil {
			return nil, fmt.Errorf("assistant: 注册 request_human_input 失败: %w", err)
		}
	}
	// M2.6 Skills 强化：SkillsDir 非空时自动注册 activate_skill（Task 6 已实现）。
	// DisableActivateSkill 为 true 时跳过（应用层自管工具集）。
	if cfg.SkillsDir != "" && !cfg.DisableActivateSkill {
		skills, _ := agent.LoadSkills(cfg.SkillsDir)
		if err := reg.Register(ActivateSkill{Skills: skills}); err != nil {
			return nil, fmt.Errorf("assistant: 注册 activate_skill 失败: %w", err)
		}
	}
	// 策略侧 Approval 与驱动侧一致（检查器据此返回 *approval.RequiredError）。
	if p, ok := cfg.Policy.(*DefaultPolicy); ok {
		p.Approval = cfg.Approval
	}
	// 修复 RunContext.Bus 始终为 nil 的旧缺陷：未显式注入时创建默认 EventBus。
	if cfg.Bus == nil {
		cfg.Bus = event.NewEventBus()
	}
	// Auditor / Truncator / Compactor 由 react.New 默认装配（nil 时使用默认实现）。

	rt := buildRunContext(model, reg, cfg)
	ag, err := assembleDriver(cfg, rt)
	if err != nil {
		return nil, err
	}
	a := &Assistant{
		agent:       ag,
		config:      cfg,
		tools:       reg,
		policy:      cfg.Policy,
		model:       model,
		persistence: cfg.Persistence,
		checkpoints: cfg.CheckpointStore,
		bus:         cfg.Bus,
	}
	a.registry = handle.NewRegistry(a.newAgentForSession, cfg.CheckpointStore, cfg.Bus)
	return a, nil
}

// buildRunContext 从配置装配共享 RunContext（供 New 与会话句柄重建复用）。
func buildRunContext(model llm.ChatModel, reg *tool.Registry, cfg Config) *driver.RunContext {
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
		PlanPlanner:         cfg.PlanPlanner,
		Reflector:           cfg.Reflector,
		ScriptPlanner:       cfg.ScriptPlanner,
		Approval:            cfg.Approval,
		Checkpoints:         cfg.CheckpointStore,
		Bus:                 cfg.Bus,
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
	return rt
}

// assembleDriver 按配置装配驱动（架构 v2 §3.5 按名选组件）：
// DriverFactory 优先 → Driver 名内置三选一 → 默认 react。
func assembleDriver(cfg Config, rt *driver.RunContext) (*driver.Agent, error) {
	if cfg.DriverFactory != nil {
		return cfg.DriverFactory(rt), nil
	}
	switch cfg.Driver {
	case "", "react":
		return react.New(rt), nil
	case "plan-execute":
		return planexecute.New(rt), nil
	case "reflection":
		return reflection.New(rt), nil
	case "scripted":
		return scripted.New(rt), nil
	default:
		return nil, fmt.Errorf("assistant: unknown driver %q", cfg.Driver)
	}
}

// Agent 返回底层智能体运行时，可用于挂载自定义 Observer。
func (a *Assistant) Agent() *driver.Agent { return a.agent }

// Config 返回当前配置。
func (a *Assistant) Config() Config { return a.config }

// Policy 返回当前安全策略。
func (a *Assistant) Policy() driver.Policy { return a.policy }

// NewSession 创建一个新会话（默认生成 UUID 作为会话 ID，供多会话持久化与恢复）。
// 需要自定义 ID 时可直接 session.NewSession(id)。配置了 Persistence 时自动挂载
// （事件同步落盘，日志即事实）。
func (a *Assistant) NewSession() *session.Session {
	sess := session.NewSession(uuid.NewString())
	if a.persistence != nil {
		sess.Persistence = a.persistence
	}
	return sess
}

// newAgentForSession 为指定会话重建绑定的智能体实例与 RunContext
// （复用当前装配的驱动/工具/策略/审批/checkpoint 等共享设施，替换 Session）。
// 供 handle.Registry 的 Create / Resume 使用。
func (a *Assistant) newAgentForSession(sess *session.Session) (*driver.Agent, *driver.RunContext, error) {
	if a == nil {
		return nil, nil, fmt.Errorf("assistant: nil assistant")
	}
	if sess == nil {
		return nil, nil, fmt.Errorf("assistant: nil session")
	}
	if sess.Persistence == nil && a.persistence != nil {
		sess.Persistence = a.persistence
	}
	rt := buildRunContext(a.model, a.tools, a.config)
	rt.Session = sess
	ag, err := assembleDriver(a.config, rt)
	if err != nil {
		return nil, nil, err
	}
	return ag, rt, nil
}

// CreateHandle 创建新会话句柄（M2.8）：注册进 Registry，可 Run / 挂起 / Resume。
func (a *Assistant) CreateHandle(ctx context.Context) (*handle.AgentHandle, error) {
	return a.registry.Create(ctx, a.NewSession())
}

// Handle 返回指定会话的句柄（M2.8）：从持久化恢复会话 + checkpoint 挂载，
// 供平台对已挂起（SuspendError）的会话执行 Resume(decision) 续跑。
func (a *Assistant) Handle(ctx context.Context, sessionID string) (*handle.AgentHandle, error) {
	if a.persistence == nil {
		return nil, fmt.Errorf("assistant: handle requires Persistence configured")
	}
	sess, err := session.Restore(a.persistence, sessionID)
	if err != nil {
		return nil, fmt.Errorf("assistant: restore %s: %w", sessionID, err)
	}
	if sess.Persistence == nil {
		sess.Persistence = a.persistence
	}
	return a.registry.Resume(ctx, sess)
}

// Registry 返回会话句柄注册表（供平台遍历/释放全部句柄）。
func (a *Assistant) Registry() *handle.Registry { return a.registry }

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

// WithSession 把 *session.Session 注入 ctx，供 activate_skill 等工具在执行期间
// 经 sessionFromCtx 取用（写会话事件用）。nil 也允许注入；调用方负责装配时机。
// 装配层（driver.RunContext 注入路径）在每轮工具调用前调用本函数把当前会话塞进 ctx。
func WithSession(ctx context.Context, sess *session.Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// confirmBridge 将旧 assistant.Confirmer（assistant.ConfirmRequest 参数）桥接为
// approval.Confirmer（approval.ConfirmRequest 参数）——两者字段同构。
type confirmBridge struct{ inner Confirmer }

func (c confirmBridge) Confirm(ctx context.Context, req approval.ConfirmRequest) (bool, error) {
	return c.inner.Confirm(ctx, ConfirmRequest{
		ToolName: req.ToolName,
		Command:  req.Command,
		Reason:   req.Reason,
	})
}
