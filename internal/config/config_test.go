package config

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Setenv("MOS_SHELL_ALLOW", "ls, cat")
	t.Setenv("MOS_MAX_ITERATIONS", "5")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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
