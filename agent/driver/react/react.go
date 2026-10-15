// Package react 提供默认 ReAct 循环驱动（思考 → 行动 → 观察 → 收尾）。
//
// S4a 形态：不带工具的最小闭环——决策器产出作答即收尾。
// 模型若请求工具调用，驱动 fail-closed 报错（S4b 接入执行通道后放开），
// 绝不静默忽略（那会让模型陷入"以为调用了其实没有"的空转）。
package react

import (
	"context"
	"errors"
	"fmt"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// ErrToolsUnsupported 表示模型请求了工具调用，但当前驱动未接入工具执行（S4a）。
var ErrToolsUnsupported = errors.New("react: 模型请求工具调用，工具执行通道尚未接入（S4b）")

// Driver 是 ReAct 循环驱动：无状态，可并发复用。
type Driver struct{}

// New 创建 ReAct 驱动。
func New() *Driver { return &Driver{} }

// Run 实现 driver.LoopDriver：一轮决策即收尾。
// 事件序列：turn/start → user → assistant → turn/end，
// 每步落日志；出错以 turn/end{reason:error} 收尾，不留孤儿 turn。
func (d *Driver) Run(ctx context.Context, rt *driver.RunContext, sess *session.Session, input string) (string, error) {
	turn := sess.NextTurn()

	// 记录本轮输入（投影即由此带上 user 消息）
	if _, err := sess.Append(session.Event{Kind: session.EventTurnStart, Turn: turn}); err != nil {
		return "", err
	}
	if _, err := sess.Append(session.Event{Kind: session.EventUser, Turn: turn, Text: input}); err != nil {
		return "", err
	}

	action, err := decide(ctx, rt, sess)
	if err != nil {
		finish(sess, turn, "error")
		return "", err
	}

	// fail-closed：请求了工具但本驱动没有执行通道（装配缺口不是模型的错）
	if action.HasToolCalls() {
		finish(sess, turn, "error")
		return "", fmt.Errorf("%w: %s", ErrToolsUnsupported, action.ToolCalls[0].Name)
	}

	// 落 assistant 事件：文本 + 思维链 + token 用量
	ev := session.Event{Kind: session.EventAssistant, Turn: turn, Text: action.Content}
	if action.ReasoningContent != "" {
		ev.Reasoning = action.ReasoningContent
	}
	if action.Usage.TotalTokens > 0 {
		usage := action.Usage
		ev.Usage = &usage
	}
	if _, err := sess.Append(ev); err != nil {
		return "", err
	}
	if _, err := sess.Append(session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: "completed"}); err != nil {
		return "", err
	}
	return action.Content, nil
}

// decide 按 rt.Stream 选择流式或非流式决策路径。
// 流式增量经 rt.OnChunk 实时透出（推理增量以 reasoning=true 标记）。
func decide(ctx context.Context, rt *driver.RunContext, sess *session.Session) (planner.Action, error) {
	sp, streaming := rt.Planner.(planner.StreamingPlanner)
	if !rt.Stream || !streaming {
		return rt.Planner.NextAction(ctx, rt.History(sess), nil)
	}
	return sp.NextActionStream(ctx, rt.History(sess), nil, func(c llm.StreamChunk) error {
		if rt.OnChunk == nil {
			return nil
		}
		if c.Reasoning != "" {
			rt.OnChunk(c.Reasoning, true)
		}
		if c.Delta != "" {
			rt.OnChunk(c.Delta, false)
		}
		return nil
	})
}

// finish 以指定原因收尾本轮（出错路径，保证不留孤儿 turn）。
func finish(sess *session.Session, turn int, reason string) {
	_, _ = sess.Append(session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: reason})
}
