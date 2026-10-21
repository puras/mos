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
| `llm`（+`llm/transport`） | ChatModel 抽象、协议适配、共享 HTTP envelope | 移植归档 | S2 |
| `tool`（+`tool/builtin`） | 工具接口、注册表、Schema 生成、`ConcurrencySafe` 并发分类器；`current_time` / `read_file` / `write_file` / `edit_file` | 移植裁剪 | S3 ✅ / S5e / S5d+ |
| `planner` + `driver` | Planner 接口 + ReAct 决策；LoopDriver + ReActDriver | planner 移植 / 驱动重写 | S4a ✅ |
| `agent/execpath` | 工具调用统一执行通道：preflight 裁决 + 调度 + 按调用序回填 | 重写 | S5a ✅ |
| `agent/hook` | 双轨制：HookBus（waterfall 可改写）+ GuardBus（serial 单调裁决） | 移植裁剪 | S5b ✅ |
| `agent/policy` | 工具名级策略：fail-closed 黑白名单 + YAML 装配 | 移植裁剪 | S5c ✅ |
| `filesystem` | 文件 jail：宿主声明可读/可写根 + Lexical/Realpath 双层校验 | 移植裁剪 | S5d ✅ |
| `sandbox` | 进程隔离（jail + 超时 + 资源限制 + 环境裁剪 + 网络控制） | 移植裁剪 | S5f |
| `agent`（上下文件） | 截断、压缩、Auditor | 重写瘦身 | S6 |
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
- **能力缺口 fail-closed**：未装 `RunContext.Tools` 却要求工具时直接报 `react.ErrToolsUnsupported`，且**不写 assistant 事件**（避免留下非法历史）；
- **内层上限**：单 turn 决策上限 `maxDecisionRounds = 16`，防模型空转；S6 的 DoomLoopGuard 与 MaxTurns 落地后由其接管停止裁决，届时重议。

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
- **目录预算制（治理规则 2）**：`HookKind` 有预算，新增一种前先删一种。当前目录：`pre-step`、`tool/pre-execute`、`tool/execute`、`tool/post-execute`、`turn-stopping`——每个都注明了消费它的切片。归档实现里的 `request`（预留、无消费者）已删；`doomloop`（重复调用检测）属 S6，届时随 `turn-stopping` 落地。
- **健壮性**：钩子返回 `(nil, nil)` 显式报 `ErrNilHookEvent` 而不是把 nil 传下去崩在下游——坏插件不许带崩整条链；注册与执行并发安全（执行前快照钩子链）。
- **接线点（S5e 全部接上）**：守卫由 `execpath.ExecuteBatch` 的 preflight 调 `GuardBus.Allow`，拒绝项不执行并以 `IsError` 回填；钩子总线 `HookBus` 的三个 `tool/*` 环节全部接线——`tool/pre-execute` 裁决调用、`tool/execute` 环绕派发、`tool/post-execute` 改写结果文本。驱动把 `RunContext.Guards` / `RunContext.Hooks` / `RunContext.Concurrency` 与轮次一并传入。
- **决策词汇（S5e）**：pre-execute 钩子返回 `Decision`：允许（默认）/ 拒绝 / 请求确认（无审批通道时 fail-closed 拒绝，S5g 落地审批后改为挂起）/ 终止本轮（该调用拒绝 + 同批后续跳过 + 本轮以 `terminated` 收尾）。链上第一个非 allow 决策生效，下游钩子不再执行。
- **错误只可加重不可减轻**（对齐 DSH）：post-execute 钩子可以把成功改成失败（发现问题就报错），但**不能**把失败洗成成功——错误是事实，审计日志不能被改写成成功。

### 3.4 策略层（S5c）

```go
// 框架的输入只有结构（格式与键名归宿主）
policy.Config{Allow: []string{"current_time", "read_file"}, Deny: []string{"shell"}}
```

```yaml
# 宿主侧的一个可能形态（键名由应用定，框架不认）
allow: [current_time, read_file]
deny:  [shell]
```

- **fail-closed（不变式 1）**：未列入 `allow` 的工具一律拒绝。平台新增自定义工具必须显式声明——“忘了声明”的后果是拒绝而不是放行。零值 `Config{}` = 拒绝一切；`nil` 接收者也拒绝而非 panic——fail-closed 是行为不只是默认值。
- **deny 优先于 allow**：同时命中以拒绝为准，`deny` 不提供任何放行后门。
- **位置**：Policy 是 `hook.Guard`，装进 `GuardBus` 即为单调守卫链的一环（S5b 双轨制），由执行通道的 preflight 调用；钩子改写不了它的结论。
- **分层：框架只认结构，不认文件、不认配置格式**。`agent/policy` 的直接依赖只有 `context`/`fmt`/`strings` + `agent/hook`：
  - 读文件（`os.ReadFile`）是宿主的活——框架替宿主打开路径等于绕过 S5d 的文件 jail（自己成了越界那一方），且部署未必有文件系统（配置在远端时直接失效）；
  - 解析 YAML/JSON 也是宿主的活——格式与键名归平台，框架不绑定任何一种：`policy.Config` **不带 yaml/json tag**，也不提供 `ParseYAML`（绑一个格式就要为它背依赖，而平台可能用 JSON、配置中心或远端下发）；
  - 宿主要严格解析（字段名拼错必须报错，否则用户以为规则生效了）就用 `yaml.Decoder.KnownFields(true)` / `json.Decoder.DisallowUnknownFields()`，参考 `examples/chat/main.go` 的 `appPolicy` + `loadPolicy`；
  - 该边界由 `agent/policy/layer_test.go` 钉住（框架包不得 import `os`/`net/http`/`yaml.v3`/…；`encoding/json` 不在其列——那是框架自己的载荷格式，`ToolCall.Args` 解析要用）。
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

## 4. 治理规则（防再膨胀）

1. 新增一个包 = PRD 有一条 FR 指名要它；否则留在 `main_bak1015` 不动。
2. 钩子/守卫/事件目录预算制：新增一个 HookKind 或事件类型前，先删一个旧的。
3. 每片合入验收门 = `go test ./...` 全绿 + 本文件/README 状态同步 + 该片一页说明，缺一不合。
4. 破坏性变更直接改、不留兼容层，但必须在提交信息与 PRD 变更记录声明。
5. **框架层不碰文件系统与网络，也不绑定配置格式**：需要 I/O 的能力以接口注入（如 `session.Persistence`），由宿主选实现；配置以结构进，键名与解析规则归宿主（`policy.Config` 不带格式 tag）。归档惯例（`agent/config.Load(data []byte)`）即此，框架替宿主打开路径等于绕过自己的文件 jail。
