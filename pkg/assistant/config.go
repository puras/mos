// Package assistant 提供开箱即用的智能体：预置常用工具、安全策略与任务循环，
// 供上层应用（CLI、服务等）一行代码集成。
//
// 本包属框架层能力：只定义组装与运行的 API 契约（New / Run / Config）。
// 模型以对象形式注入（llm.Provider），本包不关心模型接入配置
// （API Key / 地址 / 模型名如何构建属应用层事务，可参考 internal/config）。
package assistant

import "hegq.net/mos/pkg/agent"

// Config 是智能体的运行配置（框架层 API 契约）。
// 注意：模型不在此配置中——通过 New 的 provider 参数注入模型对象。
type Config struct {
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
