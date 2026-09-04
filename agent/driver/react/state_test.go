package react

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// 已验证事实：只读工具（readFile）结果经审计门写入任务状态，模型自述不写入。
func TestTaskStateVerifiedFact(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(readFile{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	var facts []agent.Event
	a := newReAct(mockModel{}, reg, func(rt *driver.RunContext) {
		rt.Observer = func(ev agent.Event) {
			if ev.Type == agent.EventFact {
				facts = append(facts, ev)
			}
		}
	})

	sess := session.NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "1+2=3?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sess.State.Goal != "1+2=3?" {
		t.Fatalf("goal = %q", sess.State.Goal)
	}
	if sess.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed", sess.State.Status)
	}
	if len(sess.State.Facts) != 1 {
		t.Fatalf("facts = %+v, want 1 verified fact", sess.State.Facts)
	}
	f := sess.State.Facts[0]
	if f.Tool != "read_file" || !strings.Contains(f.Summary, "3") {
		t.Fatalf("fact = %+v", f)
	}
	// 审计事实事件已广播
	if len(facts) != 1 {
		t.Fatalf("agent.EventFact count = %d, want 1", len(facts))
	}
	if facts[0].Fact == nil || facts[0].Fact.Summary != f.Summary {
		t.Fatalf("agent.EventFact = %+v", facts[0])
	}
}

// 未核实声明：非白名单（变更类）工具的输出不写入任务状态。
func TestTaskStateIgnoresUnverified(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(patcher{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	a := newReAct(mutatingModel{}, reg, nil)
	sess := session.NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "写文件 B"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sess.State.Facts) != 0 {
		t.Fatalf("facts = %+v, want 0（未核实声明不得入状态）", sess.State.Facts)
	}
}

// 多轮任务：已验证事实跨轮累积，原始目标只记录一次；相同证据自动去重。
func TestTaskStateAcrossRuns(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(readFile{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(varyingModel{}, reg, nil)
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "第一题"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), sess, "第二题"); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	if sess.State.Goal != "第一题" {
		t.Fatalf("goal = %q, want 首轮输入", sess.State.Goal)
	}
	// 两轮证据不同（3 与 30）→ 两条事实
	if len(sess.State.Facts) != 2 {
		t.Fatalf("facts = %+v, want 2", sess.State.Facts)
	}
	// 状态摘要应包含全部已验证事实
	snap := sess.State.Snapshot()
	if !strings.Contains(snap, "已验证事实") || strings.Count(snap, "read_file") != 2 {
		t.Fatalf("snapshot = %q", snap)
	}
}

// 相同工具 + 相同证据的事实只记录一次（审计报告去重）。
func TestTaskStateDedup(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(readFile{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(mockModel{}, reg, nil)
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "一"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), sess, "二"); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	// mock 两轮调用参数相同，证据相同 → 去重为 1 条
	if len(sess.State.Facts) != 1 {
		t.Fatalf("facts = %+v, want 1（证据去重）", sess.State.Facts)
	}
}

// 达到最大迭代次数：任务状态置为 blocked。
func TestTaskStateBlockedOnMaxTurns(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(patcher{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(alwaysCallModel{}, reg, func(rt *driver.RunContext) { rt.MaxTurns = 2 })

	sess := session.NewSession("s1")
	_, err := a.Run(context.Background(), sess, "永远不完成的任务")
	if err == nil {
		t.Fatal("want max iterations error")
	}
	if sess.State.Status != session.StatusBlocked {
		t.Fatalf("status = %v, want blocked", sess.State.Status)
	}
}

// ---- 测试用工具与模型 ----

// patcher 是变更类工具：执行"写入"但输出属于未核实声明。
type patcher struct{}

func (patcher) Name() string        { return "patcher" }
func (patcher) Description() string { return "写入文件（变更类）" }
func (patcher) Parameters() any {
	return &struct {
		Path string `json:"path"`
	}{}
}
func (patcher) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "文件 B 已写入（自述）", nil
}

// varyingModel 每轮以不同参数调用 readFile（证据不同），两轮后作答。
type varyingModel struct{}

func (varyingModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (varyingModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	var toolRounds int
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			toolRounds++
		}
	}
	if toolRounds >= 2 {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "全部完成",
		}}, nil
	}
	args := `{"a":1,"b":2}`
	if toolRounds == 1 {
		args = `{"a":10,"b":20}`
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "我来算",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: args}},
	}}, nil
}
func (varyingModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// mutatingModel 首轮调用变更类工具 patcher，随后直接作答。
type mutatingModel struct{}

func (mutatingModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (mutatingModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "文件 B 已生成",
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "我来写文件",
		ToolCalls: []llm.ToolCall{{ID: "p1", Name: "patcher", Args: `{"path":"B"}`}},
	}}, nil
}
func (mutatingModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// alwaysCallModel 永远返回工具调用，用于触发最大迭代次数。
type alwaysCallModel struct{}

func (alwaysCallModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (alwaysCallModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "继续",
		ToolCalls: []llm.ToolCall{{ID: "p1", Name: "patcher", Args: `{"path":"B"}`}},
	}}, nil
}
func (alwaysCallModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}
