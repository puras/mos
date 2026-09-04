package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/execpath"
	"github.com/puras/mos/agent/executor"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
)

// batchModel 首轮返回多个工具调用，收到全部工具结果后返回汇总答案。
type batchModel struct {
	calls []llm.ToolCall
}

func (batchModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m batchModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "全部完成 " + last.Content,
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "批量调用工具",
		ToolCalls: m.calls,
	}}, nil
}
func (batchModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestBatchToolCalls 验证多工具调用批量执行：全部执行、tool/result 按模型顺序回填、
// 日志 Seq 连续（架构 v2 §3.9 finalize 按源序回填）。
func TestBatchToolCalls(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "c1", Name: "read_file", Args: `{"a":1,"b":2}`},
		{ID: "c2", Name: "read_file", Args: `{"a":3,"b":4}`},
		{ID: "c3", Name: "read_file", Args: `{"a":5,"b":6}`},
	}
	reg := tool.NewRegistry()
	if err := reg.Register(readFile{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	a := newReAct(batchModel{calls: calls}, reg, func(rt *driver.RunContext) {
		rt.MaxToolParallel = 3
	})

	sess := session.NewSession("batch")
	reply, err := a.Run(context.Background(), sess, "批量计算")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "全部完成 11" { // 最后一个工具结果 = 5+6
		t.Fatalf("reply = %q", reply)
	}

	// 日志：3 × tool/call + 3 × tool/result，结果顺序 = 模型调用顺序
	var callsLog, resultsLog []string
	for _, ev := range sess.Events {
		switch ev.Type {
		case event.SessionEventToolCall:
			var d event.ToolCallData
			_ = json.Unmarshal(ev.Data, &d)
			callsLog = append(callsLog, d.CallID)
		case event.SessionEventToolResult:
			var d event.ToolResultData
			_ = json.Unmarshal(ev.Data, &d)
			resultsLog = append(resultsLog, fmt.Sprintf("%s=%v", d.CallID, d.Result))
		}
	}
	if len(callsLog) != 3 || len(resultsLog) != 3 {
		t.Fatalf("calls=%v results=%v", callsLog, resultsLog)
	}
	for i, id := range []string{"c1", "c2", "c3"} {
		if callsLog[i] != id {
			t.Fatalf("calls order = %v", callsLog)
		}
	}
	wantResults := []string{"c1=3", "c2=7", "c3=11"}
	for i, r := range wantResults {
		if resultsLog[i] != r {
			t.Fatalf("results order = %v, want %v", resultsLog, wantResults)
		}
	}
	// 模型面回填顺序与日志一致（user → assistant(calls=3) → tool×3 → 最终回复）
	msgs := sess.Messages()
	if len(msgs) != 6 {
		t.Fatalf("messages = %d, want 6", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 3 {
		t.Fatalf("assistant ToolCalls = %d, want 3", len(msgs[1].ToolCalls))
	}
	for i, m := range msgs[2:5] {
		if m.Role != llm.RoleTool {
			t.Fatalf("msgs[%d].Role = %v, want tool", i+1, m.Role)
		}
	}
}

// denyC2Policy 拒绝 ID=c2 的工具调用（守卫拦截语义）。
type denyC2Policy struct{}

func (denyC2Policy) Allow(_ context.Context, call *llm.ToolCall) error {
	if call.ID == "c2" {
		return errors.New("c2 不在放行名单")
	}
	return nil
}

// TestBatchToolDenied 验证守卫拦截：被拒调用显式失败回填，其余正常执行。
func TestBatchToolDenied(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "c1", Name: "read_file", Args: `{"a":1,"b":2}`},
		{ID: "c2", Name: "read_file", Args: `{"a":3,"b":4}`},
		{ID: "c3", Name: "read_file", Args: `{"a":5,"b":6}`},
	}
	reg := tool.NewRegistry()
	_ = reg.Register(readFile{})
	a := newReAct(batchModel{calls: calls}, reg, func(rt *driver.RunContext) {
		rt.Policy = denyC2Policy{}
	})

	sess := session.NewSession("deny")
	reply, err := a.Run(context.Background(), sess, "批量计算")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "全部完成 11" {
		t.Fatalf("reply = %q", reply)
	}
	var denied bool
	for _, ev := range sess.Events {
		if ev.Type != event.SessionEventToolResult {
			continue
		}
		var d event.ToolResultData
		_ = json.Unmarshal(ev.Data, &d)
		if d.CallID == "c2" {
			denied = true
			if d.Error == "" || d.Result != nil {
				t.Fatalf("c2 result = %+v, want denied error", d)
			}
		}
	}
	if !denied {
		t.Fatal("c2 未被拦截回填")
	}
}

// sleepTool 是带延迟的普通工具（模拟耗时执行，用于区分并发/串行）。
type sleepTool struct {
	mu       sync.Mutex
	segments []time.Time
}

func (*sleepTool) Name() string        { return "sleep_tool" }
func (*sleepTool) Description() string { return "耗时工具" }
func (*sleepTool) Parameters() any     { return &calcParams{} }
func (s *sleepTool) Execute(_ context.Context, args json.RawMessage) (any, error) {
	now := time.Now()
	s.mu.Lock()
	s.segments = append(s.segments, now)
	s.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	now = time.Now()
	s.mu.Lock()
	s.segments = append(s.segments, now)
	s.mu.Unlock()
	return "ok", nil
}

// sequentialTool 是声明串行执行模式的工具（对齐 PI per-tool executionMode="sequential"）。
type sequentialTool struct {
	mu       sync.Mutex
	segments []time.Time
}

func (*sequentialTool) Name() string        { return "sequential_tool" }
func (*sequentialTool) Description() string { return "串行执行工具" }
func (*sequentialTool) Parameters() any     { return &calcParams{} }
func (*sequentialTool) Sequential() bool    { return true }
func (s *sequentialTool) Execute(_ context.Context, args json.RawMessage) (any, error) {
	now := time.Now()
	s.mu.Lock()
	s.segments = append(s.segments, now)
	s.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	now = time.Now()
	s.mu.Lock()
	s.segments = append(s.segments, now)
	s.mu.Unlock()
	return "ok", nil
}

// TestBatchSequentialTool 验证 SequentialTool 标记使整批回退串行：
// 三个 30ms 工具并发 ≈60ms、串行 ≈90ms，且 sequential 工具区间与相邻工具不重叠。
func TestBatchSequentialTool(t *testing.T) {
	reg := tool.NewRegistry()
	sl := &sleepTool{}
	st := &sequentialTool{}
	_ = reg.Register(sl)
	_ = reg.Register(st)

	calls := []llm.ToolCall{
		{ID: "c1", Name: "sleep_tool", Args: `{}`},      // 30ms
		{ID: "c2", Name: "sequential_tool", Args: `{}`}, // 30ms（声明串行 → 整批串行）
		{ID: "c3", Name: "sleep_tool", Args: `{}`},      // 30ms
	}
	a := newReAct(batchModel{calls: calls}, reg, func(rt *driver.RunContext) {
		rt.MaxToolParallel = 3
	})

	start := time.Now()
	if _, err := a.Run(context.Background(), session.NewSession("seq"), "串行批量"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 85*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 85ms（应整批串行）", elapsed)
	}
	// c1 与 c2 区间不重叠、c2 与 c3 区间不重叠（串行执行）
	_, c1e := sl.segments[0], sl.segments[1]
	c2s, c2e := st.segments[0], st.segments[1]
	c3s := sl.segments[2]
	if c2s.Before(c1e) {
		t.Fatalf("c2 start %v 与 c1 end %v 重叠（未串行）", c2s, c1e)
	}
	if c3s.Before(c2e) {
		t.Fatalf("c3 start %v 与 c2 end %v 重叠（未串行）", c3s, c2e)
	}
}

// TestBatchConcurrentAudit 验证并发工具执行 + 审计写 TaskState 无数据竞争
// （rt.AddFact 串行化；配合 -race 运行）。
func TestBatchConcurrentAudit(t *testing.T) {
	calls := make([]llm.ToolCall, 6)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprintf("c%d", i+1), Name: "read_file", Args: `{"a":1,"b":2}`}
	}
	reg := tool.NewRegistry()
	_ = reg.Register(readFile{})
	a := newReAct(batchModel{calls: calls}, reg, func(rt *driver.RunContext) {
		rt.MaxToolParallel = 6
	})

	var observed []agent.EventType
	var mu sync.Mutex
	obs := func(ev agent.Event) {
		mu.Lock()
		observed = append(observed, ev.Type)
		mu.Unlock()
	}
	rt := &driver.RunContext{Tools: reg, Planner: planner.NewReAct(batchModel{calls: calls}), Observer: obs}
	_ = a
	sess := session.NewSession("audit")
	rt.Session = sess
	if _, err := NewReAct(rt).Run(context.Background(), rt, "并发审计"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sess.State.Facts) == 0 {
		t.Fatal("并发审计未产生 Facts")
	}
	mu.Lock()
	var fact, result int
	for _, ty := range observed {
		switch ty {
		case agent.EventFact:
			fact++
		case agent.EventToolResult:
			result++
		}
	}
	mu.Unlock()
	if fact == 0 || result != 6 {
		t.Fatalf("fact=%d result=%d", fact, result)
	}
}

// TestBatchSuspendSkipsResultApply 钉住 Task 4 的有意设计选择（计划 Task 4 §Step 4 顺序调整说明）：
// 批内任一工具触发 SuspendError（来自执行路径，非守卫拦截）时，整批 tool/result 均不落日志。
// 取舍：resume 时 runResumeTurn 会重跑整批并按决策表放行；旧版行为会在挂起轮 + 恢复轮各落
// 一次 normal 工具的 tool/result，导致重复日志。新版在挂起点 saveCheckpoint 后直接冒泡，
// 恢复轮才完整落结果，日志只发生一次。
func TestBatchSuspendSkipsResultApply(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "c1", Name: "read_file", Args: `{}`},
		{ID: "c2", Name: "read_file", Args: `{}`},
		{ID: "c3", Name: "read_file", Args: `{}`},
	}
	suspend := &approval.SuspendError{
		ID:       "approval-1",
		Request:  approval.ApprovalRequest{ToolName: "read_file", Reason: "test"},
		CallID:   "c3",
		HookKind: hook.HookToolPreExecute,
	}
	outs := []executor.Outcome[*llm.ToolCall, execpath.ToolOutcome]{
		{Input: &calls[0], Output: execpath.ToolOutcome{Result: 1, Text: "1"}},
		{Input: &calls[1], Output: execpath.ToolOutcome{Result: 2, Text: "2"}},
		{Input: &calls[2], Output: execpath.ToolOutcome{Err: suspend}},
	}
	sess := session.NewSession("suspend-batch")
	rt := &driver.RunContext{
		Session: sess,
		Tools:   tool.NewRegistry(),
		Hooks:   hook.NewHookBus(),
		Guards:  hook.NewGuardBus(),
	}
	d := &ReActDriver{}
	_, _, err := d.finalizeToolBatch(context.Background(), rt, sess, 1, 1, calls, &planner.Action{}, outs)
	if !errors.Is(err, suspend) {
		t.Fatalf("err = %v, want SuspendError", err)
	}
	// 整批 tool/result 应一条都没有：suspend 优先于 ApplyOutcomes
	var leaked int
	for _, ev := range sess.Events {
		if ev.Type == event.SessionEventToolResult {
			leaked++
		}
	}
	if leaked != 0 {
		t.Fatalf("tool/result leaked under suspend: %d events", leaked)
	}
}
