package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"hegq.net/mos/pkg/llm"
)

// writeEnv 写入临时 .env 文件，并将 MOS_CONFIG 指向它。
func writeEnv(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOS_CONFIG", path)
}

// clearEnv 清空依赖的配置键，避免跨用例残留（LoadEnv 不覆盖已存在的环境变量）。
func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoadEnvMissing(t *testing.T) {
	t.Setenv("MOS_CONFIG", filepath.Join(t.TempDir(), "not-exist.env"))
	t.Setenv("MOS_MODEL", "env-model")
	if err := LoadEnv(); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if os.Getenv("MOS_MODEL") != "env-model" {
		t.Fatal("已有环境变量不应被影响")
	}
}

func TestLoadEnvFile(t *testing.T) {
	clearEnv(t, "MOS_PROVIDER", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "MOS_BASE_URL",
		"MOS_MODEL", "MOS_SYSTEM_PROMPT", "MOS_MAX_TURNS", "MOS_SHELL_ALLOW", "MOS_WORKDIR")
	writeEnv(t, `# 注释行

MOS_PROVIDER=openai # 行内注释
OPENAI_API_KEY=sk-test # 行内注释
MOS_BASE_URL=https://api.example.com/v1
MOS_MODEL=deepseek-chat # 注释带尾随空格   
MOS_SYSTEM_PROMPT=你是测试智能体
MOS_MAX_TURNS=5
MOS_SHELL_ALLOW=ls, cat
MOS_WORKDIR=/tmp/work
export ANTHROPIC_API_KEY=sk-anth
`)
	if err := LoadEnv(); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if os.Getenv("MOS_PROVIDER") != "openai" {
		t.Fatalf("provider = %q", os.Getenv("MOS_PROVIDER"))
	}
	if os.Getenv("OPENAI_API_KEY") != "sk-test" {
		t.Fatalf("api_key = %q", os.Getenv("OPENAI_API_KEY"))
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "sk-anth" {
		t.Fatalf("export 前缀未处理: %q", os.Getenv("ANTHROPIC_API_KEY"))
	}
	if os.Getenv("MOS_MODEL") != "deepseek-chat" {
		t.Fatalf("model = %q", os.Getenv("MOS_MODEL"))
	}
	if os.Getenv("MOS_MAX_TURNS") != "5" {
		t.Fatalf("max_turns = %q", os.Getenv("MOS_MAX_TURNS"))
	}
	if os.Getenv("MOS_SHELL_ALLOW") != "ls, cat" {
		t.Fatalf("shell_allow = %q", os.Getenv("MOS_SHELL_ALLOW"))
	}
	if os.Getenv("MOS_WORKDIR") != "/tmp/work" {
		t.Fatalf("workdir = %q", os.Getenv("MOS_WORKDIR"))
	}
}

func TestLoadEnvNotOverrideExisting(t *testing.T) {
	writeEnv(t, "OPENAI_API_KEY=file-key\nMOS_MODEL=file-model\n")
	t.Setenv("OPENAI_API_KEY", "env-key")
	t.Setenv("MOS_MODEL", "env-model")

	if err := LoadEnv(); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if os.Getenv("OPENAI_API_KEY") != "env-key" {
		t.Fatalf("已有环境变量被覆盖: %q", os.Getenv("OPENAI_API_KEY"))
	}
	if os.Getenv("MOS_MODEL") != "env-model" {
		t.Fatalf("已有环境变量被覆盖: %q", os.Getenv("MOS_MODEL"))
	}
}

func TestLoadEnvInvalidLine(t *testing.T) {
	writeEnv(t, "GOOD=1\n这一行没有等号\n")
	if err := LoadEnv(); err == nil {
		t.Fatal("want error for invalid line")
	}
}

func TestLoadModelFromEnvFile(t *testing.T) {
	clearEnv(t, "MOS_PROVIDER", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "MOS_BASE_URL", "MOS_MODEL")
	writeEnv(t, "OPENAI_API_KEY=sk-test\nMOS_MODEL=deepseek-chat\n")
	name, model, err := LoadModel()
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if name != "openai" || model == nil {
		t.Fatalf("name=%s model=%v, want openai non-nil", name, model)
	}
}

// TestLoadModelFallbackToMockFromFile 验证协议来自文件但无 Key 时仍降级 mock。
func TestLoadModelFallbackToMockFromFile(t *testing.T) {
	clearEnv(t, "MOS_PROVIDER", "OPENAI_API_KEY", "ANTHROPIC_API_KEY")
	writeEnv(t, "MOS_PROVIDER=openai\n")
	name, model, err := LoadModel()
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if name != "mock" || model == nil {
		t.Fatalf("name=%s model=%v, want mock non-nil", name, model)
	}
}

func TestLoadConfigFromEnvFile(t *testing.T) {
	clearEnv(t, "MOS_SYSTEM_PROMPT", "MOS_MAX_TURNS", "MOS_SHELL_ALLOW", "MOS_WORKDIR")
	writeEnv(t, "MOS_SYSTEM_PROMPT=来自文件\nMOS_MAX_TURNS=4\nMOS_SHELL_ALLOW=pwd\nMOS_WORKDIR=/tmp/w\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SystemPrompt != "来自文件" {
		t.Fatalf("system prompt = %q", cfg.SystemPrompt)
	}
	if cfg.MaxTurns != 4 {
		t.Fatalf("max = %d, want 4", cfg.MaxTurns)
	}
	if cfg.Policy == nil {
		t.Fatal("policy should be constructed from env")
	}
	if err := cfg.Policy.Allow(context.Background(), &llm.ToolCall{Name: "shell", Args: `{"command":"pwd"}`}); err != nil {
		t.Fatalf("whitelisted pwd should pass: %v", err)
	}
	if err := cfg.Policy.Allow(context.Background(), &llm.ToolCall{Name: "read_file", Args: `{"path":"../x"}`}); err == nil {
		t.Fatal("path traversal should be rejected")
	}
}
