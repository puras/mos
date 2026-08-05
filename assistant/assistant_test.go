package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/llm"
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

func TestLoadConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("MOS_PROVIDER", "openai")
	t.Setenv("MOS_SHELL_ALLOW", "ls, cat")
	t.Setenv("MOS_MAX_ITERATIONS", "5")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider != "openai" || cfg.APIKey != "sk-test" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg.ShellAllowed) != 2 || cfg.ShellAllowed[0] != "ls" {
		t.Fatalf("shell allowed = %v", cfg.ShellAllowed)
	}
	if cfg.MaxIterations != 5 {
		t.Fatalf("max iterations = %d", cfg.MaxIterations)
	}
	if cfg.Observer != nil {
		t.Fatal("observer should be nil by default")
	}
}

func TestLoadConfigFallbackToMock(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider != "mock" {
		t.Fatalf("provider = %s, want mock", cfg.Provider)
	}
}

// mock 模式下端到端跑通工具调用与多轮历史。
func TestAssistantMockEndToEnd(t *testing.T) {
	a, err := New(Config{Provider: "mock"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := a.ToolNames()
	want := []string{"calculator", "current_time", "find_files", "grep", "read_file", "shell", "web_fetch", "write_file"}
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
	a, err := New(Config{Provider: "mock", WorkDir: workDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 直接验证策略拒绝
	err = a.Policy().Allow(context.Background(), toolCall("write_file", `{"path":"`+filepath.Join(workDir, "..", "evil.txt")+`"}`))
	if err == nil {
		t.Fatal("path traversal should be rejected by policy")
	}
}
