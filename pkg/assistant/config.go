// Package assistant 提供开箱即用的智能体：预置常用工具、安全策略与多协议 LLM 接入，
// 供上层应用（CLI、服务等）一行代码集成。
package assistant

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"hegq.net/mos/pkg/agent"
)

// Config 是智能体的运行配置。
type Config struct {
	// Provider 指定 LLM 协议：openai / anthropic / mock（mock 无需 API Key）。
	Provider string
	// APIKey LLM 服务密钥；本地模型可留空。
	APIKey string
	// BaseURL 协议服务地址；留空使用协议默认地址。
	BaseURL string
	// Model 模型名称；留空使用适配器默认。
	Model string
	// SystemPrompt 系统引导词。
	SystemPrompt string
	// MaxIterations 任务循环最大迭代次数。
	MaxIterations int
	// ShellAllowed shell 命令前缀白名单（空表示仅基础危险命令防御）。
	ShellAllowed []string
	// WorkDir 文件类工具允许操作的根目录（空表示不限制）。
	WorkDir string
	// Observer 任务循环事件回调（可观测工具调用与思考过程）。
	Observer agent.Observer
}

// LoadConfig 从环境变量读取配置：
//
//	MOS_PROVIDER         openai | anthropic | mock（默认 openai，无 API Key 时自动降级 mock）
//	OPENAI_API_KEY       OpenAI 兼容服务密钥
//	ANTHROPIC_API_KEY    Anthropic 服务密钥
//	MOS_BASE_URL         协议服务地址
//	MOS_MODEL            模型名称
//	MOS_SYSTEM_PROMPT    系统引导词
//	MOS_MAX_ITERATIONS   最大迭代次数（默认 10）
//	MOS_SHELL_ALLOW      允许的 shell 命令前缀，逗号分隔
//	MOS_WORKDIR          文件工具根目录
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Provider:     getenv("MOS_PROVIDER", "openai"),
		APIKey:       firstEnv("OPENAI_API_KEY", "ANTHROPIC_API_KEY"),
		BaseURL:      os.Getenv("MOS_BASE_URL"),
		Model:        os.Getenv("MOS_MODEL"),
		SystemPrompt: os.Getenv("MOS_SYSTEM_PROMPT"),
		ShellAllowed: splitCSV(os.Getenv("MOS_SHELL_ALLOW")),
		WorkDir:      os.Getenv("MOS_WORKDIR"),
	}
	if v := os.Getenv("MOS_MAX_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("assistant: MOS_MAX_ITERATIONS 无效: %w", err)
		}
		cfg.MaxIterations = n
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 10
	}
	// 无 API Key 时自动降级为 mock，保证框架可离线体验
	if cfg.Provider != "mock" && cfg.APIKey == "" {
		cfg.Provider = "mock"
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
