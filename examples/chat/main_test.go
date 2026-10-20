package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 应用层的策略文件加载：读文件 + 严格解析。框架不碰这两件事，所以覆盖在这里。
func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := "allow:\n  - current_time\n  - web_search\ndeny:\n  - shell\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadPolicy(path)
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if len(cfg.Allow) != 2 || cfg.Allow[0] != "current_time" || cfg.Allow[1] != "web_search" {
		t.Fatalf("allow 解析错误: %v", cfg.Allow)
	}
	if len(cfg.Deny) != 1 || cfg.Deny[0] != "shell" {
		t.Fatalf("deny 解析错误: %v", cfg.Deny)
	}
}

// 字段名拼错必须报错：静默忽略会让用户以为规则生效了，实际上什么都没拦。
func TestLoadPolicyRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("alow:\n  - current_time\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPolicy(path); err == nil {
		t.Fatal("字段名拼错应报错")
	}
}

// 文件缺失/不可读：报错而不是静默降级成「无限制」。
func TestLoadPolicyMissingFile(t *testing.T) {
	if _, err := loadPolicy(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("文件缺失应报错")
	}
}
