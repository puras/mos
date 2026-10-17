// Package react 提供默认 ReAct 循环驱动（思考 → 行动 → 观察 → 收尾）。
//
// 一个 turn 内是内层循环：决策 → 产出作答则收尾，否则执行工具回填 → 再决策。
// 事件序列（单 turn）：
//
//	turn/start → user → (assistant → tool/call* → tool/result*)* → assistant → turn/end
//
// 不变量：assistant 的 ToolCalls 与随后的 tool/result 按 CallID 成对——投影出的
// 对话对模型始终是合法历史（缺 tool 消息会触发服务商报错）；
// 任何出错路径都以 turn/end{reason:error} 收尾，不留孤儿 turn。
package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// ErrToolsUnsupported 表示模型请求了工具调用，但 RunContext 未装配工具注册表。
var ErrToolsUnsupported = errors.New("react: 模型请求工具调用，但 RunContext.Tools 未装配")

// ErrMaxDecisionRounds 表示单轮内决策次数耗尽仍未产出最终作答。
//
// 这是 S4b 的临时保险（防止模型无限请求工具把循环转死）；S6 引入
// DoomLoopGuard（重复调用检测）与 MaxTurns（轮次上限）后，停止裁决
// 交由那两处统一裁决，本上限届时重议。
var ErrMaxDecisionRounds = errors.New("react: 单轮决策次数超出上限（工具可能在空转）")

// maxDecisionRounds 是单个 turn 内允许的最大决策次数（含最终作答那次）。
const maxDecisionRounds = 16

// Driver 是 ReAct 循环驱动：无状态，可并发复用。
type Driver struct{}

// New 创建 ReAct 驱动。
func New() *Driver { return &Driver{} }

// Run 实现 driver.LoopDriver：执行一个 turn（可能含多次决策与工具执行）。
// 每步落日志；出错以 turn/end{reason:error} 收尾。
func (d *Driver) Run(ctx context.Context, rt *driver.RunContext, sess *session.Session, input string) (string, error) {
	turn := sess.NextTurn()

	// 记录本轮输入（投影即由此带上 user 消息）
	if _, err := sess.Append(session.Event{Kind: session.EventTurnStart, Turn: turn}); err != nil {
		return "", err
	}
	if _, err := sess.Append(session.Event{Kind: session.EventUser, Turn: turn, Text: input}); err != nil {
		return "", err
	}

	var defs []llm.Tool
	if rt.Tools != nil {
		defs = rt.Tools.List()
	}

	for round := 0; round < maxDecisionRounds; round++ {
		action, err := decide(ctx, rt, sess, defs)
		if err != nil {
			return "", d.abort(sess, turn, err)
		}

		// 装配缺口：要求了工具却没装注册表。先收尾再报错，且不写 assistant——
		// 否则日志里会留下带 ToolCalls 却无 tool/result 的非法历史。
		if action.HasToolCalls() && rt.Tools == nil {
			return "", d.abort(sess, turn, fmt.Errorf("%w: %s", ErrToolsUnsupported, action.ToolCalls[0].Name))
		}

		// 记录本次决策（作答或附带工具调用的思考），思维链与用量一并落库
		if err := appendAssistant(sess, turn, action); err != nil {
			return "", err
		}

		// 无工具调用 = 产出最终作答，收尾
		if !action.HasToolCalls() {
			if _, err := sess.Append(session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: "completed"}); err != nil {
				return "", err
			}
			return action.Content, nil
		}

		// 先把 tool/call 按模型顺序全部落日志，再执行、再按序回填 tool/result
		// （Seq 连续可重放，见 docs/ARCHITECTURE.md 循环契约）
		for _, call := range action.ToolCalls {
			if _, err := sess.Append(session.Event{
				Kind: session.EventToolCall, Turn: turn, ToolCall: &call,
			}); err != nil {
				return "", err
			}
		}
		for _, call := range action.ToolCalls {
			content, isErr := executeCall(ctx, rt, call)
			if _, err := sess.Append(session.Event{
				Kind: session.EventToolResult, Turn: turn,
				ToolResult: &session.ToolResult{CallID: call.ID, Content: content, IsError: isErr},
			}); err != nil {
				return "", err
			}
		}
		// 回到循环顶部：把工具结果投影进历史，再让决策器决策
	}
	return "", d.abort(sess, turn, ErrMaxDecisionRounds)
}

// abort 以 error 原因收尾本轮并返回原错误（落盘失败时原样返回，收尾已尽力）。
func (d *Driver) abort(sess *session.Session, turn int, cause error) error {
	_, _ = sess.Append(session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: "error"})
	return cause
}

// appendAssistant 落一条 assistant 事件：文本、思维链、工具调用与 token 用量。
func appendAssistant(sess *session.Session, turn int, action planner.Action) error {
	ev := session.Event{Kind: session.EventAssistant, Turn: turn, Text: action.Content}
	if action.ReasoningContent != "" {
		ev.Reasoning = action.ReasoningContent
	}
	if len(action.ToolCalls) > 0 {
		ev.ToolCalls = action.ToolCalls
	}
	if action.Usage.TotalTokens > 0 {
		usage := action.Usage
		ev.Usage = &usage
	}
	_, err := sess.Append(ev)
	return err
}

// executeCall 执行单个工具调用，把错误转为回填给模型的显式失败而不是中断循环：
// 工具报错（含模型幻觉出的未注册工具名）属于「结果」而非「系统故障」，
// 回填给模型让它自我纠正；真正中断 turn 的只有落盘失败与决策失败。
func executeCall(ctx context.Context, rt *driver.RunContext, call llm.ToolCall) (content string, isErr bool) {
	out, err := rt.Tools.Call(ctx, call.Name, json.RawMessage(call.Args))
	if err != nil {
		return err.Error(), true
	}
	b, err := json.Marshal(out)
	if err != nil {
		// 返回值不可序列化：退化为 Go 文本，仍属正常结果
		return fmt.Sprintf("%v", out), false
	}
	return string(b), false
}

// decide 按 rt.Stream 选择流式或非流式决策路径，把工具定义一并交给决策器
// （模型据此得知有哪些工具可用）。流式增量经 rt.OnChunk 实时透出，
// 推理增量以 reasoning=true 标记。
func decide(ctx context.Context, rt *driver.RunContext, sess *session.Session, defs []llm.Tool) (planner.Action, error) {
	history := rt.History(sess)
	sp, streaming := rt.Planner.(planner.StreamingPlanner)
	if !rt.Stream || !streaming {
		return rt.Planner.NextAction(ctx, history, defs)
	}
	return sp.NextActionStream(ctx, history, defs, func(c llm.StreamChunk) error {
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
