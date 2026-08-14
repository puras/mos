package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hegq.net/mos/llm"
	"hegq.net/mos/llm/openai"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// mockModel 模拟：首轮返回工具调用，收到工具结果后返回最终答案。
type mockModel struct{}

func (mockModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (mockModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "结果是 " + last.Content,
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:             llm.RoleAssistant,
		Content:          "我来调用工具",
		ReasoningContent: "先思考再行动",
		ToolCalls:        []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}
func (mockModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

type calcParams struct {
	A int `json:"a" mos:"desc=加数"`
	B int `json:"b" mos:"desc=被加数"`
}

type calculator struct{}

func (calculator) Name() string        { return "calculator" }
func (calculator) Description() string { return "计算两个整数之和" }
func (calculator) Parameters() any     { return &calcParams{} }
func (calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

// newReAct 便捷装配：默认 ReActDriver + 可选 RunContext 定制。
func newReAct(model llm.ChatModel, reg *tool.Registry, cfg func(*RunContext)) *Agent {
	rt := &RunContext{Tools: reg, Planner: planner.NewReAct(model)}
	if cfg != nil {
		cfg(rt)
	}
	return New(rt)
}

func TestAgentReActLoop(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	var events []Event
	a := newReAct(mockModel{}, reg, func(rt *RunContext) {
		rt.Observer = func(ev Event) { events = append(events, ev) }
		rt.SystemPrompt = "你是助手"
	})

	sess := NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "1+2=3?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}

	// 消息历史应为：user, assistant(工具调用), tool(结果), assistant(最终)
	msgs := sess.Messages()
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("msgs[1] = %+v", msgs[1])
	}
	// 工具调用轮的推理内容必须保留（thinking 模式多轮须回传）
	if msgs[1].ReasoningContent != "先思考再行动" {
		t.Fatalf("msgs[1].ReasoningContent = %q, want 先思考再行动", msgs[1].ReasoningContent)
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].Content != "3" {
		t.Fatalf("msgs[2] = %+v", msgs[2])
	}

	// 事件序列：思考 → 工具调用 → 工具结果 → 审计事实 → 思考 → 完成
	var gotTypes []EventType
	for _, ev := range events {
		gotTypes = append(gotTypes, ev.Type)
	}
	want := []EventType{EventStep, EventToolCall, EventToolResult, EventFact, EventStep, EventDone}
	if len(gotTypes) != len(want) {
		t.Fatalf("events = %v", gotTypes)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("events = %v, want %v", gotTypes, want)
		}
	}
	if events[len(events)-1].Turn != 2 {
		t.Fatalf("final turn = %d, want 2", events[len(events)-1].Turn)
	}
}

// streamingModel 支持流式：文本增量逐字发出，首轮附工具调用。
type streamingModel struct{}

func (streamingModel) Capabilities() llm.Capability { return llm.CapFunctionCalling | llm.CapStreaming }
func (streamingModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("streaming model should not be called non-streaming")
}
func (streamingModel) ChatStream(_ context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		text := "结果是 " + last.Content
		for _, r := range text {
			if err := emit(llm.StreamChunk{Delta: string(r)}); err != nil {
				return nil, err
			}
		}
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}, nil
	}
	text := "我来调用工具"
	for _, r := range text {
		if err := emit(llm.StreamChunk{Delta: string(r)}); err != nil {
			return nil, err
		}
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   text,
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}

// TestAgentRunStream 验证流式入口：增量经 EventStream 逐段广播，
// 聚合后增量拼接即最终回复，且回退/非流式事件序列不受影响。
func TestAgentRunStream(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	var events []Event
	a := newReAct(streamingModel{}, reg, func(rt *RunContext) {
		rt.Observer = func(ev Event) { events = append(events, ev) }
	})
	sess := NewSession("s1")
	reply, err := a.RunStream(context.Background(), sess, "1+2=3?")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}

	var streamText string
	var gotTypes []EventType
	for _, ev := range events {
		gotTypes = append(gotTypes, ev.Type)
		if ev.Type == EventStream {
			streamText += ev.Delta
		}
	}
	if streamText != "我来调用工具结果是 3" {
		t.Fatalf("stream text = %q", streamText)
	}
	// 事件序列：首轮增量×6 → 思考 → 工具调用 → 结果 → 事实 → 二轮增量×5 → 思考 → 完成
	want := []EventType{
		EventStream, EventStream, EventStream, EventStream, EventStream, EventStream,
		EventStep, EventToolCall, EventToolResult, EventFact,
		EventStream, EventStream, EventStream, EventStream, EventStream,
		EventStep, EventDone,
	}
	if len(gotTypes) != len(want) {
		t.Fatalf("events = %v", gotTypes)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("events = %v", gotTypes)
		}
	}
}

// TestAgentRunStreamFallback 验证无流式能力时回退：每轮一次性补齐完整内容
// （而非逐段增量），回复正确且最终事件仍为 EventDone。
func TestAgentRunStreamFallback(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	var events []Event
	a := newReAct(mockModel{}, reg, func(rt *RunContext) {
		rt.Observer = func(ev Event) { events = append(events, ev) }
	})
	sess := NewSession("s1")
	reply, err := a.RunStream(context.Background(), sess, "1+2=3?")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}
	// 回退 = 每轮恰好 1 次完整 delta（两轮），非流式模型不做逐字增量
	var streamEvents []string
	for _, ev := range events {
		if ev.Type == EventStream {
			streamEvents = append(streamEvents, ev.Delta)
		}
	}
	want := []string{"我来调用工具", "结果是 3"}
	if len(streamEvents) != len(want) {
		t.Fatalf("fallback stream events = %v, want %v", streamEvents, want)
	}
	for i := range want {
		if streamEvents[i] != want[i] {
			t.Fatalf("fallback stream events = %v, want %v", streamEvents, want)
		}
	}
}

// denyPolicy 拒绝所有工具调用。
type denyPolicy struct{}

func (denyPolicy) Allow(_ context.Context, call *llm.ToolCall) error {
	return fmt.Errorf("工具 %s 未授权", call.Name)
}

func TestAgentPolicyDenial(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(mockModel{}, reg, func(rt *RunContext) {
		rt.Policy = denyPolicy{}
	})

	sess := NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "1+2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// mock 会将工具（拒绝）结果原样返回，应包含拒绝原因
	if !strings.Contains(reply, "未授权") {
		t.Fatalf("reply = %q, want policy denial", reply)
	}
	// 工具未实际执行：msgs[2] 是拒绝反馈
	msgs := sess.Messages()
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	if !strings.Contains(msgs[2].Content, "未授权") {
		t.Fatalf("msgs[2] = %+v", msgs[2])
	}
}

// 端到端：真实 openai 适配器 + agent 两轮，验证 thinking 模式的
// reasoning_content 在第 2 轮请求中原样回传。
func TestEndToEndReasoningRoundTrip(t *testing.T) {
	var round int
	var secondReqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		b, _ := io.ReadAll(r.Body)
		if round == 2 {
			secondReqBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			// 第 1 轮：thinking 模式返回推理内容 + 工具调用
			io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"我来计算","reasoning_content":"需要知道当前时间","tool_calls":[{"id":"c1","type":"function","function":{"name":"calculator","arguments":"{\"a\":1,\"b\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		} else {
			// 第 2 轮：直接作答
			io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"结果是 3","reasoning_content":"拿到结果后回答"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`)
		}
	}))
	defer srv.Close()

	model, err := openai.New(llm.Config{"api_key": "test-key", "base_url": srv.URL, "model": "deepseek-v4-flash"})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}

	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(model, reg, nil)

	sess := NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "1+2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}
	// 第 2 轮请求必须携带第 1 轮的 reasoning_content
	if !strings.Contains(secondReqBody, `"reasoning_content":"需要知道当前时间"`) {
		t.Fatalf("第2轮请求缺少 reasoning_content 回传，请求体: %s", secondReqBody)
	}
}

// 多轮对话应保留历史，且 system 引导不进入会话事件日志。
func TestAgentMultiTurn(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(mockModel{}, reg, func(rt *RunContext) {
		rt.SystemPrompt = "你是助手"
	})
	sess := NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "第一轮"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), sess, "第二轮"); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	for _, m := range sess.Messages() {
		if m.Role == llm.RoleSystem {
			t.Fatal("system message should not persist in session log")
		}
	}
	if len(sess.Messages()) != 8 {
		t.Fatalf("messages len = %d, want 8 (两轮各 4 条)", len(sess.Messages()))
	}
}

// 平台断点续跑契约：Save → Restore → 继续 Run，消息与状态正确累积。
func TestSessionRestoreResume(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := newReAct(mockModel{}, reg, nil)

	sess := NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "第一轮"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(sess.Messages()) != 4 {
		t.Fatalf("messages after run 1 = %d, want 4", len(sess.Messages()))
	}

	data, err := sess.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	restored, err := RestoreSession(data)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if _, err := a.Run(context.Background(), restored, "第二轮"); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(restored.Messages()) != 8 {
		t.Fatalf("messages after resume = %d, want 8", len(restored.Messages()))
	}
	if restored.State.Goal != "第一轮" {
		t.Fatalf("goal after resume = %q, want 第一轮（首轮目标保留）", restored.State.Goal)
	}
	if restored.State.Status != StatusCompleted {
		t.Fatalf("status after resume = %v, want completed", restored.State.Status)
	}
}
