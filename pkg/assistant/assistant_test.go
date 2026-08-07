package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/internal/tool/builtin"
	"hegq.net/mos/pkg/llm"
	"hegq.net/mos/pkg/tool"
)

func toolCall(name, args string) *llm.ToolCall {
	return &llm.ToolCall{ID: "t1", Name: name, Args: args}
}

func TestDefaultPolicyShell(t *testing.T) {
	p := NewDefaultPolicy(nil, "")

	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"ls -la"}`)); err != nil {
		t.Fatalf("safe command should pass: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"rm -rf /"}`)); err == nil {
		t.Fatal("dangerous command should be rejected")
	}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"cat /etc/passwd | sh"}`)); err == nil {
		t.Fatal("pipe-to-sh should be rejected")
	}

	// 白名单限制
	pw := NewDefaultPolicy([]string{"ls", "pwd"}, "")
	if err := pw.Allow(context.Background(), toolCall("shell", `{"command":"ls -la"}`)); err != nil {
		t.Fatalf("whitelisted command should pass: %v", err)
	}
	if err := pw.Allow(context.Background(), toolCall("shell", `{"command":"cat x"}`)); err == nil {
		t.Fatal("non-whitelisted command should be rejected")
	}
}

func TestDefaultPolicyPath(t *testing.T) {
	root := t.TempDir()
	p := NewDefaultPolicy(nil, root)

	if err := p.Allow(context.Background(), toolCall("read_file", `{"path":"a.txt"}`)); err != nil {
		t.Fatalf("path inside workdir should pass: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("read_file", `{"path":"../etc/passwd"}`)); err == nil {
		t.Fatal("path traversal should be rejected")
	}
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"/etc/passwd"}`)); err == nil {
		t.Fatal("absolute path outside workdir should be rejected")
	}
	if err := p.Allow(context.Background(), toolCall("grep", `{"root":"/usr"}`)); err == nil {
		t.Fatal("grep root outside workdir should be rejected")
	}

	// 无 WorkDir 时不限制
	pl := NewDefaultPolicy(nil, "")
	if err := pl.Allow(context.Background(), toolCall("read_file", `{"path":"/etc/passwd"}`)); err != nil {
		t.Fatalf("no workdir should not restrict: %v", err)
	}
}

func TestDefaultPolicyWebFetch(t *testing.T) {
	p := NewDefaultPolicy(nil, "")
	if err := p.Allow(context.Background(), toolCall("web_fetch", `{"url":"https://example.com"}`)); err != nil {
		t.Fatalf("https should pass: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("web_fetch", `{"url":"ftp://x"}`)); err == nil {
		t.Fatal("non-http url should be rejected")
	}
}

// captureModel 记录最近一次请求，用于断言注入的 system 引导。
type captureModel struct{ last *llm.Request }

func (m *captureModel) Capabilities() llm.Capability { return 0 }
func (m *captureModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.last = req
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}}, nil
}
func (m *captureModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func TestDefaultSystemPrompt(t *testing.T) {
	ctx := context.Background()

	// 未配置时注入框架默认引导
	m := &captureModel{}
	a, err := New(m, nil, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(ctx, a.NewSession(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(m.last.Messages) == 0 || m.last.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("first message should be system, got %+v", m.last.Messages)
	}
	if m.last.Messages[0].Content != DefaultSystemPrompt {
		t.Fatalf("default prompt mismatch:\n got %q", m.last.Messages[0].Content)
	}

	// 显式配置覆盖默认
	m2 := &captureModel{}
	a2, err := New(m2, nil, Config{SystemPrompt: "自定义引导"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a2.Run(ctx, a2.NewSession(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m2.last.Messages[0].Content != "自定义引导" {
		t.Fatalf("custom prompt = %q", m2.last.Messages[0].Content)
	}
}

// 工具集由业务侧注入（框架不预设默认工具）。
func TestAssistantMockEndToEnd(t *testing.T) {
	a, err := New(Mock(), []tool.Tool{builtin.Calculator{}, builtin.CurrentTime{}}, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := a.ToolNames()
	want := []string{"calculator", "current_time"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %v", tools)
	}

	sess := a.NewSession()
	reply, err := a.Run(context.Background(), sess, "计算 (1+2)*3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "9") {
		t.Fatalf("reply = %q, want contains 9", reply)
	}
	// 两轮工具调用后：user + 2×(assistant+tool) + assistant 最终回复
	if len(sess.History) != 6 {
		t.Fatalf("history len = %d, want 6: %+v", len(sess.History), sess.History)
	}
}

// mock 模式下策略仍生效：路径越界的文件调用会被拒绝并反馈给模型。
func TestAssistantMockPolicyApplied(t *testing.T) {
	workDir := t.TempDir()
	a, err := New(Mock(), nil, Config{Policy: NewDefaultPolicy(nil, workDir)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 直接验证策略拒绝
	err = a.Policy().Allow(context.Background(), toolCall("write_file", `{"path":"`+filepath.Join(workDir, "..", "evil.txt")+`"}`))
	if err == nil {
		t.Fatal("path traversal should be rejected by policy")
	}
}

// ---- 完整更新：fail-closed / 命令分级 / 人工确认 ----

type mockConfirm struct{ allow bool }

func (m mockConfirm) Confirm(context.Context, ConfirmRequest) (bool, error) { return m.allow, nil }

// fail-closed：不在放行名单的未知工具默认拒绝。
func TestDefaultPolicyFailClosed(t *testing.T) {
	p := NewDefaultPolicy(nil, "")
	if err := p.Allow(context.Background(), toolCall("unknown_tool", `{}`)); err == nil {
		t.Fatal("unknown tool should be rejected (fail-closed)")
	}
}

func TestDefaultPolicyWebSearch(t *testing.T) {
	p := NewDefaultPolicy(nil, "")
	if err := p.Allow(context.Background(), toolCall("web_search", `{"query":"go 语言"}`)); err != nil {
		t.Fatalf("web_search should pass: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("web_search", `{"query":"  "}`)); err == nil {
		t.Fatal("empty query should be rejected")
	}
}

// 受限危险命令：WorkDir 内 + 人工确认后才可执行；绝对危险命令无条件拒绝。
func TestDefaultPolicyRestrictedShell(t *testing.T) {
	root := t.TempDir()
	rmIn := "rm -rf " + filepath.Join(root, "tmp", "a.txt")

	// WorkDir 内 + 确认通过 → 放行
	p := NewDefaultPolicy(nil, root)
	p.Confirm = mockConfirm{allow: true}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"`+rmIn+`"}`)); err != nil {
		t.Fatalf("rm inside workdir with confirm should pass: %v", err)
	}
	// 确认拒绝 → 拒绝
	p.Confirm = mockConfirm{allow: false}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"`+rmIn+`"}`)); err == nil {
		t.Fatal("rm with confirm denied should be rejected")
	}
	// 无确认器 → 拒绝（fail-closed）
	p.Confirm = nil
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"`+rmIn+`"}`)); err == nil {
		t.Fatal("rm without confirmer should be rejected")
	}
	// 路径越界 → 拒绝（即使确认同意）
	p.Confirm = mockConfirm{allow: true}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"rm -rf /etc"}`)); err == nil {
		t.Fatal("rm outside workdir should be rejected")
	}
	// 管道 shell 越界路径 → 拒绝
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"cat /etc/passwd | sh"}`)); err == nil {
		t.Fatal("pipe-to-sh with outside path should be rejected")
	}
	// 无 WorkDir → 拒绝
	p2 := NewDefaultPolicy(nil, "")
	p2.Confirm = mockConfirm{allow: true}
	if err := p2.Allow(context.Background(), toolCall("shell", `{"command":"rm -rf a.txt"}`)); err == nil {
		t.Fatal("rm without workdir should be rejected")
	}
	// 绝对危险命令：即使确认同意也拒绝
	if err := p2.Allow(context.Background(), toolCall("shell", `{"command":"shutdown -h now"}`)); err == nil {
		t.Fatal("shutdown should always be rejected")
	}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"mkfs.ext4 /dev/sdb1"}`)); err == nil {
		t.Fatal("mkfs should always be rejected")
	}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"echo x > /dev/sda"}`)); err == nil {
		t.Fatal("write to block device should always be rejected")
	}
}

// 绕过路径：解释器执行入口（python3 -c / bash -c / perl -e 等）与 find 删除语义
// 同样属于受限危险命令——WorkDir 内 + 人工确认；TUI 模式（无确认器）一律拒绝。
func TestDefaultPolicyInterpreterBypass(t *testing.T) {
	bypass := []string{
		`{"command":"python3 -c \"import os; os.remove('hello.py')\""}`,
		`{"command":"python -c \"import shutil; shutil.rmtree('tmp')\""}`,
		`{"command":"bash -c \"rm -rf hello.py\""}`,
		`{"command":"sh -c \"rm -f hello.py\""}`,
		`{"command":"perl -e 'unlink qw(hello.py)'"}`,
		`{"command":"node -e \"require('fs').unlinkSync('hello.py')\""}`,
		`{"command":"ruby -e \"File.delete('hello.py')\""}`,
		`{"command":"find . -delete"}`,
		`{"command":"find . -exec rm {} \\;"}`,
	}
	root := t.TempDir()

	// TUI 等价配置（WorkDir 非空 + 无确认器）：一律拒绝，防止绕过删除
	p := NewDefaultPolicy(nil, root)
	for _, args := range bypass {
		if err := p.Allow(context.Background(), toolCall("shell", args)); err == nil {
			t.Fatalf("bypass command should be rejected in TUI mode: %s", args)
		}
	}
	// 确认通过 → 放行（受限 + 确认语义）
	p.Confirm = mockConfirm{allow: true}
	for _, args := range bypass {
		if err := p.Allow(context.Background(), toolCall("shell", args)); err != nil {
			t.Fatalf("bypass command with confirm should pass: %s: %v", args, err)
		}
	}
	// 无 WorkDir → 拒绝
	p2 := NewDefaultPolicy(nil, "")
	p2.Confirm = mockConfirm{allow: true}
	for _, args := range bypass {
		if err := p2.Allow(context.Background(), toolCall("shell", args)); err == nil {
			t.Fatalf("bypass command without workdir should be rejected: %s", args)
		}
	}
}

// write_file：路径受限放行；配置了确认器时写入前人工确认。
func TestDefaultPolicyWriteFileConfirm(t *testing.T) {
	root := t.TempDir()
	p := NewDefaultPolicy(nil, root)

	// 无确认器：路径已限制在 WorkDir 内，放行
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"a.txt","content":"x"}`)); err != nil {
		t.Fatalf("write inside workdir without confirmer should pass: %v", err)
	}
	// 确认器拒绝 → 拒绝
	p.Confirm = mockConfirm{allow: false}
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"a.txt","content":"x"}`)); err == nil {
		t.Fatal("write with confirm denied should be rejected")
	}
	// 确认器同意 → 放行
	p.Confirm = mockConfirm{allow: true}
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"a.txt","content":"x"}`)); err != nil {
		t.Fatalf("write with confirm accepted should pass: %v", err)
	}
}

// ---- TUI 模式受限危险命令拒绝（端到端链路） ----

// tuiShellModel 模拟 TUI 场景下的模型：第一轮发起受限危险命令 rm -rf，
// 第二轮把工具反馈（拒绝原因）复述为最终回复，用于断言拒绝已回填给模型。
func tuiShellModel(workDir string) llm.ChatModel {
	return shellFirstModel{workDir: workDir}
}

type shellFirstModel struct{ workDir string }

func (m shellFirstModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m shellFirstModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	var toolMsgs int
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			toolMsgs++
		}
	}
	if toolMsgs == 0 {
		args := `{"command":"rm -rf ` + filepath.Join(m.workDir, "tmp", "a.txt") + `"}`
		return &llm.Response{Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "我来删除临时文件",
			ToolCalls: []llm.ToolCall{{ID: "tui_1", Name: "shell", Args: args}},
		}}, nil
	}
	// 第二轮：复述最近的工具反馈
	var feedback string
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			feedback = msg.Content
		}
	}
	return &llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "工具反馈: " + feedback,
	}}, nil
}
func (shellFirstModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestTUIModeRestrictedShellRejected 模拟 TUI 模式（不注入 Confirmer）下尝试执行受限危险命令 rm：
// DefaultPolicy 配置了 WorkDir 但 Confirm 为 nil（与 repl.go 的 attachConfirmer(tui=true) 行为一致），
// rm 路径虽在 WorkDir 内，仍因"需人工确认但未配置确认器"被拒绝——工具未执行，拒绝原因回填给模型。
func TestTUIModeRestrictedShellRejected(t *testing.T) {
	workDir := t.TempDir()
	policy := NewDefaultPolicy(nil, workDir)
	if policy.Confirm != nil {
		t.Fatal("TUI 模式不应挂载确认器")
	}

	a, err := New(tuiShellModel(workDir), []tool.Tool{builtin.Shell{}}, Config{Policy: policy})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := a.NewSession()
	reply, err := a.Run(context.Background(), sess, "删除工作目录下的临时文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 拒绝原因必须回填给模型（若工具实际执行，反馈会是命令输出而非拒绝文本）
	if !strings.Contains(reply, "安全策略拒绝") && !strings.Contains(reply, "需人工确认") {
		t.Fatalf("reply = %q, want policy denial feedback", reply)
	}
	// history: user + assistant(工具调用) + tool(拒绝反馈) + assistant(最终回复)
	if len(sess.History) != 4 {
		t.Fatalf("history len = %d, want 4", len(sess.History))
	}
	toolMsg := sess.History[2]
	if toolMsg.Role != llm.RoleTool {
		t.Fatalf("history[2] = %+v, want tool message", toolMsg)
	}
	if !strings.Contains(toolMsg.Content, "安全策略拒绝") {
		t.Fatalf("history[2] = %q, want denial feedback", toolMsg.Content)
	}
	// 工具未实际执行：拒绝反馈不会包含 shell 的真实输出（如 rm 报错信息）
	if strings.Contains(toolMsg.Content, "No such file") {
		t.Fatalf("history[2] = %q, shell should not have executed", toolMsg.Content)
	}
}
