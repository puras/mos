// Package driver 定义智能体循环驱动接口与共享运行上下文（架构 v2 §3.3/§11.3）。
// 一种智能体类型 = 一个 LoopDriver 实现（ReAct / PlanExecute / Reflection / Graph）；
// RunContext 承载驱动间共享的会话、工具、钩子、守卫与扩展点。
// 默认装配（ReActDriver + 默认钩子/守卫）见子包 agent/driver/react。
package driver

import (
	"context"
	"fmt"
	"sync"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/llm"
	"hegq.net/mos/memory"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// Policy 是工具调用的安全策略检查点（Harness Engineering）。
// 在每次工具调用执行前触发，返回错误表示拒绝该次调用。
// v2 中 Policy 注入 RunContext，作为单调守卫（GuardBus）的一部分，
// 不可被普通钩子改写绕过。
type Policy interface {
	Allow(ctx context.Context, call *llm.ToolCall) error
}

// LoopDriver 是循环驱动接口：一种智能体类型 = 一个实现
// （ReActDriver / PlanExecuteDriver / ReflectionDriver / GraphDriver，见架构 v2 §3.3）。
// MVP 先落地 ReActDriver。
type LoopDriver interface {
	Run(ctx context.Context, rt *RunContext, input string) (string, error)
}

// RunContext 是 driver 共享设施容器（架构 v2 §3.3）。
// 驱动通过它访问会话、工具、钩子、守卫与各扩展点；默认装配见 react.New。
type RunContext struct {
	Session      *session.Session
	Tools        *tool.Registry
	Planner      planner.Planner       // 决策器（默认 planner.NewReAct(model)，ReActDriver 内部使用）
	Memory       memory.Memory         // 可选记忆后端（MVP 保留接口，接入见后续 Seam 迭代）
	Sandbox      *tool.Sandbox         // shell 命令执行沙箱
	Policy       Policy                // 工具调用安全策略（注入 Guards，单调守卫）
	Auditor      agent.Auditor         // 工具结果审计门（默认 NewDefaultAuditor）
	Observer     agent.Observer        // 观测回调（emit 投影入口，API 不变）
	Assembler    agent.SystemAssembler // 槽位式系统提示词组装器（优先于 SystemPrompt）
	SystemPrompt string                // 静态系统引导词（Assembler 为空时使用）
	Retriever    memory.Retriever
	RetrieverCfg agent.RetrieverConfig
	// PlanPlanner 是 Plan&Execute 驱动的计划生成器（架构 v2 §3.3）；
	// ReAct 驱动不使用，nil 时 planexecute 驱动回退为单步执行。
	PlanPlanner planner.PlanPlanner
	// Reflector 是 Reflection 驱动的评审器（架构 v2 §3.3，Basic Reflection）；
	// ReAct/PlanExecute 驱动不使用，nil 时 reflection 驱动拒绝运行。
	Reflector planner.Reflector
	// ScriptPlanner 是 Scripted 驱动的脚本生成器（架构 v2 §3.3，PTC 模式）。
	// ReAct/PlanExecute/Reflection 驱动不使用，nil 时 scripted 驱动拒绝运行（对齐 reflection 的 fail-closed）。
	ScriptPlanner     planner.ScriptPlanner
	Truncator         agent.ResultTruncator // 工具结果截断（模型可见文本瘦身）
	Compactor         agent.Compactor       // fresh-context 压缩（两轮之间）
	ContextBudget     int                   // 模型上下文 token 预算（利用率监控，>0 启用）
	DoomLoopMaxRepeat int                   // Doom Loop 检测（>0 启用，默认阈值 3）
	// MaxTurns 是可选保险丝（>0 生效）：达到该轮数仍未完成时任务置为 blocked。
	// 0 表示不限制（默认；对齐 DSH/PI 无轮次上限，循环以模型直接作答或
	// ShouldStopAfterTurn 终止）。
	MaxTurns int
	// ShouldStopAfterTurn 是每轮结束后的外部停止裁决回调（对齐 PI shouldStopAfterTurn）：
	// 返回 true 时任务以 completed 收尾（turn/end reason "stopped"）；
	// nil 时不生效。回调可基于会话历史、本轮 Action 与工具结果做业务裁决
	// （如人工叫停、成本控制）。
	ShouldStopAfterTurn func(ctx context.Context, sess *session.Session, turn int, action *planner.Action) (bool, error)
	Stream              bool            // 流式路径：模型文本增量经 EventStream 广播
	Hooks               *hook.HookBus   // 瀑布钩子（可改写环节）
	Guards              *hook.GuardBus  // 单调守卫（不可被改写绕过）
	Bus                 *event.EventBus // 事件总线（emit 观测，可选）
	// MaxToolParallel 是单轮批量工具调用的并发上限（架构 v2 §3.9；<=0 用默认 4）。
	// 批量路径：preflight 串行决策 → 有界并发执行 → 结果按模型顺序回填。
	MaxToolParallel int
	// Telemetry 是显式传递的 span 工厂（架构 v2 §3.11，对齐 PI pi-telemetry）。
	// 可选：nil 时驱动内部降级 no-op（零开销）；平台注入适配器（OpenTelemetry / 自研）
	// 后，driver 回合 / llm 请求 / 工具执行自动打 span（model/provider 属性由平台
	// 在模型适配器/包装层补充）。
	Telemetry telemetry.TelemetryContext
	// Approval 是异步审批通道（架构 v2 §3.4，对齐 PI SuspendedOperation）。
	// 可选：nil 时 policyGuard 对需要确认的操作 fail-closed 拒绝（等价旧版无确认器行为）。
	// 策略检查器返回 *approval.RequiredError 后由 policyGuard 发起 Approval.Request；
	// 未实现 approval.Immediate 时驱动抛 SuspendError 挂起，平台 Resolve → Handle.Resume 续跑。
	Approval approval.Approval
	// Checkpoints 是 checkpoint 存储（架构 v2 §3.8，参考 LangChain checkpointer）。
	// 可选：nil 时挂起点不落盘（SuspendError 仍冒泡，但无法跨进程恢复）。
	Checkpoints checkpoint.Store
	// ResumeCheckpoint 是 Handle.Resume 注入的恢复点：driver 从挂起轮直接进入
	// 工具批量执行（跳过 LLM 调用与日志），approval guard 按 Decisions[call.ID] 查表放行。
	ResumeCheckpoint *checkpoint.Checkpoint

	// factMu 串行化审计事实写入：批量工具并发执行时多个 goroutine 同时写
	// 同一 Session.State.Facts（auditHook 内 AddFact），锁保证无数据竞争。
	factMu sync.Mutex
}

// AddFact 追加一条已验证事实（架构 v2 §3.9 日志串行化）：
// 并发工具执行时多个 goroutine 同时审计写同一 Session.State，经互斥锁串行化。
func (rt *RunContext) AddFact(f session.Fact) {
	if rt == nil {
		return
	}
	rt.factMu.Lock()
	defer rt.factMu.Unlock()
	if rt.Session != nil {
		rt.Session.State.AddFact(f)
	}
}

// Agent 是智能体实例：RunContext + LoopDriver 的组合（架构 v2 §11.3）。
// 替代原命令式 run 循环：循环行为由 Driver 实现，装配由上层负责。
type Agent struct {
	Driver LoopDriver
	RT     *RunContext
}

// New 创建智能体实例：显式指定驱动。
// 默认装配（ReActDriver + 默认钩子/守卫）见 agent/driver/react 包的 New。
func New(rt *RunContext, d LoopDriver) *Agent {
	return &Agent{Driver: d, RT: rt}
}

// Run 在会话上执行一次任务（非流式）。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
	if a == nil || a.Driver == nil {
		return "", fmt.Errorf("agent: nil driver")
	}
	rt := a.RT
	rt.Session = sess
	rt.Stream = false
	return a.Driver.Run(ctx, rt, input)
}

// RunStream 同 Run，但模型产出文本增量实时广播 EventStream 事件（打字机效果）；
// 模型或规划器不支持流式时自动回退为非流式路径，行为与 Run 一致。
func (a *Agent) RunStream(ctx context.Context, sess *session.Session, input string) (string, error) {
	if a == nil || a.Driver == nil {
		return "", fmt.Errorf("agent: nil driver")
	}
	rt := a.RT
	rt.Session = sess
	rt.Stream = true
	return a.Driver.Run(ctx, rt, input)
}

// Notify 广播观测事件（emit 投影；Observer 单层回调保留为便捷入口）。
func (rt *RunContext) Notify(ev agent.Event) {
	if rt == nil {
		return
	}
	if rt.Observer != nil {
		rt.Observer(ev)
	}
	if rt.Bus != nil {
		_ = rt.Bus.Emit(context.Background(), "observer/event", ev)
	}
}

// AssembleSystem 调用槽位式组装器产出本轮系统提示词文本（M2.6）。
func (rt *RunContext) AssembleSystem(ctx context.Context) (string, error) {
	if rt == nil {
		return "", nil
	}
	if rt.Assembler == nil {
		return rt.SystemPrompt, nil
	}
	sections, err := rt.Assembler.Assemble(ctx, rt.Session, rt.Tools.List())
	if err != nil {
		return "", err
	}
	return agent.RenderSystem(sections), nil
}

// MonitorUsage 上下文利用率监控：广播 EventContextUsage 事件。
func (rt *RunContext) MonitorUsage(turn int, usage llm.Usage) {
	if rt.ContextBudget <= 0 || usage.PromptTokens <= 0 {
		return
	}
	ratio := float64(usage.PromptTokens) / float64(rt.ContextBudget)
	rt.Notify(agent.Event{
		Type: agent.EventContextUsage, SessionID: rt.Session.ID, Turn: turn,
		Content: fmt.Sprintf("%.0f%%", ratio*100), Usage: usage,
	})
}
