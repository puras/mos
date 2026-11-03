# MoS v0.1.0 — 首个公开版本

MoS 是基于 Go 的智能体基础框架（Agent Base SDK）：统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御。长程任务的可靠性问题（上下文腐化、工具误用、不可验证的"自我声明式完成"、无法断点恢复）由框架承接，业务代码只决定"要什么、允许什么"。

## 亮点

- **一行装配**：`mos.New(mos.Config{...})` 把模型、工具、安全、上下文工程一次装好；`Describe()` 输出装配快照，装了什么、没装什么都是事实
- **统一 LLM 接入**：OpenAI 兼容 / Anthropic / Gemini 三协议，函数调用、流式、用量归一（`FreshInputTokens` 跨协议可比，缓存读/写单列）
- **工具规模化**（M2.10）：struct tag → JSON Schema 零反射；deferred 懒加载 + `tool_search`（BM25，支持中文）让工具面增长时 prompt token 亚线性
- **安全纵深四层防线**：Policy 规则 → GuardBus 单调守卫（结构上不可绕过）→ 文件 jail + 进程沙箱 → 人工审批，全库 fail-closed
- **上下文工程**：结果截断（全文落盘可回读）、token 预算压缩（不切断 ToolCall-Result 对）、槽位式提示词装配、AGENTS.md 上下文文件发现注入
- **会话可恢复**：append-only 事件日志是唯一事实来源，崩溃恢复只补事件不改写；`session.Open(dir, id)` 一行建/恢复

## 依赖

协议层已独立发布，`go get` 直接可用，仅 3 个依赖：

- [`github.com/puras/moproto`](https://github.com/puras/moproto) v0.1.0 — 协议类型 + 用量统计
- `github.com/google/uuid` v1.6.0
- `gopkg.in/yaml.v3` v3.0.1

## 快速开始

```bash
go get github.com/puras/mos@v0.1.0
```

最小示例见 [examples/minimal](https://github.com/puras/mos/tree/v0.1.0/examples/minimal)，完整演示（策略、审批、shell、压缩、截断全开）见 [examples/chat](https://github.com/puras/mos/tree/v0.1.0/examples/chat)。

跑一个真实的智能体（`examples/minimal`，十几行装配）：

```bash
cd examples/minimal
export MOS_PROVIDER=anthropic          # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...        # 按协议取对应的那把
export MOS_MODEL=your-model-name
export MOS_BASE_URL=http://your-gateway  # 可选
go run . "只回答：1+1=?"
```

## 文档

- [docs/PRD.md](https://github.com/puras/mos/tree/v0.1.0/docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](https://github.com/puras/mos/tree/v0.1.0/docs/ARCHITECTURE.md) — 目标架构与不变式，含每个子系统契约

## 许可证

[MIT](https://github.com/puras/mos/blob/v0.1.0/LICENSE)

## 尚未包含（路线图）

- MCP 接入、Skills（FR-D11）、SubAgent / AgentTeam 多智能体（M3，S8 预切片：Skills 重做 → MCP stdio → MCP 装配）
- Docker 强隔离后端（当前进程沙箱是纵深防御的一层，不是强隔离边界）
- M2.9 经验记忆已决定不做（2026-10）
