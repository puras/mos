// Package driver 定义智能体循环驱动接口与运行上下文。
//
// 一种智能体循环 = 一个 LoopDriver 实现（S4a 仅 ReAct；Plan&Execute /
// Reflection / Scripted 按 PRD 逐个回填）。驱动负责「一轮怎么转」：写会话
// 事件、调决策器、执行工具、回填结果、判定终止。
package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// LoopDriver 是循环驱动接口：一种循环类型 = 一个实现。
type LoopDriver interface {
	// Run 在给定会话上执行一次任务：写事件、决策、执行、收尾，返回最终回复。
	// sess 由调用方持有（可挂持久化后端），驱动不缓存会话。
	Run(ctx context.Context, rt *RunContext, sess *session.Session, input string) (string, error)
}

// RunContext 是驱动共享的运行上下文。
// 只装「装配期决定、运行期只读」的东西；运行期变化的状态一律进会话日志。
type RunContext struct {
	// Planner 是决策器（必填）。为 nil 时驱动 fail-closed 拒绝运行——
	// 缺能力就拒绝，不降级放行。
	Planner planner.Planner
	// Tools 是工具注册表（可选）。为 nil 时决策器收不到工具定义，
	// 模型不会知道有工具可调；若仍返回了工具调用（幻觉），驱动
	// fail-closed 报 ErrToolsUnsupported。
	Tools *tool.Registry
	// Hooks 是可改写环节的瀑布钩子总线（可选）。nil 时零开销——
	// 无钩子即无任何改写，行为与未注入完全一致。
	//
	// 钩子**不得改写工具调用载荷**（工具名 / 参数 / CallID）：载荷冻结，
	// 守卫判定的是这次调用的最终载荷。钩子能做的是：pre-execute 裁决
	// （允许 / 拒绝 / 请求确认 / 终止本轮）、execute 环绕派发（计时 / 重试）、
	// post-execute 改写结果文本。
	Hooks *hook.HookBus
	// Guards 是单调守卫总线（可选）。nil 时零开销——等价于无安全约束。
	// 守卫由执行通道在工具执行前串行裁决，先于钩子链：载荷对钩子不可变，
	// 所以钩子既改不了守卫的判定，也改不了守卫看到的载荷（双轨制，见 agent/hook）。
	Guards *hook.GuardBus
	// Concurrency 是同一批工具调用的并发上限（可选，<=0 取默认 4，1 = 串行）。
	// 并发是显式 opt-in：工具未声明本次调用可并发（tool.ConcurrencySafe）时
	// 一律独占，所以默认行为仍是串行。并发只影响完成顺序，回填顺序永远是
	// 模型的调用序。
	Concurrency int
	// MaxDecisionRounds 是单轮内决策次数上限（含最终作答）。<=0 = 不限
	// （S6 起**默认不限**：上下文增长已由压缩接管，对齐 PI/DSH；保险丝
	// 旋钮保留给无人值守场景按需设置）。
	//
	// 这是**保险丝**而非正常路径：合法长任务通常 10-40 轮；空转由连续相同
	// 调用的提醒（advisory）打断。设了上限而耗尽时，本轮以
	// react.ErrMaxDecisionRounds 报错收尾，不假装完成。
	MaxDecisionRounds int
	// Answerer 是人工审批的应答方（可选，nil = 未装配）。
	//
	// 框架负责"什么时候问、怎么记审计"，**问人由宿主实现**：弹窗、读终端、
	// 从另一个进程收决定都行。nil 时，pre-execute 的「需人工确认」降级为
	// 拒绝（fail-closed）。
	Answerer approval.Answerer
	// Compactor 是上下文压缩器（可选，nil = 不压缩）。每次决策前，超过
	// token 预算（contextWindow - reserveTokens，PI 同款）时：从投影视图
	// 定切割点（永不落在 tool 结果上）→ 模型生成结构化摘要 → 摘要作为
	// compaction 事件落会话日志（投影从此遮蔽旧历史，重放可复现）。
	// 压缩失败以错误终止本轮（fail-fast：静默超预算会在下一次调用溢出，
	// 那时报错更难懂）。参见 agent.LLMCompactor。
	Compactor agent.Compactor
	// SystemPrompt 是静态系统引导词（可选，作为历史首条消息注入）。
	SystemPrompt string
	// Stream 为 true 时优先走决策器的流式接口（若实现），
	// 文本增量经 OnChunk 实时回调。
	Stream bool
	// OnChunk 是流式文本增量回调（可选，nil 时丢弃增量）。
	// 推理增量以 dark=true 表示（应用层可据此区别渲染）。
	OnChunk func(delta string, reasoning bool)
}

// validate 校验运行上下文的必要能力（fail-closed）。
func (rt *RunContext) validate() error {
	if rt == nil || rt.Planner == nil {
		return errors.New("driver: RunContext.Planner 未设置（fail-closed）")
	}
	return nil
}

// History 组装送给决策器的对话消息：静态系统引导（若有）+ 会话事件投影。
// 投影由 session 派生，驱动不自行维护历史——「日志即事实」。
// 工具定义不进 History，由驱动单独传入决策器（对话与能力本是两件事）。
func (rt *RunContext) History(sess *session.Session) []llm.Message {
	var msgs []llm.Message
	if rt.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: rt.SystemPrompt})
	}
	return append(msgs, sess.Messages()...)
}

// Agent 是智能体实例：RunContext + LoopDriver 的组合。
type Agent struct {
	Driver LoopDriver
	RT     *RunContext
}

// New 创建智能体实例：显式指定驱动。
func New(rt *RunContext, d LoopDriver) *Agent {
	return &Agent{Driver: d, RT: rt}
}

// Run 执行一次任务（驱动自行决定流式或非流式路径）。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
	if a == nil || a.Driver == nil {
		return "", fmt.Errorf("driver: 驱动未设置")
	}
	if sess == nil {
		return "", fmt.Errorf("driver: 会话未设置")
	}
	if err := a.RT.validate(); err != nil {
		return "", err
	}
	return a.Driver.Run(ctx, a.RT, sess, input)
}
