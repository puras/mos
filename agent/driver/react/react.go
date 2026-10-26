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
	"errors"
	"fmt"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/execpath"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// ErrToolsUnsupported 表示模型请求了工具调用，但 RunContext 未装配工具注册表。
var ErrToolsUnsupported = errors.New("react: 模型请求工具调用，但 RunContext.Tools 未装配")

// ErrMaxDecisionRounds 表示单轮内决策次数耗尽仍未产出最终作答。
//
// 上限可配（RunContext.MaxDecisionRounds，默认 128）。这是**保险丝**而非
// 正常路径：空转应由连续相同调用的提醒（advisory）打断，而不是烧到上限。
var ErrMaxDecisionRounds = errors.New("react: 单轮决策次数超出上限（工具可能在空转）")

const (
	// defaultMaxDecisionRounds 是单 turn 内允许的最大决策次数（含最终作答）。
	// S6 的 compaction 落地后计划放开为不限（对齐 PI/DSH），旋钮保留。
	defaultMaxDecisionRounds = 128

	// repeatRemindAt 与 repeatRemindEvery 控制连续相同调用的提醒节奏：
	// 第 3 次起提醒，之后每 3 次再提一次（advisory，不 veto——DSH 的
	// repeat-tool-reminder 同一哲学：空转靠提醒模型改变策略，不靠硬杀）。
	repeatRemindAt    = 3
	repeatRemindEvery = 3
)

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

	maxRounds := rt.MaxDecisionRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxDecisionRounds
	}

	// 连续相同调用检测（S6 DoomLoopGuard 的最小版，advisory）：同一
	// (tool, args) 连续出现达阈值时，在该次 tool/result 末尾附加提醒，
	// 让模型改变策略或向用户说明。不拦、不改——只提醒。
	var lastCallKey string
	var repeats int

	for round := 0; round < maxRounds; round++ {
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

		// 截断的输出带的工具调用**不可信**：参数可能是半截 JSON，甚至整个
		// 参数都没传到（finish 为空 = 适配器未收到结束信号——正常应已被
		// ErrStreamTruncated 拦下，这里是双保险）。fail-closed：不执行，逐条
		// 回填错误结果（保持 assistant↔tool/result 配对合法），模型可重新发起。
		if truncatedFinish(action.FinishReason) {
			for _, call := range action.ToolCalls {
				if _, err := sess.Append(session.Event{
					Kind: session.EventToolResult, Turn: turn,
					ToolResult: &session.ToolResult{
						CallID:  call.ID,
						Content: fmt.Sprintf("模型输出被截断（finish_reason=%s），此工具调用的参数可能不完整，已拒绝执行。请重新发起该调用", action.FinishReason),
						IsError: true,
					},
				}); err != nil {
					return "", err
				}
			}
			continue
		}

		// 执行统一走 execpath 通道（守卫裁决 / 调度 / 回填顺序的全库唯一语义）；
		// 驱动只负责把结果落成 tool/result 事件。
		opts := execpath.Options{
			Turn:        turn,
			Guards:      rt.Guards,
			Hooks:       rt.Hooks,
			Concurrency: rt.Concurrency,
		}
		// 审批通道：宿主注入应答方，框架负责签发 id 与记审计。
		// **没注入应答方就不装通道**（而不是装一个空的）——这样执行通道那边
		// 的「未装配审批通道 → 拒绝」分支才真的走得到，拒绝理由里也带着
		// "为什么要人工确认"。
		if rt.Answerer != nil {
			opts.Approver = approval.NewService(rt.Answerer, sessionAudit{sess: sess})
		}
		outcomes := execpath.ExecuteBatch(ctx, rt.Tools, action.ToolCalls, opts)
		terminated := false
		for _, oc := range outcomes {
			content := oc.Content
			// 连续相同调用：计数并在阈值时附提醒（advisory）。
			key := oc.Call.Name + "\x00" + oc.Call.Args
			if key == lastCallKey {
				repeats++
			} else {
				lastCallKey = key
				repeats = 1
			}
			if repeats >= repeatRemindAt && (repeats-repeatRemindAt)%repeatRemindEvery == 0 {
				content += fmt.Sprintf("\n\n[提醒] 这已是连续第 %d 次发起完全相同的调用。若它一直失败或结果不符预期，请改变策略（换方法/先收集更多信息），或直接向用户说明现状。", repeats)
			}
			if _, err := sess.Append(session.Event{
				Kind: session.EventToolResult, Turn: turn,
				ToolResult: &session.ToolResult{CallID: oc.Call.ID, Content: content, IsError: oc.IsError},
			}); err != nil {
				return "", err
			}
			if oc.Terminate {
				terminated = true
			}
		}
		// 扩展点请求终止本轮：收尾并返回本轮内容（不再回到决策器）。
		if terminated {
			if _, err := sess.Append(session.Event{
				Kind: session.EventTurnEnd, Turn: turn, Reason: "terminated",
				Error: terminateDetail(outcomes),
			}); err != nil {
				return "", err
			}
			return action.Content, nil
		}
		// 回到循环顶部：把工具结果投影进历史，再让决策器决策
	}
	return "", d.abort(sess, turn, fmt.Errorf("%w: 已达 %d 次决策上限", ErrMaxDecisionRounds, maxRounds))
}

// abort 以 error 原因收尾本轮并返回原错误（落盘失败时原样返回，收尾已尽力）。
//
// 错误详情一并落进 turn/end：分类说"失败了"，详情说"为什么"——审计时能直接
// 看出是模型不可用、证书过期还是守卫拦截，不必再去翻终端输出。
func (d *Driver) abort(sess *session.Session, turn int, cause error) error {
	_, _ = sess.Append(session.Event{
		Kind: session.EventTurnEnd, Turn: turn, Reason: "error",
		Error: session.ErrorDetail(cause),
	})
	return cause
}

// terminateDetail 取出终止本轮那条调用的拒绝理由（作为 turn/end 的详情）。
// 理由已经在 tool/result 里落过一次，这里是给收尾事件补一句可读的来由。
func terminateDetail(outcomes []execpath.Outcome) string {
	for _, oc := range outcomes {
		if oc.Terminate {
			return oc.Content
		}
	}
	return ""
}

// truncatedFinish 判断停止原因是否意味着"输出被截断"。
//
// length/max_tokens 是明确的截断信号；空值不该出现（适配器保证收到结束
// 信号，否则报 ErrStreamTruncated），出现也按截断处理——宁可少执行一次，
// 不执行一个参数残缺的调用。
func truncatedFinish(finish string) bool {
	switch finish {
	case "", "length", "max_tokens":
		return true
	}
	return false
}

// sessionAudit 把审批审计写进会话日志。
//
// 两条事件配对：approval/asked（问了）与 approval/decided（答了），用同一个
// 请求 id 串起来——事后能看出"谁批的、批了哪次调用、等了多久"。
type sessionAudit struct{ sess *session.Session }

// Asked 记录「问了人工」。
func (a sessionAudit) Asked(req approval.Request) error {
	_, err := a.sess.Append(session.Event{
		Kind: session.EventApprovalAsked, Turn: req.Turn,
		Approval: &session.Approval{
			ID: req.ID, ToolName: req.ToolName, CallID: req.CallID, Reason: req.Reason,
		},
	})
	return err
}

// Decided 记录「人工答了」。
func (a sessionAudit) Decided(req approval.Request, out approval.Outcome) error {
	_, err := a.sess.Append(session.Event{
		Kind: session.EventApprovalDecided, Turn: req.Turn,
		Approval: &session.Approval{
			ID: req.ID, ToolName: req.ToolName, CallID: req.CallID, Outcome: out.String(),
		},
	})
	return err
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
