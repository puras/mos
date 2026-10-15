# MoS

MoS（取自 Sapient）是基于 Go 的智能体基础框架（Agent Base SDK）：统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御，帮助快速构建智能体应用与智能体平台。

## 当前状态：重建中

> 2026-10-15 起按 [docs/PRD.md](docs/PRD.md) 分片重建；v2 全量旧实现归档于 `main_bak1015` 分支（本地 + GitHub），可随时取回参考。

| 切片 | 内容 | 状态 |
| --- | --- | --- |
| S0 | 骨架（本提交） | ✅ |
| S1 | 会话与事件日志（append-only + 消息投影 + JSONL 持久化） | ✅ |
| S2 | LLM 接入（ChatModel + openai/anthropic/gemini + transport envelope + httpx seam，自归档移植） | ✅ |
| S4a | 最小 ReAct 循环（planner.ReAct + LoopDriver，无工具）——首个聊天 Demo | ✅ |
| S3 | 工具系统（schema 生成 + Registry + builtin 最小集） | ✅ |
| S4b | ReAct 接入工具执行通道（守卫 → 批量并发 → 审计回填） | ⬜ |
| S5 | 安全层（Policy + GuardBus + filesystem jail） | ⬜ |
| S6 | 上下文工程（截断 + 压缩 + TaskState/Auditor） | ⬜ |
| S7 | 装配层（`assistant.New` 一行装配 + 示例 CLI） | ⬜ |

S1–S7 完成即覆盖 PRD G1–G6 的最小闭环；此后按 PRD §5/§7 逐个回填后续里程碑（M2.8 挂起恢复 → M2.9/M2.10 → M3 多智能体 → M4 生产化）。

## 文档

- [docs/PRD.md](docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 目标架构与不变式（怎么立）
- 旧实现与旧文档考古：`main_bak1015` 分支（如 `git show main_bak1015:README.md`）

## 示例

- [examples/chat](examples/chat/) — 直连 LLM 多轮对话 Demo：流式输出、多协议切换、会话日志持久化与重启恢复（`cp .env.example .env` 后 `go run .`）

## 构建

```bash
go test ./...
```
