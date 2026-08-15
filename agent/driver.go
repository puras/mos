package agent

import (
	"context"
	"fmt"

	"hegq.net/mos/agent/event"
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

const defaultMaxTurns = 10

// LoopDriver 是循环驱动接口：一种智能体类型 = 一个实现
// （ReActDriver / PlanExecuteDriver / ReflectionDriver / GraphDriver，见架构 v2 §3.3）。
// MVP 先落地 ReActDriver。
type LoopDriver interface {
	Run(ctx context.Context, rt *RunContext, input string) (string, error)
}

// RunContext 是 driver 共享设施容器（架构 v2 §3.3）。
// 驱动通过它访问会话、工具、钩子、守卫与各扩展点；默认装配见 NewReAct。
type RunContext struct {
	Session           *Session
	Tools             *tool.Registry
	Planner           planner.Planner // 决策器（默认 planner.NewReAct(model)，ReActDriver 内部使用）
	Memory            memory.Memory   // 可选记忆后端（MVP 保留接口，接入见后续 Seam 迭代）
	Sandbox           *tool.Sandbox   // shell 命令执行沙箱
	Policy            Policy          // 工具调用安全策略（注入 Guards，单调守卫）
	Auditor           Auditor         // 工具结果审计门（默认 NewDefaultAuditor）
	Observer          Observer        // 观测回调（emit 投影入口，API 不变）
	Assembler         SystemAssembler // 槽位式系统提示词组装器（优先于 SystemPrompt）
	SystemPrompt      string          // 静态系统引导词（Assembler 为空时使用）
	Retriever         memory.Retriever
	RetrieverCfg      RetrieverConfig
	Truncator         ResultTruncator // 工具结果截断（模型可见文本瘦身）
	Compactor         Compactor       // fresh-context 压缩（两轮之间）
	ContextBudget     int             // 模型上下文 token 预算（利用率监控，>0 启用）
	DoomLoopMaxRepeat int             // Doom Loop 检测（>0 启用，默认阈值 3）
	MaxTurns          int
	Stream            bool            // 流式路径：模型文本增量经 EventStream 广播
	Hooks             *HookBus        // 瀑布钩子（可改写环节）
	Guards            *GuardBus       // 单调守卫（不可被改写绕过）
	Bus               *event.EventBus // 事件总线（emit 观测，可选）
}

// Agent 是智能体实例：RunContext + LoopDriver 的组合（架构 v2 §11.3）。
// 替代原命令式 run 循环：循环行为由 Driver 实现，装配由上层负责。
type Agent struct {
	Driver LoopDriver
	RT     *RunContext
}

// New 创建智能体：默认装配 = ReActDriver + 默认钩子/守卫（见 NewReAct）。
func New(rt *RunContext) *Agent {
	if rt.MaxTurns <= 0 {
		rt.MaxTurns = defaultMaxTurns
	}
	return &Agent{Driver: NewReAct(rt), RT: rt}
}

// Run 在会话上执行一次任务（非流式）。
func (a *Agent) Run(ctx context.Context, sess *Session, input string) (string, error) {
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
func (a *Agent) RunStream(ctx context.Context, sess *Session, input string) (string, error) {
	if a == nil || a.Driver == nil {
		return "", fmt.Errorf("agent: nil driver")
	}
	rt := a.RT
	rt.Session = sess
	rt.Stream = true
	return a.Driver.Run(ctx, rt, input)
}

// notify 广播观测事件（emit 投影；Observer 单层回调保留为便捷入口）。
func (rt *RunContext) notify(ev Event) {
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

// assembleSystem 调用槽位式组装器产出本轮系统提示词文本（M2.6）。
func (rt *RunContext) assembleSystem(ctx context.Context) (string, error) {
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
	return RenderSystem(sections), nil
}
