# MoS 架构（重建版）

> 配套 [PRD](PRD.md)。旧版架构全文（architecture-v2 + 18 篇子系统文档）在 `main_bak1015` 分支的 `docs/` 下，需要考古时去那里；本文只保留目标形态与必须继承的不变式。

## 1. 分层（目标形态）

```text
装配层    assistant        模型对象 + 工具集 + 运行配置注入，nil 用框架默认（S7）
驱动层    LoopDriver       ReAct 起步；PlanExecute/Reflection/Scripted 按 PRD 逐个回填（S4a ✅）
执行通道  execpath         守卫裁决 → 调度执行 → 结果回填，各驱动共用（S5a 骨架 ✅ / S5e 补齐）
基础设施  session/event/hook  事件日志会话 + 总线 + 钩子守卫（S1/S5）
能力层    llm / tool / filesystem / sandbox   协议适配、工具、文件 jail、进程隔离（S2/S3/S5）
协议层    moproto          外部本地库（只读，见 AGENTS.md）
```

依赖方向自上而下，禁止反向引用。唯一例外：`llm` 的核心值类型（`Message` / `ToolCall` / `Usage`）是全框架共享的对话词汇表（纯定义、零依赖），基础设施层（session 等）可直接引用——日志与协议同形，避免翻译边界丢字段；禁止的是能力层/驱动层反向依赖上层。每层在本文件登记后才允许新增包。

## 2. 不变式（从旧实现继承，任何切片不得违背）

1. **fail-closed**：未知工具拒绝；capability 缺失即 `NotConfiguredError`；必需组件（规划器/评审器）缺失拒绝运行而非降级放行；截断消息中的工具调用 fail-closed。
2. **日志即事实**：会话日志 append-only；`Append` 同步落盘 fail-fast（写失败事件不进内存）；崩溃恢复只补事件不改写（孤儿 turn 合成 `interrupted`）。
3. **审计不变式**：只有环境可验证的事实进入 `TaskState`，模型自述不算数；机器可回放信号（退出码/路径命中/事件日志）优先于 LLM 判断；有界轮次耗尽诚实停机，不造假绿灯。
4. **上下文是稀缺资源**：截断 + 压缩；压缩切割点不切断 ToolCall-Result 对；压缩摘要 = `TaskState` 快照而非 LLM 重新总结。
5. **Hook/Guard 双轨制**：可改写环节走 HookBus（waterfall，可短路/改写/替换）；安全约束走 GuardBus（serial，单调裁决，不可被钩子改写绕过）。
6. **零开销降级**：扩展点 nil 时行为与未注入完全一致（Telemetry/Observer/EventBus 等 no-op 零成本）。
7. **依赖纪律**：SDK 近零依赖（协议层 + uuid + yaml.v3）；协议仓库只读；不操作数据库（`Persistence` 接口 + 轻量默认，数据库后端由业务层注入）。

## 3. 包地图（随切片生长，勿提前创建）

| 包 | 职责 | 来源 | 切片 |
| --- | --- | --- | --- |
| `session` | 会话事件日志、消息投影、TaskState、JSONL 持久化 | 重写瘦身 | S1 |
| `llm`（+`llm/transport`） | ChatModel 抽象、协议适配、共享 HTTP envelope；**重试机制**（`WithRetry`）+ 策略结构体 | 移植归档 | S2 ✅ / S2b |
| `tool`（+`tool/builtin`） | 工具接口、注册表、Schema 生成、`ConcurrencySafe` 并发分类器；`current_time` / `read_file` / `write_file` / `edit_file` | 移植裁剪 | S3 ✅ / S5e / S5d+ |
| `planner` + `driver` | Planner 接口 + ReAct 决策；LoopDriver + ReActDriver | planner 移植 / 驱动重写 | S4a ✅ |
| `agent/execpath` | 工具调用统一执行通道：preflight 裁决 + 调度 + 按调用序回填 | 重写 | S5a ✅ |
| `agent/hook` | 双轨制：HookBus（waterfall 可改写）+ GuardBus（serial 单调裁决） | 移植裁剪 | S5b ✅ |
| `agent/policy` | 工具名级策略：fail-closed 黑白名单 + YAML 装配 | 移植裁剪 | S5c ✅ |
| `filesystem` | 文件 jail：宿主声明可读/可写根 + Lexical/Realpath 双层校验 | 移植裁剪 | S5d ✅ |
| `sandbox` | 进程隔离：超时 + 进程组击杀 + 输出上限 + 资源限制 + 环境裁剪 | 移植裁剪 | S5f-1 ✅ |
| `agent`（上下文件） | 截断、压缩、Auditor | 重写瘦身 | S6 ✅ |
| `assistant` | 一行装配 + 示例 CLI | 重写 | S7 |

> S3 边界：`tool/builtin` 只带 `current_time`（无安全副作用）。涉及文件与命令执行的
> 内置工具（read_file / write_file / find_files / grep / shell）等 S5 的 filesystem jail
> 与 sandbox 就位后再加入——不先发一版无防护的实现、再返工加锁。

### 3.1 循环契约（S4a 定型，S4b 落地工具循环）

一个 turn 的事件序列固定为 `turn/start → user → (assistant → tool/call* → tool/result*)* → assistant → turn/end`：

- **历史来源唯一**：`RunContext.History(sess)` = 静态系统引导 + 会话事件投影，驱动不自行维护 `[]Message`；工具定义单独传决策器（对话与能力是两件事）；
- **轮次序号派生**：`sess.NextTurn()` 取日志最大值 + 1，Restore 后自然接续；
- **投影合法性不变式**：assistant 的 `ToolCalls` 与随后的 `tool/result` 按 CallID 成对——否则投出的历史对服务商非法（缺 tool 消息会报错）；
- **日志顺序**：`tool/call` 按模型顺序全部先落，执行后 `tool/result` 按序回填，Seq 连续可重放（为并发执行留形，S4b 目前串行）；
- **错误分级**：工具报错（含模型幻觉出的未注册工具）是**结果**，以 `IsError` 回填给模型让它自我纠正，不中断 turn；只有落盘失败与决策失败才中断，且都以 `turn/end{reason:error}` 收尾，不留孤儿 turn；
- **收尾带详情**：`turn/end` 的 `reason` 是分类（completed / terminated / error / interrupted），另带 `error` 字段记详情（出错原因、终止理由，上限 4KB 按 UTF-8 边界截断）——**只进日志、不进模型投影**（`project()` 不投影 `turn/end`）。理由：日志是审计凭据，失败若只有分类，事后只剩“失败了”三个字，无从追查是模型不可用、证书过期还是守卫拦的；完整错误仍由返回值交给宿主，二者不互相替代；
- **能力缺口 fail-closed**：未装 `RunContext.Tools` 却要求工具时直接报 `react.ErrToolsUnsupported`，且**不写 assistant 事件**（避免留下非法历史）；
- **用量分协议读取（Anthropic 的 usage 落在两个事件）**：Anthropic 把用量拆开——`message_start.message.usage` 给**输入侧**（input_tokens + 缓存创建/读取明细），`message_delta.usage` 给**最终快照**（含 output_tokens + thinking 明细）。适配器**两侧都读、逐字段合并**（输入侧以 message_start 为权威，输出侧取 message_delta；到达哪侧取哪侧）——只读 message_delta 的网关（实测内网 MoGo 即是）会让全部用量丢失（tokens 恒为 0）。注意 Anthropic 的 `total` 口径 = 输入 + 输出 + 缓存创建 + 缓存读取（缓存单列在 Details 里，不含在 prompt），与 OpenAI 的 total = prompt + completion **不同口径**——跨协议累加会话 token 时须知这个差异。
- **分项之和 = 总计（按协议各自的口径）**：OpenAI 的 prompt **含**缓存命中 → `total = prompt + completion`；Anthropic 的 input **不含**缓存 → `total = input + 缓存写 + 缓存读 + 输出`。MoS 的显示统一拆成"新鲜输入 / 缓存读 / 缓存写 / 输出 / 共"，两种协议下**分项之和都等于总计**（session 与适配器都有不变式测试守着）。
- **用量口径统一在 `FreshInputTokens`**：`Usage.PromptTokens` 是**协议原始值**（OpenAI/Gemini 含缓存命中、Anthropic 不含），跨协议不可比；`FreshInputTokens` 是**去缓存后的计费输入**（各协议同口径，moproto 已在 `Base.FreshInputTokens` 算好）。会话累计与示例显示都用它，并把**缓存读/缓存写单列**——缓存命中是"省下来的"部分，混在"输入"里就看不出成本结构。**缓存命中率是自算指标**（`Usage.CacheHitRatio()` = cache_read / (新鲜输入 + cache_read)）：提供商不直接给，分母必须用去缓存口径的输入（否则两个协议下同一个百分比含义不同），缓存写入不计入命中。
- **流完整性检查（按协议结束标记，不按结束原因）**：适配器判定"流是否完整"看**协议的结束标记**——Anthropic 的 `message_stop`、OpenAI 的 `[DONE]`；结束**原因**（`stop_reason` / `finish_reason`）只是附加信息。两者都没有 = 流被掐断 → `ErrStreamTruncated`（半截文本 + 残缺工具参数不能当正常响应）。**为什么不能只看结束原因**：实测自建的 anthropic 兼容网关只发 `message_stop`、不发 `message_delta`（无 `stop_reason`、无最终 usage）——只看结束原因会把该网关的**所有**正常流误判成截断。Gemini 协议无独立结束标记（只有 `finishReason`），故检查更严，宿主可用 `Config["require_finish"]="false"` 关闭；驱动层的截断 fail-closed 也只认**明确的** `length`/`max_tokens`（结束原因未知 ≠ 截断，否则该类部署的工具调用会被全拒）；
- **内层上限（保险丝，可配）**：单 turn 决策上限 `RunContext.MaxDecisionRounds`（<=0 = **不限**——S6 压缩落地后的默认，对齐 PI/DSH；旋钮保留给无人值守场景）。它只防"循环烧穿"，不是正常路径——空转由**连续相同调用提醒**打断（同一 (tool, args) 连续 3 次起，在 tool/result 末尾附提醒让模型改变策略；advisory 不 veto，DSH `repeat-tool-reminder` 同一哲学）。设了上限而耗尽时以 `react.ErrMaxDecisionRounds` 诚实报错。

### 3.2 执行通道契约（S5a 骨架，S5e 补齐）

工具执行的语义全库只在 `agent/execpath` 一处，驱动只负责投影与收尾：

```text
ExecuteBatch(ctx, reg, calls, opts) []Outcome   // 结果与 calls 严格同序

① preflight（串行，按模型调用序）
     守卫裁决（GuardBus.Allow，守卫为 nil 即恒通过）
     → pre-execute 钩子链（只决策：允许/拒绝/请求确认/终止本轮）
② execute（屏障分组 + 有界滚动池）
     独占调用自成屏障；连续可并发调用成组，组内按上限并发
     → execute 钩子链环绕真实派发（并发区内，必须可重入）
③ finalize（串行，按模型调用序）
     post-execute 钩子链（裁剪/脱敏；可加重错误，不可洗白错误）
     → 归一化为 Outcome；调用方（驱动）落 tool/result 事件
```

- **语义中立于驱动**：不感知 ReAct / 未来的 PlanExecute、Scripted，也不感知会话；它只回答「这批调用各得什么结果」。
- **回填顺序 = 调用顺序**：结果按模型给定顺序排列而非完成顺序，并携带原 `CallID`，调用方靠它对回每个调用——并发化后此不变式尤其重要。
- **错误即结果**：守卫拒绝、工具报错、参数非法、模型幻觉出的未注册工具名，全部转成 `Outcome{IsError: true}` 而非返回 error——那是「本次调用失败」不是「系统故障」，回填给模型让它自我纠正。中断 turn 的只有驱动侧落盘失败与决策失败。
- **逐条独立裁决**：守卫拒绝一条不影响同批其他调用，模型能看出是哪一条被拒、为什么。
- **fail-closed 兜底**：注册表为 nil 时整批判失败，不 panic（正常路径由驱动提前拦下，这里是防御性底线）。
- **不落日志、不裁断、不重试**：日志语义属 `session` 与驱动，避免执行语义与日志语义纠缠。
- **载荷冻结（S5e）**：钩子**不得改写**工具名 / 参数 / CallID——改写即违约，该调用被拒绝并说明原因。因此守卫判定的是这次调用的**最终**载荷（载荷不可变 ⇒ 判定恒有效），守卫只跑一次，不会重复触发有副作用的守卫（配额扣减、审批请求）。要修正入参请在工具内部做（工具自己 normalize），对齐 PI（`beforeToolCall` 不可改参数）与 DSH（参数进 policy 前深冻结）的共识。
- **并发是显式 opt-in 且 fail-closed**（S5e）：工具须实现 `tool.ConcurrencySafe(args) bool` 并返回 true 才可与同批其他调用并发；未实现、参数非法、返回 false、甚至 panic 一律**独占**。所以默认行为仍是串行。分类器是「一元」的——无法表达「这两个写路径不同才安全」这类关系型规则，那种情况一律独占。
- **屏障模型**（对齐 DSH）：连续可并发调用成组，**独占调用自成屏障**，组间顺序执行。`[read(A), read(B), write(A), read(C)]` → `[read(A) ∥ read(B)]` → `[write(A)]` → `[read(C)]`。
- **只有执行体并发**：preflight 与 finalize 都按模型序串行（守卫与钩子可能携带顺序敏感状态），并发的只是工具体。`execute` 钩子链因此运行在并发区，**必须可重入**。
- **取消不留空洞**：上下文取消时未派发的调用补记合成失败结果，保证 `tool/call` 与 `tool/result` 仍然配对（对齐 DSH 的 aborted call/result 对）。
- **守卫与日志的先后（已定）**：工具调用的日志顺序固定为「先全部落 `tool/call` → 再守卫裁决与执行」，即**被拒的调用也留痕**。这与 `session` 包注释的目录语义一致（「`tool/call` 含被守卫拦截的调用」），审计优先于「不留痕」；且 `tool/call` 不投影给模型，拒绝理由只经 `tool/result` 回填给模型看。归档实现的「裁决先于落日志」写法不采用。

### 3.3 双轨制契约（S5b）

| | HookBus | GuardBus |
| --- | --- | --- |
| 语义 | waterfall：按注册顺序逐层委托，可改写 / 短路 / 替换 | serial：按注册顺序串行裁决，首个拒绝即整体拒绝并短路 |
| 用途 | 行为性扩展：压缩 / RAG / 注入 / 超时 / 重试 / 指标 / 结果裁剪 | 安全性约束：Policy / 权限 / 身份校验 |
| 可被改写绕过 | 是（它就是改写机制本身） | **否** |

- **编排顺序（不变式 5）**：工具执行时先 `GuardBus.Allow` 再进 `HookBus.Waterfall`；守卫不持有钩子总线，二者互不引用——「钩子绕过守卫」在结构上不成立（`TestGuardVerdictNotBypassableByHooks` 锁定该性质）。
- **载荷冻结（S5e 补强）**：钩子**不能**改写工具名 / 参数 / CallID（改写即违约，调用被拒）。于是「守卫看到的是改写前的载荷」与「守卫判定的就是最终载荷」**同时成立**——不需要复检，守卫只跑一次。这是 PI 与 DSH 独立收敛的共识：载荷不可由钩子改写，守卫判定才恒有效。
- **零开销降级（不变式 6）**：两条总线均可为 nil；nil 上的注册返回 no-op 移除函数，调用直接透传（Hook 原样返回载荷，Guard 一律放行）。未装扩展点的用户不付任何代价。
- **目录预算制（治理规则 2）**：`HookKind` 有预算，新增一种前先删一种。当前目录：`pre-step`（S6 接线：驱动在每次决策前跑瀑布）、`tool/pre-execute`、`tool/execute`、`tool/post-execute`——每个都注明了消费它的切片。归档实现里的 `request`（预留、无消费者）与 `turn-stopping`（S6 未消费到）均已删——没有消费者的环节不是扩展点，是债务；首个消费者出现时再加回。
- **健壮性**：钩子返回 `(nil, nil)` 显式报 `ErrNilHookEvent` 而不是把 nil 传下去崩在下游——坏插件不许带崩整条链；注册与执行并发安全（执行前快照钩子链）。
- **接线点（S5e 全部接上）**：守卫由 `execpath.ExecuteBatch` 的 preflight 调 `GuardBus.Allow`，拒绝项不执行并以 `IsError` 回填；钩子总线 `HookBus` 的三个 `tool/*` 环节全部接线——`tool/pre-execute` 裁决调用、`tool/execute` 环绕派发、`tool/post-execute` 改写结果文本。驱动把 `RunContext.Guards` / `RunContext.Hooks` / `RunContext.Concurrency` 与轮次一并传入。
- **决策词汇（S5e）**：pre-execute 钩子返回 `Decision`：允许（默认）/ 拒绝 / 请求确认（无审批通道时 fail-closed 拒绝，S5g 落地审批后改为挂起）/ 终止本轮（该调用拒绝 + 同批后续跳过 + 本轮以 `terminated` 收尾）。链上第一个非 allow 决策生效，下游钩子不再执行。
- **错误只可加重不可减轻**（对齐 DSH）：post-execute 钩子可以把成功改成失败（发现问题就报错），但**不能**把失败洗成成功——错误是事实，审计日志不能被改写成成功。

### 3.4 策略层（S5c）

```go
// 框架的输入只有结构（格式与键名归宿主）
policy.Config{Allow: []string{"read_file"}, Deny: []string{"shell", "write_*"}}
```

**语义对齐 PI 与 DSH 的共识**（两家都是两份名单都可选）：

```
允许(name) = (Allow 未配置 OR name 匹配 Allow) AND NOT name 匹配 Deny
```

| 情况 | 结果 | 依据 |
| --- | --- | --- |
| `Deny` 命中 | 一律拒绝（**优先**，无放行后门） | PI `excludedToolNames` / DSH `ToolRestriction.deny` |
| `Allow` **未配置**（nil） | **不限制** | PI `!this._allowedTools → true`；DSH `allow === undefined` |
| `Allow` 配置了但**是空集** | **全禁** | PI "An empty list exposes no tools"；DSH `allow !== undefined && !allow.has(name)` |
| `Allow` 非空 | 只有名单内的可用 | 两家一致 |

- **名字支持 `*` 通配**（PI 的语义）：`*` 匹配任意字符（含分隔符，`write_*` 能匹配 `write_a_b`）、整串锚定；精确名走集合。DSH 只支持精确名。
- **分层：策略是收窄层，不是能力清单**。fail-closed 的那道门是**注册表**——没注册的工具模型看不见也调不动；策略只负责在已给出的能力上挖洞。"只允许某几个工具"靠**不注册**其余的来表达，不必在策略里再写一遍白名单。
- **可见性与运行时是同一份名单**：`Policy.Visible` 决定工具**不出现在模型看到的清单里**（PI/DSH 的可见性层——被禁的工具不给模型看）；`Policy.Check` 是运行时守卫，拦下模型幻觉出的名字、策略中途变更、非模型路径的直接调用。两者读同一份名单，所以不会出现"看不见却能调"的矛盾。
- **可见性过滤器无副作用**：`tool.Registry.SetVisibility(func(name string) bool)`——它每次组装工具清单都会被调用，所以必须纯判定。本包只认函数值，不认识 policy / hook 之类的具体实现（装配由宿主接上）。
- **审计语义的变化**（承接 S5c 的"被拒也留痕"）：被禁的工具不在清单里 → 模型不会去调 → **不会有被拒记录**。仍会留痕的是：模型幻觉出的名字、策略中途变更、非模型路径的调用。真正的审计凭据变成**策略配置本身**。
- **不做**：PI 的 MCP 特例（allow 列表会意外影响 MCP 工具注册）——MoS 还没有 MCP 工具；DSH 的 per-scope 限制层——MoS 还没有 scope 概念。
- **分层：框架只认结构，不认文件、不认配置格式**：读文件与解析 YAML/JSON 都是宿主的活（`Config` 不带 yaml/json tag，也不提供 ParseYAML——绑一个格式就要为它背依赖，而平台可能用 JSON、配置中心或远端下发）。
- **本切片只做工具名级规则**：参数级校验（shell 命令分级、文件路径越界）依赖 jail 与沙箱（S5d/S5f），届时以 Checker 体系扩展，不在此处造无消费者的正则引擎。

### 3.5 文件 jail（S5d）

```go
// 宿主按自己的形态声明可读/可写根——框架不定义 Local / SaaS 这类部署形态
fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
	ReadRoots:  []string{workdir},
	WriteRoots: []string{workdir},
})
reg.Register(builtin.NewReadFile(fsys))
reg.Register(builtin.NewWriteFile(fsys))
```

- **机制而非形态**：框架只提供「路径必须落在宿主声明的根内」，不内置 Local/SaaS/容器 模式，也不预设「工作区」这个业务概念。宿主自己声明：平台通常是读写同一工作区；本地开发可能读放开、写受限；诊断场景显式写 `"/"`（逃逸口是**看得见的数据**，不是一个容易忘配的模式名）。
- **两层校验**：Lexical（规范化后的路径必须落在允许根子树内）→ Realpath（跟随 symlink 后的**实体路径**仍须落在允许根实体内）。只做 Lexical 时，根内一个指向 `/etc/passwd` 的 symlink 就能把内容读出边界——**读同样要走 Realpath**（归档实现只对写做，这是它的一个真实弱点）。
- **防 symlink 逃逸的三种形态**都拦：目标本身是链接、目标不存在但中间组件是越界链接（`root/escape → /etc`，写 `root/escape/new.txt`）、链接环（深度上限 32，提前拒绝而不是死循环）。
- **fail-closed**：某类操作的根为空 = 该类操作一律拒绝（不是"等于放开"）；工具没装配文件能力时返回 `*NotConfiguredError` 而非放行。
- **无隐式共享目录**：可写根只有宿主显式声明的那些——刻意**不含** `/tmp` 与 `os.TempDir()`（共享临时目录是符号链接攻击的经典目标，且多智能体并发时互相干扰）。
- **审计复用 `tool/call` + `tool/result`**：不新增事件类型（事件目录预算制）。被拒访问以 `tool/result{IsError}` 留痕，拒绝理由（路径 + 该操作允许的根）回填给模型——**说"哪些根允许"比说"什么模式"有用**。
- **诚实的边界**：本实现是「解析期校验 + 使用实体路径打开」，把校验与使用之间的窗口压到最小；它**不是**真正的 TOCTOU 消除——那需要 `openat2`/`O_NOFOLLOW` 之类的系统调用，Go 标准库给不了。它是纵深防御的一层，不是强隔离的替代品。
- **jail 限制路径，不限制后果**：根内的密钥（`.env`、`~/.git-credentials`）照样读得到；根内的可执行位置（`.git/hooks/*`）照样能写。所以 jail 是纵深的一层，S5f 进程 sandbox 与部署侧容器化不可省。

### 3.6 文件工具（read / write / edit）

三个工具，各自参照不同的对位实现：**read / write 参照 DSH**（`packages/fs/tool-fs`），
**edit 参照 PI**（`packages/coding-agent/src/core/tools/edit.ts`）——PI 的多编辑数组
在真实模型上更耐用，DSH 的单处替换更简单；这里取 PI 的形状、DSH 的落盘保障：

| 工具 | 干什么 | 关键语义 |
| --- | --- | --- |
| `read_file` | 读内容 | **行寻址**（`offset` = 起始行号，1 起；`limit` = 行数，参照 PI）；**上限先到先算**：2000 行 **或** 50KB（PI 的 `DEFAULT_MAX_LINES`/`DEFAULT_MAX_BYTES`），显式给了 `limit` 就以它为准；**超长行单独截断并标记** `... (line truncated to 2000 chars)`（DSH 的 `maxLineLength` 对位）——minified 文件一行几百 KB，只靠总量上限会把预算吃光；**续读提示**参照 PI：`[Showing lines 12-31 of 40. Use offset=32 to continue.]` / `[End of file - total 40 lines]` |
| `write_file` | 整体写 | **只覆盖，不追加**（DSH 也没有 append：追加让「这次改了什么」变含糊）；**原子写**（同目录临时文件 + rename，写一半失败不留半个文件）；回执 `{created, replaced_bytes}`——`created` 对应 DSH 的 `Created`/`Updated` 措辞，`replaced_bytes` 是被覆盖丢弃的原有字节数（PI 的 edit 会报「改了几处」，同一类信息） |
| `edit_file` | 精确替换 | 参数 `{path, edits: [{old_string, new_string}]}`（**PI 形状**：一次调用改多处）；每处 `old_string` 在**原文件**里必须**唯一**命中，多处命中 → 报错并报出次数（让模型补足上下文）；各处的 `old_string` **互不重叠**（改同一片区域要合并成一条）；**不做 `replace_all`**——唯一性即安全；匹配前**行尾归一**（CRLF→LF）、写回时**还原原行尾**；**BOM 保留**；拒绝二进制；**原子写**（DSH 的 `writeFileAtomic`）；回执 `{replacements, first_changed_line, bytes}` |

- **为什么要有 edit**：整文件重写要求模型重新产出整个文件——它既怕改错别处、又无法确认自己改对了没，于是反复重写（实测：13 次写、8 次读同一批文件，直到单轮决策次数上限）。精确替换只要求给出要改的那几行，意图与结果一一对应。
- **edit 不创建文件**：文件不存在就报错——创建是 `write_file` 的活。
- **edit 是写操作**：走写 jail（`OpWrite`），且不声明 `tool.ConcurrencySafe` → 批内独占（屏障）。
- **不做"静默改多处"**：多处命中一律报错。唯一性即安全（PI 的规则）。
- **容错模型发来的怪形状**：有些模型把 `edits` 发成单个对象或 JSON 字符串（PI 源码点名过 Opus 4.6 / GLM-5.1），MoS 直接收下归一，而不是报错让模型重试。
- **尚未实现（DSH 有，MoS 留待后续）**：
  - **写前必须先读**（DSH 的 `fs-observation-policy`：按会话记录"已观察"状态，写/edit 前必须先读，且写时校验版本防丢失更新）。MoS 现在工具只有 `ctx`，拿不到会话身份，要做需要先给工具注入会话作用域——**排在 S5f 之后的单独一片**（S5h）。它是唯一能**根治**"模型覆盖别人文件"的机制：把"我是不是该先看一眼"从模型的自觉变成框架的保证。
  - **行号输出**（DSH 的 read 每行带行号 `12: code`；PI 不带）：MoS 只做行**寻址**，不带行号——行号会放大每个 token，将来 S6 做上下文预算时再一起定。
  - **图片读取**：MoS 的 `llm.ContentPart` 已支持多模态，但**工具结果通道只走 JSON 文本**，要支持得改 `execpath` 的结果通道。

### 3.7 进程沙箱（S5f-1）

```go
sb := &sandbox.Local{WorkDir: root, Timeout: 120 * time.Second, MaxTimeout: 300 * time.Second, SpillDir: spillDir}
reg.Register(builtin.NewShell(sb))
```

| 层 | 手段 | 挡住什么 |
| --- | --- | --- |
| ① 工作目录 | `cmd.Dir` | 相对路径的默认落点（**由宿主固定，不交给模型指定**——那等于给逃逸口） |
| ② 超时 + 进程组击杀 | `Setpgid` + `SIGKILL(-pgid)` | 死循环、fork 出的孤儿进程 |
| ③ 输出上限 | 内存只留前 N 字节，**丢弃而非阻塞** | 输出爆炸撑爆内存 / 管道写满挂死 |
| ④ 资源限制 | `ulimit -t/-v/-f` | 吃满 CPU / 内存 / 磁盘 |
| ⑤ 环境裁剪 | 白名单（PATH/HOME/LANG/…） | 把主机密钥泄给模型执行的命令 |
| ⑥ 网络 | 剥离代理变量 | 尽力而为，**挡不住直连** |

- **超时的缺省与上界都由宿主定**（两个旋钮，DSH 的 `timeoutMs` / `maxTimeoutMs` 对位）：
  - `Local.Timeout` = **缺省**（模型不指定时用它），缺省 30s（保守）；
  - `Local.MaxTimeout` = **上界**（模型最多能要求到哪），未配置时退化为 `Timeout`（**不放宽**）；
  - 模型可以在调用里要求更短（`{command, timeout}`，收紧自己的额度永远允许），要求更长则被夹到上界——超时是**资源预算**（卡死的命令占着 worker 槽、烧 CPU），若模型能无限加长，宿主设的预算就形同虚设；
  - 解析式同 DSH 的 `clampTimeout`：`min(请求 ?? 缺省, 上界)`。
  - 示例用缺省 120s / 上界 300s（慢的 `go test` 需要更多，但有天花板）。
- **预算可以是"活的"**（DSH 的 `Volatile` 对位）：`Local.Budget func() Budget` 是可选的动态来源，每次调用前解析一次——宿主据此在运行期调整预算（配置中心热更新、按租户给不同额度、按负载收紧）。
  - **为什么是函数而不是字段**：宿主要改的是**背后的状态**。直接改字段在工具并发执行时是数据竞争（race detector 会报）。正确姿势是让函数去读一个并发安全的状态：

    ```go
    var cell atomic.Value // 存 sandbox.Budget
    sb.Budget = func() sandbox.Budget { return cell.Load().(sandbox.Budget) }
    cell.Store(sandbox.Budget{Default: time.Minute, Max: 10 * time.Minute}) // 并发安全地改
    ```

  - 动态预算返回零值 → 回落到静态字段（不丢配置）；动态只给 `Max` 不给 `Default` → 缺省用静态的，上界用动态的。
  - 要**整体换掉** `Budget` 字段本身，只在装配期做（并发开始后换请换新的 Runner 实例）。
- **诚实的边界**：**限资源，不限文件**。本地实现限制的是"这次运行能消耗多少"，**不限制命令能碰哪些文件**——命令仍能访问运行用户权限内的任意路径。路径范围的强制隔离需要外部后端（bwrap / Landlock / Seatbelt / 容器），Go 标准库给不了；`sandbox.Runner` 接口就是留给它们的接缝（PRD 的 FR-E2）。命令级的语义约束属策略层（S5f-2）。
- **平台差异**：CPU 时间（`ulimit -t`）与单文件大小（`ulimit -f`）POSIX 平台都支持；**虚拟内存（`ulimit -v`）只有 Linux 等支持**，macOS 上会被跳过（每条限制都带 `|| true`，不支持的跳过而不是让命令失败）。
- **fail-closed**：没装配 `sandbox.Runner` 的 shell 工具**拒绝执行**——不存在"没配沙箱就跑裸命令"的默认路径（归档曾有该缺陷，用兜底 Runner 修）。
- **命令跑了但失败 ≠ 工具坏了**：非零退出码是**正常结果**——回执带 `exit_code`，输出末尾附 `[exit code: N]` 标记；只有"命令根本没起来"（启动失败 / 超时被杀 / 沙箱拒绝）才 `IsError`（对齐 DSH `packages/shell/tool-bash/src/render.ts`：那里非零退出同样不是 isError，isError 只留给 spawn 错误与中断）。这个区分不是洁癖：把非零退出当失败，**模型就得自己写 hack 绕开**（实测它会拼 `; echo "exit=$?"` 让整条命令以 0 退出，把失败塞进输出里）——"命令失败了"本来就该是事实，由模型判断要不要重试。
- **输出截断有逃生通道**：超限时完整输出由 Runner 落盘（`Result.OutputPath`），回执给模型 `output_path`，它可以 `read_file` 读全文（PI 的 `full_output_path` 对位，PRD 的"落盘逃生通道"）。落盘目录由宿主指定——默认的 `os.TempDir()` 在文件 jail 的可写根之外。
- **默认不装配**：示例的 shell 工具默认**不注册**——不给模型这个能力，比给它一个必然失败的工具干净。
- **开了 shell 就给默认系统引导词**：工具在清单里 ≠ 模型会主动用它。引导词说清三件事——有什么能力、工作目录在哪、为什么要用（"改 → 跑 → 看输出 → 再改"，别靠猜）。`MOS_SYSTEM_PROMPT` 可覆盖。
- **配套练手项目 `examples/chat/lab/`**：独立的 go module，故意留一个逻辑 bug，让 agent 修好并跑测试。**嵌套 module 被根 module 忽略**——仓库自己的 `go build ./...` / `go test ./...` 始终是绿的。

### 3.8 参数级决策点（S5f-2a）

**框架只提供决策点，规则归宿主。** 什么算"危险"由宿主决定，框架不替宿主决定——PI 把判定交给扩展（`tool_call` 事件 + `block`），DSH 干脆放在隔离层（沙箱档位 + 提权审批），两边的框架核心**都没有危险命令表**。

```go
// 宿主写规则（agent/policy 的参数级 API）
hp := policy.NewParamPolicy("宿主规则",
    func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
        if call.Tool == "shell" {
            cmd, err := policy.Arg(call, "command")
            if err != nil {
                return policy.VerdictDeny, "参数解析失败：" + err.Error()  // fail-closed
            }
            if hasRecursiveDelete(cmd) {
                return policy.VerdictAsk, "递归删除需要人工确认"
            }
        }
        return policy.VerdictAllow, ""
    })

// 两个门都要装
guards.Add(hp.Guard())                              // deny → 拒绝（单调，不可被洗白）
hooks.On(hook.HookToolPreExecute, hp.Approval())   // ask  → 需人工确认
```

- **两个门各管一档**：守卫（Guard）单调、只判放/拒、不可被钩子洗白；pre-execute 钩子能表达"需人工确认"第三档。混用会丢掉其中一档的能力——只装守卫 → ask 档直接放行；只装钩子 → deny 档不再单调。
- **理由回填给模型**："为什么被拒"比"被拒了"有用——模型要靠理由自我纠正。
- **检查器拿到的是只读快照**（`policy.Call` 传值）：物理上改不动载荷，"载荷冻结"这条不变式不会被参数级检查器破坏。
- **参数解析失败 = 拒绝**（fail-closed）：参数都读不懂，谈不上判定。
- **ask 档当前 = 拒绝**：没有审批通道时由执行通道 fail-closed（S5g 落地后才是"挂起等审批"）。
- **分层**：工具名级名单（`Policy` 的 Allow/Deny）管"能不能调这个工具"；参数级规则管"这次调用的参数危不危险"。示例里两者串在同一条守卫链上（先名字级、后参数级）。

### 3.9 人工审批（S5g）

**框架负责编排，问人由宿主注入。** 与 PI / DSH 同构：PI 的扩展用 `ctx.ui.confirm`，DSH 用 `approval/request` 事件上的 answerer 链——框架本身都不画界面。

```go
// 宿主注入"问人"的那一方（agent/approval 的 Answerer）
rt.Answerer = terminalAnswerer{in: in, out: os.Stderr}   // 弹窗 / 终端 / 别的进程都行

// 框架侧：签发 id → 记"问了" → 问人 → 归一化 → 记"答了"
svc := approval.NewService(rt.Answerer, sessionAudit{sess: sess})
```

- **四档结果**（DSH 的词汇）：`approved` / `rejected` / `cancelled` / `unavailable`。`unavailable`（没人应答、通道坏了）一律 fail-closed。
- **未装配即拒绝**（NFR-1）：没有 `Answerer` 时，pre-execute 的 `ask` 降级为拒绝（fail-closed）——不"先执行再问"。拒绝理由里带着"为什么要人工确认"。
- **批准只对本次调用生效**（DSH 的 allowed-once）：不缓存批准。
- **通道报错 ≠ 人拒绝**：报错归一到 `unavailable`（fail-closed），但原因要带出去——模型要分得清"人没答"和"通道崩了"。
- **审计写失败就中止**：审计链断了就不知道谁批的（"日志即事实"）。
- **审计落会话日志**：新增两个事件类型 `approval/asked` 与 `approval/decided`（同一个 id 配对）。这是 PRD 点名的例外（§可观测「approval/* 等事件全量落会话日志」、G4「审批挂起-恢复 E2E」），不走事件目录预算制。
- **拒绝文案自带"不许绕"的语义**（对齐 DSH `escalation.ts:203` 的 "it stays denied, so stop and explain instead of working around it"）：回填给模型的是「该操作保持拒绝状态，请停下向用户说明，不要尝试用其他命令或方式达成同一目的」。这不是文案洁癖——**实测不加这句话，模型会在被拒后拆解命令绕开规则**（把 `git rm -r lab` 改成逐个 `git rm` 达成同一目的）；合法的重试只有一条路：征得同意后**原样**重新发起（会再次弹审批，批准不缓存）。
- **人工拒绝不终止本轮**：终止会剥夺模型"向用户解释"的机会——拒绝的正确结果是模型停止调工具、改为说明，这需要循环继续一轮。框架另有显式的 `cancel` 决策（终止本轮），那是"扩展点要求终止"，不是"人拒绝"。
- **词法规则的教训**（2026-10 事故）：宿主规则曾键在"含字母 r 的 flag"上——`git status --short` 误触发，而逐个 `git rm` 漏过，被拒后模型拆解命令绕过拦截。规则必须键在**语义**（"这是删除命令"）而不是**字母形状**上；且词法规则终究只是业务规则，`find -delete`、脚本语言仍绕得过——**边界只能是隔离（S5f-2b）**。
- **边界：S5g 是阻塞式**——驱动线程等一个人工决定（DSH 也是这样：工具调用 await 一个 promise）。**跨进程的挂起-恢复**（进程死了还能接着跑）属 `agent/checkpoint` 那一片；审批接口不关心决定怎么来，所以那一片落地时这里不用改。

### 3.10 路径强制隔离（S5f-2b：macOS Seatbelt ✅ / Linux Landlock ⬜）

**边界只能是对进程的内核约束**。词法规则与审批是"减事故面"——2026-10 的事故证明它们会被拆解命令绕过；而内核约束与命令怎么拼写无关（`git rm -r` 和逐个 `git rm` 删的是同一个禁区）、子进程继承、符号链接在 vnode 层解析（软链逃逸无效）。

**默认语义（fail-closed）**：

- `Local.DefaultScope`：nil 时从 WorkDir 派生（读 = 写 = WorkDir）——**忘了配置 = 只有工作区**，不是全盘。"忘记声明"这个最常见的错误，后果必须是最小权限；
- `Request.PathScope` 只能**收窄**（默认范围的子集）；放宽是宿主**装配期**的权力，运行期拒绝——放宽是宿主的政策，不是模型（调用方）的权力；
- 写根含 `/` = **显式全开**：不套约束直接跑（容器场景的正当逃生门——"/" 是一次性容器的根）。危险写在配置里，审查看得见；框架不替宿主判断"你的机器是不是一次性的"。

**macOS 后端（Seatbelt）**：SBPL profile 只收窄**写**——`(allow default)` → `(deny file-write*)` → 放行 `/dev/null` 与写根（DSH `profiles.ts` 同款，实战验证过的最小集）。读强制不做：`deny file-read*` 会碎掉动态链接器与系统缓存，成本远超收益，且事故威胁模型全在写侧；`ReadRoots` 参与收窄校验但此后端暂不强制（强制待 Landlock）。SBPL 按**内核解析后**的路径匹配，写根先过 `EvalSymlinks` 规范化（`/var` → `/private/var` 不规范化会让工作区写被自己拦住）。

**fail-closed 纪律**：sandbox-exec 不可用 → `ErrPathScopeUnsupported`；非 darwin 平台尚无后端 → 非全开的调用明确报错。**绝不退化成"没隔离的假装隔离"**——"假装有边界"比"没有边界"更危险。

**与文件 jail 的分工**：jail 管**工具**（read_file/write_file/edit_file）的路径；`PathScope` 管**子进程**能碰到的路径——命令可以绕过工具直接 `cat /etc/passwd`，jail 管不着。两层都要。

### 3.11 模型调用的重试（S2b）

**机制在框架，策略在宿主**——与 PI / DSH 同构：重试是模型调用层的事，不该散落到业务各处各写一遍；但"重试几次、退避多久、哪些错误值得重试"是产品决策，框架不发明默认策略。

```go
llm.WithRetry(model, llm.RetryPolicy{
    MaxAttempts: 3,        // 总尝试次数（含首次）；<=1 = 不重试
    BaseDelay:   time.Second,
    MaxDelay:    30 * time.Second,
    Jitter:      0.1,      // <0 关闭；0 用默认 0.1
    Retryable:   nil,      // nil 用默认分类；宿主可覆盖
    OnScheduled: func(attempt int, delay time.Duration, err error) { /* 记日志/指标 */ },
})
```

- **默认不装配就没有重试**（fail-fast，把问题早暴露）；机制在 `llm`，参数由宿主给（示例：`-retry-attempts` / `-retry-delay`）。
- **只重试瞬时错误**（`llm.DefaultRetryable`）：429 / 408 / 5xx / 连接层失败 / 空响应。**4xx、鉴权失败、配额耗尽、用户取消一律快速失败**——重试确定性的错误只是浪费配额、拖长失败、放大日志。
- **状态码不能随字符串一起丢**：`llm.StatusError{Provider, StatusCode, Message}` 由适配器层返回（`transport.ParseStatusError`），`llm.StatusCodeOf(err)` 穿透错误链取状态码。分类靠**结构化字段**，不靠 parse 文本。
- **退避是有界指数 + 抖动**：`min(BaseDelay × 2^(n-1), MaxDelay)`，再乘 (1 ± Jitter)——抖动避开惊群（多个实例同时失败、同时重试、同时再失败）。
- **取消是终止性的**：ctx 结束（取消/超时）立即停止，不再重试；退避等待中 ctx 结束则直接返回 `ctx.Err()`。
- **流式只在还没吐出内容时重试**：增量已下发的话重试会让用户看到重复内容。
- **重试要看得见**：`OnScheduled` / `OnFinished` 回调——静默重试是查不出问题的。
- 对标：PI `packages/ai/src/utils/retry.ts`、DSH `packages/llm/llm/src/retry-policy.ts`。

### 3.12 写前观察（S5h：防丢失更新）

**问题**：模型 read 之后文件被 shell/另一个会话/人改了，再 write/edit 就会盖掉它没见过的改动。

**契约**（对齐 DSH 的 `fs-observation-policy`，落在 `tool/builtin` 的 `ObserveGate`——不是 Guard（管权限）也不是 Hook（管循环），是文件工具契约的一部分，与原子写同级）：

| 调用 | 无观察记录 | 记录=存在 | 记录=不存在 |
| --- | --- | --- | --- |
| `read_file` | 记录（存在→版本；不存在→absent） | 刷新版本 | 刷新 |
| `write_file` | createIfAbsent（已存在→拒"先读再覆盖"） | CAS（变了/没了→拒"自读取后已被修改/删除"） | 现在却存在→拒（它读后出现了） |
| `edit_file` | **拒**："编辑前必须先 read_file" | CAS | 拒："文件不存在，创建用 write_file" |

- **版本令牌** = `dev:ino:size:mtimeNs:ctimeNs`（身份+新鲜度；dev/ino 让"删了重建同名文件"也能被识别）。unix 取完整令牌，其他平台退化 `size:mtimeNs`（文档注明）。
- **CAS 在写的时刻做**——读与写之间的任何改动都会被抓到，模型被迫重读，看到真实状态后再决定。shell 改了文件 → edit 被拒 → 模型重读，这正是 S5h 的核心价值。
- **新建文件不需要先读**（createIfAbsent）——正常创建流程不受影响。
- **能力由宿主决定**：`FileTools(fsys, gate)`，nil 门 = 旧行为（无条件变更）。
- **并发与边界**：进程内写/改独占执行（execpath 屏障调度），校验窗口天然串行；**跨进程竞争不设防**（DSH 的 hard-link no-replace 暂未做）——文档写明。

### 3.13 上下文工程（S6：截断 + 压缩 + TaskState/Auditor）

包 `agent`（截断 / 压缩 / 审计门三件套）+ `session.TaskState`（外置可信状态）。上下文是稀缺资源（不变式 4），三件套各管一段：

**TaskState 不存储、从事件日志派生**（`session.DeriveTaskState`）——"日志即事实"（不变式 2）的自然延伸：Goal = 首条用户输入；Facts = 审计门信任工具的**无错** `tool/result`（证据即回填原文，(tool, evidence) 去重）；Status 派生三档（pending / running / completed；blocked 留给 M3，不猜）。Restore 后免费恢复，Persistence 无需感知它的存在。Pending / Blocked 字段是 M3 Manager 角色的预留位。

**审计门**（`agent.Auditor`）：只有环境自证的只读工具（`current_time`、`read_file`）的输出可进 TaskState；变更类（shell、write_file）的输出是"未核实声明"，要进状态需用只读工具二次取证，或宿主 `Trust` 显式扩名单（决策在宿主，机制在框架）。nil 审计门 = 全不信任（fail-closed）——模型自述永远不进状态（不变式 3）。

**截断**（`agent.ResultTruncator` → `TruncateHook` 挂 `tool/post-execute`）：行/字节双限制（默认 2000 行 / 64KB），按工具定向保留——`read_file` 留头（结构在前）、`shell` 留尾（报错在后）、其余两头各半；截断说明带"少了多少"。逃生通道：全文落盘（宿主指定目录，**必须在文件 jail 可读根内**——jail 外的路径是模型兑现不了的承诺），落盘失败诚实降级为"只报截断"。日志落的是截断后的回填——日志记的就是模型实际看到的。

**压缩**（`agent.Compactor`，每个决策轮前重算）：历史超 `KeepRecent`（默认 12）时，旧消息让位给一份摘要 + 最近原样保留。两条不变式：
- **切割点不切断 ToolCall-Result 对**：保留区首条若是孤儿 tool 结果（调用方被切），切割点前移到成对为止；前移到头（无可丢弃区）就放弃压缩——不拿摘要冒充历史；
- **摘要 = TaskState 快照，而非 LLM 重新总结**（不变式 4）：已验证事实的投影，不是模型的转述。

压缩是派生行为，无需持久化任何"压缩状态"；`OnCompact` 回调供宿主观测（静默压缩是查不出问题的）。

**决策路径顺序固定**：`History` 投影 → 压缩（框架行为，要读日志派生状态，不经钩子）→ `pre-step` 瀑布（宿主钩子：RAG / 注入等）→ 决策器。两者均零开销降级（Compactor / Hooks 为 nil 时与未注入完全一致）。

**轮次上限放开**：压缩接管上下文增长后，`MaxDecisionRounds <= 0` = 不限（默认，对齐 PI/DSH）；保险丝旋钮保留给无人值守场景。

## 4. 治理规则（防再膨胀）

1. 新增一个包 = PRD 有一条 FR 指名要它；否则留在 `main_bak1015` 不动。
2. 钩子/守卫/事件目录预算制：新增一个 HookKind 或事件类型前，先删一个旧的。
3. 每片合入验收门 = `go test ./...` 全绿 + 本文件/README 状态同步 + 该片一页说明，缺一不合。
4. 破坏性变更直接改、不留兼容层，但必须在提交信息与 PRD 变更记录声明。
5. **框架层不碰文件系统与网络，也不绑定配置格式**：需要 I/O 的能力以接口注入（如 `session.Persistence`），由宿主选实现；配置以结构进，键名与解析规则归宿主（`policy.Config` 不带格式 tag）。归档惯例（`agent/config.Load(data []byte)`）即此，框架替宿主打开路径等于绕过自己的文件 jail。
