# 钩子与守卫子系统（agent/hook）

> 来源：`agent/hook/hook.go`（HookBus + 共享类型） + `agent/hook/guard.go`（GuardBus） + `agent/hook/doomloop.go`（DoomLoopGuard）。
> 目标读者：扩展智能体可改写环节的开发者；理解"钩子可绕过 vs 守卫不可绕过"双轨制的工程师。

---

## 1. 一句话定位

**`agent/hook` 是 agent 的"中间件系统"**：把循环流程拆成 6 个可干预的环节（HookKind），每个环节允许业务方注册"瀑布钩子"（可改写/短路/替换）或"守卫"（不可绕过、任一拒绝即整体拒绝）。**Hook 与 Guard 双轨制是 v2 纵深防御的核心机制** —— 这条线在 `driver.md §3.1` 已经提过，本文是它的具体实现。

文件分布（Hook 与 Guard 各占一文件，对称清晰）：

| 文件 | 职责 |
|---|---|
| `hook.go` | 共享部分：`HookKind` 6 项枚举、`HookEvent` 载荷、`Hook`/`HookBus`（瀑布链） |
| `guard.go` | `Guard`/`GuardBus`（串行裁决链） |
| `doomloop.go` | `DoomLoopGuard`：同工具同参数连续调用检测（v2 §3.4） |

---

## 2. 核心抽象：6 个 HookKind

源码（hook.go:15-22）定义了 6 个可改写环节：

```go
const (
    HookPreStep         HookKind = "pre-step"          // 改写模型输入（压缩 / RAG / 注入）
    HookRequest         HookKind = "request"           // 改写请求配置（预留）
    HookToolPreExecute  HookKind = "tool/pre-execute"  // 拒绝 / 改写工具调用（Policy / 审批）
    HookToolExecute     HookKind = "tool/execute"      // around dispatch（超时 / 重试 / 指标）
    HookToolPostExecute HookKind = "tool/post-execute" // 改写结果 / 审计
    HookTurnStopping    HookKind = "turn-stopping"     // 终止判定（串行，无 next）
)
```

### 2.1 各环节语义对照

| HookKind | 典型用途 | 是否可短路 | 备注 |
|---|---|---|---|
| `pre-step` | 压缩历史、注入上下文、RAG 召回 | ✅ 不调 next 即不调模型 | 改写 `ev.Messages` |
| `request` | 改 temperature / tools / model 等 | ✅ | **预留**，见 driver 接线说明 |
| `tool/pre-execute` | Policy 校验、权限拦截、改写参数 | ✅ 不调 next 即拒绝 | 改写 `ev.ToolCall` |
| `tool/execute` | 超时、重试、指标采集 | ❌ 必须调 next 执行 | around dispatch |
| `tool/post-execute` | 截断、审计、改写结果 | ✅ | 改写 `ev.ResultText` |
| `turn-stopping` | 裁决本轮是否结束 |  串行执行 | **无 next**，每钩返回裁决 |

### 2.2 `HookEvent` 载荷（按环节用不同字段）

```go
type HookEvent struct {
    Kind       HookKind
    Messages   []session.InternalMessage  // pre-step：模型输入
    Req        *llm.Request               // request：请求配置（预留）
    ToolCall   *llm.ToolCall              // tool/pre-execute、tool/execute
    Result     any                        // tool/execute、tool/post-execute：原始返回值
    ResultText string                     // tool/post-execute：给模型看的文本（截断后）
    Err        error                      // tool/execute：执行错误
    Turn       int
    Step       int
}
```

不同 Kind 只用对应字段，其它字段为 nil/空。这是"宽 struct、按场景使用"的典型 Go 风格 —— 比为每个环节定义独立事件类型更省样板。

---

## 3. HookBus：瀑布钩子

### 3.1 数据结构

```go
type HookBus struct {
    mu     sync.RWMutex
    hooks  map[HookKind][]hookEntry  // 按 Kind 分组
    nextID uint64
}

type hookEntry struct {
    id uint64
    h  Hook
}
```

**关键点**：`id` 自增单调递增，配合返回的移除函数实现**幂等移除**（见 §3.3）。

### 3.2 Hook 函数签名

```go
type Next func(ctx context.Context, ev *HookEvent) (*HookEvent, error)
type Hook func(ctx context.Context, ev *HookEvent, next Next) (*HookEvent, error)
```

三参数模型：
- `ctx`：上下文
- `ev`：载荷（可改写）
- `next`：调用下游的回调（**不调 = 短路**）

### 3.3 `On` 注册 + 幂等移除

```go
func (b *HookBus) On(kind HookKind, h Hook) func()
```

返回的移除函数用 `sync.Once` 包裹，多次调用只生效一次（hook.go:72-86）。这种"返回移除函数 + 幂等"是 Go 里非常地道的资源管理惯用法（类似 `http.Server.RegisterOnShutdown`）。

### 3.4 `Waterfall` 瀑布链执行（hook.go:88-113）

实现细节：

```go
func (b *HookBus) Waterfall(ctx context.Context, ev *HookEvent) (*HookEvent, error) {
    if b == nil { return ev, nil }                    // nil bus = no-op
    
    b.mu.RLock()
    entries := append([]hookEntry(nil), b.hooks[ev.Kind]...)  // 拷贝 slice
    b.mu.RUnlock()
    
    chain := make([]Hook, len(entries))
    for i, e := range entries { chain[i] = e.h }
    
    var run func(i int, cur *HookEvent) (*HookEvent, error)
    run = func(i int, cur *HookEvent) (*HookEvent, error) {
        if i >= len(chain) { return cur, nil }        // 链尾
        h := chain[i]
        next := func(c context.Context, e *HookEvent) (*HookEvent, error) {
            return run(i+1, e)
        }
        return h(ctx, cur, next)
    }
    return run(0, ev)
}
```

**几个细节值得品**：

1. **快照语义**：在锁内拷贝 entries 切片后立即释放读锁。后续链式调用**不持锁**，避免钩子间互相阻塞（死锁风险）+ 允许钩子内回调 `On` 注册新钩子（虽然不会影响本次链）。
2. **递归闭包**：`run` 通过闭包捕获 `chain`，实现 `next(i+1)` 调用下游。
3. **短路即终止**：钩子不调 `next` 即返回当前 ev，整个链在此终止（可表示"拒绝执行"或"已自处理"）。
4. **nil 安全**：`b == nil` 直接返回 ev，对调用方友好。

### 3.5 瀑布模型示意

```
钩子A → 改写 ev → next → 钩子B → 改写 ev → next → 钩子C → next → 链尾
   │                    │                    │
   └─ 不调 next? ───────┴─ 不调 next? ───────┴─ 不调 next?
        ↓                   ↓                   ↓
       短路                短路                链尾自然终止
```

---

## 4. GuardBus：单调守卫

### 4.1 与 Hook 的根本区别

```go
type Guard func(ctx context.Context, ev *HookEvent) error
```

| 维度 | Hook | Guard |
|---|---|---|
| 签名 | `(ctx, ev, next)` 返回 `(ev, err)` | `(ctx, ev)` 返回 `error` |
| 链结构 | 瀑布（嵌套 next 调用） | 串行（for 循环） |
| 拒绝语义 | 不调 next（短路） | 返回非 nil error |
| 改写能力 | ✅ 可改写 ev | ❌ 只能读 |
| 顺序敏感 | 是（嵌套） | 否（顺序无关紧要，谁先拒都一样） |

注释（guard.go:8-9）写得很明确：

> "Guards 是单调守卫：串行裁决，短路即拒；**不可被 hook 改写绕过**（Policy / 权限 / 身份校验是不可重排、不可绕过的安全防线）"

也就是说：**安全相关的检查必须用 Guard，不能用 Hook**。业务钩子可以跳过自己的中间件，但跳不过 Guard。

### 4.2 `Allow` 串行裁决（guard.go:55-67）

```go
func (b *GuardBus) Allow(ctx context.Context, ev *HookEvent) error {
    if b == nil { return nil }                         // nil bus = 全通过
    
    b.mu.RLock()
    entries := append([]guardEntry(nil), b.guards...)  // 拷贝
    b.mu.RUnlock()
    for _, e := range entries {
        g := e.g
        if err := g(ctx, ev); err != nil {
            return err                                  // 任一拒绝即整体拒绝
        }
    }
    return nil
}
```

简单 for 循环，任一返回 error 立即返回 —— **短路语义天然由 for 循环实现**，不需要递归。

### 4.3 `Add` + 幂等移除

同 HookBus 的 `On`，返回幂等的移除函数（guard.go:30-52）。

---

## 5. Hook vs Guard 的实战对照

| 场景 | 用 Hook | 用 Guard |
|---|---|---|
| 注入上下文 | ✅ pre-step 改写 Messages | |
| RAG 召回 | ✅ pre-step 追加检索结果 | |
| 改写工具参数 | ✅ tool/pre-execute | |
| Policy 权限校验 | | ✅ 不可绕过 |
| Doom Loop 检测 | | ✅ 终止任务，不可绕过 |
| 工具超时重试 | ✅ tool/execute（around） | |
| 工具结果截断 | ✅ tool/post-execute | |
| 审计（写 Fact） | ✅ tool/post-execute | |
| 身份校验 | | ✅ 不可绕过 |
| 终止本轮裁决 | ✅ turn-stopping（无 next） | |

**判断标准**：能被业务"关掉而不影响安全"的用 Hook；"关掉等于关掉安全防线"的用 Guard。

---

## 6. DoomLoopGuard（M2.5 上下文工程）

### 6.1 问题：Doom Loop

注释（doomloop.go:11-14）：

> "相同工具连续多次相同参数调用（如模型反复重试同一失败操作）时判定为循环。命中后任务应中断（状态 blocked），避免空转耗尽上下文"

这是常见的"模型卡死"症状：上一次调用失败 → 模型重试相同参数 → 再次失败 → 再次重试 → 上下文被错误信息淹没。

### 6.2 检测器实现（doomloop.go:14-42）

```go
type doomLoopDetector struct {
    maxRepeat int       // 连续相同调用多少次判定
    toolName  string    // 上一次调用
    argsHash  string    // 上一次参数（TrimSpace 后的原文）
    count     int       // 连续次数
}

func (d *doomLoopDetector) observe(call *llm.ToolCall) bool {
    h := strings.TrimSpace(call.Args)
    if d.toolName == call.Name && d.argsHash == h {
        d.count++
    } else {
        d.toolName = call.Name
        d.argsHash = h
        d.count = 1
    }
    return d.count >= d.maxRepeat
}
```

**判据**：`(toolName, argsHash)` 元组相同即累加；任意一项不同即重置。

**指纹用原文而非哈希**（注释没说但看实现）：用 `TrimSpace(call.Args)` 作为 hash，优点是简单可调试，缺点是 args 大小写/空白差异可能漏判。生产可以考虑换成规范化 + sha1，但当前实现保持了"易读、易调试"的取向。

### 6.3 默认阈值：`maxRepeat <= 0 → 3`

注释（doomloop.go:25-28）说明默认 3。这与 `driver.RunContext.DoomLoopMaxRepeat` 的注释一致（默认 3，可配置）。

### 6.4 DoomLoopError：致命错误

```go
type DoomLoopError struct {
    ToolName string
    Repeat   int
}
```

实现 `error` 接口，错误信息清晰（doomloop.go:51-53）。循环层识别 `*DoomLoopError` 后**终止任务并置状态 blocked**。

### 6.5 Guard 实现（doomloop.go:69-77）

```go
type DoomLoopGuard struct {
    maxRepeat int
    det       *doomLoopDetector
}

func (g *DoomLoopGuard) Guard(_ context.Context, ev *HookEvent) error {
    if g == nil || ev == nil || ev.ToolCall == nil { return nil }
    if g.det.observe(ev.ToolCall) {
        return &DoomLoopError{ToolName: ev.ToolCall.Name, Repeat: g.maxRepeat}
    }
    return nil
}
```

**注意**：DoomLoopGuard 是 Guard 而非 Hook，注释（doomloop.go:58-60）明文：

> "与 Policy 守卫同级注册在 GuardBus（不可被普通钩子改写绕过）"

如果 Doom Loop 检查做成 Hook，恶意/有 bug 的钩子完全可以跳过检测 —— 这就违背了"安全防线不可绕过"的原则。

### 6.6 与 `MaxTurns` 的区别

| 机制 | 触发条件 | 状态 | 用途 |
|---|---|---|---|
| `MaxTurns` | 达到指定轮数 | blocked | **保险丝**，控制成本上限 |
| `DoomLoopGuard` | 同工具同参数连续 N 次 | blocked | **死循环检测**，避免无意义空转 |

两者互补：MaxTurns 是硬上限（兜底），DoomLoopGuard 是软检测（提前发现）。两者都置 blocked，都通过 GuardBus 实现（详见 `driver.md §4.5`）。

---

## 7. 与其它模块的协作边界

### 7.1 hook 包不依赖 driver

`hook` 包只依赖：
- `agent/session`（`InternalMessage`）
- `llm`（`ToolCall`、`Request`）

**不依赖** `agent/driver`、`agent`、`planner`。这是包依赖的"低层 vs 高层"区分：
- `hook` 是**基础设施**（与 driver 并列）
- `driver` 是**装配层**（消费 hook）

### 7.2 与 driver 的接线位置

driver.md §4.3 / §4.6 提到的：
- `Policy` 注入 `RunContext.Policy` → driver 注册为 Guard
- `Hooks` / `Guards` 字段就是 `HookBus` / `GuardBus` 实例
- driver 在各环节调 `Waterfall` / `Allow`

具体接线由驱动实现（react/planexecute/...）负责，hook 包只提供基础设施。

### 7.3 与 session 的关系

`HookEvent.Messages` 是 `[]session.InternalMessage`（含 `ExcludeFromContext`/`Summary`/`Source` 元数据）。

这意味着钩子拿到的不是 `[]llm.Message`，而是**内部消息** —— 钩子可读 ExcludeFromContext 的元数据（比如做"审计所有消息包括被排除的"），但不能直接把内部结构传给模型（必须经 `session.ConvertToLlm`）。

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **Hook 可短路，Guard 可拒绝，不可绕过** | 双轨实现 | 纵深防御 |
| **On/Add 返回幂等移除函数** | `sync.Once` 包裹 | 资源管理安全 |
| **Waterfall 不持锁** | 快照 entries 后立即 RUnlock | 避免钩子间死锁 |
| **nil bus 全部 no-op** | `b == nil` 分支 | 装配友好 |
| **Doom Loop 检测用 Guard 而非 Hook** | `DoomLoopGuard.Guard` | 安全防线不可绕过 |
| **Doom Loop 状态置 blocked** | 循环层识别 `*DoomLoopError` | 与 MaxTurns 协同 |
| **HookEvent 按 Kind 用字段** | 宽 struct | 不为每环节定义独立类型 |

---

## 9. 自定义扩展姿势

### 9.1 写一个瀑布钩子（审计场景）

```go
auditHook := func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
    // 1. 改写前的日志
    log.Printf("tool call: %s args=%s", ev.ToolCall.Name, ev.ToolCall.Args)
    
    // 2. 委托下游
    out, err := next(ctx, ev)
    
    // 3. 改写后日志
    log.Printf("tool result: %v err=%v", out.Result, out.Err)
    return out, err
}

remove := bus.On(hook.HookToolExecute, auditHook)
defer remove()  // 资源清理
```

### 9.2 写一个守卫（拒绝危险命令）

```go
denyShell := func(ctx context.Context, ev *hook.HookEvent) error {
    if ev.ToolCall != nil && ev.ToolCall.Name == "shell" {
        if strings.Contains(ev.ToolCall.Args, "rm -rf /") {
            return fmt.Errorf("shell: dangerous command denied")
        }
    }
    return nil
}

bus.Add(denyShell)
```

### 9.3 短路示例（pre-step 注入 RAG 后跳过模型）

```go
ragHook := func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
    if chunks := retrieve(ev.Messages); len(chunks) > 0 {
        ev.Messages = append(ev.Messages, chunks...)
    }
    return next(ctx, ev)  // 不短路，继续走模型
}
```

```go
// 或者：完全短路（不调 next）
forceAnswer := func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
    ev.Messages = append(ev.Messages, session.AssistantMsg("硬编码答案", ""))
    return ev, nil  // 不调 next = 不调模型
}
```

### 9.4 写自定义 Guard（类似 DoomLoop）

实现 `Guard` 函数签名 + `Add` 到 GuardBus 即可。比 DoomLoop 简单的比如"运行时长上限"、"金额上限"等。

### 9.5 移除钩子

```go
remove := bus.On(hook.HookPreStep, myHook)
// ... 某个时机：
remove()  // 幂等可重复调用
```

---

## 10. 一句话总结

**`agent/hook` 用「Hook 瀑布链（可改写）+ Guard 串行链（不可绕过）」双轨制实现 v2 纵深防御：6 个 HookKind 覆盖从模型输入到工具执行到终止裁决的全流程，HookBus/GuardBus 都提供幂等注册与快照执行，DoomLoopGuard 是 Guard 范式的典型实现 —— 把"安全防线不可绕过"这条不变量钉死在类型系统里（`Hook` 有 `next` 可短路，`Guard` 没有）。**