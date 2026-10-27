# S7 装配层（`mos.New`）

装配层是 SDK 的大门：把「模型 + 工具 + 策略 + 安全 + 上下文工程」一次性装成一个
可跑的智能体，宿主只提供**策略**（要什么、允许什么），不重复写样板。

```go
a, err := mos.New(mos.Config{
    Model:     model,
    Tools:     reg,
    Approval:  answerer,      // 问人：宿主实现
    Sandbox:   &sandbox.Local{WorkDir: dir},
    Policy:    &policy.Config{Allow: []string{"read_file"}},
    Rules:     hostRules,     // 参数级规则：deny→守卫 / ask→审批
})
reply, err := a.Run(ctx, sess, "读一下 README")
```

## 1. 不变式

| # | 不变式 | 含义 |
| --- | --- | --- |
| A1 | **能力即证据** | **能力**字段（审批/沙箱）是接口/对象；`nil` = **不提供该能力**（模型看不到、需要时一律拒绝）。不用「开关 + 参数」表达「有没有」 |
| A2 | **缺能力 = fail-closed** | `Model == nil` → `New` 报错；`Approval == nil` → 需人工确认的调用一律拒绝；`Sandbox == nil` → 不注册 shell |
| A3 | **配置 ≠ 装配** | `Config` 不含文件路径、配置格式、密钥来源——YAML/JSON 解析与密钥获取归宿主（`policy` 层已是这样做的） |
| A4 | **装配快照是事实** | `Describe()` 只描述「装了什么 / 没装什么」，不描述「应该装什么」；宿主不再各写一份横幅 |

### 能力 vs 机制：两种语义，别混

| 类别 | 字段 | 语义 |
| --- | --- | --- |
| **能力**（只有宿主有） | `Approval` / `Sandbox` | 给了才有。`nil` = 没有（模型看不到 / 一律拒绝） |
| **机制**（框架造得出，参数是量） | `Compactor` / `Truncator` | 默认装，显式关（`DisableCompaction` / `TruncateMaxBytes < 0`）；宿主自备则用宿主的 |

宿主自备时优先用宿主的（想换摘要提示词、想换截断策略时）。

### 宿主自己的钩子：装配之后挂

装配层把「审批钩子」放在 `pre-execute` 的**最前**（ask 决策要短路后面的钩子——
被拦下的调用不该再打「即将执行」）。所以宿主自己的观测钩子**在 `New` 之后**挂：

```go
a, _ := mos.New(cfg)
hooks := a.RunContext().Hooks
hooks.On(hook.HookToolPreExecute, logPreExecute)
```

## 2. API 形状

```go
package mos

// Config 是装配层配置：全字段可选，零值 = 框架默认（见 §3）。
type Config struct {
    // ── 必填 ──
    Model llm.ChatModel // nil → New 报错（A2）

    // ── 能力（nil = 不提供，A1）──
    Tools       *tool.Registry        // 工具注册表（nil = 无工具）
    Approval    approval.Answerer     // 人工审批：问人由宿主实现
    Sandbox     *sandbox.Local        // shell 执行器（nil = 不注册 shell）
    Observe     *builtin.ObserveGate  // 写前观察（nil = 不启用，保持旧行为）
    Compactor   agent.Compactor       // 上下文压缩（nil = 不压缩）
    Truncator   agent.Truncator       // 工具结果截断（nil = 不截断）
    Persistence session.Persistence   // 会话落盘后端（nil = 内存会话）

    // ── 策略（结构体，零值 = 框架默认）──
    Policy *policy.Config   // 名字级 Allow/Deny（nil = 不收紧）
    Rules  []policy.Check   // 参数级规则：deny→守卫，ask→审批
    Scope  *sandbox.PathScope // 路径强制范围（nil = 从 WorkDir 派生）

    // ── 运行参数 ──
    WorkDir           string // 默认 os.Getwd()
    Concurrency       int    // <=0 = 默认 4
    MaxDecisionRounds int    // <=0 = 不限（S6 起默认不限）
}

// New 装配智能体。缺 Model 或参数非法直接报错（A2）。
func New(cfg Config) (*Agent, error)

// Agent 是装配产物：模型 + 工具 + 策略 + 安全 + 上下文工程。
type Agent struct{ /* ... */ }

// Run 执行一次任务（驱动自行决定流式/非流式路径）。会话由调用方持有——
// 多会话/并行场景（Web 控制台）不被单会话 API 卡住。
func (a *Agent) Run(ctx context.Context, sess *session.Session, input string) (string, error)

// Describe 返回装配快照（多行文本），供宿主直接打印。
func (a *Agent) Describe() string

// RunContext 暴露底层运行上下文：需要运行期改参数（如换并发）时用。
func (a *Agent) RunContext() *driver.RunContext
```

**为什么不用 option 函数**：仓库现有风格已是结构体（`policy.Config` / `sandbox.Local` /
`CompactorConfig` / `TruncatorConfig`）；结构体顺序无关、可序列化、可比较、审查友好。
option 函数适合「字段爆炸 + 想藏顺序」，MoS 的配置分组清晰，用结构体更好读。

## 3. 默认值表（集中在 `defaults.go`，一处可改）

| 项 | 默认 | 依据 |
| --- | --- | --- |
| 上下文窗口 | **128K**（`agent.DefaultContextWindow`） | 未设置就用默认；确切窗口更小的模型由宿主显式配置 |
| 响应预留 | 16K | PI 同款 |
| 压缩后保留近期 | 20K | PI 同款 |
| 工具结果截断上限 | **32KB** | 一次工具结果 ≈ 8K token，占 128K 可用预算 ~7%；全文落盘可回读，不丢信息 |
| 并发上限 | 4 | 既有默认（仅对声明可并发的工具生效） |
| 决策轮次 | 不限 | S6 起由压缩接管上下文增长；保险丝留给无人值守场景显式设置 |
| 截断落盘目录 | `<WorkDir>/.mos-trunc` | 逃生通道要在工作区可读根内，否则「回读全文」不可达 |
| 文件工具根 | `<WorkDir>` | fail-closed：没声明就只有工作区 |
| 路径强制范围 | 从 `<WorkDir>` 派生 | 只能收窄，不能在运行期放宽；`"/"` 是显式逃生门 |

> 截断上限从 64KB 收到 **32KB**（`agent.DefaultTruncateMaxBytes`）：64KB ≈ 16K token，
> 单次工具结果就吃掉 128K 窗口的 14%，几次叠加就过半。全文仍落盘，代价只是多一次回读。

## 4. `Describe()` 的输出形态

框架生成「装了什么」的事实描述，宿主直接打印——不再各写一份横幅（现在 chat 示例
自己写了 20 行）：

```
MoS chat 已就绪（provider=anthropic model=agnes-2.5-flash session=chat 已有 0 条事件 / 累计 0 tokens）
  已注册工具: current_time, read_file, write_file, edit_file
  模型可见工具: current_time, read_file, write_file, edit_file
  文件沙箱: /path/to/workdir（越界路径会被拒绝，工具不会执行）
  并发上限: 4（仅对声明可并发的工具生效）
  上下文压缩: 已装配（窗口 131072 / 预留 16384 / 保留 20000 tokens）
  结果截断: 32768 字节（全文落 /path/to/workdir/.mos-trunc，read_file 可回读）
  人工审批: 未装配（需确认的调用一律拒绝，fail-closed）
```

未装配的能力**显式写出来**（"未装配" / "一律拒绝"），不让宿主用沉默代替事实。

## 5. 示例改造

`examples/chat/main.go`（724 行）里，装配样板从 ~120 行变成 ~10 行：

```go
a, err := mos.New(mos.Config{
    Model:     model,
    Tools:     reg,
    Approval:  answerer,
    Sandbox:   sb,          // 仅当 -allow-shell
    Observe:   fileGate,
    Compactor: compactor,
    Policy:    cfg,         // 宿主解析的 policy.Config
    Rules:     hostRules,
    Scope:     scope,
    WorkDir:   jailRoot,
})
```

宿主仍自己负责（框架不越界）：YAML 解析、密钥获取、终端审批 UI、工具选择、
REPL 循环、事件落盘的落盘位置。

## 6. 与 PI / DSH 的对位

| | PI | DSH | MoS（本设计） |
| --- | --- | --- | --- |
| 装配形态 | 扁平 options 接口 | 声明式配置 + 分层加载 | `Config` 结构体（与仓库现有风格一致） |
| 默认值 | 逐字段注释 | 集中在配置层 | `defaults.go` 一处 + `Describe()` 可查 |
| 子件 | 接口注入 | 接口注入 | **能力即证据**：`nil` = 不提供 |
| 配置格式 | 不绑 | 绑（YAML/环境变量） | **不绑**（A3） |
| 装配可观测 | 靠调用方打印 | schema 可导出 | `Describe()` 事实快照（A4） |

## 7. 测试计划

1. `Config.Model == nil` → `New` 报错（A2）；
2. `nil` 能力 → 不出现在 `Describe()`、模型看不到该工具、需要审批的调用被拒；
3. 默认值断言：窗口 128K / 预留 16K / 保留 20K / 截断 32KB / 并发 4；
4. `Describe()` 快照内容断言（含「未装配」的显式措辞）；
5. 集成：最小能力装一套 → `Run` 一轮 → 事件落会话、工具结果截断落盘可回读。

## 8. 运行期变更

**改 `Config` 变量不会生效**：`mos.New(cfg Config)` 按值接收，Agent 持有的是装配
那一刻的快照。避免"改一半生效一半"（只换了策略没换工具可见性）与并发写字段。

运行期改变行为有三条通道：

| 通道 | 机制 | 生效时机 |
| --- | --- | --- |
| 宿主实现内部可变 | 注入接口（`approval.Answerer`、`sandbox.Local.Budget`、自备 `Compactor`） | 立即（宿主自己保证并发安全） |
| 工具/钩子/守卫动态增删 | `tool.Registry`、`HookBus.On`、`GuardBus.Add` 都带锁并返回移除函数 | 下一轮 |
| **装配层旋钮** | `SetConcurrency` / `SetMaxDecisionRounds` / `SetPolicy` / `SetRules` | **下一轮** |

**为什么"下一轮生效"**：一轮里工具载荷已冻结、守卫已判过，中途换策略会让本轮历史
与下一轮不一致；轮边界（`turn/start` / `turn/end`）是天然的一致点。

**并发安全**：旋钮值存原子变量；每轮跑的是运行上下文的**浅拷贝**（驱动只读副本），
所以并行会话之间没有共享可变状态——钩子/守卫/注册表是共享的，那是有意的。

**不允许**：并发直接改 `RunContext` / `sandbox.Local` 的字段（数据竞争 + 半更新 +
会话日志不反映）。`sandbox.Local.Budget func()` 就是为此存在的替代品。

**不落审计事件**：会话日志记的是"发生了什么"，不是"配置长什么样"；策略是宿主
策略，换了值从 `Describe()` 能查到。

## 9. 本切片不做

- YAML/JSON 解析、配置来源、密钥获取（宿主）；
- option 函数式 API；
- 配置文件路径进 `Config`；
- 隐式读环境变量；
- 装配层替宿主选工具（工具集是宿主决策，A1 的延伸）。
