# MoS PRD（产品需求文档）

> 版本：v0.2（草案）｜ 日期：2026-11-04 ｜ 维护：puras
> 更新记录：**2026-11-04** —— **FR-D11 可用性优化（S8.4-C）**：**技能正文调用时现读 + 宿主显式重扫**——目录扫描只取 frontmatter 元数据与正文路径（`Skill.Body` → `Skill.BodyPath`），正文在 `skill` 工具 / `/name` 展开时现读（改了 `SKILL.md` 立刻生效）；`Catalog` 改快照持有者（`atomic.Pointer[Snapshot]`，重扫整体换指针）；`Agent.ReloadSkills()` 显式、幂等、全量重扫，**摘要不含正文**（改正文不必 reload）；清单变化走 **user 侧替换目录消息**（静态提示词不重渲染，保 prompt cache）；正文预算改为调用时校验。**破坏性变更**：`ExpandSkill` / `ExpandInvocation` 增返回 `error`。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 可用性优化第二片（S8.4-B）**：**认证可诊断 + `TokenProvider`**——`ErrorKind` 新增 `auth` 类（HTTP 401/403 与取凭据失败）；`ServerConfig.TokenProvider`（每次请求取一次，**不缓存不刷新**，空串 = 不注入头）；凭据注入点 = HTTP RoundTripper（`tool/mcp/auth.go`，把状态码变成结构化分类，而不是解析 SDK 的错误文本）；静态 `Headers` 此前是死配置，本片接线。**不做** OAuth 流程（凭据生命周期归宿主）。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 可用性优化第一片（S8.4-A）**：**MCP 服务器摘要进静态系统提示词**——`agent/prompt` 新增 `mcp_servers` 节（`OrderMCPServers = 850`）、`tool/mcp/summary.go` 渲染器（只列连接成功且有 deferred 工具的服务器；摘要取静态 `ServerConfig.Description` 首行；整节 ≤4KB、单条 ≤250 字节；渲染层自己排序保证确定性）、装配层把提示词渲染点后移到 MCP 装配之后。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 MCP 接入落地（S8.2 + S8.3）**：新包 `tool/mcp`（**官方 Go SDK** `github.com/modelcontextprotocol/go-sdk` v1.8.0，stdio + streamable-http 两个 transport，协议基线 `2026-07-28`）+ 工具适配（`mcp__<server>__<tool>` 命名、schema 直通、结果投影）+ 两阶段原子换代（换代失败保留上一代）+ 装配入口（`mos.Config.MCP`：一开即装、默认 deferred、hidden 档、失败隔离、服务器说明每会话一次性 user 侧注入、`Describe()` 快照、`Agent.Close()`）。**依赖纪律破例**：新增 9 个传递依赖，SDK 只在 `tool/mcp` 内部使用、类型不外泄。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.2-mcp.md`。
> 更新记录：**2026-11-04** —— **FR-D11 Skills 重做落地（S8.1）**：新包 `agent/skill`（frontmatter 解析 + 目录发现 + `<available_skills>` 清单 + `<skill_content>` 正文 + 用户显式 `/name` 展开器 + 本轮激活状态）+ 专用 `skill` 工具（正文按需加载，走普通工具结果/历史）+ `skill/activate` 事件 + 装配入口（`mos.Config.Skills` / `Agent.ExpandSkill`）；**技能目录不再要求登记为文件读根**——目录列表 = 宿主声明的授权边界，读操作走注入的 `Reader`。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.1-skills.md`。
> 更新记录：**2026-11-03** —— 按重建树实况**回填与收口**：§4 改为「重建树现状 / 旧树未带回」两栏对照（原文整段描述的是 10-15 前的旧树，含重建未带回的 EventBus / seam / scope / telemetry / 4 驱动 / Skills / RAG / Doom Loop 等）；新增 **FR-G（重建期补齐）** 三条——FR-G1 上下文文件、FR-G2 联网工具、FR-G3 挂起-恢复（checkpoint + handle，**重建未做**，是 FR-E6 的前置）；FR-A1/FR-A2 标 ✅（协议层已独立发布、v0.1.0 已发）；失效文档引用（`docs/architecture-v2.md` / `docs/sdk-contracts.md` / `docs/agent/*` / `FEATURES.md` / `examples/{cli,protocli,agentcli}`）换成重建树实际文件。
> 更新记录：**2026-11-02** —— Skills 状态从「已交付（M2.6）」改为 **FR-D11 待建**（重建树无实现；用户拍板：按新架构重做，不移植旧码）；FR-D8 五步交互的「请求制定」改为「**暴露策略（exposure）**」；开放问题 2（MCP transport）定「先 stdio」、5（SubAgent/MCP 先后）定「MCP 先」。分析见 `docs/ARCHITECTURE.md` §3.16。
> 需求权威来源：`docs/ARCHITECTURE.md`（目标架构与不变式，含各子系统契约）+ `docs/platform-contract.md`（平台契约：事件 JSON / 状态序列化 / 并发）+ `docs/assembly.md`（装配层）+ `README.md` 路线图；本文将其整理为可跟踪的产品需求视图。
> 状态约定：**重建树现状见 §4**（§4.1 分层 / §4.2 能力盘点按「重建树现状 vs 旧树未带回」两栏写）；待建需求见 §5（FR-A ~ FR-G）+ §6（NFR）。API 细节以 `docs/ARCHITECTURE.md` 与源码为准。

---

## 1. 产品概述

### 1.1 一句话定位

MoS（取自 Sapient）是基于 Go 的**智能体基础框架（Agent Base SDK）**：提供统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御，帮助快速构建智能体应用与智能体平台。

### 1.2 解决的问题

在模型能力之上，长程任务的可靠性瓶颈在 **harness**（驾驭层）：上下文腐化、工具误用、不可验证的"自我声明式完成"、无法断点恢复、缺乏审计。MoS 把这些工程问题沉淀为可复用的框架能力：

- **状态写文件不写上下文**：`TaskState` 外置、会话事件日志（append-only）是唯一事实来源；
- **只接受环境独立验证过的事实**：`Auditor` 审计门，模型自述不算数；
- **上下文越少越好**：截断（M2.3）+ fresh-context 压缩（M2.5）+ 槽位式系统提示词（M2.6）+ Skills 懒加载（**M3a 已落地，FR-D11**）；
- **能写成 Linter 的约束别写成文档**：`Policy` 执行前拦截 + `GuardBus` 单调守卫 + `Sandbox` 执行中隔离。

### 1.3 三层职责边界（MoS 是 SDK，不越界）

| 层 | 归属 | 职责 |
| --- | --- | --- |
| MoS | 本仓库（SDK 内核） | Agent 任务循环、工具系统、会话/审计/状态、上下文工程、钩子守卫、审批与断点恢复、事件观测 |
| SaaS 平台 | 独立平台（MOSSCloud） | HTTP/WS API、多租户、鉴权、任务调度、计费、Dashboard、对象存储、人工确认通道 |
| 协议层 | `github.com/puras/moproto`（**已独立发布**，v0.1.0） | LLM 协议归一、密钥管理、模型路由；MoS 只消费其纯定义包，仓库只读 |

MoS 明确不做：HTTP 服务、多租户、任务队列、计费、可视化；不操作数据库（持久化只提供 `Persistence` 接口 + 内存/JSONL 默认实现，数据库后端由业务层注入）；不做运行期插件热加载（只做装配时选组件）。

### 1.4 设计理念（需求推导原则）

Harness Engineering 四铁律 + Loop Engineering 九杠杆 + Audit Gate 审计式状态迁移 + Graph Engineering 判据门控（满足分支/并行/持久/可审计才引入显式图）+ 补偿面理论（harness 随模型变强主动拆组件）。详见 README「工程参考」章节。

---

## 2. 目标用户与使用场景

| 用户 | 场景 | 入口 |
| --- | --- | --- |
| 应用开发者 | 在 Go 工程中嵌入一个开箱即用的智能体（工具 + 安全策略 + 审计） | `mos.New` 一行装配（根包） |
| 平台开发者 | 自建智能体平台：声明式配置装配、挂起-恢复、事件流对接观测 | `mos.Config`（结构进，格式归宿主）、`session.Persistence`；挂起-恢复待建（FR-G3） |
| 框架研究者 | 理解/扩展 agent 运行时（驱动、钩子、守卫、安全边界） | `docs/ARCHITECTURE.md`、`docs/tutorial.html`、`examples/chat` |
| 业务集成方 | 换模型/换后端而不改业务代码 | `llm.Registry`（协议注册表）+ `llm` 装饰器（Retry / Logging / Timeout / Fallback / Cache） |

---

## 3. 产品目标与非目标

### 3.1 目标（G）

| 编号 | 目标 | 度量 |
| --- | --- | --- |
| G1 | 循环可替换：ReAct / Plan&Execute / Reflection / Scripted 按 `LoopDriver` 实现，任务可路由切换 | 新增一种循环 = 一个 `LoopDriver.Run` 实现，不污染共享设施 |
| G2 | 环节可改写：模型输入 / 工具执行前后等环节可被钩子改写，安全约束不可被钩子绕过（HookBus / GuardBus 双轨制） | 纵深防御测试覆盖（守卫绕过不可行） |
| G3 | 提供者可替换：Sandbox / Memory / Retriever / Policy / Driver 经 `agent/seam` 按名注册、YAML 装配 | 纯配置可组装可运行 Agent（`config.Assemble`） |
| G4 | 会话可重放可恢复：append-only 事件日志 + `Restore` + Checkpoint 挂起-续跑 | 崩溃恢复测试（孤儿 turn 合成 interrupted）；审批挂起-恢复 E2E |
| G5 | 安全纵深：Policy（规则层）→ Sandbox（隔离层）→ 审批（人工层）→ 审计（验证层）四层防线 | fail-closed 纪律全绿（NFR-1） |
| G6 | 近零依赖：SDK 仅依赖协议层库 + `google/uuid` + `gopkg.in/yaml.v3` | `go.mod` require 审计 |

> **实际覆盖度（2026-11-03 对账重建树）**：G2 / G5 / G6 已闭环；**G1** 只有 ReAct 一个驱动（PlanExecute / Reflection / Scripted 待回填）；**G3** 无 `agent/seam` 与 YAML 装配（`agent/policy` 支持 YAML，框架层未做）；**G4** 的「Checkpoint 挂起-续跑」未做（→ FR-G3）。README 曾写「S1–S7 完成即覆盖 G1–G6 最小闭环」，按重建树实况**不成立**，以本表为准。

### 3.2 非目标（不做）

产品层（Web UI / 网关 / 调度 / 多租户 / 计费）、数据库驱动与迁移、运行期 DI 容器、模型训练与微调、CLI 产品化（`examples/*` 仅为示例与学习材料）。

---

## 4. 现状盘点（重建树 vs 旧树基线）

### 4.1 分层架构（重建树现状）

```text
装配层：mos.New（根包；模型 + 工具 + 安全 + 上下文工程一次装成，nil 用框架默认）
驱动层：LoopDriver（ReAct 已落地；PlanExecute / Reflection / Scripted 待回填）
执行通道：agent/execpath（守卫裁决 → 屏障式有界并发 → 按调用序回填，各驱动共用）
基础设施：session 事件日志 + agent/hook（HookBus / GuardBus）+ agent/prompt + agent/contextfile
能力层：llm（+transport 共用 envelope）/ tool（builtin + web + searchtools）/ filesystem / sandbox / httpx
协议层：moproto（外部已发布仓库，只读）
```

> 旧树（`main_bak1015`）多出的层——EventBus / Scope / Telemetry seam / `agent/seam` / `agent/config` / DriverRouter / `memory` ——**重建未带回**。是否回填按 PRD 逐项决定：目前只有 **FR-G3（挂起-恢复）** 明确要回填，其余待定（§4.2 末列逐项标出）。

### 4.2 能力盘点（按域）

| 域 | 重建树现状（2026-11-03） | 旧树有、重建未带回 |
| --- | --- | --- |
| LLM 接入 | `llm.ChatModel` 抽象；OpenAI / Anthropic / Gemini 三适配器（函数调用 + 流式 + 用量归一 + FinishReason 透传）；`llm/transport` 共享 HTTP envelope；`llm.Registry` 协议注册表；5 个装饰器（Retry / Logging / Timeout / Fallback / Cache）；`StatusError` 结构化状态码 | — |
| 工具系统 | `tool`：结构体 tag → JSON Schema（注册时缓存）+ 并发 opt-in 分类器 + deferred 懒加载（`tool/searchtools` 的 `tool_search`，BM25）；`tool/builtin` 9 个工具（`current_time` / `read` / `write` / `edit` / `grep` / `glob` / `ls` / `grant_access` / `shell`）；`tool/web`（`web_search` / `web_fetch` + SSRF 防御） | `tool/extension`（5 家搜索提供商）、`tool.SequentialTool` 串行回退 |
| 安全 | `agent/policy` 名字级黑白名单（fail-closed）+ 参数级决策点（deny → 守卫 / ask → 审批，规则归宿主）；`agent/hook` HookBus（可改写）+ GuardBus（单调裁决）；`filesystem` 双层校验 jail + 运行期可加根（`RootSet`）+ 写前观察门；`sandbox` 进程隔离（超时 + 进程组击杀 + 输出上限 + 资源限制 + 环境裁剪）+ macOS Seatbelt 路径强制；`agent/approval` 四档人工审批 | `DefaultPolicy` 分级（绝对危险 / 受限危险）、`request_human_input` 工具 |
| 会话与状态 | `session` append-only 事件日志 + 确定性投影 + JSONL 持久化（fail-fast）+ 崩溃恢复（孤儿 turn 合成 interrupted）+ `TaskState` 外置 + `agent` 的 Auditor 审计门 | fork（Header 已预留 `parentId` / `seedSeq` 字段，能力未做） |
| 上下文工程 | `agent` 截断（行/字节双限 + 全文落盘可回读）+ fresh-context 压缩（token 预算触发 + 模型生成摘要落日志 + 投影遮蔽）；`agent/prompt` 分节装配（Add / Set / Complete + 工具自带指引通道）；`agent/contextfile` 上下文文件发现（AGENTS.md / CLAUDE.md 向上递归 + 同目录 override + 字节预算）；**`agent/skill` 技能（FR-D11，2026-11-04 落地）**：目录发现 + 清单进系统提示词 + 专用 `skill` 工具按需加载正文 | 40% Dumb Zone 告警、Doom Loop 守卫、RAG / `memory.Retriever` |
| 循环生态 | `agent/driver` LoopDriver + `agent/driver/react` ReAct；批量工具三阶段执行（preflight → 屏障式有界并发 → 按调用序回填） | PlanExecute / Reflection / Scripted 驱动、HybridRouter、`agent/driver/scripted` |
| 生命周期 | `agent/approval`（问人由宿主注入 + 审计落会话日志）；`filesystem.RootSet` 运行期加根 | `agent/scope`、`agent/checkpoint`、`agent/handle`（→ FR-G3） |
| 可观测 | 事件类型覆盖 turn / assistant / tool / approval / compaction；`sess.Events()` 可遍历；`Describe()` 装配快照（装了什么 + 没装什么 + 降级警告） | `EventBus` 四模式、`Observer` 回调、Telemetry seam、`fs/resolve` 审计事件 |
| 装配 | 根包 `mos.New(mos.Config)` + 运行期旋钮（并发 / 决策轮次 / 换模型）+ `mos.NewModel` / `session.Open` / `builtin.ToolsIn` 样板收进框架；`mos.Version` 版本常量 | `assistant.Config`、`agent/seam` + `agent/config`（YAML → Agent）、声明式驱动选择 |
| 示例与文档 | `examples/minimal`（装配层最小示例）、`examples/chat`（全功能演示）、`examples/agents`（多 Agent 协作示例）；`docs/ARCHITECTURE.md` / `docs/platform-contract.md` / `docs/assembly.md` / `docs/tutorial.html` / `docs/index.html` / `docs/release-v0.1.0.md` | `examples/cli` / `examples/protocli` / `examples/agentcli`、`docs/agent/` 18 篇、`docs/assistant.md`、`docs/llm.md`、`FEATURES.md` |

### 4.3 里程碑交付记录

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 / M1.5 / M1.6 | 任务循环、多协议接入、工具系统、事件观测 / 装配 + Policy / 执行沙箱 | ✅（旧树交付；重建树重写为根包装配层） |
| M2.1 ~ M2.7 | 状态外置、审计门、输出截断、fresh-context 压缩、槽位式系统提示词 | ✅（重建期 S6 重做：截断 + 压缩 + TaskState/Auditor + `agent/prompt`）；**Doom Loop / 40% Dumb Zone / RAG 未带回**（不排期） |
| v2 架构重构（v2.3 ~ v2.17） | EventBus / Session 日志 / Hook+Guard / RunContext+LoopDriver / seam / router / executor / scope / 持久化 / telemetry | ◐ 重建带回：Session 日志 / Hook+Guard / RunContext+LoopDriver / 持久化；**未带回**：EventBus / seam / router / executor / scope / telemetry（归档在 `main_bak1015`，按 §4.2 末列处置） |
| M2.8 异步 Approval | `agent/approval`：四档结果 + 问人由宿主注入 + `approval/asked` / `approval/decided` 审计落会话日志 | ✅（重建期 S5g） |
| M2.8+ 文件工具 Jail | `filesystem`：Lexical + Realpath 双校验 + symlink 逃逸防护；写前观察门（S5h） | ✅（重建期 S5d / S5h） |
| M2.8+ Checkpointer + AgentHandle | 跨进程挂起-续跑（checkpoint 快照 + handle create/resume/dispose） | ⬜ **重建未做** → FR-G3（FR-E6 的前置） |
| M2.10 | FR-C 工具规模化：deferred 懒加载 + `tool_search` BM25 发现激活（C1 缓存在重建时已落地；C2/C4 守护测试锁定）；C3 的「超阈值自动启用」改为「deferred 集非空才装配 `tool_search`」（PI/DSH 均无阈值机制，不自创，分析见 ARCHITECTURE §3.15） | ✅（重建周期 S7.3） |
| M2.9 | FR-B 经验记忆 | ❌ 已决定不做（2026-10，用户拍板） |
| 重建补齐 | FR-G1 上下文文件（`agent/contextfile` + `agent/prompt`）/ FR-G2 联网工具（`tool/web`） | ✅（2026-10/11 落地，PRD 于 2026-11-03 回填登记） |
| M3a | FR-D11 Skills 重做：`agent/skill` 新包（frontmatter 解析 + 目录发现 + `<available_skills>` 清单 + `<skill_content>` 正文 + `/name` 展开器 + 本轮激活状态）+ 专用 `skill` 工具（正文走普通工具结果）+ `skill/activate` 事件 + 装配入口（`mos.Config.Skills` / `Agent.ExpandSkill`） | ✅（2026-11-04，S8.1；说明见 `docs/s8.1-skills.md`） |
| M3a | FR-D8 MCP 接入：`tool/mcp` 新包（**官方 Go SDK**：`github.com/modelcontextprotocol/go-sdk` v1.8.0；stdio + streamable-http 两个 transport；协议基线 `2026-07-28`）+ 工具适配（`mcp__<server>__<tool>` 命名、schema 直通、结果投影）+ 两阶段原子换代 + 装配入口（`mos.Config.MCP`：一开即装、默认 deferred、hidden 档、失败隔离、服务器说明每会话一次性注入、`Describe()` 快照） | ✅（2026-11-04，S8.2 + S8.3；说明见 `docs/s8.2-mcp.md`） |
| M3a | FR-D8 可用性优化 **S8.4-A**：MCP 服务器摘要进**静态**系统提示词（`agent/prompt` 新增 `mcp_servers` 节 + `tool/mcp` 渲染器 + 装配期接线；只列连上且有 deferred 工具的服务器） | ✅（2026-11-04，S8.4-A；说明见 `docs/s8.4-usability.md`） |
| M3a | FR-D8/D11 可用性优化 **S8.4-B/C/D/E**：认证可诊断 + `TokenProvider`、技能正文现读 + `Agent.ReloadSkills()`、`allowed-tools` 求交诊断、MCP 资源工具（三个共享工具） | 🔄（B ✅ / C ✅ 2026-11-04；D / E ⬜；分析见 `docs/ARCHITECTURE.md` §3.16） |
| M3 ~ M5 | 见 §5 需求 | ◐ M3a（FR-D11 Skills / FR-D8 MCP）已落地；M3b（SubAgent / AgentTeam / 并行 DAG）未开始 |

---

## 5. 功能需求（Roadmap → FR）

优先级定义：**P0** = 发布/下一里程碑阻塞项；**P1** = 本版本目标；**P2** = 规划中、实施前需修订细化。

### 5.1 FR-A：发布前待办（P0，已完成）

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-A1 | 协议层 `moproto` 脱离本地 `replace`：拆分为独立 module 或发布到可达仓库（对齐 AGENTS.md「对外发布前需拆分协议层」待办）；MoS 对协议层只消费 `pkg/` 纯定义包，**协议仓库只读**（缺陷与建议走 PR/issue，不直接改） | 外部工程 `go get github.com/puras/mos` 无需本地路径即可构建；`go.mod` 无本地 replace | ✅（2026-11-03：replace 已移除，moproto v0.1.0 已独立发布，`go get` 可用） |
| FR-A2 | 文档与实现同步：README 等处 import 路径 `hegq.net/mos` → `github.com/puras/mos`；FEATURES.md 更新记录补齐 2026-09 ~ 10 变更（capability 拆分、`llm/transport`、moproto 迁移、agentcli/docs 新增） | 文档 grep 无旧模块路径；新贡献者按 README 可跑通 | ✅（模块路径已统一；`FEATURES.md` 未建——属旧树文档，随旧树归档；重建树的文档集见 §4.2 末行） |

### 5.2 FR-B：M2.9 经验记忆（知识进化第 1 层，P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-B1 | 执行历史沉淀为经验库：成功策略 / 已知陷阱入库，按成功率 / 时间 / 成本加权评分择优 | 经验写入 → 评分 → 检索 → 注入决策链路 E2E 测试 |
| FR-B2 | 决策时检索注入：复用 M2.7 `memory.Retriever` 基础设施与注入钩子 | 未注入经验时行为与现状完全一致（零回归）；注入走 `CustomMsg(Source="experience")`，不进会话历史 |

### 5.3 FR-C：M2.10 工具规模化（P1，已交付，见 §4.3 里程碑表）

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-C1 | schema 生成缓存：`generateSchema` 反射结果按工具 memo 化 | 循环内不再重复反射（基准测试佐证） | ✅ 重建时已落地（`Registry.Register` 时生成） |
| FR-C2 | schema 压缩：删 title / $defs、过滤注入参数 | 注入 schema 体积下降且参数解析不受影响 | ✅ 生成器本就不产出 title/$defs（守护测试锁定） |
| FR-C3 | deferred 懒加载：工具只发名字进 prompt，`tool_search` 按需取 schema；工具描述占上下文超阈值（参考 Claude Code 10%）自动启用，小工具集保持全量注入 | 工具数增长时 prompt token 亚线性；默认行为对现有用户不变 | ✅ deferred 集 + `tool_search`（BM25）已落地；**「10% 阈值」未做**——PI/DSH 均无此机制（PI 配置驱动 / DSH 作者标记），不自创，改为「deferred 集非空才装配」 |
| FR-C4 | 静态系统提示保 prompt cache：动态内容移至 user 消息侧 | 连续两轮请求 system 段逐字节一致（缓存命中） | ✅ 分节装配器装配期渲染一次（守护测试锁定） |

### 5.4 FR-D：M3 多智能体编排（P1，建议分期 M3a / M3b）

| 编号 | 需求 | 关键约束 / 验收标准 |
| --- | --- | --- |
| FR-D1 | Coordinator 路由与编排拓扑：按意图分派（规则硬路由保证确定性可审计，LLM 分类兜底）；拓扑先「监督者/层次化」，后演进「批评者-审查者」 | 基于 `agent/router` 扩展；路由决策落会话日志可回放 |
| FR-D2 | 三权分离 Manager / Executor / Auditor：派发、执行、审计职责分属不同角色 | 审计不变式对每个成员独立生效（NFR-5） |
| FR-D3 | SubAgent 子智能体（首选 agents-as-tools 形态）：声明式定义（`name` / `description` 自动委派匹配 / `model` 覆盖 / `tools` 白名单 / 提示词正文，对齐 `.claude/agents/` 风格）；实现 `tool.Tool` 包装进 Registry，主 Agent 经 function calling 调用只收结构化结果 | 工具白名单即 blast radius 收缩（fail-closed）；独立 Context，中间工具调用留内部；回传自述仍须 Auditor 验证；决策判据入文档——instructions/tools/policy 真正改变才拆 |
| FR-D4 | 双阶段架构（Plan&Solve）：Initializer 写 plan.md 退出 → Executor 接力执行，不共享 Context Window；Replanner 随新约束修订计划 | 计划文件落 `runs/<run-id>`（依赖 FR-E6 可先落工作目录）；跨窗口接力 E2E |
| FR-D5 | LLMCompiler 多智能体并行 DAG：基于 `agent/driver/scripted`（单智能体先行版已落地）扩展 Map-Reduce——分支并发、结果经 TaskState 汇聚、合并只读分支摘要、并行度作为计划元数据 | DAG 校验 fail-closed（依赖环 / 节点存在性）；并发分支数有界 |
| FR-D6 | AgentTeam 智能体团队：Team Lead + Teammates + Shared Task List（`pending → in_progress → completed` + `blocks/blockedBy` 依赖自动解锁）+ Mailbox 点对点消息；状态落盘（角色/上下文/决策/任务文件/名册写 `runs/<run-id>`，中断后可重建）；工作树隔离（独立工作目录/分支，测试通过才合并） | 事件 `subagent/*`、`task_claim/task_complete` 落日志；团队重建演练（kill 后一条命令恢复） |
| FR-D7 | 合约化任务 + Elo 择优：可验证交付物 + 协商澄清 + 子合约分解；多方案生成按 Elo 排序择优 | 交付物可机器验证（退出码 / 路径命中 / 事件日志），禁止 LLM 自评闭环 |
| FR-D8 | MCP 接入：宿主以结构声明服务器与 transport（stdio / streamable-http；**先 stdio**；`mcp.json` 解析归宿主，框架不绑配置格式）；五步交互（发现 → **暴露策略**（exposure：`deferred`/`direct`/`hidden` + 逐工具覆盖）→ 调用 → 错误回传 → 结果注入）；不做 OAuth（凭据由宿主注入）。**协议层用官方 Go SDK**（v1.8.0，基线 `2026-07-28`；SDK 类型不外泄） | MCP 工具进 Registry 后与内置工具同受 Policy/Guard 管辖；单个 MCP 失败不阻塞其余加载；MCP 工具默认 deferred（经 `tool_search` 发现激活），工具面增长不吃线性 prompt；**模型知道有哪些 MCP 服务器**——有 deferred 工具的服务器摘要进静态系统提示词（S8.4-A）；**认证可诊断**——HTTP 401/403 归认证类 + 宿主可注入 `TokenProvider`（S8.4-B）；**资源可达**——三个共享资源工具 + 显式 server 参数（S8.4-E） |
| FR-D9 | Agent Loop 升级：Turn 语义完善 + 错误注入反馈（错误信息解释"为何违反规则 + 正确做法"，Linter 报错即上下文工程） | 错误注入后模型自纠成功率可观测（测试集） |
| FR-D10 | 显式图边界（判据门控）：节点 / 边 / 状态 schema + reducers / checkpointer；编译期校验（节点可达性、状态字段一致性、边类型合法性），不合法图拒绝运行 | **仅在满足分支/并行/持久/可审计判据时引入**——图是可选编排层，不是默认架构 |
| FR-D11 | Skill 能力（Skills 重做，M3a）：装配期技能目录（`SKILL.md` + frontmatter：name/description/when-to-use/allowed-tools/disable-model-invocation/metadata）+ 专用 `skill` 工具（正文按需加载，进工具结果）+ 技能清单进系统提示词（静态，保 prompt cache）+ `skill/*` 事件 | 渐进式披露（元数据进上下文、正文按需）；**不做技能级工具收窄**（PI 有字段无实现、DSH 无字段 → 不自创；`allowed-tools` 只解析留痕 + 诊断）；用户显式 `/name` 由宿主调用展开器（框架不自动扫文本）；**技能目录列表 = 宿主声明的授权边界**（读走注入的 `Reader`，不重复登记 Roots）；`disable-model-invocation` 的技能不进清单、模型不可加载（fail-closed）；**正文调用时现读** + 宿主显式 `Agent.ReloadSkills()` 重扫（不接 watcher，S8.4-C）；`allowed-tools` × 当前可见工具集**求交诊断**（只诊断不收窄，S8.4-D） |

### 5.5 FR-E：M4 生产化（P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-E1 | 异常处理与恢复三阶段：检测（工具输出验证 / API 错误码 / 超时）→ 处理（日志/重试/回退/优雅降级/通知）→ 恢复（状态回滚 / 诊断 / 自我纠正 / 升级） | 注入故障演练集通过；升级路径接 Approval |
| FR-E2 | 沙箱强隔离后端（L3）：Docker / containerd 替换进程级实现——**宿主侧实现，经 `sandbox.Runner` 注入**（强隔离属宿主侧：它替换整条执行能力，不是同世界路径强制后端；MoS 出同世界后端，归属见 ARCHITECTURE §3.10.1） | `sandbox.Runner` 接口不变，后端可切换（前置：`mos.Config.Sandbox` 从 `*sandbox.Local` 改成接口）；越狱测试通过 |
| FR-E3 | 模型分级路由：按任务复杂度选档（简单→经济模型、复杂→强模型），附顺序回退链 | 分级决策可配置可审计；回退链演练通过 |
| FR-E4 | 评估与监控（知识进化第 2 层 = 配置进化）：轨迹评估 Golden 测试集 + LLM-as-a-Judge 结构化评分 + 漂移检测 + 配置 A/B 择优（工具描述 / 提示词 / 路由与 Policy 规则版本化晋级） | 评估跑批可重复；版本化档案支持回滚 |
| FR-E5 | 优先级调度与配额：紧急性/重要性/依赖关系，限流配额按优先级区分 | 调度策略接口化（策略由平台注入） |
| FR-E6 | 生命周期控制 API + `runs/<run-id>` 可恢复运行目录：启动/暂停/恢复/终止；状态/中间结果/审计回放走文件系统（Workspace 即真相），断点续传 | kill -9 后可从运行目录恢复续跑 |
| FR-E7 | 人工网关 + Dashboard 可视化（平台侧消费 SDK 事件流实现） | 事件契约（`docs/platform-contract.md` §2 事件 JSON）稳定不再变更 |
| FR-E8 | 限流 / 重试 / 缓存（横切能力下沉至 llm 装饰器与 transport 层） | 指标可观测 |
| FR-E9 | 插件化：对齐 Agent Plugins 1.0 目录规范（`plugin.json` + `skills/` + `mcp.json` + 客户端命名空间目录）；组件独立校验（单个 skill 无效跳过、MCP 失败不阻塞其余）；安装/权限/来源验证走 MoS 的 Policy/Sandbox/Approval | 合规 plugin 目录可被 `mos.New` 加载；可移植字段不污染 |
| FR-E10 | M2.8 收尾：Approval TTL 自动超时、checkpoint 加密/签名（多句柄并发恢复维持业务层职责） | TTL 到期 fail-closed（默认拒绝）；checkpoint 防篡改校验 |

### 5.6 FR-F：M5 自我进化第 3 层（P2，实验性）

| 编号 | 需求 | 关键约束 |
| --- | --- | --- |
| FR-F1 | 独立"进化 Agent"：不参与主循环、只消费审计事件流，在工作区修改外围策略/工具代码 → 编译 → 跑测试 → 通过才归档；核心引擎人类控制，合并走人工批准；版本档案支持回滚 | 硬约束：没有外部基准就没有进化——闭环必须有机器可回放信号，禁止 LLM 自评进化（共谋确认）；每层可回滚 |

### 5.7 FR-G：重建期补齐的基础能力（P1）

> 这组是**重建周期里长出来、当时没登记**的能力（治理规则 1：新增一个包 = PRD 有一条 FR 指名要它）。回填它们是为了把闸门闭上，**不是新增范围**。FR-G1/G2 已交付；FR-G3 是 FR-E6 的前置。

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-G1 | 上下文文件（`agent/contextfile` + `agent/prompt` 分节装配）：候选名优先级（`AGENTS.override.md` > `AGENTS.md` > `AGENTS.MD` > `CLAUDE.md` > `CLAUDE.MD`）+ 向上递归 + 同目录 override + 真实路径去重 + 字节预算裁剪；正文作为「项目约定」节进系统提示词 | 装配快照写明装了哪些文件、多少字节、降级警告；读取以 `Reader` 接口注入（框架不碰文件系统）；未装则零开销 | ✅（2026-10-27 落地；2026-11-03 回填登记） |
| FR-G2 | 联网工具包（`tool/web`）：`web_search` / `web_fetch`，含 SSRF 防御（私有网段 / 环回拦截） | 抓取目标越界即拒；工具进注册表后与内置工具同受 Policy/Guard 管辖 | ✅（2026-10-28 落地；2026-11-03 回填登记） |
| FR-G3 | 挂起-恢复（`agent/checkpoint` + `agent/handle`）：把「跑到一半」的智能体挂起，恢复后接着跑；审批决定可跨进程留存 | kill 后从 checkpoint 恢复续跑 E2E；checkpoint 只记可重放事实；**是 FR-E6（`runs/<run-id>` 可恢复运行目录）的前置**。设计草案见 `docs/ARCHITECTURE.md` §3.17（PI/DSH 对账 + 取舍 + 待拍板项） | ⬜ **未做**（重建树无此包）；建议排期先于 M3a |

---

## 6. 非功能需求（NFR）

| 编号 | 需求 | 现状 / 度量 |
| --- | --- | --- |
| NFR-1 | **fail-closed 纪律**：未知工具拒绝、未声明文件根即无根（越界一律拒）、缺模型/缺沙箱即装配报错、截断工具调用 fail-closed、审批未配置即拒绝、缺能力不降级放行 | 已执行；新增能力一律按此纪律设计并有测试 |
| NFR-2 | **防御性 nil-safe**：`RunContext` 方法对 nil 接收者安全；扩展点 nil 时降级为框架默认或 no-op，行为不变 | 已执行 |
| NFR-3 | **零开销降级**：钩子 / 守卫 / 压缩器 / 截断器 / 审批通道未注入时零额外开销（未注入 = 行为与没用该能力完全一致） | no-op 路径基准测试 |
| NFR-4 | **日志即事实**：会话日志 append-only、`Append` 同步落盘 fail-fast、事件 Seq 连续可重放；日志不允许截断（崩溃恢复只补事件不改写） | 已执行；重放测试覆盖 |
| NFR-5 | **审计不变式**：任何智能体（含 SubAgent / 队友）的自述不入 `TaskState`，环境验证说了算；机器可回放信号优先于 LLM 判断 | 已执行；M3 多智能体延续 |
| NFR-6 | **依赖纪律**：近零依赖（协议层 + uuid + yaml.v3）；协议仓库只读（AGENTS.md 硬规则）；MoS 只消费协议层 `pkg/` 纯定义包，不依赖网关实现层 | `go.mod` 审计 + import 边界检查 |
| NFR-7 | **可测试性**：适配器用 httptest 协议模拟不依赖真实网络；关键行为用守护测试钉住（如 schema 逐字节不变、提示词两次 Render 一致） | `go test ./...` 全绿（注：需嵌套 Seatbelt 的沙箱 / shell 类测试要在允许 sandbox 的常规环境跑） |
| NFR-8 | **API 演进纪律**：未发布、无兼容负担，可按目标架构直接落地；但破坏性变更（如文件工具改名 `read_file`→`read`、提示词分节改名）必须在文档更新记录显式声明 | 更新记录惯例执行 |

---

## 7. 里程碑规划

| 里程碑 | 内容（对应 FR） | 优先级 | 依赖 |
| --- | --- | --- | --- |
| **R（发布准备）** | FR-A1 协议层独立发布、FR-A2 文档同步 | P0 | ✅ 已完成（v0.1.0 已发布，2026-11） |
| ~~**M2.9**~~ | ~~FR-B1/B2 经验记忆~~ | ~~P1~~ | 已决定不做（2026-10） |
| **M2.10** | ~~FR-C1~C4 工具规模化~~ | ~~P1~~ | ✅ 已交付（S7.3） |
| **FR-G3 挂起-恢复** | checkpoint + handle（跨进程续跑） | P1 | 无；**FR-E6 的前置**，建议先于 M3a |
| **M3a** | FR-D1/D2/D3/D8/D9/**D11**（路由拓扑、三权分离、SubAgent、MCP、Loop 升级、Skills 重做；MCP 先于 SubAgent） | P1 | R |
| **M3b** | FR-D4~D7/D10（双阶段、并行 DAG、AgentTeam、合约+Elo、显式图） | P1 | M3a |
| **M4** | FR-E1~E10 生产化 | P1 | M3a（部分项可并行） |
| **M5** | FR-F1 自我进化（实验） | P2 | M4 的 E4（评估与机器信号） |

> 排期原则：不设日历日期，按依赖顺序推进；每个里程碑完成后在 `docs/ARCHITECTURE.md` 更新记录与本文 §4.3 同步状态。

---

## 8. 风险与开放问题

### 8.1 风险

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | 规则层黑名单可被解释器入口/混淆命令绕过（README「诚实的边界」），进程级沙箱非强隔离 | 安全边界定位为纵深防御之一；FR-E2 强隔离后端是最终答案；SaaS 部署强制 `saas-jailed` preset |
| R2 | 审计门天花板 = 模型自分解清单的完整性（漏画的交付物审计也发现不了）；"换一个 LLM 盖章"不构成独立性 | 保持环境只读验证 + 机器信号；LLM 评审只作质量补充不作完成判定；有界轮次耗尽诚实停机（`audit_unmet`） |
| R3 | M3 范围过大（10 项 FR），一次性实施易失控 | 分期 M3a/M3b；逐项按决策判据门控（不满足判据不引入：SubAgent「合同论」、显式图四判据） |
| R4 | 文档与实现漂移（**已发生两次**：Skills 在 PRD 里一直标 ✅、重建树里却没有；M2.8 的 ✅ 掩盖了 checkpoint / handle 缺口） | 治理规则 1 的闸门靠 FR 登记维持：新包必登记 FR；每次切片合入同步 §4.2 / §4.3 + 顶部更新记录 |
| R5 | 上下文工程经验参数（窗口默认 128K、预留 16384、保留 20000 token）随模型迭代可能失效 | 补偿面理论：随模型变强复测并主动简化 harness；参数全部可配置（`mos.Config.ContextWindow` / `ReserveTokens` / `KeepRecentTokens`） |
| R6 | 协议层已独立发布（FR-A1 ✅），但仓库**无 CI**：外部拉取能否构建只靠人工一次验证 | 加 `go build/test` workflow（macOS + Linux；Linux 顺带跑 Landlock 路径） |

### 8.2 开放问题（待用户/维护者决策）

1. ~~`moproto` 拆独立 module 的时间点与版本策略~~ —— **已决（2026-11-03）**：已拆为独立 module 并发布 v0.1.0，`go get github.com/puras/mos@v0.1.0` 直接可用。
2. ~~MCP transport 优先级~~ —— **已决（2026-11-02）**：先 stdio（无网络依赖、易测、与进程管理同构），streamable-http 紧随（`httpx` 已有）。
3. 远程沙箱后端选型：Docker 还是 containerd（或两者都做适配器）？—— **归属已定（2026-11）**：强隔离属**宿主侧**（整体替换执行环境，不是 `sandbox.Runner` 的同世界后端，见 ARCHITECTURE §3.10.1），MoS 只出同世界路径强制后端（Landlock / Seatbelt / Windows 受限令牌）。剩余待决：宿主侧用 Docker 还是 containerd。
4. ~~M2.9 经验记忆的默认介质~~ —— **已作废（2026-10）**：M2.9 决定不做。
5. ~~M3a 中 SubAgent 与 MCP 的先后~~ —— **已决（2026-11-02）**：**MCP 先**（边界独立可单测；工具面立稳后，SubAgent 的工具白名单才有稳定母集）。

---

## 9. 术语与引用

- 目标架构与子系统契约：`docs/ARCHITECTURE.md`（分层、不变式、各子系统契约，含 PI/DSH 对位分析）。
- 装配层：`docs/assembly.md`（`mos.New` 的不变式、默认值表、运行期变更）。
- 平台集成契约：`docs/platform-contract.md`（职责边界 / 事件 JSON / 状态序列化 / 并发 / 人工审批）。
- 上手与产品介绍：`docs/tutorial.html`（可复制可编译的教程）、`docs/index.html`（产品介绍）。
- 发布说明：`docs/release-v0.1.0.md`（v0.1.0 = 首个公开版本；`main` 上的开发算 v0.2.0，发布时才打标签）。
- 旧实现与旧文档（考古用，`main_bak1015` 分支）：`docs/architecture-v2.md`、`docs/sdk-contracts.md`、`docs/agent/`（18 篇）、`docs/assistant.md`、`docs/llm.md`、`FEATURES.md`、`examples/{cli,protocli,agentcli}`。
