# 顶层装配入口（assistant）

> 来源：`assistant/` 整个包 —— `assistant.go`（核心装配）+ `config.go`（配置结构）+ `policy.go` / `policy_shell.go` / `policy_file.go`（默认 Policy）+ `request_human_input.go`（M2.8 人工介入工具）+ `activate_skill.go`（M2.6 技能激活工具）+ `confirm.go`（旧同步确认器接口）。
> 目标读者：理解"如何一行代码集成 MoS"的平台开发者；所有 agent 子系统文档都会引用此文档作为入口。

---

## 1. 一句话定位

**`assistant` 是 MoS 的顶层装配入口**：把"模型对象 + 工具集 + 框架概念（策略/规划/审计/记忆/审批/checkpoint/持久化/事件总线）"按 `Config` 装配成可运行实例（`*Assistant`），提供 `New / Run / RunStream / CreateHandle / Handle / Resume` 一行集成的 API。**平台只需写 YAML/Config + 注册 provider，业务只用 `agent.Run(...)` 调智能体**。

源码顶端的注释（assistant.go:80-82）一段话讲清定位：

> "New 使用模型对象、工具集与运行配置构建智能体（v2：默认装配 = ReActDriver + 默认钩子）。model、tools 与 Config 中的框架概念均由调用方注入（nil 时使用框架默认实现），本函数不关心模型接入配置与具体工具实现。"

文件分布：

| 文件 | 行数级 | 主题 |
|---|---|---|
| `assistant.go` | ~350 行 | Assistant 主体 + New + Run + Handle |
| `config.go` | ~150 行 | Config + FileAccessConfig |
| `policy.go` | ~180 行 | DefaultPolicy + Checker 表 + Mode 推导 |
| `policy_shell.go` | ~170 行 | shell 命令分级防御 |
| `policy_file.go` | ~85 行 | 文件路径校验 |
| `request_human_input.go` | ~115 行 | M2.8 人工介入工具 |
| `activate_skill.go` | ~135 行 | M2.6 技能激活工具 |
| `confirm.go` | ~20 行 | 旧 Confirmer 接口（Deprecated） |

---

## 2. 整体装配链路

```
YAML 配置（或编程式 Config）
   ↓ assistant.New(model, tools, cfg)
   ├─ 校验 FileAccess.WorkspaceRoot 必填（fail-closed）
   ├─ 工具注册（tool.Registry）
   ├─ 框架概念 nil 时填充默认值
   │     ├─ Policy      → NewDefaultPolicy(nil, "")
   │     ├─ Planner     → planner.NewReAct(model)
   │     ├─ SystemPrompt → DefaultSystemPrompt（默认引导词）
   │     ├─ Confirm → Approval 桥接（NewConfirmerAdapter）
   │     ├─ Approval != nil → 注册 request_human_input 工具
   │     ├─ SkillsDir 非空 → 注册 activate_skill 工具
   │     ├─ EventBus     → event.NewEventBus()
   │     └─ DefaultPolicy.Approval = cfg.Approval
   ├─ buildRunContext（共享 RT：Tools/Planner/Memory/Policy/Auditor/Observer/.../FS）
   ├─ assembleDriver（按 cfg.Driver / DriverFactory 选 react/plan-execute/reflection/scripted）
   └─ handle.NewRegistry(a.newAgentForSession, cfg.CheckpointStore, cfg.EventBus)
        ↓
   *Assistant { agent, config, tools, policy, model, persistence, checkpoints, bus, registry, runContext }
```

**两个关键特征**：

1. **nil → 默认实现** —— 所有"框架概念"字段都能不传，框架会用内置默认实现兜底
2. **共享 RunContext** —— `buildRunContext` 在 New 与 newAgentForSession（handle.Registry.Create/Resume）里都调用，保证每个会话的 RT 拥有**完全相同的框架配置**（FS、Truncator、Compactor 等共享能力）

---

## 3. `Assistant` 结构（assistant.go:28-40）

```go
type Assistant struct {
    agent       *driver.Agent          // 配置好的驱动实例（react/plan-execute/...）
    config      Config                // 装配配置（公开：Config() 返回）
    tools       *tool.Registry        // 工具注册表（公开：ToolNames() 返回）
    policy      driver.Policy         // 安全策略

    model       llm.ChatModel         // 保留供新会话重建 RunContext（Handle.Create/Resume）
    persistence session.Persistence   // 事件日志后端
    checkpoints checkpoint.Store      // checkpoint 存储
    bus         *event.EventBus       // 事件总线
    registry    *handle.Registry      // 会话句柄注册表（Handle.Create/Resume 入口）
    runContext  *driver.RunContext    // 共享 RunContext（暴露 FS 给工具层）
}
```

**关键设计**：

- **`model`/`persistence`/`checkpoints`/`bus` 单独保留引用** —— 这些是"会话级重建需要"的字段。`newAgentForSession` 调 `buildRunContext(a.model, a.tools, a.config)` 复用装配期配置，但 RT.Session 会替换为新会话
- **`runContext` 暴露 `FS`** —— `Assistant.FS()` 返回 `runContext.FS`，工具经 `fs.WithFileSystem(ctx, a.FS())` 注入

---

## 4. `New` 装配核心（assistant.go:83-151）

### 4.1 校验顺序（fail-closed）

1. **FileAccess.WorkspaceRoot 必填** —— 立刻返回 error，避免后续装配消耗（assistant.go:84-86）
2. **工具注册** —— 每个工具都 `reg.Register(t)`，重复注册立刻报错
3. **nil 默认值填充** —— Policy/Planner/SystemPrompt/Approval/EventBus
4. **条件注册工具** —— Approval≠nil → request_human_input；SkillsDir≠空 → activate_skill
5. **DefaultPolicy.Approval 镜像** —— `p.Approval = cfg.Approval` 让 checker 内部能用到 Approval
6. **buildRunContext + assembleDriver** —— 装配共享 RT + 驱动实例
7. **handle.NewRegistry** —— 会话句柄注册表

### 4.2 工具的"条件自动注册"（assistant.go:108-122）

```go
if cfg.Approval != nil {
    reg.Register(RequestHumanInput{})    // M2.8 人工介入
}
if cfg.SkillsDir != "" && !cfg.DisableActivateSkill {
    skills, _ := agent.LoadSkills(cfg.SkillsDir)
    reg.Register(ActivateSkill{Skills: skills})   // M2.6 技能激活
}
```

**关键设计**：工具注册**不是无脑全开**，而是按框架配置自动装配。`DisableActivateSkill` 让应用层可以**自管技能注入**而不被框架覆盖。

### 4.3 `DefaultPolicy.Approval` 镜像（assistant.go:124-126）

```go
if p, ok := cfg.Policy.(*DefaultPolicy); ok {
    p.Approval = cfg.Approval
}
```

**问题**：用户既注入了自定义 Policy 又注入了 Approval，**DefaultPolicy 内部需要的 Approval 字段怎么拿到？**

**答案**：直接强转 `*DefaultPolicy` 后赋值。注释强调这是"驱动侧一致性" —— checker 返回 `*approval.RequiredError` 时需要知道走哪个 Approval 通道（驱动层 policyGuard 会用 `cfg.Approval`，但 checker 自己也要知道）。

**这意味着**：

| 用户行为 | 后果 |
|---|---|
| 用默认 Policy + 注入 Approval | DefaultPolicy.Approval 被镜像，正常工作 |
| 用自定义 Policy + 注入 Approval | checker 不感知 Approval，**无法异步确认** |
| 用默认 Policy + 不注 Approval | shell 受限危险命令拒绝（fail-closed），write_file 路径受限放行（旧版行为） |

### 4.4 `buildRunContext` 共享 RT（assistant.go:154-195）

```go
rt := &driver.RunContext{
    Tools: reg, Planner: cfg.Planner, Policy: cfg.Policy,
    Auditor: cfg.Auditor, Observer: cfg.Observer,
    Truncator: cfg.Truncator, Compactor: cfg.Compactor,
    Approval: cfg.Approval, Checkpoints: cfg.CheckpointStore,
    EventBus: cfg.EventBus,
    MaxTurns: cfg.MaxTurns, ShouldStopAfterTurn: cfg.ShouldStopAfterTurn,
    ContextBudget: cfg.ContextBudget, DoomLoopMaxRepeat: cfg.DoomLoopMaxRepeat,
    PlanPlanner: cfg.PlanPlanner, Reflector: cfg.Reflector,
    ScriptPlanner: cfg.ScriptPlanner,
    // M2.6 系统提示词三层选择：
}
switch {
case cfg.Assembler != nil:
    rt.Assembler = cfg.Assembler
case cfg.ProjectDocDir != "" || cfg.SkillsDir != "":
    rt.Assembler = agent.NewDefaultSystemAssembler(...)   // 自动构建默认组装器
default:
    rt.SystemPrompt = cfg.SystemPrompt                  // 静态引导（既有行为）
}
rt.FS = buildFileSystem(cfg)
```

**三层 Assembler 选择**：

1. **用户自定义** —— `cfg.Assembler` 优先
2. **自动默认** —— 配置了 `ProjectDocDir` 或 `SkillsDir` 时自动构建槽位式组装器
3. **静态引导** —— 两者都没配，回退到 SystemPrompt 字符串（v1 行为）

详见 `docs/agent/system.md`。

### 4.5 `buildFileSystem` fail-closed（assistant.go:204-215）

```go
policy := &fs.FileAccessPolicy{
    WorkspaceRoot: cfg.FileAccess.WorkspaceRoot,
    JailReads: cfg.FileAccess.JailReads,
    JailWrites: cfg.FileAccess.JailWrites,
    WritableExtra: cfg.FileAccess.WritableExtra,
}
if policy.JailReads || policy.JailWrites {
    return &fs.Sandboxed{Policy: policy}
}
return fs.NewLocal(fs.ModeDangerFull)
```

**关键点**（注释 assistant.go:200-203 强调）：

> "**jail 开关的唯一事实来源是 FileAccessConfig.Jail\***，不是 Policy 的具体类型"

—— 第三方 Policy 实现（未实现 `Mode()` 方法）配 `JailWrites=true` 时**同样必须拿到 Sandboxed**，否则 jail 静默失效（fail-open → 灾难）。`fs.Sandboxed.Mode()` 内部由同一份 `FileAccessPolicy` 推导，与 `DefaultPolicy.Mode()` 同形映射。

### 4.6 `assembleDriver` 三选一（assistant.go:219-235）

```go
func assembleDriver(cfg Config, rt *driver.RunContext) (*driver.Agent, error) {
    if cfg.DriverFactory != nil {
        return cfg.DriverFactory(rt), nil    // 优先级 1：自定义工厂
    }
    switch cfg.Driver {
    case "", "react":
        return react.New(rt), nil
    case "plan-execute":
        return planexecute.New(rt), nil
    case "reflection":
        return reflection.New(rt), nil
    case "scripted":
        return scripted.New(rt), nil
    default:
        return nil, fmt.Errorf("assistant: unknown driver %q", cfg.Driver)
    }
}
```

**优先级**：`DriverFactory` > `Driver` 名内置装配。

`DriverFactory` 是给自定义驱动用的（按 RT 现场构造），`Driver` 字符串是给"四大内置驱动"用的（YAML 友好）。

---

## 5. `Config` 配置结构（config.go:28-133）

Config 是框架层 API 契约。**模型与工具不在 Config 中**（分别通过 New 的位置参数注入），框架概念（Policy/Planner/Auditor/Memory/Observer/...）以对象形式注入，nil 时用默认实现。

### 5.1 字段分组

| 分组 | 字段 | 默认行为 |
|---|---|---|
| **驱动** | `Driver` / `DriverFactory` / `PlanPlanner` / `Reflector` / `ScriptPlanner` | `react` + nil planner（可跑但 plan-execute/reflection 必需） |
| **系统提示** | `SystemPrompt` / `Assembler` / `ProjectDocDir` / `SkillsDir` / `DisableActivateSkill` | 默认引导词 + 静态注入 |
| **模型循环** | `MaxTurns` / `ShouldStopAfterTurn` / `ContextBudget` / `DoomLoopMaxRepeat` | 0 = 关闭 |
| **框架能力** | `Policy` / `Planner` / `Auditor` / `Memory` / `Retriever` / `RetrieverMinScore` / `Observer` / `Truncator` / `Compactor` | nil = 默认实现 |
| **基础设施** | `Runner` / `HTTPClient` / `FileAccess` | nil = 兜底 |
| **M2.8 持久化** | `Approval` / `Confirm` / `CheckpointStore` / `Persistence` / `EventBus` | nil = fail-closed / 内存 |

### 5.2 关键字段语义

**`MaxTurns`**（config.go:46-49）：

> "可选保险丝：一次任务循环的最大 Turn 数，>0 时达到上限仍未完成则阻塞；0 表示不限制（默认，对齐 DSH/PI 无轮次上限）"

**`ShouldStopAfterTurn`**（config.go:50-52）：每轮结束外部裁决（PI shouldStopAfterTurn 对齐）。

**`ContextBudget`**（config.go:106-108）：

> "模型上下文窗口 token 预算（M2.5 上下文利用率监控）：每次模型调用后统计 PromptTokens/预算 利用率并广播 EventContextUsage 事件"

**`DoomLoopMaxRepeat`**（config.go:110-112）：

> "相同工具连续 maxRepeat 次相同参数调用时判定为循环，任务中断（状态 blocked）"

**`Memory` / `Retriever`**（config.go:75-83）：v2 预留 —— 当前 Memory 配置不生效（事件日志已经是事实来源），Retriever 是 RAG 扩展点。

**`Runner`**（config.go:86-89）：v2.24 从 tool.Sandbox 拆出。`nil` 时 shell 工具兜底 `sandbox.FromContext` → Default（含进程组击杀，避免孤儿进程）。

**`HTTPClient`**（config.go:90-93）：v2.24 引入。`nil` 时兜底 `httpx.Default`（60s 超时）。

**`FileAccess`**（config.go:94-97 / config.go:135-142）：

```go
type FileAccessConfig struct {
    WorkspaceRoot string   // 必填（fail-closed）
    JailReads     bool     // DSH SaaS/read-only = true
    JailWrites    bool     // DSH 默认 = true
    WritableExtra []string // 附加可写根
}
```

### 5.3 M2.8 持久化字段

```go
Approval        approval.Approval       // 异步审批通道
Confirm         Confirmer               // 旧同步确认器（Deprecated，自动桥接）
CheckpointStore checkpoint.Store        // checkpoint 存储
Persistence     session.Persistence     // 事件日志后端
EventBus        *event.EventBus         // 事件总线
```

**DefaultPolicy.Approval 镜像**（assistant.go:124）—— 见 §4.3。

**Confirm 自动桥接**（assistant.go:105-107）：

```go
if cfg.Approval == nil && cfg.Confirm != nil {
    cfg.Approval = approval.NewConfirmerAdapter(confirmBridge{inner: cfg.Confirm})
}
```

—— 保留 CLI y/N 体验（同步），但内部走 Approval 异步通路。

---

## 6. Run / RunStream / Handle 三类入口

### 6.1 `Run`（assistant.go:313-315）

```go
func (a *Assistant) Run(ctx context.Context, sess *session.Session, input string) (string, error) {
    return a.agent.Run(ctx, sess, input)
}
```

**最简路径**：会话 + 输入 → 直接驱动执行。

### 6.2 `RunStream`（assistant.go:319-321）

```go
func (a *Assistant) RunStream(...) (string, error) {
    return a.agent.RunStream(ctx, sess, input)
}
```

**打字机效果**：模型文本增量实时广播为 `EventStream` 事件。模型/规划器不支持流式时自动回退为非流式路径。

### 6.3 `CreateHandle`（assistant.go:289-291）

```go
func (a *Assistant) CreateHandle(ctx context.Context) (*handle.AgentHandle, error) {
    return a.registry.Create(ctx, a.NewSession())
}
```

**新会话 + Handle 注册**：UUID 自动生成（`uuid.NewString()`），M2.8 异步审批的入口。

### 6.4 `Handle`（assistant.go:295-307）

```go
func (a *Assistant) Handle(ctx context.Context, sessionID string) (*handle.AgentHandle, error) {
    if a.persistence == nil {
        return nil, fmt.Errorf("assistant: handle requires Persistence configured")
    }
    sess, err := session.Restore(a.persistence, sessionID)
    if err != nil { ... }
    return a.registry.Resume(ctx, sess)
}
```

**从持久化恢复**：load 事件日志 → 重建会话 → 注册 Handle。

**`Persistence == nil` 时返回 error**（fail-closed）—— 没有持久化无法跨进程恢复。

### 6.5 `NewSession`（assistant.go:258-264）

```go
func (a *Assistant) NewSession() *session.Session {
    sess := session.NewSession(uuid.NewString())
    if a.persistence != nil {
        sess.Persistence = a.persistence
    }
    return sess
}
```

**UUID 默认 + 持久化挂载**：让"日志即事实"对新会话自动生效。

### 6.6 `newAgentForSession`（assistant.go:269-286）—— Handle 重建核心

```go
func (a *Assistant) newAgentForSession(sess *session.Session) (*driver.Agent, *driver.RunContext, error) {
    rt := buildRunContext(a.model, a.tools, a.config)
    rt.Session = sess
    ag, err := assembleDriver(a.config, rt)
    ...
}
```

**复用 + 替换**：

- **复用**：`a.model` + `a.tools` + `a.config`（共享装配）
- **替换**：`rt.Session = sess`（每个 Handle 独立会话）

**为什么需要？** `handle.Registry.Create / Resume` 必须为指定会话构建独立驱动实例 —— 不能让所有会话共享一个 RT（否则多会话并发会数据竞争）。

---

## 7. `DefaultPolicy` 默认安全策略（policy.go）

### 7.1 一句话定位

**DefaultPolicy 是 MoS 的"工具调用安全门"**：把所有工具名 → 检查器的映射做成**注册表**（`map[string]Checker`），让新增工具策略 = 注册一行，平台自定义工具可通过 `DefaultPolicy.Register` 注入策略。

注释（policy.go:21-32）一段话讲清哲学：

> "shell 命令分级防御：①绝对危险命令（shutdown/mkfs/dd 等）无条件拒绝；②受限危险命令（rm/sudo/chmod/管道 shell）路径受限 + Approval 人工确认；③普通命令可配置白名单。未知工具默认拒绝（fail-closed）。"

### 7.2 数据结构（policy.go:33-47）

```go
type DefaultPolicy struct {
    ShellAllowed []string              // 命令前缀白名单（空 = 仅基础防御）
    WorkDir      string                // 文件/受限危险命令的操作根
    Approval     approval.Approval     // M2.8 异步审批通道
    FileAccess   FileAccessConfig      // 镜像自 Config.FileAccess

    mu       sync.RWMutex
    checkers map[string]Checker        // 工具名 → 检查器（未注册默认拒绝）
}
```

### 7.3 `Mode()` 推导 fs.Mode（policy.go:52-61）

```go
func (p *DefaultPolicy) Mode() fs.Mode {
    switch {
    case p.FileAccess.JailReads && p.FileAccess.JailWrites:
        return fs.ModeSaaSJailed          // 读写都 jail
    case p.FileAccess.JailReads || p.FileAccess.JailWrites:
        return fs.ModeLocalJailed         // 一侧 jail
    default:
        return fs.ModeDangerFull          // 零值：显式全开放
    }
}
```

**与 fs.Sandboxed.Mode 同形映射** —— 保证策略侧推导的 fs 模式与装配侧选出的 fs 实现一致。

### 7.4 内置检查器清单（policy.go:71-82）

| 工具 | 检查器 | 策略 |
|---|---|---|
| `shell` | `checkShell` | 分级防御（§7.5） |
| `read_file` | `checkFileField("path")` | 路径限于 WorkDir |
| `write_file` | `checkWrite` | 路径 + Approval 确认 |
| `find_files` | `checkFileField("root")` | 路径 |
| `grep` | `checkFileField("root")` | 路径 |
| `web_fetch` | `checkWebFetch` | http/https 协议 |
| `web_search` | `checkWebSearch` | 关键词长度约束 |
| `current_time` | `allow` | 无副作用 |
| `request_human_input` | `allow` | 人工介入工具（自证） |
| `activate_skill` | `allow` | 技能激活（自证/只读） |

**未注册的工具默认拒绝**（policy.go:87-95）：

```go
if !ok {
    return fmt.Errorf("未知工具 %q 不在安全策略放行名单，已拒绝", call.Name)
}
```

—— **fail-closed 哲学**：扩展点 = 注册；不注册 = 不允许。

### 7.5 `checkShell` 三级防御（policy_shell.go:13-55）

```
① 绝对危险命令（无条件拒绝）：
   - shutdown / reboot / halt / poweroff / dd / mkfs.*
   - "> /dev/sd" / ":(){"  （fork 炸弹）

② 命令前缀白名单（非空时）：
   - 不匹配 → 拒绝
   - 匹配 → 放行

③ 受限危险命令（路径受限 + 异步确认）：
   - rm / sudo / chmod / chown / mv / kill
   - 解释器 -c/-e：bash -c / sh -c / python3 -c / python -c / perl -e / node -e / ruby -e / php -r
   - find -delete / find -exec
   - 管道 shell：| sh / | bash
   ↓
   路径受限在 WorkDir 内（checkPathsInWorkDir）+
   Approval 异步确认 → *approval.RequiredError
```

**关键设计**：

- **解释器 -c/-e 列入受限**（policy_shell.go:116-140 注释解释）：
  > "防止模型绕过黑名单改用 python3 -c / bash -c / find -delete 达成删除等破坏性意图"
- **路径校验启发式**（policy_file.go:60-80）—— 取非 flag token 且像路径的逐一校验
- **未配置 Approval** → 受限危险命令 fail-closed 拒绝（policy_shell.go:43-44）

### 7.6 `checkFile` / `withinDir` 多根支持（policy_file.go:13-69）

```go
func withinDir(p *fs.FileAccessPolicy, target string) error {
    for _, root := range fs.WritableRoots(p) {        // 多根（workspace + extra + /tmp + tempdir）
        if pathUnderRoot(root, target) {
            return nil
        }
    }
    return fmt.Errorf("路径 %q 超出工作目录 %q", target, p.WorkspaceRoot)
}
```

**`fs.WritableRoots`**：平台能配置 `WritableExtra` 加多个可写根，与 sandbox 共用同一根集（防配置漂移）。

### 7.7 `Register` 平台扩展点（policy.go:99-110）

```go
func (p *DefaultPolicy) Register(name string, chk Checker) error {
    if name == "" || chk == nil {
        return fmt.Errorf("policy: 注册检查器需要非空工具名与检查器")
    }
    p.mu.Lock()
    defer p.mu.Unlock()
    if _, exists := p.checkers[name]; exists {
        return fmt.Errorf("policy: 工具 %q 已注册策略检查器", name)
    }
    p.checkers[name] = chk
    return nil
}
```

**不变量**：

| 约束 | 含义 |
|---|---|
| `name == "" / chk == nil` 拒绝 | 输入合法性 |
| 重复注册拒绝 | 防止覆盖（强制显式语义） |
| `mu` 互斥 | 并发注册安全 |
| **装配期调用**（注释 policy.go:98） | 首次 Run 前调用，避免运行时改安全语义 |

---

## 8. `RequestHumanInput` 人工介入工具（request_human_input.go）

### 8.1 一句话定位

**M2.8 的"模型主动挂起请求人类"**：模型在工具选择阶段调 `request_human_input` → 工具发 `Approval.Request` → 返回 `*SuspendError` → driver 挂起（checkpoint 落库）+ 退出 Run → 平台 Resolve → Resume 续跑 → 工具读取决策表直接返回答案。

注释（request_human_input.go:21-32）讲清三阶段：

> "**首次调用**：经 ctx 读取审批通道（Approval.Request）→ 返回 *approval.SuspendError 挂起；**resume 恢复**：经 ctx 读取决策表，该调用已有回答 → 直接返回回答（工具结果）；**已拒绝** → 返回错误（原因反馈给模型）。"

### 8.2 三阶段 Execute 逻辑（request_human_input.go:46-110）

```go
// 阶段 1: resume 决策表命中 → 直接返回
if decs := approval.ResumeDecisions(ctx); decs != nil {
    if d, ok := decs[callID]; ok {
        if !d.Approved { return nil, fmt.Errorf(...) }
        return map[string]any{"answer": d.Content, "answered_by": d.DecidedBy}, nil
    }
}

// 阶段 2: 审批通道缺失 → fail-closed
ap, ok := approval.ApprovalFrom(ctx)
if !ok || ap == nil {
    return nil, fmt.Errorf("...未配置 Approval...fail-closed")
}

// 阶段 3: 发起异步请求
id, err := ap.Request(ctx, req)
if err != nil { return nil, err }

// 阶段 4: Immediate（同步确认器）立即返回
if im, ok := ap.(approval.Immediate); ok {
    if d, found := im.Decision(ctx, id); found { ... }
}

// 阶段 5: 异步 → 挂起
return nil, &approval.SuspendError{
    ID: id, Request: req, CallID: callID,
    HookKind: hook.HookToolExecute,
    ReplayTurn: 0, ReplayStep: 0,   // 由 driver 在 saveCheckpoint 时填充
}
```

### 8.3 关键设计点

**CallID 注入**（line 55）：`tool.CallIDFrom(ctx)` —— execpath.ExecuteHook 在工具执行前注入当前调用的 ID，决策表按 call ID 查。

**Immediate 分支**（lines 90-100）：如果 Approval 同时实现 `approval.Immediate`（即决策立即可用），不挂起直接返回 —— CLI 同步确认器走这条路。

**未配置 Approval 时 fail-closed**（line 77）：明确报错"未配置 Approval（人工确认通道）"—— 不是 panic，是返回 error 给模型，让模型知道"这条路走不通"。

---

## 9. `ActivateSkill` 技能激活工具（activate_skill.go）

### 9.1 一句话定位

**M2.6 Skills 强化**（Task 6）：模型经 `activate_skill` 工具触发一个技能，把**正文 + references + 允许工具列表**注入本次 Run；正文由工具直接返回给模型（**不进事件日志，避免大字段污染**）。

注释（activate_skill.go:17-25）一段话讲清流程：

> "1. 解析 name → 在 Skills 中查找匹配；2. 读取 SKILL.md 正文（去掉 frontmatter）；3. sess 注入：`sess.AddTurnSkill(target)` 累积 + 更新 `sess.TurnToolSubset`；4. 经 ctx 取出装配层注入的 *session.Session → 写 `skill/activate` 事件；5. 返回 `SkillActivationResult` 给模型。"

### 9.2 `Sequential` 声明（activate_skill.go:44-49）

```go
func (ActivateSkill) Sequential() bool { return true }
```

**为什么 Sequential？** 工具改写共享会话状态（`sess.TurnSkills` / `sess.TurnToolSubset` + `sess.Append` 写事件）。模型一次返回多个 `activate_skill` 时并发执行会产生数据竞争（**Seq 重复/丢失 + 白名单切片撕裂 —— 而白名单是安全控制**）。声明串行让 `execpath.ExecuteBatch` 把整批回退为顺序执行，与 `rt.AddFact` 用 `factMu` 串行化 `TaskState.Facts` 同源。

### 9.3 `SkillActivationResult` 不进事件日志（activate_skill.go:57-63）

```go
type SkillActivationResult struct {
    Name         string   `json:"name"`
    Body         string   `json:"body"`             // 大字段：只返给模型，不入 event log
    References   []string `json:"references"`
    AllowedTools []string `json:"allowed_tools"`    // 安全控制
}
```

**关键设计**：正文（可能很大）不进 Event Log，避免会话日志被大字段污染。事件日志只记元数据（name / allowed_tools / references），日志即事实的核心数据契约不被破坏。

### 9.4 与装配层的衔接（assistant.go:115-122）

```go
if cfg.SkillsDir != "" && !cfg.DisableActivateSkill {
    skills, _ := agent.LoadSkills(cfg.SkillsDir)
    reg.Register(ActivateSkill{Skills: skills})
}
```

**装配层 + 工具执行 + 会话状态**三层协作：

```
assistant.New 时：
  → agent.LoadSkills(cfg.SkillsDir)        // 读 SKILL.md frontmatter（不读正文）
  → reg.Register(ActivateSkill{Skills})    // 注册工具到工具集

模型调 activate_skill 时：
  → readSkillBody(target.Dir)              // 按需读正文（懒加载）
  → sess.AddTurnSkill(target)              // 累积到会话状态
  → sess.TurnToolSubset 更新（白名单并集）
  → sess.Append(skill/activate 事件)       // 写元数据
  → 返回 SkillActivationResult 给模型      // 正文走单独通道
```

---

## 10. `Confirmer` 旧同步确认器（confirm.go，Deprecated）

```go
// Deprecated: M2.8 起人工介入改为异步审批（agent/approval.Approval）。
// 旧同步确认器经 agent/approval.NewConfirmerAdapter 桥接为 Approval
// （保留 CLI y/N 体验）；新代码应直接实现 agent/approval.Approval。
type Confirmer interface {
    Confirm(ctx context.Context, req ConfirmRequest) (bool, error)
}
```

**保留理由**：CLI 用户习惯 y/N 同步体验。`confirmBridge`（assistant.go:338-346）把 `Confirmer` 包成 `approval.Confirmer`，经 `NewConfirmerAdapter` 桥接成 `Approval`：

```go
if cfg.Approval == nil && cfg.Confirm != nil {
    cfg.Approval = approval.NewConfirmerAdapter(confirmBridge{inner: cfg.Confirm})
}
```

—— **新代码不应该再用 Confirmer**，直接实现 `approval.Approval`。

---

## 11. DefaultSystemPrompt 默认引导词（assistant.go:51-78）

70 行的内置默认系统提示词，分五段：

| 段落 | 主题 |
|---|---|
| 一、任务处理方法 | 理解 → 拆解 → 工具验证 → 总结 |
| 二、工具使用规范 | 按需选择 → 阅读结果 → 检索优先 → 先读后写 → 优先专用 |
| 三、错误处理与坚持 | 诊断 → 重试 → 止损 → 诚实 |
| 四、安全与边界 | shell 限制 → 敏感信息 → 危险请求确认 |
| 五、多轮与语言 | 复用上下文 → 答用户同语言 |

**设计参考**（注释 assistant.go:45-47）：

> "Claude Code：身份/任务规范/工具规则/高风险审慎/输出规则的分段结构；Manus：agent loop 与'任务方法论 + 局限性'的完整表达；OpenHands / Cline：问题解决工作流（拆解-执行-验证）与失败诊断重试准则。"

—— 综合主流 agent 框架的 system prompt 设计思路。

**不重复工具清单**（注释 assistant.go:49-50）：

> "工具名称与参数描述已随 function calling 提供给模型，此处不重复清单，只保留'何时用、如何用、如何纠错、安全与输出规范'等高信号准则。"

---

## 12. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **FileAccess.WorkspaceRoot 必填** | assistant.go:84-86 | fail-closed |
| **未注册工具默认拒绝** | DefaultPolicy.Allow | fail-closed |
| **未配置 Approval 受限命令拒绝** | policy_shell.go:43-44 | fail-closed |
| **DriverFactory > Driver 名** | assembleDriver | 优先级 |
| **Config 字段 nil → 默认实现** | assistant.go:95-130 | 零配置可跑 |
| **DefaultPolicy.Approval 镜像** | assistant.go:124-126 | checker 内部用 |
| **Assembler 三层选择** | buildRunContext | 自定义 > 项目指令 > 静态 |
| **持久化是 Handle 的前提** | Handle() 校验 | fail-closed |
| **模型/工具/框架概念分三注入** | New 三位置参数 + Config | 关注点分离 |
| **工具条件自动注册** | Approval→request_human_input；SkillsDir→activate_skill | 装配层而非配置层 |
| **请求人类必须 Approval** | RequestHumanInput | fail-closed |
| **Sequential 工具强制串行** | ActivateSkill.Sequential() | 防并发写 |
| **Confirm 桥接为 Approval** | confirmBridge + NewConfirmerAdapter | 兼容旧 API |
| **共享 RunContext 不共享 Session** | newAgentForSession | 多会话并发安全 |
| **fs.Mode 由 FileAccessConfig 推导** | buildFileSystem | 单一事实来源 |
| **skill 正文不进事件日志** | SkillActivationResult | 防大字段污染 |

---

## 13. 自定义扩展姿势

### 13.1 最小集成（YAML）

```yaml
driver: react
model: claude-sonnet-4-5
tools: [builtin]
file_access:
  workspace_root: /home/user/project
  jail_reads: true
  jail_writes: true
```

```go
reg := seam.NewSeamRegistry()
seam.RegisterModel(reg, "claude-sonnet-4-5", anthropic.NewChatModel(...))
seam.RegisterTool(reg, "builtin", func() any { return builtin.Default() })

agent, err := config.Assemble(reg, yamlBytes)
sess := agent.NewSession()
reply, err := agent.Run(ctx, sess, "你好")
```

### 13.2 编程式装配

```go
agent, err := assistant.New(
    myChatModel,
    []tool.Tool{builtin.Default()...},
    assistant.Config{
        Driver: "plan-execute",
        PlanPlanner: myCustomPlanner{},
        FileAccess: assistant.FileAccessConfig{
            WorkspaceRoot: "/home/user/project",
            JailWrites: true,
        },
        Approval: myAsyncApproval{},   // M2.8 异步审批
        Persistence: myPersistence{},
        CheckpointStore: myCheckpointStore{},
    },
)
```

### 13.3 M2.8 异步审批流（CLI）

```go
// 装配阶段（保留 y/N 体验）
agent, _ := assistant.New(model, tools, assistant.Config{
    Confirm: &myCLIConfirmer{},   // 旧 Confirmer 自动桥接为 Approval
    FileAccess: ...,
})

// 运行时
handle, _ := agent.CreateHandle(ctx)
for {
    reply, err := handle.Run(ctx, "执行 rm -rf node_modules")
    if err == nil { break }                          // 完成
    if errors.Is(err, approval.ErrSuspended) {       // 挂起
        // CLI: 用户 y/N → 决策
        decision := myCLIConfirmer.Ask(...)
        handle.Resume(ctx, decision)                 // 续跑
        continue
    }
    return err
}
```

### 13.4 M2.8 异步审批流（平台）

```go
// 平台：异步调用 agent.Run
go func() {
    reply, err := handle.Run(ctx, input)
    platformCh <- RunResult{Reply: reply, Err: err}    // 挂起时 Err = SuspendError
}()

// 平台轮询：决策到来 → 注入
for pending := range platformCh {
    if pending.IsSuspended {
        // 平台 UI 弹窗 + 收集决策
        decision := waitForUserDecision()
        pending.Handle.Resume(ctx, decision)
    }
}
```

### 13.5 自定义驱动

```go
agent, _ := assistant.New(model, tools, assistant.Config{
    DriverFactory: func(rt *driver.RunContext) *driver.Agent {
        return myCustomDriver.New(rt)
    },
    FileAccess: ...,
})
```

### 13.6 自定义 Policy + 新工具

```go
policy := assistant.NewDefaultPolicy(nil, "/workspace")
policy.Register("my_custom_tool", func(ctx context.Context, args string) error {
    // 自定义检查逻辑
    return nil
})
agent, _ := assistant.New(model, []tool.Tool{
    builtin.Default()...,
    myCustomTool{},
}, assistant.Config{
    Policy: policy,
    FileAccess: assistant.FileAccessConfig{WorkspaceRoot: "/workspace"},
})
```

### 13.7 多会话并行

```go
h1, _ := agent.CreateHandle(ctx)
h2, _ := agent.CreateHandle(ctx)
go h1.Run(ctx, "任务 1")
go h2.Run(ctx, "任务 2")
```

—— 每个 Handle 独立的 RT + Session，`buildRunContext` + `newAgentForSession` 保证不共享可变状态。

### 13.8 暴露 FS 给工具（v2.24+）

```go
// 业务代码（与 assistant 解耦）
fs := assistant.FS()   // fs.FileSystem
ctx := fs.WithFileSystem(ctx, fs)
```

—— 工具经 `fs.WithFileSystem(ctx)` 取用，统一应用 FileAccess 策略。

---

## 14. 与其他包的关系

```
assistant
   ├─ 依赖 agent（核心抽象）
   │     ├─ agent/driver（驱动实现）
   │     ├─ agent/system（槽位组装）
   │     ├─ agent/handle（会话句柄）
   │     ├─ agent/checkpoint（挂起存储）
   │     ├─ agent/approval（异步审批）
   │     ├─ agent/scope（资源回收）
   │     ├─ agent/event（事件总线）
   │     └─ agent/session（会话）
   ├─ 依赖 llm（ChatModel）
   ├─ 依赖 planner（Planner/PlanPlanner/Reflector/ScriptPlanner）
   ├─ 依赖 tool（Registry/Tool）
   ├─ 依赖 filesystem（Sandboxed/Local）
   ├─ 依赖 sandbox（Runner）
   ├─ 依赖 memory（Memory/Retriever）
   └─ 不依赖 agent/execpath（间接通过 driver 调）
```

**`assistant` 是 MoS 唯一的"装配入口"**：平台应用只引 `assistant` + 注入 `llm.ChatModel` + `tool.Tool` + `Config` 就能跑。

---

## 15. 一句话总结

**`assistant` 是 MoS 的"顶层装配适配器"：把"模型对象 + 工具集 + 框架概念（策略/规划/审计/审批/checkpoint/持久化/事件总线）"按 Config 装配成可运行实例（*Assistant），提供 New + Run/RunStream + CreateHandle/Handle 一行集成 API。零配置可跑（所有 Config 字段 nil 时有默认实现）、fail-closed（WorkspaceRoot 必填 + 未知工具默认拒绝 + 未配 Approval 受限命令拒绝）、共享 RunContext + per-session RT 重建让多会话并行安全、DefaultPolicy 用检查器注册表（map[string]Checker）让自定义工具 = Register 一行、M2.8 异步审批经 ConfirmerAdapter 桥接保留 CLI y/N 体验 —— 是 v2 §3.5 "按名选组件"哲学的最顶层落地。**