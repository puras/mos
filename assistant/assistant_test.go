package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/memory"
	"hegq.net/mos/tool"
	"hegq.net/mos/tool/builtin"
	fs "hegq.net/mos/filesystem"
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
	a, err := New(m, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}})
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
	a2, err := New(m2, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, SystemPrompt: "自定义引导"})
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

// ---- M2.6 系统提示词动态组装装配 ----

// 配置 SkillsDir / ProjectDocDir 时自动构建默认槽位组装器：
// 系统约束（默认引导）+ 任务定义 + 当前状态 + 工具签名 + 技能清单。
func TestAssistantSystemAssemblerAuto(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	skillDir := filepath.Join(root, "web-debug")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: web-debug\ndescription: 排查网页抓取失败\n---\n步骤化正文，仅触发时读取"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	m := &captureModel{}
	a, err := New(m, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, SkillsDir: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(ctx, a.NewSession(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := m.last.Messages[0].Content
	for _, want := range []string{"【系统约束】", "【任务定义】", "【技能清单】", "web-debug"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system = %q, want contains %q", sys, want)
		}
	}
	// 技能正文不注入（懒加载清单）
	if strings.Contains(sys, "仅触发时读取") {
		t.Fatalf("skill body should not be injected: %q", sys)
	}
}

// 显式注入自定义组装器时直接生效，SystemPrompt 不参与组装。
func TestAssistantSystemAssemblerCustom(t *testing.T) {
	ctx := context.Background()
	custom := agent.SystemAssemblerFunc(func(_ context.Context, _ *session.Session, _ []llm.Tool) ([]agent.SystemSection, error) {
		return []agent.SystemSection{{Title: "自定义", Content: "来自应用层"}}, nil
	})

	m := &captureModel{}
	a, err := New(m, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, Assembler: custom, SystemPrompt: "不应出现"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(ctx, a.NewSession(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := m.last.Messages[0].Content
	if !strings.Contains(sys, "来自应用层") || strings.Contains(sys, "不应出现") {
		t.Fatalf("system = %q, want custom assembler output", sys)
	}
}

// ---- M2.6 Skills 强化 Task 7：activate_skill 自动注册 ----

// 配置 SkillsDir 时自动注册 activate_skill（Task 7）：模型可调用，
// 工具名出现在 ToolNames() 中。
func TestAssistant_RegistersActivateSkill(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "git")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: git\ndescription: 提交辅助\n---\n# Body\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a, err := New(&captureModel{}, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, SkillsDir: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	names := a.ToolNames()
	var found bool
	for _, n := range names {
		if n == "activate_skill" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("activate_skill should be registered, got tools: %v", names)
	}
}

// DisableActivateSkill=true 时不注册（即使 SkillsDir 非空）。
func TestAssistant_DisableActivateSkill(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "git")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: git\ndescription: 提交辅助\n---\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a, err := New(&captureModel{}, nil, Config{
		FileAccess:           FileAccessConfig{WorkspaceRoot: t.TempDir()},
		SkillsDir:            root,
		DisableActivateSkill: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, n := range a.ToolNames() {
		if n == "activate_skill" {
			t.Errorf("activate_skill should NOT be registered when DisableActivateSkill=true")
		}
	}
}

// SkillsDir 为空时不注册 activate_skill（无 Skills 来源也无意义）。
func TestAssistant_NoSkillsDir_NoActivateSkill(t *testing.T) {
	a, err := New(&captureModel{}, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, n := range a.ToolNames() {
		if n == "activate_skill" {
			t.Errorf("activate_skill should NOT be registered without SkillsDir")
		}
	}
}

// ---- M2.7 长期记忆 RAG 检索注入装配 ----

// stubRetriever 固定返回预设分块的检索器，供装配层验证。
type stubRetriever struct{ docs []memory.Document }

func (s *stubRetriever) Retrieve(_ context.Context, _ string, _ int) ([]memory.Document, error) {
	return s.docs, nil
}

// 配置 Retriever 后每轮按任务目标检索并以 [rag] 来源注入；
// 注入消息不写入会话历史。
func TestAssistantRetrieverInjection(t *testing.T) {
	ctx := context.Background()
	m := &captureModel{}
	a, err := New(m, nil, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()},
		Retriever: &stubRetriever{docs: []memory.Document{
			{Source: "doc-x", Content: "远程数据库连接串位于配置中心", Score: 0.7},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := a.NewSession()
	if _, err := a.Run(ctx, sess, "数据库密码在哪"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := m.last.Messages[len(m.last.Messages)-1]
	for _, want := range []string{"[rag]", "检索到的相关记忆", "doc-x", "远程数据库连接串"} {
		if !strings.Contains(last.Content, want) {
			t.Fatalf("injected content missing %q:\n%s", want, last.Content)
		}
	}
	for _, msg := range sess.Messages() {
		if strings.Contains(msg.Content, "检索到的相关记忆") {
			t.Fatalf("rag injection leaked into session history: %+v", sess.Messages())
		}
	}
}

// RetrieverMinScore 知识守门：全部检索分块低于阈值时广播 EventKnowledgeGap，
// 不注入任何 rag 内容（交由模型经外部工具补齐）。
func TestAssistantRetrieverMinScoreGap(t *testing.T) {
	ctx := context.Background()
	m := &captureModel{}
	var events []agent.Event
	a, err := New(m, nil, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()},
		Retriever: &stubRetriever{docs: []memory.Document{
			{Source: "doc-x", Content: "与查询无关", Score: 0.1},
		}},
		RetrieverMinScore: 0.5,
		Observer:          func(ev agent.Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(ctx, a.NewSession(), "隔离级别是什么"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range m.last.Messages {
		if strings.Contains(msg.Content, "[rag]") {
			t.Fatalf("knowledge gap should not inject rag: %+v", msg)
		}
	}
	var gap bool
	for _, ev := range events {
		if ev.Type == agent.EventKnowledgeGap {
			gap = true
			if ev.Content != "隔离级别是什么" {
				t.Fatalf("gap content = %q, want 查询文本", ev.Content)
			}
		}
	}
	if !gap {
		t.Fatalf("no EventKnowledgeGap emitted: %+v", events)
	}
}

// directModel 不调用工具直接作答（本地桩，替代已移除的离线演示模型）。
type directModel struct{}

func (directModel) Capabilities() llm.Capability { return 0 }
func (directModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
}
func (directModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// 策略装配后仍生效：路径越界的文件调用会被拒绝。
func TestAssistantPolicyApplied(t *testing.T) {
	workDir := t.TempDir()
	// FileAccess.WorkspaceRoot 与 Policy.WorkDir 用同一目录（Task 12 重构后两者联动）：
	// 越界路径选 /etc/passwd（不在 workDir / os.TempDir / WritableExtra 任一根下）。
	a, err := New(directModel{}, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: workDir}, Policy: NewDefaultPolicy(nil, workDir)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 直接验证策略拒绝
	err = a.Policy().Allow(context.Background(), toolCall("write_file", `{"path":"/etc/passwd"}`))
	if err == nil {
		t.Fatal("path traversal should be rejected by policy")
	}
}

// ---- 完整更新：fail-closed / 命令分级 / 人工确认 ----

// mockApproval 是 Approval 的测试桩（Request 返回固定 ID，不挂起决策）。
type mockApproval struct{}

func (mockApproval) Request(context.Context, approval.ApprovalRequest) (approval.ApprovalID, error) {
	return "mock-approval-id", nil
}
func (mockApproval) Resolve(context.Context, approval.ApprovalID, approval.ApprovalDecision) error {
	return nil
}

// requireApproval 断言策略返回需要人工确认的标记（M2.8：决策由驱动层 Approval 处理）。
func requireApproval(t *testing.T, err error) {
	t.Helper()
	var reqErr *approval.RequiredError
	if !errors.As(err, &reqErr) {
		t.Fatalf("err = %v, want *approval.RequiredError", err)
	}
	if reqErr.Request.ToolName == "" {
		t.Fatalf("RequiredError.Request 缺失: %+v", reqErr)
	}
}

// fail-closed：不在放行名单的未知工具默认拒绝。
func TestDefaultPolicyFailClosed(t *testing.T) {
	p := NewDefaultPolicy(nil, "")
	if err := p.Allow(context.Background(), toolCall("unknown_tool", `{}`)); err == nil {
		t.Fatal("unknown tool should be rejected (fail-closed)")
	}
}

// Register 扩展点：自定义工具注册检查器后按策略放行/拒绝；重复注册报错。
func TestDefaultPolicyRegister(t *testing.T) {
	p := NewDefaultPolicy(nil, "")
	chk := func(_ context.Context, args string) error {
		if strings.Contains(args, "blocked") {
			return fmt.Errorf("blocked by custom checker")
		}
		return nil
	}
	if err := p.Register("custom_tool", chk); err != nil {
		t.Fatalf("register should succeed: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("custom_tool", `{"q":"ok"}`)); err != nil {
		t.Fatalf("registered tool should pass: %v", err)
	}
	if err := p.Allow(context.Background(), toolCall("custom_tool", `{"q":"blocked"}`)); err == nil {
		t.Fatal("custom checker rejection should be enforced")
	}
	if err := p.Register("custom_tool", chk); err == nil {
		t.Fatal("duplicate register should be rejected")
	}
	if err := p.Register("", chk); err == nil {
		t.Fatal("empty name register should be rejected")
	}
	if err := p.Register("nil_chk", nil); err == nil {
		t.Fatal("nil checker register should be rejected")
	}
	// 未注册工具仍然 fail-closed
	if err := p.Allow(context.Background(), toolCall("still_unknown", `{}`)); err == nil {
		t.Fatal("unregistered tool should remain fail-closed")
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

// 受限危险命令：WorkDir 内 + 人工确认后才可执行（M2.8：策略返回 *approval.RequiredError，
// 实际决策由驱动层 Approval 处理）；绝对危险命令无条件拒绝。
func TestDefaultPolicyRestrictedShell(t *testing.T) {
	root := t.TempDir()
	rmIn := "rm -rf " + filepath.Join(root, "tmp", "a.txt")

	// WorkDir 内 + 配置 Approval → 返回需要人工确认标记（由驱动层审批）
	p := NewDefaultPolicy(nil, root)
	p.Approval = mockApproval{}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"`+rmIn+`"}`)); err == nil {
		t.Fatal("rm inside workdir with approval configured should require approval")
	} else {
		requireApproval(t, err)
	}
	// 无 Approval → 拒绝（fail-closed）
	p.Approval = nil
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"`+rmIn+`"}`)); err == nil {
		t.Fatal("rm without approval should be rejected")
	}
	// 路径越界 → 拒绝（即使配置 Approval）
	p.Approval = mockApproval{}
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"rm -rf /etc"}`)); err == nil {
		t.Fatal("rm outside workdir should be rejected")
	}
	// 管道 shell 越界路径 → 拒绝
	if err := p.Allow(context.Background(), toolCall("shell", `{"command":"cat /etc/passwd | sh"}`)); err == nil {
		t.Fatal("pipe-to-sh with outside path should be rejected")
	}
	// 无 WorkDir → 拒绝（即使配置 Approval）
	p2 := NewDefaultPolicy(nil, "")
	p2.Approval = mockApproval{}
	if err := p2.Allow(context.Background(), toolCall("shell", `{"command":"rm -rf a.txt"}`)); err == nil {
		t.Fatal("rm without workdir should be rejected")
	}
	// 绝对危险命令：即使配置 Approval 也拒绝
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
// 同样属于受限危险命令——WorkDir 内 + 人工确认；TUI 模式（无 Approval）一律拒绝。
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

	// TUI 等价配置（WorkDir 非空 + 无 Approval）：一律拒绝，防止绕过删除
	p := NewDefaultPolicy(nil, root)
	for _, args := range bypass {
		if err := p.Allow(context.Background(), toolCall("shell", args)); err == nil {
			t.Fatalf("bypass command should be rejected in TUI mode: %s", args)
		}
	}
	// 配置 Approval → 需要人工确认标记
	p.Approval = mockApproval{}
	for _, args := range bypass {
		if err := p.Allow(context.Background(), toolCall("shell", args)); err == nil {
			t.Fatalf("bypass command with approval should require approval: %s", args)
		} else {
			requireApproval(t, err)
		}
	}
	// 无 WorkDir → 拒绝
	p2 := NewDefaultPolicy(nil, "")
	p2.Approval = mockApproval{}
	for _, args := range bypass {
		if err := p2.Allow(context.Background(), toolCall("shell", args)); err == nil {
			t.Fatalf("bypass command without workdir should be rejected: %s", args)
		}
	}
}

// write_file：路径受限放行；配置了 Approval 时写入前人工确认（M2.8 异步审批）。
func TestDefaultPolicyWriteFileConfirm(t *testing.T) {
	root := t.TempDir()
	p := NewDefaultPolicy(nil, root)

	// 无 Approval：路径已限制在 WorkDir 内，放行
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"a.txt","content":"x"}`)); err != nil {
		t.Fatalf("write inside workdir without approval should pass: %v", err)
	}
	// 配置 Approval → 需要人工确认标记
	p.Approval = mockApproval{}
	if err := p.Allow(context.Background(), toolCall("write_file", `{"path":"a.txt","content":"x"}`)); err == nil {
		t.Fatal("write with approval configured should require approval")
	} else {
		requireApproval(t, err)
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

// TestTUIModeRestrictedShellRejected 模拟 TUI 模式（不注入 Approval）下尝试执行受限危险命令 rm：
// DefaultPolicy 配置了 WorkDir 但 Approval 为 nil（与 repl.go 的 attachConfirmer(tui=true) 行为一致），
// rm 路径虽在 WorkDir 内，仍因"需人工确认但未配置 Approval"被拒绝——工具未执行，拒绝原因回填给模型。
func TestTUIModeRestrictedShellRejected(t *testing.T) {
	workDir := t.TempDir()
	policy := NewDefaultPolicy(nil, workDir)
	if policy.Approval != nil {
		t.Fatal("TUI 模式不应挂载审批器")
	}

	a, err := New(tuiShellModel(workDir), []tool.Tool{builtin.Shell{}}, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, Policy: policy})
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
	if len(sess.Messages()) != 4 {
		t.Fatalf("history len = %d, want 4", len(sess.Messages()))
	}
	toolMsg := sess.Messages()[2]
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

// TestAssistantProvidesFileSystemSandboxed 验证 New 按 Policy.Mode()=SaaSJailed 装配 Sandboxed。
func TestAssistantProvidesFileSystemSandboxed(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{FileAccess: FileAccessConfig{
		WorkspaceRoot: dir,
		JailReads:     true,
		JailWrites:    true,
	}}
	cfg.Policy = NewDefaultPolicy(nil, dir)
	a, err := New(directModel{}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fsys := a.FS()
	if fsys == nil {
		t.Fatal("FS 未装配")
	}
	if fsys.Mode() != fs.ModeSaaSJailed {
		t.Errorf("Mode=%s want=saas-jailed", fsys.Mode())
	}
	// 越界应被 Sandboxed 拒
	if _, err := fsys.Resolve(context.Background(), fs.Request{Path: "/etc/passwd", Op: fs.OpRead}); err == nil {
		t.Error("Sandboxed 应拒绝越界读")
	}
}

// TestAssistantProvidesFileSystemLocalWhenDangerFull 验证零值 JailReads/JailWrites → Local 透传。
func TestAssistantProvidesFileSystemLocalWhenDangerFull(t *testing.T) {
	dir := t.TempDir()
	a, err := New(directModel{}, nil, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: dir},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fsys := a.FS()
	if fsys == nil {
		t.Fatal("FS 未装配")
	}
	if fsys.Mode() != fs.ModeDangerFull {
		t.Errorf("Mode=%s want=danger-full", fsys.Mode())
	}
}

// thirdPartyPolicy 是只实现 driver.Policy 接口（无 Mode 方法）的第三方策略，
// 用于验证 buildFileSystem 不依赖 *DefaultPolicy 类型断言推导 jail 开关。
type thirdPartyPolicy struct{}

func (thirdPartyPolicy) Allow(context.Context, *llm.ToolCall) error { return nil }

// fsProbeTool 在 Execute 里抓取 ctx 中的 FileSystem，用于验证注入点已生效。
type fsProbeTool struct{ got chan fs.FileSystem }

func (fsProbeTool) Name() string        { return "fs_probe" }
func (fsProbeTool) Description() string { return "探测 ctx 中的 FileSystem（测试用）" }
func (fsProbeTool) Parameters() any     { return map[string]any{"type": "object"} }
func (p fsProbeTool) Execute(ctx context.Context, _ json.RawMessage) (any, error) {
	select {
	case p.got <- fs.FromContext(ctx):
	default:
	}
	return "ok", nil
}

// probeModel 首轮调 fs_probe，次轮直接作答。
type probeModel struct{}

func (probeModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (probeModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}}, nil
		}
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "p1", Name: "fs_probe", Args: `{}`}},
	}}, nil
}
func (probeModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestToolCtxCarriesFileSystem 验证工具执行时 ctx 里已注入 RunContext.FS。
// 这是 Task 15–18 四个文件工具改造的前提：它们都用 fs.FromContext(ctx) 取能力，
// 若注入点缺失则全部 fail-closed 为 *NotConfiguredError（工具直接不可用）。
func TestToolCtxCarriesFileSystem(t *testing.T) {
	dir := t.TempDir()
	probe := fsProbeTool{got: make(chan fs.FileSystem, 1)}
	pol := NewDefaultPolicy(nil, dir)
	if err := pol.Register("fs_probe", func(context.Context, string) error { return nil }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	a, err := New(probeModel{}, []tool.Tool{probe}, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: dir, JailReads: true, JailWrites: true},
		Policy:     pol,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), a.NewSession(), "探测"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case got := <-probe.got:
		if got == nil {
			t.Fatal("工具 ctx 未注入 FileSystem（Task 15–18 会全线 fail-closed）")
		}
		if got.Mode() != fs.ModeSaaSJailed {
			t.Errorf("注入的 FS Mode=%s want=saas-jailed", got.Mode())
		}
	default:
		t.Fatal("工具未被执行")
	}
}

// TestBuildFileSystemJailsWithThirdPartyPolicy 验证 fail-closed：第三方 Policy 实现
// （没有 Mode 方法）配 JailWrites=true 时，仍必须拿到 Sandboxed 而非 Local 透传。
// jail 开关的唯一事实来源是 FileAccessConfig.Jail*，不是 Policy 的具体类型。
func TestBuildFileSystemJailsWithThirdPartyPolicy(t *testing.T) {
	dir := t.TempDir()
	a, err := New(directModel{}, nil, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: dir, JailReads: true, JailWrites: true},
		Policy:     thirdPartyPolicy{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fsys := a.FS()
	if fsys == nil {
		t.Fatal("FS 未装配")
	}
	if fsys.Mode() != fs.ModeSaaSJailed {
		t.Errorf("Mode=%s want=saas-jailed（第三方 Policy 不应导致 jail 失效）", fsys.Mode())
	}
	if _, err := fsys.Resolve(context.Background(), fs.Request{Path: "/etc/passwd", Op: fs.OpWrite}); err == nil {
		t.Error("JailWrites=true 时越界写必须被拒绝（fail-open 缺口）")
	}
}

// TestBuildFileSystemJailReadsOnlyIsLocalJailed 验证只 jail 读时映射到 local-jailed。
func TestBuildFileSystemJailReadsOnlyIsLocalJailed(t *testing.T) {
	dir := t.TempDir()
	a, err := New(directModel{}, nil, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: dir, JailReads: true},
		Policy:     thirdPartyPolicy{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := a.FS().Mode(); got != fs.ModeLocalJailed {
		t.Errorf("Mode=%s want=local-jailed", got)
	}
}

// TestNewRequiresWorkspaceRoot 验证 FileAccess.WorkspaceRoot 必填（Task 10 fail-closed）：
// 零值 FileAccessConfig{}.WorkspaceRoot=="" 时 assistant.New 必须立即返回错误，
// 且错误文案点名 WorkspaceRoot，便于上层排查。
func TestNewRequiresWorkspaceRoot(t *testing.T) {
	_, err := New(directModel{}, nil, Config{})
	if err == nil {
		t.Fatal("空 WorkspaceRoot 应使 New 失败")
	}
	if !strings.Contains(err.Error(), "WorkspaceRoot") {
		t.Errorf("错误文案应点名 WorkspaceRoot，got=%s", err.Error())
	}
}

// readFileModel 首轮调 read_file 读指定路径，次轮直接作答。
type readFileModel struct{ path string }

func (readFileModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m readFileModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}}, nil
		}
	}
	args, _ := json.Marshal(map[string]any{"path": m.path})
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "r1", Name: "read_file", Args: string(args)}},
	}}, nil
}
func (readFileModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestFsResolveEventEmittedEndToEnd 验证 fs/resolve 审计事件在真实驱动路径上真的发出。
// Task 19 建好了事件契约与 Sandboxed 的发射逻辑，但装配层若不把 bus 注入 ctx，
// BusFromContext 恒为 nil → 事件永久静默，审计能力形同不存在。
func TestFsResolveEventEmittedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "audit.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	bus := event.NewEventBus()
	var mu sync.Mutex
	var got []event.FsResolveData
	bus.On(event.SessionEventFsResolve, event.ModeEmit, func(_ context.Context, payload any) (any, error) {
		if d, ok := payload.(event.FsResolveData); ok {
			mu.Lock()
			got = append(got, d)
			mu.Unlock()
		}
		return nil, nil
	})

	a, err := New(readFileModel{path: target}, []tool.Tool{builtin.ReadFile{}}, Config{
		FileAccess: FileAccessConfig{WorkspaceRoot: dir, JailReads: true, JailWrites: true},
		Policy:     NewDefaultPolicy(nil, dir),
		Bus:        bus,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), a.NewSession(), "读文件"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("未收到 fs/resolve 事件（装配层未把 bus 注入 ctx，审计通道空转）")
	}
	ev := got[0]
	if ev.Decision != "allowed" {
		t.Errorf("decision=%q want=allowed", ev.Decision)
	}
	if ev.Op != "read" {
		t.Errorf("op=%q want=read", ev.Op)
	}
	if ev.Mode != "saas-jailed" {
		t.Errorf("mode=%q want=saas-jailed", ev.Mode)
	}
	if ev.Target != target {
		t.Errorf("target=%q want=%q", ev.Target, target)
	}
}
