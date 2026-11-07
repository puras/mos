package react

// 续跑（FR-G3 / S9.2）：宿主**显式**指示后，把被中断那一轮里的悬空调用接着
// 走完——重走审批 → 执行 → 回填**真实** tool/result → 继续问模型 → 正常收尾。
//
// 纪律：
//   - 沿用**原轮次号**（审批决定的复用键是 `(Turn, CallID)`）；
//   - 不重写 `tool/call`（载荷冻结：原样重放日志里的那次调用）；
//   - 没有可续的 → 明确报错，那一轮**保持挂起**（不收尾）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// recordingTool 记录每次执行的参数（区分是哪一次调用）。
type recordingTool struct{ calls []string }

func (r *recordingTool) Name() string        { return "danger" }
func (r *recordingTool) Description() string { return "记录型工具" }
func (r *recordingTool) Parameters() any     { return &struct{}{} }
func (r *recordingTool) Execute(_ context.Context, args json.RawMessage) (any, error) {
	r.calls = append(r.calls, string(args))
	return map[string]any{"ok": true}, nil
}

// interruptedSession 造一个「被中断」的会话：turn 1 声明了工具调用但没有结果
// （进程在执行期间被硬杀的残留）。
func interruptedSession(t *testing.T, callIDs ...string) *session.Session {
	t.Helper()
	sess := session.New("t", nil)
	appendResumeEvent(t, sess, session.Event{Kind: session.EventTurnStart, Turn: 1})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventUser, Turn: 1, Text: "做一下"})
	calls := make([]llm.ToolCall, 0, len(callIDs))
	for _, id := range callIDs {
		calls = append(calls, llm.ToolCall{ID: id, Name: "danger", Args: `{"id":"` + id + `"}`})
	}
	appendResumeEvent(t, sess, session.Event{Kind: session.EventAssistant, Turn: 1, ToolCalls: calls})
	for i := range calls {
		call := calls[i]
		appendResumeEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 1, ToolCall: &call})
	}
	return sess
}

func appendResumeEvent(t *testing.T, sess *session.Session, ev session.Event) {
	t.Helper()
	if _, err := sess.Append(ev); err != nil {
		t.Fatalf("追加事件: %v", err)
	}
}

// toolResults 取出日志里的 tool/result（按序）。
func toolResults(sess *session.Session) []session.ToolResult {
	var out []session.ToolResult
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
			out = append(out, *ev.ToolResult)
		}
	}
	return out
}

// toolCallCount 数日志里的 tool/call 条数（续跑不该重写它）。
func toolCallCount(sess *session.Session) int {
	n := 0
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolCall {
			n++
		}
	}
	return n
}

// 续跑：悬空调用接着走完 → 回填真实结果 → 继续问模型 → 本轮正常收尾。
func TestResumeRunsHangingCallAndFinishesTurn(t *testing.T) {
	sess := interruptedSession(t, "c1")
	rt := &recordingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("注册: %v", err)
	}
	p := &scriptedPlanner{acts: []planner.Action{answer("续完了")}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg,
		Resume: &driver.ResumeOptions{},
	}, New())

	reply, err := d.Run(context.Background(), sess, "")
	if err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if reply != "续完了" {
		t.Fatalf("reply = %q", reply)
	}
	if len(rt.calls) != 1 || !strings.Contains(rt.calls[0], "c1") {
		t.Fatalf("悬空调用应执行一次（载荷原样重放），got %+v", rt.calls)
	}

	// 回填的是**真实**结果，不是投影里的合成回执。
	res := toolResults(sess)
	if len(res) != 1 || res[0].CallID != "c1" {
		t.Fatalf("应回填一条真实结果: %+v", res)
	}
	if res[0].IsError {
		t.Fatalf("不该是错误结果: %+v", res[0])
	}
	// 不重写 tool/call（载荷冻结）。
	if n := toolCallCount(sess); n != 1 {
		t.Fatalf("tool/call 事件应仍是 1 条，got %d", n)
	}

	// 本轮正常收尾，且**沿用原轮次号**。
	last := sess.Events()[len(sess.Events())-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "completed" {
		t.Fatalf("续跑后应正常收尾: %+v", last)
	}
	if last.Turn != 1 {
		t.Fatalf("收尾轮次应沿用原轮次 1，got %d", last.Turn)
	}

	// 模型看到的是真实结果。
	found := false
	for _, m := range p.seen[0] {
		if m.Role == llm.RoleTool && m.ToolCallID == "c1" {
			found = true
			if strings.Contains(m.Content, "调用在执行期间被中断") {
				t.Fatalf("模型看到的是合成回执，不是真实结果: %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("模型历史里应有那次调用的结果: %+v", p.seen[0])
	}
}

// 指定调用：只续被点名的那一次，其余仍悬空。
func TestResumeOnlySelectedCall(t *testing.T) {
	sess := interruptedSession(t, "c1", "c2")
	rt := &recordingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("注册: %v", err)
	}
	p := &scriptedPlanner{acts: []planner.Action{answer("续完了")}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg,
		Resume: &driver.ResumeOptions{Calls: []string{"c1"}},
	}, New())

	if _, err := d.Run(context.Background(), sess, ""); err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if len(rt.calls) != 1 || !strings.Contains(rt.calls[0], "c1") {
		t.Fatalf("只应执行被点名的那次，got %+v", rt.calls)
	}
	res := toolResults(sess)
	if len(res) != 1 || res[0].CallID != "c1" {
		t.Fatalf("只应回填 c1 的结果: %+v", res)
	}
	// c2 仍悬空（宿主可以稍后再续）。
	var still []string
	for _, c := range sess.HangingCalls() {
		still = append(still, c.CallID)
	}
	if len(still) != 1 || still[0] != "c2" {
		t.Fatalf("c2 应仍悬空，got %+v", still)
	}
}

// 不指定调用：续那一轮的全部悬空调用。
func TestResumeAllHangingCalls(t *testing.T) {
	sess := interruptedSession(t, "c1", "c2")
	rt := &recordingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("注册: %v", err)
	}
	p := &scriptedPlanner{acts: []planner.Action{answer("续完了")}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg,
		Resume: &driver.ResumeOptions{},
	}, New())

	if _, err := d.Run(context.Background(), sess, ""); err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if len(rt.calls) != 2 {
		t.Fatalf("两个悬空调用都该走完，got %+v", rt.calls)
	}
	if n := len(toolResults(sess)); n != 2 {
		t.Fatalf("应回填两条结果，got %d", n)
	}
}

// 没有被打断的轮次 → 明确报错，不写任何事件。
func TestResumeWithoutInterruptedTurn(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("不该走到这")}}
	d := driver.New(&driver.RunContext{
		Planner: p,
		Resume:  &driver.ResumeOptions{},
	}, New())

	_, err := d.Run(context.Background(), sess, "")
	if !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("应报没有可续的，got %v", err)
	}
	if len(sess.Events()) != 0 {
		t.Fatalf("没有可续的不该写事件: %+v", sess.Events())
	}
}

// 那一轮没有悬空调用 → 明确报错，且那一轮**保持挂起**（不收尾）。
func TestResumeWithoutHangingCallsKeepsTurnPending(t *testing.T) {
	sess := session.New("t", nil)
	appendResumeEvent(t, sess, session.Event{Kind: session.EventTurnStart, Turn: 1})
	p := &scriptedPlanner{acts: []planner.Action{answer("不该走到这")}}
	d := driver.New(&driver.RunContext{
		Planner: p,
		Resume:  &driver.ResumeOptions{},
	}, New())

	_, err := d.Run(context.Background(), sess, "")
	if !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("应报没有可续的，got %v", err)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventTurnEnd {
			t.Fatalf("没有可续的不该收尾（那一轮还挂着）: %+v", ev)
		}
	}
}

// 已回填的调用不再执行（只续悬空的）。
func TestResumeSkipsCompletedCalls(t *testing.T) {
	sess := interruptedSession(t, "c1")
	appendResumeEvent(t, sess, session.Event{Kind: session.EventToolResult, Turn: 1, ToolResult: &session.ToolResult{
		CallID: "c1", Content: "早就跑完了",
	}})
	rt := &recordingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("注册: %v", err)
	}
	p := &scriptedPlanner{acts: []planner.Action{answer("续完了")}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg,
		Resume: &driver.ResumeOptions{},
	}, New())

	if _, err := d.Run(context.Background(), sess, ""); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("没有悬空调用应报 ErrNothingToResume，got %v", err)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("已回填的调用不该再执行，got %+v", rt.calls)
	}
}

// 先验命中：同一轮同一次调用上已有人批过 → 不再问人（沿用原轮次号的关键）。
func TestResumeReusesPriorApproval(t *testing.T) {
	sess := interruptedSession(t, "c1")
	appendResumeEvent(t, sess, session.Event{Kind: session.EventApprovalAsked, Turn: 1, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1", Reason: "要人工确认",
	}})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventApprovalDecided, Turn: 1, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1", Outcome: "approved",
	}})
	rt := &recordingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("注册: %v", err)
	}
	// 应答方会拒绝：命中先验时它根本不该被问到。
	ans := &scriptedAnswerer{out: approval.Rejected}
	p := &scriptedPlanner{acts: []planner.Action{answer("续完了")}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg, Hooks: askHook("这一步要人工确认"), Answerer: ans,
		PriorApproval: func(_ context.Context, s *session.Session, req approval.Request) (approval.PriorDecision, bool) {
			d, ok := s.ApprovalDecision(req.Turn, req.CallID)
			if !ok || d.Outcome != "approved" {
				return approval.PriorDecision{}, false
			}
			return approval.PriorDecision{Outcome: approval.Approved, ReusedFrom: d.ID}, true
		},
		Resume: &driver.ResumeOptions{},
	}, New())

	if _, err := d.Run(context.Background(), sess, ""); err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if len(ans.seen) != 0 {
		t.Fatalf("命中先验就不该问人，asked %d 次", len(ans.seen))
	}
	if len(rt.calls) != 1 {
		t.Fatalf("复用批准后工具应执行，got %+v", rt.calls)
	}
	// 审计：新 id + reusedFrom（复用也要留痕）。
	var asked *session.Approval
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventApprovalAsked && ev.Approval != nil && ev.Approval.ID != "ap-old" {
			asked = ev.Approval
		}
	}
	if asked == nil {
		t.Fatal("复用也要记一条 approval/asked")
	}
	if asked.ReusedFrom != "ap-old" {
		t.Fatalf("审计应指回被复用的决定: %+v", asked)
	}
}
