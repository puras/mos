package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// countingReadFile 记录实际执行次数（验证截断时工具不执行）。
type countingReadFile struct{ n int }

func (c *countingReadFile) Name() string        { return "read_file" }
func (c *countingReadFile) Description() string { return "读取文件内容" }
func (c *countingReadFile) Parameters() any     { return &calcParams{} }
func (c *countingReadFile) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	c.n++
	return map[string]any{"sum": 3}, nil
}

// truncModel 第一轮返回被截断的响应（finish_reason=length + 工具调用），第二轮正常作答。
type truncModel struct{ n int }

func (m *truncModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *truncModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	m.n++
	if m.n == 1 {
		return &llm.Response{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				Content:   "被截断",
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: `{"a":1,"b":2}`}},
			},
			FinishReason: "length",
		}, nil
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "完成"}, FinishReason: "stop"}, nil
}
func (m *truncModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestTruncatedToolCallFailClosed：输出被截断时，已产出的工具调用不执行、
// 显式失败回填（模型面有 tool/result 错误），循环继续到下一轮正常完成。
func TestTruncatedToolCallFailClosed(t *testing.T) {
	calc := &countingReadFile{}
	reg := tool.NewRegistry()
	if err := reg.Register(calc); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := New(&RunContext{
		Tools:    reg,
		Planner:  planner.NewReAct(&truncModel{}),
		MaxTurns: 5,
	})

	sess := session.NewSession("s1")
	out, err := a.Run(context.Background(), sess, "计算")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "完成" {
		t.Fatalf("最终作答应来自第二轮: %q", out)
	}
	if calc.n != 0 {
		t.Fatalf("截断产生的工具调用不应执行，实际执行 %d 次", calc.n)
	}

	// 模型面必须能重建出截断的失败结果（不变式：tool/call 必有 tool/result 回填）
	msgs := sess.Messages()
	var toolErrs int
	for _, m := range msgs {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "截断") {
			toolErrs++
		}
	}
	if toolErrs != 1 {
		t.Fatalf("应有一条截断失败的工具结果，实际 %d", toolErrs)
	}
}

// TestTruncatedFinishDetection 覆盖协议停止原因映射（length / max_tokens / 正常）。
func TestTruncatedFinishDetection(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"length", true},
		{"max_tokens", true},
		{"MAX_TOKENS", true}, // gemini 流式转小写后
		{"stop", false},
		{"end_turn", false},
		{"tool_use", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isTruncatedFinish(c.reason); got != c.want {
			t.Fatalf("isTruncatedFinish(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}
