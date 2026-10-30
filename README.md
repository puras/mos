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
| S5d | `filesystem` 文件 jail（Lexical + Realpath 双校验 + symlink 逃逸防护）+ `read`/`write` 接入 | ✅ |
| S5e | 通道三阶段（守卫 preflight + 钩子三环节 + 屏障式有界并发 + 按序回填） | ✅ |
| S5f-1 | `sandbox` 进程隔离（超时 + 进程组击杀 + 输出上限 + 资源限制 + 环境裁剪）+ `shell` 工具 | ✅ |
| S2b | `llm` 层加固：类型化状态码（`StatusError`）+ 策略化重试（只重试瞬时错误 / 指数退避 + 抖动 / 取消终止 / 可观测） | ✅ |
| S5f-2a | 参数级决策点（deny → 守卫 / ask → pre-execute 钩子，规则归宿主）+ 路径强制范围的诚实接缝 | ✅ |
| S5f-2b | 路径强制隔离：默认范围=工作区（fail-closed）、只能收窄、全开=显式逃生门；macOS Seatbelt ✅ / Linux Landlock ⬜ | 🔶 |
| S5g | 人工审批（`agent/approval`：四档结果 + 问人由宿主注入 + `approval/asked`/`decided` 审计落会话日志） | ✅ |
| S5h | 写前观察（`ObserveGate`：不许覆盖没读过的文件 + 版本 CAS 防丢失更新；能力由宿主决定） | ✅ |
| S5i | 系统提示词装配 `agent/prompt`：分节 Builder（Add 严格 / Set 覆写 / Complete 整段替换 / Clone 派生）+ 工具自带指引通道（`tool.Guideline`，文案归工具包、顺序归中心常量）+ chat 示例接线 | ✅ |
| S5i.1 | 上下文文件 `agent/contextfile`（AGENTS.md / CLAUDE.md 等）：候选名优先级 + 向上递归 + 同目录 override + 真实路径去重 + 字节预算裁剪；I/O 以 `Reader` 注入（框架不碰文件系统）+ 提示词「项目约定」节 | ✅ |
| S5i.2 | 装配层入口：`mos.Config.Context`（目录 + 发现参数，全局目录默认 `~/.mos`）+ `mos.Config.Prompt` 分节装配器（项目约定进正确槽位；与 `SystemPrompt` 二选一）+ `Describe()` 写明装了什么/降级警告 | ✅ |
| S7.2 | 多模型目录 `mos.Config.Models`（ID/Label/Chat）+ 运行期换模型 `Agent.SetModel`（转发壳，下一轮生效）+ `Agent.Compactor()` 取回压缩器（宿主可主动压一次） | ✅ |
| S6 | 上下文工程（截断 + 压缩，逐项对齐 PI/DSH：token 预算触发、模型生成结构化摘要、摘要落日志投影遮蔽、切割点不切断工具对；TaskState/Auditor 基础设施；轮次默认不限） | ✅ |
| S7 | 装配层（根包 `mos`：`mos.New(Config)` 一行装配 + 运行期旋钮 + `Describe()` 装配快照；能力即证据、缺能力 fail-closed） | ✅ |
| S7.1 | 工具名对齐 PI/DSH（`read`/`write`/`edit`）+ 搜索三件套（`grep`/`glob`/`ls`：只读、走同一份 jail、可并发、结果有上限） | ✅ |
| S7.2 | 联网工具包 `tool/web`（`web_search`/`web_fetch`：包级插件，厂商由宿主注入；抓取带 SSRF 防御——公网地址校验 + 地址钉住 + 只跟同源重定向 + 体积上限；外部内容标注） | ✅ |
| S7.3 | 工具规模化 M2.10（FR-C：deferred 懒加载 + `tool_search` BM25 发现激活，deferred 集非空才装配；C1 缓存已有、C2/C4 守护测试锁定；PI/DSH 分析见 ARCHITECTURE §3.15） | ✅ |

S1–S7 完成即覆盖 PRD G1–G6 的最小闭环；此后按 PRD §5/§7 逐个回填后续里程碑（M2.10 工具规模化 → M3 多智能体 → M4 生产化；M2.9 经验记忆已决定不做）。

## 文档

- [docs/PRD.md](docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 目标架构与不变式（怎么立）
- 旧实现与旧文档考古：`main_bak1015` 分支（如 `git show main_bak1015:README.md`）

## 示例

- [examples/chat](examples/chat/) — 直连 LLM 多轮对话 Demo：流式输出、多协议切换、会话日志持久化与重启恢复（`cp .env.example .env` 后 `go run .`）
  - **会话中途授权目录**：用户说"B 目录你也可以看"时，模型调 `grant_access` 申请 → 终端审批 → 立即可访问（文件 jail 与 shell 两层同步生效，全程留痕）
  - `-allow-shell` 开启**验证回路**：agent 能改代码后自己跑 `go build` / `go test` 看真实输出再改（配套 `lab/` 练手项目：修好一个故意留 bug 的测试）
  - `-context-window` 开启**上下文压缩**（PI 同款：超过窗口-预留时模型生成结构化摘要落日志，投影遮蔽旧历史；`-reserve-tokens`/`-keep-recent-tokens` 调预算）、`-trim` 结果截断（全文落盘可回读，默认 32KB）
  - `-agents`（默认开）加载**上下文文件**：从工作目录向上递归找 `AGENTS.md` / `CLAUDE.md`，注入系统提示词的「项目约定」节（PI 同款：同目录的 `AGENTS.override.md` 替换该目录的 AGENTS.md / CLAUDE.md）。装配走根包入口（`mos.Config.Context`），全局目录默认 `~/.mos`
- [examples/minimal](examples/minimal/) — **装配层的最小可跑示例**：`mos.NewModel` + `mos.New` + `session.Open` 十几行装出一个真能跑的智能体（证明"一行装配"不是空话）

## 构建

```bash
go test ./...
```
