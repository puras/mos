// Package assistant 提供开箱即用的智能体：预置常用工具、安全策略与多协议 LLM 接入，
// 供上层应用（CLI、服务等）一行代码集成。
//
// 本包属框架层能力：只定义组装与运行的 API 契约（Config / New / Run）。
// 具体配置来源（环境变量、配置文件等）属应用层，由上层自行组装，
// 可参考 internal/config 的应用层实现。
package assistant

import "hegq.net/mos/pkg/agent"

// Config 是智能体的运行配置（框架层 API 契约）。
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
