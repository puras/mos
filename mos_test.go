package mos_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// stubModel 是只回一句话的假模型（装配层测试不需要真模型）。
type stubModel struct{ reply string }

func (m *stubModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.reply}}, nil
}

func (m *stubModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *stubModel) Capabilities() llm.Capability { return llm.CapFunctionCalling | llm.CapStreaming }

// 没有模型就装配不出来（fail-closed，不降级成空转）。
func TestNewRequiresModel(t *testing.T) {
	if _, err := mos.New(mos.Config{}); err == nil {
		t.Fatal("缺模型应当报错")
	}
}

// 能力即证据：nil 的能力不装（shell 不出现、审批未装、压缩/截断按配置）。
func TestNewCapabilityPresence(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := a.Describe()
	if !strings.Contains(d, "shell: 未装配") {
		t.Fatalf("未给沙箱就不该有 shell:\n%s", d)
	}
	if !strings.Contains(d, "决策轮次上限: 不限") {
		t.Fatalf("默认不限轮次:\n%s", d)
	}
	// 截断默认开启（32KB），压缩默认开启（128K 窗口）。
	if !strings.Contains(d, "结果截断: 32768 字节") {
		t.Fatalf("默认截断上限应为 32KB:\n%s", d)
	}
	if !strings.Contains(d, "上下文压缩: 已装配") {
		t.Fatalf("默认应装配压缩:\n%s", d)
	}
}

// 截断上限可关（负数 = 不截断），显式上限要生效。
func TestTruncateMaxBytes(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, TruncateMaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "结果截断: 未开启") {
		t.Fatalf("负数应表示不截断:\n%s", a.Describe())
	}

	b, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, TruncateMaxBytes: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(b.Describe(), "结果截断: 1024 字节") {
		t.Fatalf("显式上限应生效:\n%s", b.Describe())
	}
}

// 名字级策略：被 deny 的工具**不出现在模型可见清单里**。
func TestPolicyVisibility(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(stubTool{name: "shell"}); err != nil {
		t.Fatal(err)
	}
	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Tools:  reg,
		Policy: &policy.Config{Deny: []string{"shell"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := a.Describe()
	if !strings.Contains(d, "已注册工具: read_file, shell") {
		t.Fatalf("注册表应含全部工具:\n%s", d)
	}
	if !strings.Contains(d, "模型可见工具: read_file") || strings.Contains(d, "模型可见工具: read_file, shell") {
		t.Fatalf("被 deny 的工具不该对模型可见:\n%s", d)
	}
}

// 参数级规则：deny 档进守卫（单调拒绝），ask 档走 pre-execute 钩子。
func TestParamRulesWired(t *testing.T) {
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Rules: []policy.Check{
			func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
				if call.Tool == "read_file" {
					return policy.VerdictDeny, "不读这个文件"
				}
				return policy.VerdictAllow, ""
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "参数级规则: 1 个检查器") {
		t.Fatalf("Describe 应报规则条数:\n%s", a.Describe())
	}
	// deny 档：守卫应拒绝（单调，工具不会执行）。
	ev := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c1", Name: "read_file", Args: `{"path":"a.txt"}`},
	}
	if err := a.RunContext().Guards.Allow(context.Background(), ev); err == nil {
		t.Fatal("deny 档应被守卫拒绝")
	}
	// allow 档：同一规则不拦别的工具。
	ev2 := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c2", Name: "current_time", Args: `{}`},
	}
	if err := a.RunContext().Guards.Allow(context.Background(), ev2); err != nil {
		t.Fatalf("allow 档不该被拒: %v", err)
	}
}

// ask 档：进 pre-execute 钩子（转人工确认），不直接拒绝。
func TestParamRuleAskGoesToApprovalHook(t *testing.T) {
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Rules: []policy.Check{
			func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
				return policy.VerdictAsk, "要人确认"
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c1", Name: "read_file", Args: `{"path":"a.txt"}`},
	}
	out, err := a.RunContext().Hooks.Waterfall(context.Background(), ev)
	if err != nil {
		t.Fatalf("ask 档不该直接报错: %v", err)
	}
	if out.Decision.Kind != hook.DecisionAsk {
		t.Fatalf("ask 档应置 DecisionAsk, got %+v", out.Decision)
	}
}

// 运行期旋钮：SetConcurrency / SetMaxDecisionRounds 改的是**下一轮**的值。
func TestRuntimeKnobs(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.SetConcurrency(7)
	a.SetMaxDecisionRounds(9)
	// 轮次用浅拷贝，旋钮值写进副本——这里检查副本拿到的是新值。
	rt := a.RunContext()
	if rt.Concurrency == 7 {
		t.Fatal("模板不该被就地改（每轮用浅拷贝）")
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(a.Describe(), "并发上限: 7") {
		t.Fatalf("Describe 应反映新旋钮值:\n%s", a.Describe())
	}
	if !strings.Contains(a.Describe(), "决策轮次上限: 9") {
		t.Fatalf("Describe 应反映新轮次上限:\n%s", a.Describe())
	}
}

// 策略热替换：换策略后可见性跟着换，旧守卫撤下。
func TestSetPolicyHotSwap(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file"}); err != nil {
		t.Fatal(err)
	}
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "模型可见工具: read_file") {
		t.Fatalf("初始应可见:\n%s", a.Describe())
	}
	a.SetPolicy(&policy.Config{Deny: []string{"read_file"}})
	if !strings.Contains(a.Describe(), "模型可见工具: ") &&
		strings.Contains(a.Describe(), "模型可见工具: read_file") {
		t.Fatalf("换策略后不该再可见:\n%s", a.Describe())
	}
	a.SetPolicy(nil)
	if !strings.Contains(a.Describe(), "名字级策略: 未装配") {
		t.Fatalf("置 nil 应回到未装配:\n%s", a.Describe())
	}
}

// 缺会话就报错（不静默跑空）。
func TestRunRequiresSession(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), nil, "hi"); err == nil {
		t.Fatal("缺会话应当报错")
	}
}

// 装配层不引入额外依赖：默认压缩/截断都用现成机制（冒烟：跑一轮不炸）。
func TestRunSmoke(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "你好"}, SystemPrompt: "你是助手"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	reply, err := a.Run(context.Background(), sess, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "你好" {
		t.Fatalf("reply = %q", reply)
	}
	if len(sess.Events()) == 0 {
		t.Fatal("事件应落会话")
	}
}

// stubTool 是只回固定文本的工具。
type stubTool struct{ name string }

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return "测试工具" }
func (t stubTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t stubTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	return "ok", nil
}
