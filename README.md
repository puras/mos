# MoS

**MoS** 是基于 Go 的智能体基础框架（Agent Base SDK）：统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御。用它构建智能体应用与智能体平台时，长程任务的可靠性问题（上下文腐化、工具误用、不可验证的"自我声明式完成"、无法断点恢复）由框架承接，业务代码只决定"要什么、允许什么"。

## 快速开始

> 当前开发阶段经本地 `replace` 引用协议层（`moproto` / `mog`），外部工程直接 `go get` 尚不可用——这是发布前待办（PRD FR-A1）。克隆本仓库后即可编译运行。

```bash
git clone https://github.com/puras/mos.git && cd mos
go build ./... && go test ./...
```

跑一个真实的智能体（`examples/minimal`，十几行装配）：

```bash
cd examples/minimal
export MOS_PROVIDER=anthropic          # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...        # 按协议取对应的那把
export MOS_MODEL=your-model-name
export MOS_BASE_URL=http://your-gateway  # 可选
go run . "只回答：1+1=?"
```

作为 SDK 引入时的最小代码：

```go
// ① 模型：协议 + 凭据 + 重试（参数是宿主的，机制是框架的）。
model, _ := mos.NewModel(mos.ModelConfig{
    Provider: "anthropic",
    APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
    Model:    "your-model",
    Retry:    llm.RetryPolicy{MaxAttempts: 3},
})

// ② 工具：无副作用默认集 + 文件工具一套（jail 根 = 工作目录，写前观察门开启）。
reg := tool.NewRegistry()
for _, tl := range builtin.Default() {
    reg.Register(tl)
}
fileTools, _ := builtin.FileToolsIn(".")
for _, tl := range fileTools {
    reg.Register(tl)
}

// ③ 一行装配：模型、工具、安全、上下文工程一次装好。
a, _ := mos.New(mos.Config{Model: model, Tools: reg})
fmt.Print(a.Describe()) // 装配快照：装了什么、没装什么，都是事实

// ④ 会话（JSONL 持久化，重启可恢复）+ 跑一轮。
sess, _ := session.Open("data", "my-agent")
reply, _ := a.Run(context.Background(), sess, "帮我看看这个项目")
```

完整演示（策略、审批、shell、压缩、截断全开）：[examples/chat](examples/chat/)。

## 核心概念

| 概念 | 位置 | 一句话 |
| --- | --- | --- |
| **装配层** | 根包 `mos` | `mos.New(Config)` 一行装配；能力即证据，缺能力 fail-closed；`Describe()` 输出装配快照 |
| **循环驱动** | `agent/driver` | `LoopDriver` 接口；ReAct 已落地，PlanExecute / Reflection / Scripted 按 PRD 回填 |
| **会话** | `session` | append-only 事件日志是唯一事实来源；消息投影派生对话历史；JSONL 持久化、崩溃可恢复 |
| **工具系统** | `tool` | 结构体 tag 自动生成 JSON Schema（注册时缓存）；显式并发 opt-in；deferred 懒加载 + `tool_search` 支撑工具面规模化 |
| **执行通道** | `agent/execpath` | 守卫裁决 → 屏障式有界并发 → 按调用序回填，各驱动共用；错误即结果（回填给模型自我纠正） |
| **安全纵深** | `agent/policy` / `agent/hook` / `filesystem` / `sandbox` / `agent/approval` | Policy 规则 → Guard 守卫 → Sandbox 隔离 → 人工审批四层防线；HookBus 可改写、GuardBus 不可绕过 |
| **上下文工程** | `agent`（truncate/compact）+ `agent/prompt` + `agent/contextfile` | 结果截断（全文落盘可回读）+ token 预算压缩（模型生成摘要落日志）；系统提示词分节装配；AGENTS.md 等上下文文件发现与注入 |
| **能力包** | `tool/builtin` / `tool/web` / `tool/searchtools` / `httpx` | 文件五件套与搜索三件套；联网搜索/抓取（SSRF 防御）；工具发现 |

## 特性

### 统一 LLM 接入

- OpenAI 兼容 / Anthropic / Gemini 三协议适配：函数调用、流式、用量归一；
- 用量口径统一：`FreshInputTokens`（去缓存计费输入）跨协议可比，缓存读/写单列，命中率自算；
- 策略化重试：只重试瞬时错误、指数退避 + 抖动、取消即终止；
- 流完整性按协议结束标记判定（半截文本与残缺工具参数 fail-closed）。

### 工具系统

```go
type SearchArgs struct {
    Query string `json:"query" mos:"desc=搜索词,required=true"`
    Limit int    `json:"limit,omitempty" mos:"desc=返回上限"`
}
```

- struct tag → JSON Schema，注册时生成缓存（循环内零反射）；
- 工具实现 `tool.ConcurrencySafe(args) bool` 并返回 true 才可与同批调用并发，默认独占（fail-closed）；
- **deferred 懒加载**（M2.10）：`RegisterDeferred` 注册的工具不进清单，模型经 `tool_search`（BM25，支持中文）按需激活，下一轮生效——工具面增长时 prompt token 亚线性；deferred 集非空时 `tool_search` 自动装配，小工具集行为不变。

### 安全纵深（四层防线，逐层收窄）

1. **Policy 规则层** `agent/policy`：工具名级黑白名单，未知工具一律拒绝；
2. **Guard 守卫层** `agent/hook`：GuardBus serial 单调裁决，钩子结构上无法绕过；载荷冻结（钩子不得改写工具名/参数/CallID，守卫判定恒有效）；
3. **隔离层** `filesystem` + `sandbox`：文件 jail（Lexical + Realpath 双校验、symlink 逃逸防护、写前观察门）+ 进程沙箱（超时、进程组击杀、资源限制、环境裁剪；macOS Seatbelt 路径强制，Linux Landlock 规划中）；运行期可加根（`grant_access` 流程，两层同步生效、全程留痕）；
4. **人工 + 审计层** `agent/approval`：四档审批结果，问人由宿主注入（终端/Web），审计落会话日志。

参数级决策点：`mos.Config.Rules` 可对特定工具的参数做 deny / ask（例：`write` 到约定文件 → 转人工确认）。

### 会话与可恢复性

- append-only 事件日志：`Append` 同步落盘 fail-fast，日志不允许截断；崩溃恢复只补事件不改写（孤儿 turn 合成 interrupted）；
- 消息投影确定性可重放；工具调用与结果按 CallID 配对，投影对服务商永远合法；
- `session.Open(dir, id)` 一行建/恢复；`sess.TotalUsage()` 给出跨轮累计用量。

### 上下文工程

- **截断**：工具结果超限就地瘦身（行/字节双限），全文落工作区 `.mos-trunc/`，`read` 可回读；
- **压缩**：超过 token 预算（contextWindow − reserve）时模型生成结构化摘要落会话日志，投影从此遮蔽旧历史；切割点永不切断 ToolCall-Result 对；
- **提示词装配** `agent/prompt`：分节 Builder（身份 / 约定 / 项目上下文 / 工具指引 / 运行时事实，顺序中心化），装配期渲染一次、之后静态不变（利于 prompt cache）；
- **上下文文件** `agent/contextfile`：AGENTS.md / CLAUDE.md 向上递归发现，`AGENTS.override.md` 同目录替换，字节预算裁剪，全局约定 `~/.mos`（SDK 内嵌模板，只写不覆盖）。

### 可观测

- 事件类型覆盖 turn / assistant / tool / approval / compaction 等全链路，宿主可遍历 `sess.Events()` 或对接自有事件总线；
- `Describe()` 装配快照：装了什么、没装什么、降级警告，全部如实写明。

## 安全模型与边界（诚实的部分）

- **fail-closed 是全库纪律**：未知工具拒绝、capability 缺失即报错、审批未配置即拒绝、截断消息中的工具调用拒绝放行；
- 进程沙箱是**纵深防御的一层**，不是强隔离边界（黑名单可被解释器绕过）；面向不可信工作负载请叠加强隔离后端（规划中，PRD FR-E2）；
- 框架**不碰文件系统与网络**：需要 I/O 的能力以接口注入（`session.Persistence`、`contextfile.Reader`），宿主选实现；
- 近零依赖：协议层 + `google/uuid` + `gopkg.in/yaml.v3`，无其他第三方依赖。

## 文档

- [docs/PRD.md](docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 目标架构与不变式（怎么立），含每个子系统契约
- 旧实现与旧文档考古：`main_bak1015` 分支（如 `git show main_bak1015:README.md`）

## 示例

| 示例 | 演示内容 |
| --- | --- |
| [examples/minimal](examples/minimal/) | 装配层最小可跑示例：十几行装出一个真能跑的智能体 |
| [examples/chat](examples/chat/) | 完整演示：流式输出、多协议切换、会话持久化与恢复、策略与审批、shell 验证回路、压缩与截断、AGENTS.md 注入、会话中途授权目录 |
| [examples/agents](examples/agents/) | 多 Agent 协作：拆解 / 执行 / 验收三角色 + 任务列表驱动 |

## 路线图

- ✅ **已交付**：统一 LLM 接入、工具系统、ReAct 循环、执行通道、安全纵深、上下文工程、装配层、联网工具、工具规模化（M2.10）
- ⬜ **R（发布准备，P0）**：协议层脱离本地 replace，外部工程 `go get` 可构建
- ⬜ **M3** 多智能体：SubAgent（agents-as-tools）、MCP 接入、AgentTeam、并行 DAG
- ⬜ **M4** 生产化：Docker 强隔离、模型分级路由、评估与监控、可恢复运行目录
- ⬜ **M5** 自我进化（实验性）

完整里程碑与验收标准见 [docs/PRD.md](docs/PRD.md) §7。M2.9（经验记忆）已决定不做（2026-10）。

## 构建

```bash
go build ./...   # 编译
go test ./...    # 测试（沙箱 / shell 类测试需允许 Seatbelt 授权的环境）
```

(End of file)
