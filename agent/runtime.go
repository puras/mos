package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"hegq.net/mos/llm"
	"hegq.net/mos/memory"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

const defaultMaxTurns = 10

// Policy 是工具调用的安全策略检查点（Harness Engineering）。
// 在每次工具调用执行前触发，返回错误表示拒绝该次调用。
type Policy interface {
	Allow(ctx context.Context, call *llm.ToolCall) error
}

// Agent 是智能体运行时，驱动 感知-推理-行动 的任务循环。
type Agent struct {
	planner      planner.Planner
	tools        *tool.Registry
	policy       Policy
	auditor      Auditor
	memory       memory.Memory
	sandbox      *tool.Sandbox
	truncator    ResultTruncator
	maxTurns     int
	systemPrompt string
	observer     Observer
}

// Option 是 Agent 的配置项。
type Option func(*Agent)

// New 创建 Agent。默认使用 ReAct 规划器，可经 WithPlanner 替换。
func New(model llm.ChatModel, tools *tool.Registry, opts ...Option) *Agent {
	a := &Agent{
		planner:  planner.NewReAct(model),
		tools:    tools,
		auditor:  NewDefaultAuditor(),
		maxTurns: defaultMaxTurns,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// WithPlanner 设置自定义规划策略。
func WithPlanner(p planner.Planner) Option {
	return func(a *Agent) { a.planner = p }
}

// WithSystemPrompt 设置系统引导词。
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.systemPrompt = p }
}

// WithMaxTurns 设置一次任务循环的最大 Turn 数（一个 Turn = 一次模型调用 + 其触发的所有工具执行）。
// 达到上限仍未完成时任务状态置为 blocked。
func WithMaxTurns(n int) Option {
	return func(a *Agent) { a.maxTurns = n }
}

// WithPolicy 设置工具调用安全策略（Harness Engineering）。
func WithPolicy(p Policy) Option {
	return func(a *Agent) { a.policy = p }
}

// WithAuditor 设置工具结果审计器（Audit Gate）。
// 未设置时使用默认审计器：仅白名单只读工具的输出可写入任务状态。
func WithAuditor(ad Auditor) Option {
	return func(a *Agent) { a.auditor = ad }
}

// WithMemory 设置会话记忆存储（外部注入 memory.Memory 实现）。
// 未设置时任务历史仅存于会话对象，不持久化。
func WithMemory(m memory.Memory) Option {
	return func(a *Agent) { a.memory = m }
}

// WithObserver 注册事件回调，用于观测流式输出与工具调用过程。
func WithObserver(o Observer) Option {
	return func(a *Agent) { a.observer = o }
}

// WithSandbox 设置 shell 命令的执行沙箱（M1.6 隔离层）：
// 工作目录 jail + 资源限制 + 进程组超时。nil 时工具使用默认执行方式。
func WithSandbox(s *tool.Sandbox) Option {
	return func(a *Agent) { a.sandbox = s }
}

// WithResultTruncator 设置工具结果截断器（M2.3 上下文工程）：
// 对"给模型看的工具结果文本"做行数/字节上限的瘦身，超限时截断并携带
// 逃生通道提示（完整结果落盘路径，模型可经 read_file 按偏移回读）。
// 截断只影响模型可见文本，工具原始返回值（含审计）不受影响。
// 未设置时工具结果原样注入上下文。
func WithResultTruncator(tr ResultTruncator) Option {
	return func(a *Agent) { a.truncator = tr }
}

// Run 在会话上执行一次任务（非流式）：接收用户输入，循环 推理→工具调用 直至产出最终回复。
func (a *Agent) Run(ctx context.Context, sess *Session, input string) (string, error) {
	return a.run(ctx, sess, input, false)
}

// RunStream 同 Run，但规划器产出模型的文本增量时实时广播
// Event{Type: EventStream, Delta: ...}，供应用层实现打字机效果。
// 模型或规划器不支持流式时自动回退为非流式路径，行为与 Run 一致。
func (a *Agent) RunStream(ctx context.Context, sess *Session, input string) (string, error) {
	return a.run(ctx, sess, input, true)
}

func (a *Agent) run(ctx context.Context, sess *Session, input string, stream bool) (string, error) {
	if a.planner == nil {
		return "", fmt.Errorf("agent: no planner configured")
	}
	if a.tools == nil {
		return "", fmt.Errorf("agent: no tool registry configured")
	}

	// 注入执行沙箱（M1.6）：工具（shell 等）从 ctx 取沙箱执行，未配置时用默认方式
	ctx = tool.WithSandbox(ctx, a.sandbox)

	// 组装本轮完整消息：system 引导 + 会话历史（配置了 Memory 时以其为准）+ 用户输入
	var prev []llm.Message
	if a.memory != nil {
		var err error
		if prev, err = a.memory.Load(ctx, sess.ID); err != nil {
			return "", fmt.Errorf("agent: load memory: %w", err)
		}
	} else {
		prev = sess.History
	}
	history := make([]llm.Message, 0, len(prev)+2)
	if a.systemPrompt != "" {
		history = append(history, llm.Message{Role: llm.RoleSystem, Content: a.systemPrompt})
	}
	history = append(history, prev...)
	userMsg := llm.Message{Role: llm.RoleUser, Content: input}
	history = append(history, userMsg)
	if err := a.append(ctx, sess, userMsg); err != nil {
		return "", fmt.Errorf("agent: save memory: %w", err)
	}

	// 任务状态初始化：首次运行记录原始目标，进入 running。
	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != StatusRunning {
		sess.State.Status = StatusRunning
	}

	for i := 0; i < a.maxTurns; i++ {
		turn := i + 1
		action, err := a.nextAction(ctx, sess, turn, history, stream)
		if err != nil {
			return "", fmt.Errorf("agent: plan turn %d: %w", turn, err)
		}

		// 暴露本轮思考（Loop Engineering 观测点）
		if action.Content != "" {
			a.notify(Event{Type: EventStep, SessionID: sess.ID, Turn: turn, Content: action.Content})
		}

		if action.ToolCall == nil {
			// 直接作答，任务完成
			reply := llm.Message{Role: llm.RoleAssistant, Content: action.Content, ReasoningContent: action.ReasoningContent}
			history = append(history, reply)
			if err := a.append(ctx, sess, reply); err != nil {
				return "", fmt.Errorf("agent: save memory: %w", err)
			}
			sess.State.Status = StatusCompleted
			a.notify(Event{Type: EventDone, SessionID: sess.ID, Turn: turn, Content: action.Content})
			return action.Content, nil
		}

		// 记录模型发起的工具调用
		call := llm.Message{
			Role:             llm.RoleAssistant,
			Content:          action.Content,
			ReasoningContent: action.ReasoningContent, // thinking 模式须原样回传
			ToolCalls:        []llm.ToolCall{*action.ToolCall},
		}
		history = append(history, call)
		if err := a.append(ctx, sess, call); err != nil {
			return "", fmt.Errorf("agent: save memory: %w", err)
		}
		a.notify(Event{Type: EventToolCall, SessionID: sess.ID, Turn: turn, ToolCall: action.ToolCall})

		// 策略检查（Harness Engineering）后再执行工具
		var result any
		executed := false
		if a.policy != nil {
			if err := a.policy.Allow(ctx, action.ToolCall); err != nil {
				result = "工具调用被安全策略拒绝: " + err.Error()
			}
		}
		if result == nil {
			result, err = a.tools.Call(ctx, action.ToolCall.Name, json.RawMessage(action.ToolCall.Args))
			executed = true
		}
		resultMsg := formatToolResult(result)
		if err != nil {
			resultMsg = "工具执行出错: " + err.Error()
		}
		// M2.3 上下文瘦身：截断只作用于模型可见文本，Auditor 仍审计原始 result
		if a.truncator != nil {
			if s, truncated := a.truncator.Truncate(action.ToolCall.Name, resultMsg); truncated {
				resultMsg = s
			}
		}
		feedback := llm.Message{
			Role:       llm.RoleTool,
			Content:    resultMsg,
			ToolCallID: action.ToolCall.ID,
			Name:       action.ToolCall.Name,
		}
		history = append(history, feedback)
		if err := a.append(ctx, sess, feedback); err != nil {
			return "", fmt.Errorf("agent: save memory: %w", err)
		}
		a.notify(Event{Type: EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: action.ToolCall, ToolResult: result})

		// 审计门（Audit Gate）：仅将审计器认可的环境事实写入任务状态
		if executed && err == nil && a.auditor != nil {
			if facts, auditErr := a.auditor.Audit(ctx, action.ToolCall, result); auditErr == nil {
				for _, f := range facts {
					sess.State.AddFact(f)
					a.notify(Event{Type: EventFact, SessionID: sess.ID, Turn: turn, Fact: &f})
				}
			}
		}
	}

	sess.State.Status = StatusBlocked
	return "", fmt.Errorf("agent: max turns %d exceeded", a.maxTurns)
}

func (a *Agent) notify(ev Event) {
	if a.observer != nil {
		a.observer(ev)
	}
}

// nextAction 执行单轮规划。流式路径下转发模型文本增量事件（EventStream）；
// 规划器不支持流式接口时回退非流式 NextAction。
func (a *Agent) nextAction(ctx context.Context, sess *Session, turn int, history []llm.Message, stream bool) (planner.Action, error) {
	tools := a.tools.List()
	if !stream {
		return a.planner.NextAction(ctx, history, tools)
	}
	sp, ok := a.planner.(planner.StreamingPlanner)
	if !ok {
		return a.planner.NextAction(ctx, history, tools)
	}
	action, err := sp.NextActionStream(ctx, history, tools, func(chunk llm.StreamChunk) error {
		if chunk.Delta != "" {
			a.notify(Event{Type: EventStream, SessionID: sess.ID, Turn: turn, Delta: chunk.Delta})
		}
		return nil
	})
	if err != nil {
		return planner.Action{}, err
	}
	return action, nil
}

// append 将会话消息写入会话历史，并（若配置了 Memory）持久化到记忆存储。
func (a *Agent) append(ctx context.Context, sess *Session, msg llm.Message) error {
	sess.History = append(sess.History, msg)
	if a.memory != nil {
		return a.memory.Append(ctx, sess.ID, msg)
	}
	return nil
}

// formatToolResult 将工具返回值统一序列化为给模型看的文本。
func formatToolResult(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
