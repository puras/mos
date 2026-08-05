package assistant

import (
	"context"
	"fmt"

	"hegq.net/mos/pkg/agent"
	"hegq.net/mos/pkg/llm"
	"hegq.net/mos/pkg/llm/anthropic"
	"hegq.net/mos/pkg/llm/openai"
	"hegq.net/mos/pkg/tool"
	"hegq.net/mos/pkg/tool/builtin"
)

// Assistant 是组装好的智能体实例：多协议 Provider + 内置工具集 + 安全策略。
// 外部工程可直接使用 New + Run 集成。
type Assistant struct {
	agent  *agent.Agent
	config Config
	tools  *tool.Registry
	policy *DefaultPolicy
}

// New 根据配置构建智能体。
func New(cfg Config) (*Assistant, error) {
	provider, err := buildProvider(cfg)
	if err != nil {
		return nil, err
	}

	reg := tool.NewRegistry()
	for _, t := range defaultTools() {
		if err := reg.Register(t); err != nil {
			return nil, fmt.Errorf("assistant: 注册工具失败: %w", err)
		}
	}

	policy := NewDefaultPolicy(cfg.ShellAllowed, cfg.WorkDir)
	opts := []agent.Option{
		agent.WithPolicy(policy),
		agent.WithSystemPrompt(cfg.SystemPrompt),
		agent.WithObserver(cfg.Observer),
	}
	if cfg.MaxIterations > 0 {
		opts = append(opts, agent.WithMaxIterations(cfg.MaxIterations))
	}
	ag := agent.New(provider, reg, opts...)
	return &Assistant{agent: ag, config: cfg, tools: reg, policy: policy}, nil
}

// Agent 返回底层智能体运行时，可用于挂载自定义 Observer。
func (a *Assistant) Agent() *agent.Agent { return a.agent }

// Config 返回当前配置。
func (a *Assistant) Config() Config { return a.config }

// Policy 返回当前安全策略。
func (a *Assistant) Policy() *DefaultPolicy { return a.policy }

// NewSession 创建一个新会话。
func (a *Assistant) NewSession() *agent.Session { return agent.NewSession("") }

// Run 在会话上执行一次任务。
func (a *Assistant) Run(ctx context.Context, sess *agent.Session, input string) (string, error) {
	return a.agent.Run(ctx, sess, input)
}

// ToolNames 返回已注册的工具名列表。
func (a *Assistant) ToolNames() []string { return a.tools.Names() }

// buildProvider 根据配置创建 LLM Provider。
func buildProvider(cfg Config) (llm.Provider, error) {
	switch cfg.Provider {
	case "mock":
		return mockProvider{}, nil
	case "openai":
		return openai.New(llm.Config{
			"api_key":  cfg.APIKey,
			"base_url": cfg.BaseURL,
			"model":    cfg.Model,
		})
	case "anthropic":
		return anthropic.New(llm.Config{
			"api_key":  cfg.APIKey,
			"base_url": cfg.BaseURL,
			"model":    cfg.Model,
		})
	default:
		return nil, fmt.Errorf("assistant: 未知 Provider %q（支持 openai/anthropic/mock）", cfg.Provider)
	}
}

// defaultTools 返回预置工具集。
func defaultTools() []tool.Tool {
	return []tool.Tool{
		builtin.Calculator{},
		builtin.CurrentTime{},
		builtin.WebFetch{},
		builtin.Shell{},
		builtin.ReadFile{},
		builtin.WriteFile{},
		builtin.FindFiles{},
		builtin.Grep{},
	}
}

// ---- mock Provider：离线演示用 ----

type mockProvider struct{}

func (mockProvider) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming
}

func (mockProvider) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	// 依据历史中已完成的工具调用轮次，依次演示 calculator → current_time → 最终答复
	var toolRounds int
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			toolRounds++
		}
	}

	if toolRounds >= 2 {
		// 两轮工具调用已完成，汇总结果作答
		var calcResult string
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && m.Name == "calculator" {
				calcResult = m.Content
			}
		}
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "（mock）计算完成，结果为 " + calcResult + "。配置环境变量并重启后接入真实模型。",
		}}, nil
	}

	toolName, args := "calculator", `{"expr":"(1+2)*3"}`
	if toolRounds == 1 {
		toolName, args = "current_time", `{}`
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "让我来处理",
		ToolCalls: []llm.ToolCall{{ID: "mock_1", Name: toolName, Args: args}},
	}}, nil
}

func (mockProvider) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}
