package config

import "testing"

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
