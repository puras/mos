package react

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
	"hegq.net/mos/tool/builtin"
	fs "hegq.net/mos/filesystem"
)

// newTestTruncator 构造小上限截断器，便于在测试中触发截断。
func newTestTruncator(t *testing.T, maxBytes, maxLines int) (agent.ResultTruncator, string) {
	t.Helper()
	dir := t.TempDir()
	return agent.NewDefaultTruncator(agent.TruncatorConfig{MaxBytes: maxBytes, MaxLines: maxLines, Dir: dir}), dir
}

func TestTruncatorWithinLimits(t *testing.T) {
	tr, _ := newTestTruncator(t, 4096, 100)
	content := strings.Repeat("line\n", 10)
	out, truncated := tr.Truncate("read_file", content)
	if truncated {
		t.Fatalf("未超限却发生截断")
	}
	if out != content {
		t.Fatalf("未超限内容被改动: %q", out)
	}
}

func TestTruncatorHeadTailStrategy(t *testing.T) {
	tr, _ := newTestTruncator(t, 0, 5) // 仅行数限制

	// head 类（read_file）：保留头部，尾部被省略
	lines := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		lines = append(lines, "line_"+strconv.Itoa(i))
	}
	content := strings.Join(lines, "\n")
	out, truncated := tr.Truncate("read_file", content)
	if !truncated {
		t.Fatalf("超限未截断")
	}
	if !strings.Contains(out, "line_0") || !strings.Contains(out, "line_1") {
		t.Fatalf("head 截断未保留头部: %q", out)
	}
	if strings.Contains(out, "line_19") {
		t.Fatalf("head 截断不应保留尾部: %q", out)
	}

	// tail 类（shell）：保留尾部（报错在最后），头部被省略
	out, truncated = tr.Truncate("shell", content)
	if !truncated {
		t.Fatalf("超限未截断")
	}
	if !strings.Contains(out, "line_19") {
		t.Fatalf("tail 截断未保留尾部: %q", out)
	}
	if strings.Contains(out, "line_0") {
		t.Fatalf("tail 截断不应保留头部: %q", out)
	}

	// 通用（write_file）：head+tail 各保留一半
	out, truncated = tr.Truncate("write_file", content)
	if !truncated {
		t.Fatalf("超限未截断")
	}
	if !strings.Contains(out, "line_0") || !strings.Contains(out, "line_19") {
		t.Fatalf("通用截断应保留 head+tail: %q", out)
	}
	if !strings.Contains(out, "中间省略") {
		t.Fatalf("截断文本缺少省略标记: %q", out)
	}
}

func TestTruncatorByteLimitUTF8Safe(t *testing.T) {
	// 多字节中文内容，字节上限落在字符中间时不得切出非法 UTF-8
	content := strings.Repeat("中文字符", 50) // 每个字 3 字节
	tr, _ := newTestTruncator(t, 100, 0)
	out, truncated := tr.Truncate("web_fetch", content)
	if !truncated {
		t.Fatalf("超字节未截断")
	}
	if !strings.Contains(out, "中") {
		t.Fatalf("截断后应保留完整中文字符: %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("截断产生非法 UTF-8: %q", out)
	}
	// 截断体（逃生通道提示之前）不得超过"内容上限 + 截断标记"的预算
	body := strings.TrimSuffix(out[:strings.Index(out, "[工具输出已截断")], "\n\n")
	if len([]byte(body)) > 100+len("\n... [输出过长已截断] ...\n") {
		t.Fatalf("截断体仍超预算: %d", len([]byte(body)))
	}
}

// 逃生通道闭环：截断落盘后，模型可用 read_file 按偏移回读完整内容。
func TestTruncatorEscapeHatchReRead(t *testing.T) {
	lines := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		lines = append(lines, "full_line_"+strconv.Itoa(i))
	}
	full := strings.Join(lines, "\n")
	tr, spillDir := newTestTruncator(t, 0, 10)
	out, truncated := tr.Truncate("read_file", full)
	if !truncated {
		t.Fatalf("超限未截断")
	}
	if !strings.Contains(out, "完整输出已保存至") || !strings.Contains(out, "read_file") {
		t.Fatalf("逃生通道提示缺失: %q", out)
	}

	// 从提示中提取落盘路径，用 read_file 回读验证完整性
	pathStart := strings.Index(out, "已保存至 ") + len("已保存至 ")
	pathEnd := strings.Index(out[pathStart:], "，")
	spillPath := out[pathStart : pathStart+pathEnd]

	// read_file 经 ctx 取 fs capability（file-tool-jail）；注入 jail 到截断器落盘目录。
	fsCtx := fs.WithFileSystem(t.Context(), &fs.Sandboxed{Policy: &fs.FileAccessPolicy{
		WorkspaceRoot: spillDir, JailReads: true, JailWrites: true,
	}})
	resp, err := builtin.ReadFile{}.Execute(fsCtx, json.RawMessage(`{"path":"`+spillPath+`"}`))
	if err != nil {
		t.Fatalf("read_file 回读失败: %v", err)
	}
	m := resp.(map[string]any)
	if got := m["content"].(string); got != full {
		t.Fatalf("回读内容与完整输出不一致（len %d vs %d）", len(got), len(full))
	}
	if size := int(m["size"].(int64)); size != len(full) {
		t.Fatalf("落盘文件大小不一致: %d vs %d", size, len(full))
	}
}

// ---- 集成测试：截断只作用于模型可见文本，Auditor 仍收到完整原始结果 ----

// bigOutputTool 返回超限大文本的工具（名称为 head 类 read_file）。
type bigOutputTool struct{}

func (bigOutputTool) Name() string        { return "read_file" }
func (bigOutputTool) Description() string { return "测试大输出" }
func (bigOutputTool) Parameters() any     { return &struct{}{} }
func (bigOutputTool) Execute(context.Context, json.RawMessage) (any, error) {
	return strings.Repeat("data_line\n", 100), nil
}

// bigOutputModel 模拟：首轮调用 read_file 工具，收到工具结果后返回最终答案。
type bigOutputModel struct{}

func (bigOutputModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (bigOutputModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "读取文件",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: `{}`}},
	}}, nil
}
func (bigOutputModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// captureAuditor 捕获审计时收到的原始结果。
type captureAuditor struct {
	gotRaw any
}

func (c *captureAuditor) Audit(_ context.Context, _ *llm.ToolCall, result any) ([]session.Fact, error) {
	c.gotRaw = result
	return nil, nil
}

func TestTruncatorInAgentKeepsAuditRaw(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(bigOutputTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	aud := &captureAuditor{}
	tr, _ := newTestTruncator(t, 0, 10)
	a := newReAct(bigOutputModel{}, reg, func(rt *driver.RunContext) {
		rt.SystemPrompt = "你是助手"
		rt.Auditor = aud
		rt.Truncator = tr
	})

	sess := session.NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "读文件"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 模型可见消息中的 tool 消息应被截断（含逃生通道提示）
	msgs := sess.Messages()
	var toolMsg *session.InternalMessage
	for i := range msgs {
		if msgs[i].Role == llm.RoleTool {
			toolMsg = &msgs[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("历史中缺少 tool 消息")
	}
	if !strings.Contains(toolMsg.Content, "完整输出已保存至") {
		t.Fatalf("tool 消息应被截断并携带逃生通道提示: %q", toolMsg.Content)
	}
	if strings.Contains(toolMsg.Content, "data_line\n") == false || strings.Count(toolMsg.Content, "data_line") > 15 {
		t.Fatalf("tool 消息应仅保留截断后的部分: %q", toolMsg.Content)
	}

	// Auditor 必须拿到完整原始结果（截断不得影响审计）
	raw, ok := aud.gotRaw.(string)
	if !ok {
		t.Fatalf("auditor 收到非 string 结果: %T", aud.gotRaw)
	}
	if strings.Count(raw, "data_line") != 100 {
		t.Fatalf("auditor 应收完整 100 行，实际 %d 行", strings.Count(raw, "data_line"))
	}
}

// 逃生通道落盘路径应位于配置的目录下。
func TestTruncatorSpillDir(t *testing.T) {
	tr, dir := newTestTruncator(t, 0, 3)
	_, truncated := tr.Truncate("shell", strings.Repeat("x\n", 10))
	if !truncated {
		t.Fatalf("超限未截断")
	}
	entries, err := filepath.Glob(filepath.Join(dir, "mos-trunc", "mos-shell_*.txt"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("逃生通道未写入配置目录: %v", err)
	}
}
