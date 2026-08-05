// Package config 是 MoS 应用层配置：从 .env 文件与环境变量
// 构建模型对象与智能体运行配置。
//
// 本包属应用层能力（绑定 MOS_* 环境变量与 .env 约定），仅供本仓库内部使用
// （CLI 等入口）。框架层只认模型对象与运行配置，不关心配置来源。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"hegq.net/mos/pkg/assistant"
	"hegq.net/mos/pkg/llm"
	"hegq.net/mos/pkg/llm/anthropic"
	"hegq.net/mos/pkg/llm/openai"
)

// ConfigPath 返回配置文件路径：MOS_CONFIG 环境变量指定，默认 .env。
func ConfigPath() string {
	if p := os.Getenv("MOS_CONFIG"); p != "" {
		return p
	}
	return ".env"
}

// LoadEnv 读取配置文件（默认 .env，路径可用 MOS_CONFIG 覆盖）并写入环境变量。
// 已存在的环境变量优先，配置文件不覆盖；文件不存在时静默跳过。
// 每行一条 KEY=VALUE，支持 # 注释、空行与可选的 export 前缀。
func LoadEnv() error {
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: 读取配置文件失败: %w", err)
	}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ") // 兼容 shell 可导出的写法
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return fmt.Errorf("config: 配置文件 %s 第 %d 行格式错误: %q", ConfigPath(), i+1, raw)
		}
		k = strings.TrimSpace(k)
		if os.Getenv(k) == "" {
			os.Setenv(k, strings.TrimSpace(v))
		}
	}
	return nil
}

// LoadModel 从 .env 文件与环境变量构建模型对象（llm.Model）：
//
//	MOS_PROVIDER         openai | anthropic | mock（默认 openai，无 API Key 时自动降级 mock）
//	OPENAI_API_KEY       OpenAI 兼容服务密钥
//	ANTHROPIC_API_KEY    Anthropic 服务密钥
//	MOS_BASE_URL         协议服务地址
//	MOS_MODEL            模型名称
//
// 环境变量优先于 .env 文件；返回协议名称与模型对象，调用方只需持有模型对象，无需关心接入细节。
func LoadModel() (string, llm.Model, error) {
	if err := LoadEnv(); err != nil {
		return "", nil, err
	}
	name := getenv("MOS_PROVIDER", "openai")
	apiKey := firstEnv("OPENAI_API_KEY", "ANTHROPIC_API_KEY")
	// 无 API Key 时自动降级为 mock，保证应用可离线体验
	if name != "mock" && apiKey == "" {
		name = "mock"
	}

	cfg := llm.Config{
		"api_key":  apiKey,
		"base_url": os.Getenv("MOS_BASE_URL"),
		"model":    os.Getenv("MOS_MODEL"),
	}
	switch name {
	case "mock":
		return name, assistant.Mock(), nil
	case "openai":
		p, err := openai.New(cfg)
		return name, p, err
	case "anthropic":
		p, err := anthropic.New(cfg)
		return name, p, err
	default:
		return "", nil, fmt.Errorf("config: 未知协议 %q（支持 openai/anthropic/mock）", name)
	}
}

// LoadConfig 从 .env 文件与环境变量读取智能体运行配置：
//
//	MOS_SYSTEM_PROMPT    系统引导词
//	MOS_MAX_ITERATIONS   一次任务循环的最大迭代次数（默认 10）
//	MOS_SHELL_ALLOW      允许的 shell 命令前缀，逗号分隔
//	MOS_WORKDIR          文件工具根目录
//
// 环境变量优先于 .env 文件；最大迭代次数默认 10。
func LoadConfig() (*assistant.Config, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}
	cfg := &assistant.Config{
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
