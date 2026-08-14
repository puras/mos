# MoS 功能说明

> 生成日期：2026-08-13 ｜ 对应提交：`4dc8b6f`（M2.7 长期记忆）
> 状态：**M1 ~ M2.7 已完成**；M2.8 部分落地（Confirmer）；M2.9 / M2.10 / M3 / M4 / M5 待开发

## 1. 项目定位

MoS（Sapient）是基于 Go 的智能体基础框架（Agent Base），提供统一 LLM 接入、声明式工具系统、会话记忆与可插拔规划，帮助快速构建智能体应用。设计理念：**状态写文件不写上下文、只接受环境独立验证过的事实、上下文越少越好**（Harness Engineering / Audit Gate / Loop Engineering）。

## 2. 已实现功能

### 2.1 LLM 接入（`llm/`）
- 统一 `ChatModel` 抽象：消息 / 工具调用 / 用量 / 流式输出，模型对象注入，零第三方依赖
- 协议适配器：OpenAI 兼容（DeepSeek / 通义千问 / GLM / Ollama 等）、Anthropic Claude、Google Gemini，均支持函数调用与流式输出
- 协议注册表 `Registry` + `Factory`，支持按需注册协议

### 2.2 工具系统（`tool/`）
- 声明式工具：结构体 + tag 自动生成 JSON Schema，参数自动校验解析（`tool.Parse`）
- `Registry` 注册 / 查询 / 统一执行；`Call` 入口带参数容错（拼接 JSON 合并为单一对象）
- 内置工具包 `tool/builtin`：calculator / current_time / web_fetch / read_file / write_file / find_files / grep，纯标准库
- 扩展工具包 `tool/extension`：shell（沙箱受限执行）、web_search（DuckDuckGo / Tavily / Serper / Brave / SearXNG）
- 工具输出截断（M2.3）：行数 / 字节双重限制 + 按工具类型智能截断（read/web 截头、shell 截尾、其余 head+tail）+ 逃生通道（完整结果落盘、提示模型按偏移回读）

### 2.3 Agent 核心循环（`agent/` + `planner/`）
- ReAct 循环（`planner.ReAct`），`Planner` / `StreamingPlanner` 接口可插拔
- 事件观测：思考 → 工具调用 → 工具结果 → 事实 → 完成，携带 Turn 序号，支持流式端到端输出
- 两层消息与翻译边界（M2.4）：`InternalMessage` 附加运行期元数据；`convertToLlm` / `fromLlm` 统一翻译，摘要 / 自定义消息转 user 角色并标注来源
- fresh-context 压缩（M2.5）：超 `KeepRecent` 触发，不切断 ToolCall-Result 对；摘要直接采用 `TaskState.Snapshot()`；上下文利用率 40% Dumb Zone 告警；Doom Loop 检测（相同工具+参数连续 3 次 → blocked）

### 2.4 审计式任务状态（M2.1 + M2.2）
- `TaskState`：目标 / 已验证事实 / 状态机，独立于消息历史、跨轮可恢复
- 审计门 `Auditor`：仅接受白名单只读工具的可验证输出写入状态，模型自述不算数

### 2.5 系统提示词动态组装（M2.6）
- 槽位化：系统约束 / 任务定义 / 当前状态 / 工具签名 / 项目指令 / 技能清单 / 历史摘要，每轮刷新运行时状态
- CLAUDE.md 向上递归（顶层在前、就近覆盖）；Skills 懒加载（SKILL.md frontmatter 清单，正文按需读取）

### 2.6 长期记忆（M2.7）
- `memory/longterm`：SQLite（modernc 纯 Go 驱动，零 CGO）+ FTS5 全文索引
- 混合检索 = FTS5 BM25 + Go 侧向量余弦（未配置 Embedder 退化为纯 BM25）；中文检索（CJK 空格化）
- RAG 注入：`agent.WithRetriever` 每轮按 Goal 检索，知识守门（源验证 / 冲突裁决 / 知识差距检测 / MaxChunks 截断），注入只作用于当轮发送副本、不进历史

### 2.7 安全防护（M1.5 + M1.6 + M2.8 雏形）
- `Policy` 执行前拦截：绝对危险命令无条件拒绝；受限危险命令（rm / sudo / 解释器入口等）须 WorkDir 内 + 人工确认；路径越界防护；fail-closed
- `Sandbox` 执行中隔离：工作目录 jail + 超时 + 输出上限 + 进程资源限制（CPU / 内存 / 单文件）+ 环境裁剪 + 网络控制
- `Confirmer` 人工确认器（M2.8 轻量雏形）：CLI 纯文本 y/N 确认；TUI 模式 fail-closed

### 2.8 组装层与示例（`assistant/` + `examples/assistant`）
- `assistant.New`：注入模型对象 + 工具集 + 运行配置，nil 时用框架默认（策略 / 规划器 / 审计器 / 记忆 / 观察）
- 环境变量 / `.env` 配置（优先级：环境变量 > .env > 默认值）；无 Key 自动降级 mock 离线模型
- 交互式 CLI：TUI（charmbracelet）流式输出 + 思考折叠卡片 + 焦点分区 + 主题 + 鼠标策略

## 3. 架构组成

| 包 | 职责 |
| --- | --- |
| `llm/` | 模型抽象、OpenAI / Anthropic / Gemini 适配器 |
| `tool/` | 工具注册与执行、内置 / 扩展工具包、沙箱 |
| `planner/` | ReAct 规划器接口与默认实现 |
| `agent/` | 核心循环、消息翻译、压缩、RAG、审计、状态、截断、Doom Loop |
| `memory/` | 记忆接口、longterm（sqlite + FTS5 + 向量） |
| `assistant/` | 组装层、默认策略、确认器 |
| `examples/assistant/` | 独立示例工程（TUI CLI + 配置加载） |

SDK 零第三方依赖（仅依赖本地 `hegq.net/mogo` 与 `modernc.org/sqlite`）；示例工程依赖 charmbracelet 系列。

## 4. 完整性评估

### 4.1 对照路线图
- ✅ **已完成（10 项）**：M1、M1.5、M1.6、M2.1 ~ M2.7
- 🔶 **部分完成（1 项）**：M2.8 仅落地 `Confirmer` 轻量雏形；`request_human_input` 工具、Checkpointer 断点恢复未实现
- ❌ **未开始（5 项）**：M2.9 经验记忆、M2.10 工具规模化、M3 多智能体编排、M4 生产化、M5 自我进化

### 4.2 质量现状
- 测试：SDK + 示例共 169 个测试函数、30 个测试文件，**全部通过**（含 llm 适配器 httptest 协议模拟）
- 修复：`llm/gemini` 编译错误（`mogo` 依赖 `ParseGeminiStreamChunk` 签名新增 `rawFrame` 参数，mos 未同步导致 build 失败，已适配修复）

### 4.3 已知问题与风险
- **文档过时**：README「特性」仍写「长期向量记忆（sqlite-vec）规划中」，与已完成的 M2.7（sqlite + FTS5 混合检索）不一致，建议同步
- **工作区未提交改动**：`tool/tool.go` + `tool/parse_test.go`（拼接 JSON 合并容错增强）尚未提交
- **待办缺口**：上下文懒加载 / 渐进式披露、token 预算上限、上下文重置与跨 Session handoff、模型分级路由、异常恢复、可恢复运行目录（M4）等仍为空
- **README 完整性**：路线图 M2.9 / M2.10 / M3 条目内容详实但均为规划，实现前需按实际落地情况修订

## 5. 快速验证

```bash
# SDK 全量测试
go test ./...

# 示例工程测试
cd examples/assistant && go test ./...

# 运行交互式 CLI（无 Key 自动降级 mock）
cd examples/assistant && go run .
```
