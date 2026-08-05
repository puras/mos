// Command assistant 是基于 MoS 基础库（pkg/）的完整智能体 CLI，
// 支持多协议 LLM 与内置工具集。
//
// 直接运行（无 API Key 时进入 mock 离线模式）：
//
//	export OPENAI_API_KEY=sk-xxx   # 或 ANTHROPIC_API_KEY
//	export MOS_MODEL=deepseek-chat # 可选
//	go run ./cmd/assistant
//
// 交互命令：/exit 退出，/reset 新开会话，/tools 查看工具，/help 帮助。
//
// 业务逻辑位于 internal/cli（本仓库内部实现，依赖 pkg 基础库）。
package main

import "hegq.net/mos/internal/cli"

func main() {
	cli.Run()
}
