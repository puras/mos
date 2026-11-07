package react

// 续跑（FR-G3 / S9.2）：把被中断的那一轮里的悬空调用接着走完。
//
// 前提：**宿主显式指示**。框架不自动拾起挂起的轮次（fail-closed 取向）——
// 没人指示就没人续；宿主先用 `session.PendingApprovals()` /
// `session.HangingCalls()` 看清现场，再决定要不要续。
//
// 语义：
//   - 沿用**原轮次号**（审批决定的复用键是 `(Turn, CallID)`）：同一轮里
//     已批过的决定能被复用，不再重复问人；
//   - 悬空调用的载荷**原样重放**（来自日志，不重写参数——载荷冻结契约）；
//   - 执行走与正常路径**同一条** execpath 通道（守卫裁决 / 审批 / 调度语义
//     一致）：续跑不是特权路径，策略照常管它；
//   - 回填**真实** tool/result（不是投影里那种合成失败回执）——模型看得见
//     「那次调用到底发生了什么」。

import (
	"context"
	"errors"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/execpath"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// ErrNothingToResume 表示没有可续的东西：没有被打断的轮次，或那一轮没有
// 悬空调用（都已回填）。
//
// **不是故障**：没人续是正常状态（框架不自动拾起）。宿主可以先查
// （session.PendingApprovals / session.HangingCalls）再决定。
var ErrNothingToResume = errors.New("react: 没有可续跑的悬空调用")

// resumeCalls 把那一轮的悬空调用接着走完：重走审批 → 执行 → 回填真实
// tool/result。
//
// 没有可续的调用 → ErrNothingToResume（那一轮**保持挂起**，不收尾——
// 宿主可能稍后再续；收尾等于把「还欠一次调用」的事实抹掉）。
func (d *Driver) resumeCalls(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, ids []string) error {
	calls := hangingCalls(sess, turn, ids)
	if len(calls) == 0 {
		return ErrNothingToResume
	}
	outcomes := execpath.ExecuteBatch(ctx, rt.Tools, calls, execOptions(rt, sess, turn))
	for _, oc := range outcomes {
		if _, err := sess.Append(session.Event{
			Kind: session.EventToolResult, Turn: turn,
			ToolResult: &session.ToolResult{CallID: oc.Call.ID, Content: oc.Content, IsError: oc.IsError},
		}); err != nil {
			return err
		}
	}
	return nil
}

// hangingCalls 取出要续的悬空调用（按日志序）。
//
// 载荷来自日志里的 tool/call 事件：**原样重放**（ID / 名字 / 参数都不改）。
func hangingCalls(sess *session.Session, turn int, ids []string) []llm.ToolCall {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]llm.ToolCall, 0, 4)
	for _, h := range sess.HangingCalls() {
		if h.Turn != turn {
			continue
		}
		if len(want) > 0 && !want[h.CallID] {
			continue
		}
		out = append(out, llm.ToolCall{ID: h.CallID, Name: h.ToolName, Args: h.Args})
	}
	return out
}

// execOptions 造执行通道的选项（守卫 / 钩子 / 审批通道）。
//
// 审批通道：宿主注入应答方，框架负责签发 id 与记审计。
// **没注入应答方就不装通道**（而不是装一个空的）——这样执行通道那边的
// 「未装配审批通道 → 拒绝」分支才真的走得到，拒绝理由里也带着
// "为什么要人工确认"。
//
// 先验决定（FR-G3 / S9.1）：命中就复用，不再问人。实现方在装配层
// （它才知道怎么从会话日志派生）。
func execOptions(rt *driver.RunContext, sess *session.Session, turn int) execpath.Options {
	opts := execpath.Options{
		Turn:        turn,
		Guards:      rt.Guards,
		Hooks:       rt.Hooks,
		Concurrency: rt.Concurrency,
	}
	if rt.Answerer != nil {
		svc := approval.NewService(rt.Answerer, sessionAudit{sess: sess})
		if rt.PriorApproval != nil {
			svc.SetPrior(func(ctx context.Context, req approval.Request) (approval.PriorDecision, bool) {
				return rt.PriorApproval(ctx, sess, req)
			})
		}
		opts.Approver = svc
	}
	return opts
}
