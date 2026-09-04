package llm

import (
	"errors"
	"strings"
	"testing"
)

// TestRegistryRegisterCreateNames：协议注册表的按名注册/创建/枚举。
func TestRegistryRegisterCreateNames(t *testing.T) {
	r := NewRegistry()
	mk := func(name string) Factory {
		return func(cfg Config) (ChatModel, error) {
			if cfg["api_key"] == "" {
				return nil, errors.New("missing api_key")
			}
			return &fakeModel{}, nil
		}
	}
	r.Register("alpha", mk("alpha"))
	r.Register("beta", mk("beta"))

	// Names 排序返回
	names := r.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("names = %v", names)
	}

	// 按名创建：正常配置
	m, err := r.Create("alpha", Config{"api_key": "k"})
	if err != nil {
		t.Fatalf("Create(alpha): %v", err)
	}
	if m == nil {
		t.Fatal("Create returned nil model")
	}

	// 工厂错误透传
	if _, err := r.Create("beta", Config{}); err == nil {
		t.Fatal("factory error should propagate")
	}

	// 未注册协议报错（含已注册列表提示）
	_, err = r.Create("unknown", Config{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider error = %v", err)
	}
}

// TestRegistryEmpty：空注册表 Names 为空、Create 报错。
func TestRegistryEmpty(t *testing.T) {
	r := NewRegistry()
	if n := r.Names(); len(n) != 0 {
		t.Fatalf("names = %v, want empty", n)
	}
	if _, err := r.Create("any", Config{}); err == nil {
		t.Fatal("create on empty registry should fail")
	}
}
