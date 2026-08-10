package config

import (
	"context"
	"testing"
	"time"

	"hegq.net/mos/pkg/llm"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("MOS_SHELL_ALLOW", "ls, cat")
	t.Setenv("MOS_MAX_TURNS", "5")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Policy == nil {
		t.Fatal("policy should be constructed from env")
	}
	if err := cfg.Policy.Allow(context.Background(), &llm.ToolCall{Name: "shell", Args: `{"command":"ls -la"}`}); err != nil {
		t.Fatalf("whitelisted command should pass: %v", err)
	}
	if err := cfg.Policy.Allow(context.Background(), &llm.ToolCall{Name: "shell", Args: `{"command":"touch x"}`}); err == nil {
		t.Fatal("non-whitelisted command should be rejected")
	}
	if cfg.MaxTurns != 5 {
		t.Fatalf("max turns = %d", cfg.MaxTurns)
	}
	if cfg.Observer != nil {
		t.Fatal("observer should be nil by default")
	}
	if cfg.Sandbox == nil {
		t.Fatal("sandbox should be constructed by default (MOS_SANDBOX=1)")
	}
}

// 沙箱环境变量解析：超时/输出/资源限制/网络开关/jail 目录。
func TestLoadSandbox(t *testing.T) {
	t.Setenv("MOS_SANDBOX", "1")
	t.Setenv("MOS_SANDBOX_TIMEOUT", "30")
	t.Setenv("MOS_SANDBOX_MAX_OUTPUT", "128")
	t.Setenv("MOS_SANDBOX_CPU_SECONDS", "5")
	t.Setenv("MOS_SANDBOX_MEM_KB", "1048576")
	t.Setenv("MOS_SANDBOX_MAX_FILE_KB", "1024")
	t.Setenv("MOS_SANDBOX_NO_NET", "1")

	sb, err := loadSandbox("/tmp/mos-work")
	if err != nil {
		t.Fatalf("loadSandbox: %v", err)
	}
	if sb == nil {
		t.Fatal("sandbox should be constructed")
	}
	if sb.WorkDir != "/tmp/mos-work" {
		t.Fatalf("workdir = %q", sb.WorkDir)
	}
	if sb.Timeout != 30*time.Second {
		t.Fatalf("timeout = %v", sb.Timeout)
	}
	if sb.MaxOutput != 128<<10 {
		t.Fatalf("max output = %d", sb.MaxOutput)
	}
	if sb.CpuSeconds != 5 || sb.MemKB != 1048576 || sb.MaxFileKB != 1024 {
		t.Fatalf("limits = cpu:%d mem:%d file:%d", sb.CpuSeconds, sb.MemKB, sb.MaxFileKB)
	}
	if !sb.NoNet {
		t.Fatal("no_net should be true")
	}
}

func TestLoadSandboxDisabled(t *testing.T) {
	t.Setenv("MOS_SANDBOX", "0")
	sb, err := loadSandbox("")
	if err != nil {
		t.Fatalf("loadSandbox: %v", err)
	}
	if sb != nil {
		t.Fatal("disabled sandbox should be nil")
	}
}

func TestLoadSandboxInvalid(t *testing.T) {
	t.Setenv("MOS_SANDBOX_TIMEOUT", "abc")
	if _, err := loadSandbox(""); err == nil {
		t.Fatal("invalid timeout should error")
	}
}

func TestLoadModelWithKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("MOS_MODEL", "deepseek-chat")
	name, model, err := LoadModel()
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if name != "openai" {
		t.Fatalf("name = %s, want openai", name)
	}
	if model == nil {
		t.Fatal("model should not be nil")
	}
}

func TestLoadModelFallbackToMock(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	name, model, err := LoadModel()
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if name != "mock" {
		t.Fatalf("name = %s, want mock", name)
	}
	if model == nil {
		t.Fatal("model should not be nil")
	}
}
