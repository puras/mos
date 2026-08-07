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
	"time"

	"hegq.net/mos/internal/tool/search"
	"hegq.net/mos/pkg/assistant"
	"hegq.net/mos/pkg/llm"
	"hegq.net/mos/pkg/llm/anthropic"
	"hegq.net/mos/pkg/llm/gemini"
	"hegq.net/mos/pkg/llm/openai"
	"hegq.net/mos/pkg/tool"
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
// 每行一条 KEY=VALUE，支持整行与行内 # 注释、空行与可选的 export 前缀。
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
			os.Setenv(k, strings.TrimSpace(stripComment(v)))
		}
	}
	return nil
}

// stripComment 截断值中的行内注释：仅当 # 前是空白（或位于值首）时才视为注释，
// 避免误伤 URL 等含 # 的值。
func stripComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// LoadModel 从 .env 文件与环境变量构建模型对象（llm.ChatModel）：
//
//	MOS_PROVIDER         openai | anthropic | gemini | mock（默认 openai，无 API Key 时自动降级 mock）
//	OPENAI_API_KEY       OpenAI 兼容服务密钥
//	ANTHROPIC_API_KEY    Anthropic 服务密钥
//	GEMINI_API_KEY       Gemini 服务密钥
//	MOS_BASE_URL         协议服务地址
//	MOS_MODEL            模型名称
//
// 环境变量优先于 .env 文件；返回协议名称与模型对象，调用方只需持有模型对象，无需关心接入细节。
func LoadModel() (string, llm.ChatModel, error) {
	if err := LoadEnv(); err != nil {
		return "", nil, err
	}
	name := getenv("MOS_PROVIDER", "openai")
	apiKey := firstEnv("OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY")
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
	case "gemini":
		p, err := gemini.New(cfg)
		return name, p, err
	default:
		return "", nil, fmt.Errorf("config: 未知协议 %q（支持 openai/anthropic/gemini/mock）", name)
	}
}

// LoadConfig 从 .env 文件与环境变量读取智能体运行配置：
//
//	MOS_SYSTEM_PROMPT    系统引导词（覆盖框架默认引导 assistant.DefaultSystemPrompt）
//	MOS_MAX_TURNS        一次任务循环的最大 Turn 数（默认 10）
//	MOS_SHELL_ALLOW      允许的 shell 命令前缀，逗号分隔（构造默认安全策略）
//	MOS_WORKDIR          文件工具根目录（构造默认安全策略与沙箱 jail）
//	MOS_SANDBOX          执行沙箱总开关（默认 1 开启；0 关闭则 shell 用默认执行方式）
//	MOS_SANDBOX_TIMEOUT  沙箱单次命令超时秒数（默认 10）
//	MOS_SANDBOX_MAX_OUTPUT  沙箱输出上限 KB（默认 64）
//	MOS_SANDBOX_CPU_SECONDS 沙箱 CPU 时间上限秒数（0 不限制）
//	MOS_SANDBOX_MEM_KB   沙箱虚拟内存上限 KB（0 不限制）
//	MOS_SANDBOX_MAX_FILE_KB 沙箱单文件大小上限 KB（0 不限制）
//	MOS_SANDBOX_NO_NET   沙箱网络隔离（1 开启，尽力而为：剥离代理变量）
//
// 环境变量优先于 .env 文件；最大 Turn 数默认 10。
// 安全策略由本应用层构造框架默认实现（assistant.NewDefaultPolicy）后注入，
// 框架层只认概念对象（agent.Policy / tool.Sandbox），不感知原始配置项。
func LoadConfig() (*assistant.Config, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}
	workDir := os.Getenv("MOS_WORKDIR")
	cfg := &assistant.Config{
		SystemPrompt: os.Getenv("MOS_SYSTEM_PROMPT"),
		Policy:       assistant.NewDefaultPolicy(splitCSV(os.Getenv("MOS_SHELL_ALLOW")), workDir),
	}
	if v := os.Getenv("MOS_MAX_TURNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: MOS_MAX_TURNS 无效: %w", err)
		}
		cfg.MaxTurns = n
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}
	sandbox, err := loadSandbox(workDir)
	if err != nil {
		return nil, err
	}
	cfg.Sandbox = sandbox
	return cfg, nil
}

// loadSandbox 从环境变量构造执行沙箱（MOS_SANDBOX=0 时返回 nil，shell 用默认执行方式）。
func loadSandbox(workDir string) (*tool.Sandbox, error) {
	if getenv("MOS_SANDBOX", "1") == "0" {
		return nil, nil
	}
	sb := &tool.Sandbox{
		WorkDir: workDir,
		NoNet:   getenv("MOS_SANDBOX_NO_NET", "0") == "1",
	}
	// 秒 → Duration
	if v := os.Getenv("MOS_SANDBOX_TIMEOUT"); v != "" {
		sec, err := strconv.Atoi(v)
		if err != nil || sec <= 0 {
			return nil, fmt.Errorf("config: MOS_SANDBOX_TIMEOUT 无效: %q", v)
		}
		sb.Timeout = time.Duration(sec) * time.Second
	}
	// KB → 字节
	if v := os.Getenv("MOS_SANDBOX_MAX_OUTPUT"); v != "" {
		kb, err := strconv.Atoi(v)
		if err != nil || kb <= 0 {
			return nil, fmt.Errorf("config: MOS_SANDBOX_MAX_OUTPUT 无效: %q", v)
		}
		sb.MaxOutput = int64(kb) << 10
	}
	if v := os.Getenv("MOS_SANDBOX_CPU_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: MOS_SANDBOX_CPU_SECONDS 无效: %q", v)
		}
		sb.CpuSeconds = int64(n)
	}
	if v := os.Getenv("MOS_SANDBOX_MEM_KB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: MOS_SANDBOX_MEM_KB 无效: %q", v)
		}
		sb.MemKB = int64(n)
	}
	if v := os.Getenv("MOS_SANDBOX_MAX_FILE_KB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: MOS_SANDBOX_MAX_FILE_KB 无效: %q", v)
		}
		sb.MaxFileKB = int64(n)
	}
	return sb, nil
}

// LoadToolNames 返回按需启用的工具名列表（MOS_TOOLS，逗号分隔）；
// 空表示使用业务侧全部工具。
func LoadToolNames() []string {
	return splitCSV(os.Getenv("MOS_TOOLS"))
}

// LoadSearchProvider 从 .env 文件与环境变量构建搜索提供商（web_search 工具使用）：
//
//	MOS_SEARCH_PROVIDER   duckduckgo | tavily | serper | brave | searxng（默认 duckduckgo）
//	MOS_SEARCH_REGION     duckduckgo 区域参数（如 cn-cn）
//	MOS_SEARCH_API_KEY    tavily / serper / brave 的密钥
//	MOS_SEARCH_BASE_URL   searxng 地址；duckduckgo 自定义端点
//
// 环境变量优先于 .env 文件；提供商缺失必需配置时返回错误（如未配置 API Key），
// 调用方可据此回退到默认提供商。
func LoadSearchProvider() (search.Provider, error) {
	if err := LoadEnv(); err != nil {
		return nil, err
	}
	return search.New(search.Config{
		Provider: getenv("MOS_SEARCH_PROVIDER", "duckduckgo"),
		Region:   os.Getenv("MOS_SEARCH_REGION"),
		APIKey:   os.Getenv("MOS_SEARCH_API_KEY"),
		BaseURL:  os.Getenv("MOS_SEARCH_BASE_URL"),
	})
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
