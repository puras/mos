// Command assistant 是基于 MoS SDK（hegq.net/mos）的完整示例应用：
// 多协议 LLM（openai/anthropic/gemini）、内置工具集（pkg/tool/builtin）与
// 扩展工具（pkg/tool/extension：shell / web_search）组成的交互式 CLI。
//
// 运行（在 examples/assistant 目录下）：
//
//	export OPENAI_API_KEY=sk-xxx   # 或 ANTHROPIC_API_KEY
//	export MOS_MODEL=deepseek-chat # 可选
//	go run .
//
// 配置写入本目录 .env（键名与环境变量一致，路径可用 MOS_CONFIG 覆盖）；
// 环境变量优先于 .env 文件。
//
// 交互命令：/exit 退出，/reset 新开会话，/tools 查看工具，/help 帮助。
//
// 业务逻辑位于本工程的 internal/（cli / config），仅依赖 SDK 的 pkg/ 基础库。
package main

import "hegq.net/mos/examples/assistant/internal/cli"

func main() {
	cli.Run()
}
