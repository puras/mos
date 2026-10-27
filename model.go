package mos

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/gemini"
	"github.com/puras/mos/llm/openai"
)

// ModelConfig 是模型装配配置：协议 + 凭据 + 模型名，外加两个可选装饰器
// （重试 / 日志）。
//
// **参数是宿主的，机制是框架的**：框架负责把协议适配器、重试退避、日志回调
// 接好；用哪个协议、拿哪个模型、重试几次、怎么打日志，由宿主决定。
type ModelConfig struct {
	// Provider 是协议名：openai / anthropic / gemini（必填）。
	Provider string
	// APIKey 是凭据（必填）。框架**不读环境变量**（配置来源归宿主）——
	// 用 APIKeyEnv 公布的环境变量名，宿主自己去取。
	APIKey string
	// BaseURL 是网关地址（可选）：留空用各适配器自带的默认。
	BaseURL string
	// Model 是模型名（必填）。
	Model string
	// MaxTokens 是输出上限（可选）：留空用适配器默认（anthropic 有硬上限，
	// 太小会让回答半截、工具参数残缺）。
	MaxTokens string
	// Extra 是适配器私有参数（如 anthropic 的 thinking 预算、gemini 的
	// require_finish）。同名键会覆盖上面几个标准键。
	Extra llm.Config
	// Retry 是重试策略。零值 = 不重试。
	Retry llm.RetryPolicy
	// OnRetry 是重试排程回调（静默重试是查不出问题的）。
	OnRetry func(attempt int, delay time.Duration, err error)
	// Log 是调用结束回调（非空 = 挂日志装饰器）。
	Log func(llm.CallResult)
}

// NewModel 按配置装配一个对话模型：协议适配器 + 重试 + 日志。
//
// 缺 Provider / APIKey / Model 直接报错（fail-closed：拿半截配置去跑，
// 错误会推迟到调用时才暴露，难查）。
func NewModel(cfg ModelConfig) (llm.ChatModel, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		return nil, errors.New("mos: 未指定协议（ModelConfig.Provider 必填）")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("mos: 未提供 %s 的凭据（ModelConfig.APIKey 必填）", provider)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("mos: 未指定模型名（ModelConfig.Model 必填）")
	}

	llmCfg := llm.Config{
		"api_key": cfg.APIKey,
		"model":   cfg.Model,
	}
	if cfg.BaseURL != "" {
		llmCfg["base_url"] = cfg.BaseURL
	}
	if cfg.MaxTokens != "" {
		llmCfg["max_tokens"] = cfg.MaxTokens
	}
	for k, v := range cfg.Extra {
		llmCfg[k] = v
	}

	model, err := defaultRegistry().Create(provider, llmCfg)
	if err != nil {
		return nil, fmt.Errorf("mos: 创建 %s 模型: %w", provider, err)
	}

	// 重试：只重试瞬时错误（限流 / 5xx / 连接失败 / 空响应）——重试确定性的
	// 错误只是浪费配额。参数由宿主给。
	if cfg.Retry.MaxAttempts > 1 {
		model = llm.WithRetry(model, cfg.Retry)
	}
	if cfg.Log != nil {
		model = llm.WithLogging(model, cfg.Log)
	}
	return model, nil
}

// APIKeyEnv 返回某协议约定的密钥环境变量名：
//
//	openai    → OPENAI_API_KEY
//	anthropic → ANTHROPIC_API_KEY
//	gemini    → GEMINI_API_KEY
//
// 框架**不读环境变量**（配置来源归宿主，docs/assembly.md A3）——只公布名字，
// 宿主自己去取（`os.Getenv(mos.APIKeyEnv(p))`）。未知协议返回空串。
func APIKeyEnv(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	default:
		return ""
	}
}

// defaultRegistry 返回内置协议适配器的注册表（openai / anthropic / gemini）。
//
// 宿主要别的协议（或要换工厂实现）时自己建注册表——这里只是"开箱即用"的那份。
func defaultRegistry() *llm.Registry {
	reg := llm.NewRegistry()
	reg.Register("openai", openai.New)
	reg.Register("anthropic", anthropic.New)
	reg.Register("gemini", gemini.New)
	return reg
}
