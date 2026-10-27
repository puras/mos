package mos_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos"
	"github.com/puras/mos/llm"
)

// 缺配置要报错（fail-closed：半截配置的错误会推迟到调用时才暴露）。
func TestNewModelRequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  mos.ModelConfig
	}{
		{"缺协议", mos.ModelConfig{APIKey: "k", Model: "m"}},
		{"缺凭据", mos.ModelConfig{Provider: "openai", Model: "m"}},
		{"缺模型名", mos.ModelConfig{Provider: "openai", APIKey: "k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := mos.NewModel(c.cfg); err == nil {
				t.Fatal("应当报错")
			}
		})
	}
	// 未知协议也要报错（不能静默出个空模型）。
	if _, err := mos.NewModel(mos.ModelConfig{Provider: "nope", APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("未知协议应当报错")
	}
}

// 装配出来的模型能真调（协议适配器接对了）。
func TestNewModelUsable(t *testing.T) {
	m, err := mos.NewModel(mos.ModelConfig{
		Provider: "openai", APIKey: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1",
	})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	if m == nil {
		t.Fatal("模型为 nil")
	}
	if m.Capabilities()&llm.CapFunctionCalling == 0 {
		t.Fatal("openai 适配器应声明 function calling 能力")
	}
}

// 密钥环境变量名：只公布名字，框架不读环境变量。
func TestAPIKeyEnv(t *testing.T) {
	cases := map[string]string{
		"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY",
	}
	for p, want := range cases {
		if got := mos.APIKeyEnv(p); got != want {
			t.Fatalf("APIKeyEnv(%q) = %q, want %q", p, got, want)
		}
	}
	if got := mos.APIKeyEnv("nope"); got != "" {
		t.Fatalf("未知协议应返回空串, got %q", got)
	}
}

// 重试与日志装饰器：宿主给的回调会被调用。
func TestNewModelDecorators(t *testing.T) {
	retried := false
	logged := false
	m, err := mos.NewModel(mos.ModelConfig{
		Provider: "openai", APIKey: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1",
		Retry: llm.RetryPolicy{
			MaxAttempts: 2, BaseDelay: time.Millisecond,
			OnScheduled: func(attempt int, delay time.Duration, err error) { retried = true },
		},
		Log: func(r llm.CallResult) { logged = true },
	})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	// 调一次：连不上 → 触发重试回调与日志回调。
	_, _ = m.Chat(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}})
	if !retried {
		t.Fatal("重试回调应被调用")
	}
	if !logged {
		t.Fatal("日志回调应被调用")
	}
}

// MaxTokens 透传到适配器（anthropic 少了它会拿默认上限）。
func TestNewModelPassesMaxTokens(t *testing.T) {
	m, err := mos.NewModel(mos.ModelConfig{
		Provider: "anthropic", APIKey: "k", Model: "m", MaxTokens: "4096",
	})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	if m == nil {
		t.Fatal("模型为 nil")
	}
	if !strings.Contains(mos.APIKeyEnv("anthropic"), "ANTHROPIC") {
		t.Fatal("环境变量名不对")
	}
}
