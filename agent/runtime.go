package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
)

const defaultMaxIterations = 10

// Agent 是智能体运行时，驱动 感知-推理-行动 的任务循环。
type Agent struct {
	planner       planner.Planner
	tools         *tool.Registry
	maxIterations int
	systemPrompt  string
	observer      Observer
}

// Option 是 Agent 的配置项。
type Option func(*Agent)

// New 创建 Agent。默认使用 ReAct 规划器，可经 WithPlanner 替换。
func New(provider llm.Provider, tools *tool.Registry, opts ...Option) *Agent {
	a := &Agent{
		planner:       planner.NewReAct(provider),
		tools:         tools,
		maxIterations: defaultMaxIterations,
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

// WithMaxIterations 设置任务循环的最大迭代次数。
func WithMaxIterations(n int) Option {
	return func(a *Agent) { a.maxIterations = n }
}

// WithObserver 注册事件回调，用于观测流式输出与工具调用过程。
func WithObserver(o Observer) Option {
	return func(a *Agent) { a.observer = o }
}

// Run 在会话上执行一次任务：接收用户输入，循环 推理→工具调用 直至产出最终回复。
func (a *Agent) Run(ctx context.Context, sess *Session, input string) (string, error) {
	if a.planner == nil {
		return "", fmt.Errorf("agent: no planner configured")
	}
	if a.tools == nil {
		return "", fmt.Errorf("agent: no tool registry configured")
	}

	// 组装本轮完整消息：system 引导 + 会话历史 + 用户输入
	history := make([]llm.Message, 0, len(sess.History)+2)
	if a.systemPrompt != "" {
		history = append(history, llm.Message{Role: llm.RoleSystem, Content: a.systemPrompt})
	}
	history = append(history, sess.History...)
	userMsg := llm.Message{Role: llm.RoleUser, Content: input}
	history = append(history, userMsg)
	sess.History = append(sess.History, userMsg)

	for i := 0; i < a.maxIterations; i++ {
		action, err := a.planner.NextAction(ctx, history, a.tools.List())
		if err != nil {
			return "", fmt.Errorf("agent: plan iteration %d: %w", i, err)
		}

		if action.ToolCall == nil {
			// 直接作答，任务完成
			reply := llm.Message{Role: llm.RoleAssistant, Content: action.Content}
			history = append(history, reply)
			sess.History = append(sess.History, reply)
			a.notify(Event{Type: EventDone, SessionID: sess.ID, Content: action.Content})
			return action.Content, nil
		}

		// 记录模型发起的工具调用
		call := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   action.Content,
			ToolCalls: []llm.ToolCall{*action.ToolCall},
		}
		history = append(history, call)
		sess.History = append(sess.History, call)
		a.notify(Event{Type: EventToolCall, SessionID: sess.ID, ToolCall: action.ToolCall})

		// 执行工具并回填结果
		result, err := a.tools.Call(ctx, action.ToolCall.Name, json.RawMessage(action.ToolCall.Args))
		resultMsg := formatToolResult(result)
		if err != nil {
			resultMsg = "工具执行出错: " + err.Error()
		}
		feedback := llm.Message{
			Role:       llm.RoleTool,
			Content:    resultMsg,
			ToolCallID: action.ToolCall.ID,
			Name:       action.ToolCall.Name,
		}
		history = append(history, feedback)
		sess.History = append(sess.History, feedback)
		a.notify(Event{Type: EventToolResult, SessionID: sess.ID, ToolCall: action.ToolCall, ToolResult: result})
	}

	return "", fmt.Errorf("agent: max iterations %d exceeded", a.maxIterations)
}

func (a *Agent) notify(ev Event) {
	if a.observer != nil {
		a.observer(ev)
	}
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
