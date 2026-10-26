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
| S4b | ReAct 接入工具执行通道（工具调用执行 + 回填 + 投影合法性） | ✅ |
| S5a | 执行通道骨架 `agent/execpath`（preflight 空 + 串行执行 + 按调用序回填） | ✅ |
| S5b | 双轨制 `agent/hook`（HookBus waterfall + GuardBus 单调裁决）+ 执行通道 preflight 接线 | ✅ |
| S5c | Policy 规则层 `agent/policy`（fail-closed 黑白名单；框架只认结构，文件与格式归宿主） | ✅ |
| S5d | `filesystem` 文件 jail（Lexical + Realpath 双校验 + symlink 逃逸防护）+ `read_file`/`write_file` 接入 | ✅ |
| S5e | 通道三阶段（守卫 preflight + 钩子三环节 + 屏障式有界并发 + 按序回填） | ✅ |
| S5f-1 | `sandbox` 进程隔离（超时 + 进程组击杀 + 输出上限 + 资源限制 + 环境裁剪）+ `shell` 工具 | ✅ |
| S2b | `llm` 层加固：类型化状态码（`StatusError`）+ 策略化重试（只重试瞬时错误 / 指数退避 + 抖动 / 取消终止 / 可观测） | ✅ |
| S5f-2a | 参数级决策点（deny → 守卫 / ask → pre-execute 钩子，规则归宿主）+ 路径强制范围的诚实接缝 | ✅ |
| S5f-2b | 路径强制隔离：默认范围=工作区（fail-closed）、只能收窄、全开=显式逃生门；macOS Seatbelt ✅ / Linux Landlock ⬜ | 🔶 |
| S5g | 人工审批（`agent/approval`：四档结果 + 问人由宿主注入 + `approval/asked`/`decided` 审计落会话日志） | ✅ |
| S5h | 写前观察（`ObserveGate`：不许覆盖没读过的文件 + 版本 CAS 防丢失更新；能力由宿主决定） | ✅ |
| S6 | 上下文工程（截断 + 压缩 + TaskState/Auditor；DoomLoopGuard 最小版——连续相同调用提醒 + 轮次上限可配——已先行落地） | ⬜ |
| S7 | 装配层（`assistant.New` 一行装配 + 示例 CLI） | ⬜ |

S1–S7 完成即覆盖 PRD G1–G6 的最小闭环；此后按 PRD §5/§7 逐个回填后续里程碑（M2.8 挂起恢复 → M2.9/M2.10 → M3 多智能体 → M4 生产化）。

## 文档

- [docs/PRD.md](docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 目标架构与不变式（怎么立）
- 旧实现与旧文档考古：`main_bak1015` 分支（如 `git show main_bak1015:README.md`）

## 示例

- [examples/chat](examples/chat/) — 直连 LLM 多轮对话 Demo：流式输出、多协议切换、会话日志持久化与重启恢复（`cp .env.example .env` 后 `go run .`）
  - `-allow-shell` 开启**验证回路**：agent 能改代码后自己跑 `go build` / `go test` 看真实输出再改（配套 `lab/` 练手项目：修好一个故意留 bug 的测试）

## 构建

```bash
go test ./...
```
