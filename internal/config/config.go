// Package config 是 MoS 应用层配置：从环境变量组装 assistant.Config。
//
// 本包属应用层能力（绑定 MOS_* 环境变量约定），仅供本仓库内部使用
// （CLI 等入口）；框架层只定义 Config 契约，不关心配置来源。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"hegq.net/mos/pkg/assistant"
)

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
func LoadConfig() (*assistant.Config, error) {
	cfg := &assistant.Config{
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
			return nil, fmt.Errorf("config: MOS_MAX_ITERATIONS 无效: %w", err)
		}
		cfg.MaxIterations = n
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 10
	}
	// 无 API Key 时自动降级为 mock，保证应用可离线体验
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
