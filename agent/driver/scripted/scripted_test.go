package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// fakePlanner 是固定脚本的 ScriptPlanner（不依赖模型），供测试确定性编排。
type fakePlanner struct{ plan planner.ScriptedPlan }

func (f *fakePlanner) Script(context.Context, string, []llm.Message, []llm.Tool) (planner.ScriptedPlan, error) {
	return f.plan, nil
}

// synthModel 是合成轮用的假模型：固定返回一段文本。
type synthModel struct{ reply string }

func (m *synthModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Content: m.reply}}, nil
}
func (m *synthModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *synthModel) Capabilities() llm.Capability { return 0 }

// toolCallModel 是合成轮返回工具调用的假模型（触发 fail-closed）。
type toolCallModel struct{}

func (m *toolCallModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{
		Content:   "",
		ToolCalls: []llm.ToolCall{{ID: "t1", Name: "t1", Args: "{}"}},
	}}, nil
}
func (m *toolCallModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *toolCallModel) Capabilities() llm.Capability { return 0 }

// newSession 是测试固定 ID 的会话创建助手。
func newSession(id string) *session.Session { return session.NewSession(id) }

// TestScriptedDriverEndToEnd 验证 ScriptedDriver 完整流程：脚本生成 → 分波执行 →
// 合成回复 → 日志完整性（script/start + script/end + tool/call + tool/result + turn/end）。
func TestScriptedDriverEndToEnd(t *testing.T) {
	reg := tool.NewRegistry()
	for _, tl := range []tool.Tool{
		orderTool{name: "t1"}, orderTool{name: "t2"},
	} {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:   reg,
		Planner: planner.NewReAct(&synthModel{reply: "完成，共处理了 2 步。"}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal: "g",
			Steps: []planner.ScriptStep{
				{ID: "a", Tool: "t1"},
				{ID: "b", Tool: "t2", DependsOn: []string{"a"}},
			},
		}},
	}
	ag := New(rt)
	reply, err := ag.Run(context.Background(), sess, "g")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "完成") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	// 日志完整性：两个工具调用都落了 tool/call + tool/result，且 script/end 收尾
	logs := eventTypes(sess)
	for _, want := range []string{"script/start", "script/end", "tool/call", "tool/result", "turn/end"} {
		if !containsStr(logs, want) {
			t.Fatalf("log missing %q: %v", want, logs)
		}
	}
	// script/end 载荷含执行报告：每步状态 + 命名输出摘要（架构 v2 §6 契约）
	var scriptEnd event.ScriptEndData
	for _, ev := range sess.Events {
		if ev.Type != "script/end" {
			continue
		}
		if err := json.Unmarshal(ev.Data, &scriptEnd); err != nil {
			t.Fatalf("unmarshal script/end: %v", err)
		}
		break
	}
	if scriptEnd.Steps["a"] != "done" || scriptEnd.Steps["b"] != "done" {
		t.Fatalf("script/end Steps = %v, want a=done, b=done", scriptEnd.Steps)
	}
	if got := scriptEnd.Outputs["a"]; got != "result-t1" {
		t.Fatalf("script/end Outputs[a] = %#v, want result-t1", got)
	}
	if got := scriptEnd.Outputs["b"]; got != "result-t2" {
		t.Fatalf("script/end Outputs[b] = %#v, want result-t2", got)
	}
}

// TestScriptedDriverRejectsNoPlanner 验证 fail-closed：未配置 ScriptPlanner 时驱动拒绝运行。
func TestScriptedDriverRejectsNoPlanner(t *testing.T) {
	rt := &driver.RunContext{Tools: tool.NewRegistry()}
	ag := New(rt)
	_, err := ag.Run(context.Background(), newSession("s"), "g")
	if err == nil {
		t.Fatal("expected error when ScriptPlanner missing")
	}
}

// TestScriptedDriverSynthesisToolCallsFailClosed 验证合成轮返回工具调用 → fail-closed
// （不执行工具调用，记失败回填 + 状态 blocked）。
func TestScriptedDriverSynthesisToolCallsFailClosed(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(orderTool{name: "t1"})
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:   reg,
		Planner: planner.NewReAct(&toolCallModel{}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal:  "g",
			Steps: []planner.ScriptStep{{ID: "a", Tool: "t1"}},
		}},
	}
	ag := New(rt)
	if _, err := ag.Run(context.Background(), sess, "g"); err == nil {
		t.Fatal("expected error when synthesis returns tool calls")
	}
}

// 辅助：记录日志事件类型 / 判断包含。
func eventTypes(sess *session.Session) []string {
	out := make([]string, 0, len(sess.Events))
	for _, ev := range sess.Events {
		out = append(out, ev.Type)
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// ---- M2.8 suspend → saveCheckpoint → resume E2E ----

// countedTool 记录 Execute 调用次数（钉住 "已挂起步骤不重跑" 不变量）。
type countedTool struct {
	name  string
	calls atomic.Int32
}

func (c *countedTool) Name() string        { return c.name }
func (c *countedTool) Description() string { return c.name }
func (c *countedTool) Parameters() any     { return struct{}{} }
func (c *countedTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	c.calls.Add(1)
	return "result-" + c.name, nil
}

// requirePolicy 是按工具名触发 *approval.RequiredError 的策略（仅对指定工具名）。
// 其他工具直接放行，模拟 "只有特定危险工具需审批" 的真实场景。
type requirePolicy struct{ toolName string }

func (p requirePolicy) Allow(_ context.Context, call *llm.ToolCall) error {
	if call.Name == p.toolName {
		return &approval.RequiredError{
			Request: approval.ApprovalRequest{ToolName: call.Name, Reason: "needs approval"},
		}
	}
	return nil
}

// mockApproval 是 approval.Approval 的最小内存实现：每次 Request 分配 ID。
// 不实现 Immediate（Decision 返回 (_, false)），policyGuard 在第一轮走挂起路径。
// resume 轮靠 rt.ResumeCheckpoint.Decisions 优先放行，Approval.Request 不再被调用。
type mockApproval struct {
	mu    sync.Mutex
	calls int
}

func (m *mockApproval) Request(_ context.Context, _ approval.ApprovalRequest) (approval.ApprovalID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return approval.ApprovalID("ap-" + strconv.Itoa(m.calls)), nil
}

func (m *mockApproval) Resolve(_ context.Context, _ approval.ApprovalID, _ approval.ApprovalDecision) error {
	return nil
}

// TestScriptedDriverSuspendResumeEndToEnd 钉住 M2.8 脚本驱动挂起→恢复完整流：
//   - 3 步脚本 (a → b → c)；策略对工具 a 触发 *approval.RequiredError，
//     policyGuard 发起 Approval.Request → 无 Immediate 决策 → SuspendError；
//   - 第一次 Run 返回 SuspendError，checkpoint 落 MemStore，工具 a 未真正执行；
//   - 第二次 Run 注入 ResumeCheckpoint + Decisions[step:a]=allow，
//     resume 路径跳过 LLM 调用（不重发 script/start），直接重放挂起波后
//     续跑 b/c，最终脚本 a/b/c 均 done，tool/call+tool/result 只落一次。
func TestScriptedDriverSuspendResumeEndToEnd(t *testing.T) {
	aTool := &countedTool{name: "a"}
	reg := tool.NewRegistry()
	for _, tl := range []tool.Tool{aTool, orderTool{name: "b"}, orderTool{name: "c"}} {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	ap := &mockApproval{}
	store := checkpoint.NewMemStore()
	sess := newSession("s")

	plan := planner.ScriptedPlan{
		Goal: "g",
		Steps: []planner.ScriptStep{
			{ID: "a", Tool: "a"},
			{ID: "b", Tool: "b", DependsOn: []string{"a"}},
			{ID: "c", Tool: "c", DependsOn: []string{"b"}},
		},
	}
	rt := &driver.RunContext{
		Tools:         reg,
		Planner:       planner.NewReAct(&synthModel{reply: "全部完成"}),
		ScriptPlanner: &fakePlanner{plan: plan},
		Policy:        requirePolicy{toolName: "a"},
		Approval:      ap,
		Checkpoints:   store,
	}
	ag := New(rt)

	// ---- 第一次 Run：挂起 ----
	_, err := ag.Run(context.Background(), sess, "g")
	var suspendErr *approval.SuspendError
	if !errors.As(err, &suspendErr) {
		t.Fatalf("first Run err = %v, want SuspendError", err)
	}
	// 工具 a 在挂起点尚未真正执行
	if got := aTool.calls.Load(); got != 0 {
		t.Fatalf("after suspend: tool a Execute count = %d, want 0", got)
	}
	// checkpoint 落库
	saved, loadErr := store.Load(context.Background(), "s")
	if loadErr != nil {
		t.Fatalf("checkpoint not saved: %v", loadErr)
	}
	if saved.ScriptState == nil || len(saved.Calls) == 0 {
		t.Fatalf("checkpoint payload incomplete: ScriptState=%v Calls=%d", saved.ScriptState, len(saved.Calls))
	}
	// 挂起点脚本状态：step a 仍未 done（status=running，outputs 为空）
	if got := saved.ScriptState.Status["a"]; got == "done" {
		t.Fatalf("checkpoint ScriptState.Status[a] = %q, want non-done (still in-flight)", got)
	}

	// ---- 第二次 Run：从 checkpoint 恢复 ----
	saved.Decisions = map[string]approval.ApprovalDecision{
		"step:a": {Approved: true, DecidedBy: "test"},
	}
	rt.ResumeCheckpoint = saved

	reply, err := ag.Run(context.Background(), sess, "g")
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if !strings.Contains(reply, "全部完成") {
		t.Fatalf("unexpected reply: %q", reply)
	}

	// 工具 a 跨两次 Run 仅调用一次（resume 不重放已完成/已挂起步骤）
	if got := aTool.calls.Load(); got != 1 {
		t.Fatalf("tool a Execute count across both runs = %d, want 1", got)
	}

	// script/start 仅在首次 Run 发出（resume 不重发）
	var scriptStarts int
	for _, ev := range sess.Events {
		if ev.Type == event.SessionEventScriptStart {
			scriptStarts++
		}
	}
	if scriptStarts != 1 {
		t.Fatalf("script/start count = %d, want 1 (resume should not re-emit)", scriptStarts)
	}

	// script/end Steps 全部 done；Outputs 含三步命名结果
	var scriptEnd event.ScriptEndData
	var foundEnd bool
	for _, ev := range sess.Events {
		if ev.Type != event.SessionEventScriptEnd {
			continue
		}
		if err := json.Unmarshal(ev.Data, &scriptEnd); err != nil {
			t.Fatalf("unmarshal script/end: %v", err)
		}
		foundEnd = true
	}
	if !foundEnd {
		t.Fatal("script/end not found after resume")
	}
	for _, id := range []string{"a", "b", "c"} {
		if got := scriptEnd.Steps[id]; got != "done" {
			t.Fatalf("script/end Steps[%q] = %q, want done", id, got)
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := scriptEnd.Outputs[id]; !ok {
			t.Fatalf("script/end Outputs[%q] missing (have %v)", id, scriptEnd.Outputs)
		}
	}

	// checkpoint/restore 事件落在 resume 路径
	var restoreCount int
	for _, ev := range sess.Events {
		if ev.Type == event.SessionEventCheckpointRestore {
			restoreCount++
		}
	}
	if restoreCount != 1 {
		t.Fatalf("checkpoint/restore count = %d, want 1", restoreCount)
	}
}
