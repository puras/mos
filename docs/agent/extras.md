# Agent 基础设施三件套（executor / seam / router）

> 来源：`agent/executor/executor.go`（并发执行原语） + `agent/seam/seam.go`（组件注册表） + `agent/router/router.go`（任务路由）。
> 目标读者：理解"agent 子系统的零碎基础设施"的所有人；想自定义驱动 / 组件注册 / 路由策略的开发者。

---

## 一、本卷范围

这是 Task #9 合并文档的最后一批。三个包都是"被多处复用、单一职责、几十到一百多行"的小包：

| 包 | 主题 | 复用方 |
|---|---|---|
| `executor` | 有界并发批量执行原语（generic） | execpath |
| `seam` | 组件注册表（kind + name → factory） | config |
| `router` | 任务路由（规则优先 + LLM 分类 + fallback） | 平台调度 |

它们都遵循"语义中立、零上层依赖"的设计原则 —— 不感知 driver/hook/session 的具体语义。

---

# Part 1: `agent/executor` —— 有界并发批量执行原语

## 1.1 一句话定位

**`executor` 是 MoS 唯一的"并发批量执行器"**：用泛型 + 三阶段模型（preflight 串行决策 → 有界并发执行 → 按输入顺序回填）提供一个**与具体业务语义无关**的并发原语，**所有需要"批量处理多个独立任务"的代码都应该用它**。

源码顶端的注释（executor.go:1-4）一段话讲清定位：

> "有界并发批量执行原语（架构 v2 §3.9 并发执行语义）：三阶段模型（preflight 串行决策 → 有界并发执行 → 按输入顺序回填）。**语义中立：不感知工具/钩子/日志，由调用方闭包注入，保持零上层依赖**"

文件只有一个 `executor.go`，约 100 行：

| 元素 | 行号 | 职责 |
|---|---|---|
| `DefaultMaxParallel` | 12 | 默认并发度 4 |
| `Item[T]` | 16-19 | 批量条目（含 Sequential 标记） |
| `Preflight[T]` | 23 | preflight 决策函数签名 |
| `Execute[T, R]` | 26 | execute 执行函数签名 |
| `Outcome[T, R]` | 29-34 | 单条结果 |
| `Run[T, R]` | 46-115 | 三阶段执行核心 |

## 1.2 三个泛型类型 + 两个签名

```go
type Item[T any] struct {
    Input      T
    Sequential bool
}

type Preflight[T any] func(ctx context.Context, input T) (allow bool, err error)
type Execute[T any, R any] func(ctx context.Context, input T) (R, error)

type Outcome[T any, R any] struct {
    Input  T
    Output R
    Err    error
    Denied bool  // preflight 拦截：未执行，显式失败回填
}
```

**泛型设计的好处**：

| 包 | T 类型 | R 类型 |
|---|---|---|
| `agent/execpath` 调用 | `*llm.ToolCall` | `ToolOutcome` |
| 业务自用（假想） | `string` (URL) | `[]byte` (内容) |
| 等等 | 任意 | 任意 |

execpath 是它**唯一的内置使用方**（executor.go:46 + execpath.go:112），但任何想"批量处理"的人都能直接用它。

## 1.3 `Run` 三阶段实现（executor.go:46-115）

### 阶段 1: preflight（串行决策）

```go
for i, it := range items {
    if err := ctx.Err(); err != nil {
        entries[i].err = err      // ctx 已取消 → 标记但不进 denied
        continue
    }
    allow, err := preflight(ctx, it.Input)
    if !allow || err != nil {
        entries[i].err = err
        entries[i].denied = true  // 拦截标记
        continue
    }
    allowed = append(allowed, i)  // 放行索引
}
```

**两条不变量**：

1. **串行执行** —— preflight 一律串行，保证决策一致性（不能让两个 Policy 检查同时见到不一致状态）
2. **ctx 取消传播** —— 已取消的项 `entries[i].err = ctx.Err()` 但**不标记 denied**（因为没真的"被决策拒绝"，是系统取消）

### 阶段 2: execute（有界并发 / 串行回退）

```go
if sequential {
    for _, i := range allowed {
        entries[i].output, entries[i].err = execute(ctx, items[i].Input)
    }
} else {
    sem := make(chan struct{}, maxParallel)
    var wg sync.WaitGroup
    for _, i := range allowed {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            select {
            case sem <- struct{}{}:
                defer func() { <-sem }()
            case <-ctx.Done():
                entries[idx].err = ctx.Err()
                return
            }
            entries[idx].output, entries[idx].err = execute(ctx, items[idx].Input)
        }(i)
    }
    wg.Wait()
}
```

**三个反直觉点**：

1. **`sequential` 触发条件**（executor.go:53-59）：
   ```go
   sequential := maxParallel <= 1
   for i := range items {
       if items[i].Sequential {
           sequential = true
           break
       }
   }
   ```
   即：只要任一条目声明 `Sequential=true` 或 `maxParallel<=1`，**整批**回退串行。这是有意为之 —— 并发集合里混一个 sequential 项会让同步变复杂。

2. **信号量模式** —— `sem := make(chan struct{}, maxParallel)` + `sem <- struct{}{}` 阻塞式获取 + `defer <-sem` 释放。这是 Go 里**最简洁的有界并发实现**。

3. **ctx 取消对正在执行项的影响** —— `select { case sem <- ...: case <-ctx.Done(): }` 让**未拿到信号量**的项立刻 ctx.Err()，但**已拿到信号量正在执行**的项不会被强制取消（要等 execute 函数自己响应 ctx）。

注释（executor.go:42-43）：

> "ctx 取消传播：**尚未开始**（preflight 未执行）的条目直接回填 ctx.Err()；**已启动**的执行随 ctx 取消终止。"

### 阶段 3: finalize（按输入顺序回填）

```go
outs := make([]Outcome[T, R], len(items))
for i, it := range items {
    outs[i] = Outcome[T, R]{Input: it.Input, Output: entries[i].output, Err: entries[i].err, Denied: entries[i].denied}
}
```

**关键不变量**：`outs[i].Input == items[i].Input`，**顺序一致**。调用方可以放心 `for i, out := range outs` 与 `for i, call := range calls` 一一对应。

注释（executor.go:29）：

> "Outcome 是单个条目的最终结果，按输入顺序回填（与输入等长同序）"

## 1.4 与 execpath 的衔接

`execpath.ExecuteBatch` 把 executor 当成"机制"用，把自己的策略（preflight = GuardBus + Denied/SuspendError 三态分类、execute = executeTool 钩子链）当成"策略"包进去：

```go
// execpath.go:112
outs := executor.Run(bctx, items, rt.MaxToolParallel, preflight, execute)
```

**executor 是机制，execpath 是策略** —— 这是 v2 §3.9 的清晰分工。

## 1.5 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **preflight 串行** | 阶段 1 for 循环 | 决策一致性 |
| **顺序回填** | 阶段 3 | 与输入等长同序 |
| **顺序保证 Sequential 整批串行** | sequential 标志 + 任一 Sequential 即触发 | 避免混合并发复杂度 |
| **ctx 取消只影响未启动项** | `select { case <-ctx.Done(): }` | 已启动的执行由 execute 自己响应 |
| **Denied 与 Err 区分** | Denied=true + Err 非 nil | preflight 拦截 vs 运行错误 |
| **零上层依赖** | 只 import "context" / "sync" | 任何批量场景都能用 |

## 1.6 自定义扩展姿势

### 用 executor 批量下载

```go
type download struct{ URL string }
type result struct{ Body []byte }

items := make([]executor.Item[download], len(urls))
for i, u := range urls {
    items[i] = executor.Item[download]{Input: download{URL: u}}
}

preflight := func(ctx context.Context, d download) (bool, error) {
    return !isBlocked(d.URL), nil  // 黑名单
}
execute := func(ctx context.Context, d download) (result, error) {
    resp, err := http.Get(d.URL)
    if err != nil { return result{}, err }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    return result{Body: body}, nil
}

outs := executor.Run(ctx, items, 8, preflight, execute)
for i, out := range outs {
    if out.Denied { log.Printf("%s blocked", items[i].Input.URL); continue }
    if out.Err != nil { log.Printf("%s failed: %v", items[i].Input.URL, out.Err); continue }
    save(items[i].Input.URL, out.Output.Body)
}
```

---

# Part 2: `agent/seam` —— 组件注册表

## 2.1 一句话定位

**`seam` 是 MoS 的"组件注册表"**：用 `(kind, name) → Factory` 三元组做装配期查找，让"按名解析组件"成为可能（config 包就是用它）。**注册是装配期行为**，不是运行期热加载（注释明确点出"Go SDK 惯用法：编译期注册 + 运行期选择"）。

源码顶端的注释（seam.go:1-7）一段话讲清定位：

> "组件注册表 SeamRegistry（架构 v2 §3.5，对齐 dsh Capability Seam）：组件按 (kind, name) 注册为工厂，装配层按名解析。注册/解析是'装配时'行为，不引入运行期插件热加载。**工厂返回 any，消费端按接口断言——注册表自身不依赖任何上层类型，保持基础设施层无反向依赖**"

文件只有一个 `seam.go`，约 100 行。

## 2.2 数据结构

```go
type SeamRegistry struct {
    mu        sync.RWMutex
    providers map[Kind]map[string]Factory
}

type Factory func() any
```

**关键设计**：

- **`Factory func() any`** —— 返回 `any`，调用方按接口断言（`v.(llm.ChatModel)` 等）。**注册表自身不依赖任何上层类型**，避免 `agent/seam → llm → agent/seam` 依赖环。
- **`map[Kind]map[string]Factory`** —— 两级 map，第一级按 Kind 分类，第二级按 Name 索引
- **`sync.RWMutex`** —— 读多写少（Resolve 用 RLock），适合注册期一次写 + 装配期多次读

## 2.3 13 种 Kind（seam.go:19-37）

```go
const (
    KindDriver    Kind = "driver"
    KindPolicy    Kind = "policy"
    KindModel     Kind = "model"
    KindTool      Kind = "tool"
    KindSandbox   Kind = "sandbox"
    KindMemory    Kind = "memory"
    KindRetriever Kind = "retriever"
    KindAuditor   Kind = "auditor"
    KindTruncator Kind = "truncator"
    KindCompactor Kind = "compactor"
    KindPlanner   Kind = "planner"
    KindReflector Kind = "reflector"

    // 文件系统 capability —— 平台可注册自定义 FileSystem 实现
    KindFileSystem Kind = "filesystem"
)
```

注释（seam.go:34-36）：

> "平台可注册自定义 FileSystem 实现（如远程/对象存储后端）；**内置装配走 assistant.buildFileSystem 直挂 RunContext.FS，不经本注册表**"

也就是说：`KindFileSystem` 是给自定义实现用的，**内置 fs 装配不经过这里** —— 这是个有意思的"双重路径"设计。

## 2.4 四个核心方法

### `Register`（seam.go:56-68）

```go
func (s *SeamRegistry) Register(kind Kind, name string, f Factory) {
    if s == nil { return }
    s.mu.Lock()
    defer s.mu.Unlock()
    byName := s.providers[kind]
    if byName == nil {
        byName = make(map[string]Factory)
        s.providers[kind] = byName
    }
    byName[name] = f     // 后注册覆盖先注册
}
```

**关键设计**：后注册覆盖先注册（注释 seam.go:45）—— 让平台可以用同一个名字"覆盖"默认实现。

### `Resolve`（seam.go:71-82）

```go
func (s *SeamRegistry) Resolve(kind Kind, name string) (any, error) {
    if s == nil {
        return nil, fmt.Errorf("seam: nil registry")
    }
    s.mu.RLock()
    f, ok := s.providers[kind][name]
    s.mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("seam: %s provider %q not registered", kind, name)
    }
    return f(), nil
}
```

**注意**：Resolve 每次**都调 Factory**（注释 seam.go:39）：

> "工厂返回 any，每次解析调用工厂，**产出独立实例**（有状态组件不共享）"

也就是说：两次 `Resolve(KindModel, "claude")` 拿到的是**两个独立实例** —— 这对 Model/Planner 之类的有状态组件很重要。

### `Has`（seam.go:85-93）

判别 (kind, name) 是否已注册。RLock 保护，无错误返回。

### `Names`（seam.go:96-108）

返回某 Kind 下所有已注册名字（**排序稳定**）：

```go
sort.Strings(names)   // 注释："便于枚举"
```

注释（seam.go:95）：

> "排序稳定，便于枚举"

—— 平台 UI 列组件清单时需要稳定顺序。

## 2.5 与 config 包的衔接

config.AssembleConfig 6 个 resolve* 函数**全部走 SeamRegistry.Resolve**：

```go
// config.go:137-145
func resolveModel(reg *seam.SeamRegistry, name string) (llm.ChatModel, error) {
    v, err := reg.Resolve(seam.KindModel, name)
    if err != nil { return nil, fmt.Errorf("config: model: %w", err) }
    m, ok := v.(llm.ChatModel)
    if !ok { return nil, fmt.Errorf("config: model provider %q is not llm.ChatModel (%T)", name, v) }
    return m, nil
}
```

**两层错误**：

| 层级 | 错误信息 |
|---|---|
| SeamRegistry 层 | `seam: model provider "x" not registered` |
| Config 层 | `config: model: seam: ...` （wrap 上一层） |

—— `fmt.Errorf("%w")` 链让上层 `errors.Is` 能追溯到底。

## 2.6 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **零上层依赖** | `Factory func() any` | 不引入反向依赖 |
| **同名覆盖** | `byName[name] = f` | 平台可覆盖默认实现 |
| **每次 Resolve 出新实例** | `return f()` | 有状态组件不共享 |
| **并发安全** | sync.RWMutex | 注册期写 + 装配期读 |
| **Names 排序** | `sort.Strings` | UI 列清单稳定 |
| **nil registry 安全** | 早返回错误 | 装配友好 |

## 2.7 自定义扩展姿势

### 注册自定义模型

```go
reg := seam.NewSeamRegistry()
seam.Register(reg, seam.KindModel, "claude", func() any {
    return anthropic.NewChatModel(apiKey)  // 返回 llm.ChatModel 实现
})
```

### 注册自定义工具

```go
seam.Register(reg, seam.KindTool, "my_search", func() any {
    return myCustomSearchTool{}  // 返回 tool.Tool 实现
})
```

### 注册策略

```go
seam.Register(reg, seam.KindPolicy, "restrictive", func() any {
    return assistant.NewDefaultPolicy(allowList, workspace)
})
```

### 列出已注册组件（UI 用）

```go
toolNames := reg.Names(seam.KindTool)
// ["builtin", "my_search"]  （排序后）
```

### 覆盖默认实现

```go
// 默认 react
seam.Register(reg, seam.KindDriver, "react", func() any { return &MyCustomReact{} })
// 任何装配都会用 MyCustomReact 而不是默认 react
```

---

# Part 3: `agent/router` —— 任务路由

## 3.1 一句话定位

**`router` 是 MoS 的"任务路由器"**：按用户输入**选择 LoopDriver**，让平台能用一个入口服务所有类型的智能体任务。三层兜底：**规则硬路由（确定性/可审计） → LLM 分类器（模糊输入） → fallback（默认驱动）**。

源码顶端的注释（router.go:1-4）一段话讲清定位：

> "提供任务路由：按输入选择 LoopDriver（架构 v2 §3.6）。**HybridRouter 规则（确定性/可审计）优先 + LLM 分类兜底（模糊输入）**，返回的驱动为裸实例：Run 时经 RunContext 惰性完成默认装配"

文件只有一个 `router.go`，约 120 行。

## 3.2 三种角色 + 两个接口

```go
type DriverRouter interface {
    Select(ctx context.Context, input string, sess *session.Session) (driver.LoopDriver, error)
}

type DriverFactory func() driver.LoopDriver

type Rule struct {
    Keyword string
    Driver  string
}
```

**DriverRouter** 是平台对外的接口；**DriverFactory** 是注入驱动的钩子；**Rule** 是确定性路由规则。

注释（router.go:25-27）：

> "Router 每次 Select 经工厂产出独立实例，**避免共享实例的并发写**（如 PlanExecuteDriver 惰性装配 executor）"

## 3.3 `HybridRouter` 结构（router.go:39-44）

```go
type HybridRouter struct {
    drivers    map[string]DriverFactory
    rules      []Rule
    classifier planner.Classifier
    fallback   string
}
```

四个字段：

| 字段 | 作用 |
|---|---|
| `drivers` | 驱动名 → 工厂 |
| `rules` | 确定性规则列表（**按注册顺序匹配**） |
| `classifier` | LLM 分类器（可选） |
| `fallback` | 兜底驱动名 |

## 3.4 Functional Options 配置（router.go:47-74）

四个 Option 函数：

```go
WithDriver(name string, d driver.LoopDriver) Option       // 注册驱动实例
WithFactory(name string, f DriverFactory) Option          // 注册工厂（每次新建）
WithRule(keyword, driverName string) Option               // 追加确定性规则
WithClassifier(c planner.Classifier) Option               // 注入 LLM 分类器
WithFallback(name string) Option                          // 兜底驱动名
```

注释（router.go:59）：

> "WithRule 追加确定性规则（**按注册顺序匹配，先命中先选**）"

—— 顺序敏感，先注册的规则优先。

## 3.5 `NewHybrid` 默认装配（router.go:78-95）

```go
func NewHybrid(opts ...Option) *HybridRouter {
    r := &HybridRouter{
        drivers: map[string]DriverFactory{
            planner.DriverReact:      func() driver.LoopDriver { return &react.ReActDriver{} },
            planner.DriverPlanExec:   func() driver.LoopDriver { return &planexecute.PlanExecuteDriver{} },
            planner.DriverReflection: func() driver.LoopDriver { return &reflection.ReflectionDriver{} },
            planner.DriverScripted:   func() driver.LoopDriver { return &scripted.ScriptedDriver{} },
        },
        rules: []Rule{
            {Keyword: "先规划", Driver: planner.DriverPlanExec},
        },
        fallback: planner.DriverReact,
    }
    for _, o := range opts { o(r) }
    return r
}
```

**默认装配**：

| 驱动 | 工厂 |
|---|---|
| `react` | `&react.ReActDriver{}` |
| `plan-execute` | `&planexecute.PlanExecuteDriver{}` |
| `reflection` | `&reflection.ReflectionDriver{}` |
| `scripted` | `&scripted.ScriptedDriver{}` |

**预置规则**：`"先规划"` 关键词 → `plan-execute`。

**fallback**：react。

业务侧可以用 Option 覆盖任意字段。

## 3.6 `Select` 三层兜底（router.go:98-111）

```go
func (r *HybridRouter) Select(ctx context.Context, input string, sess *session.Session) (driver.LoopDriver, error) {
    lower := strings.ToLower(input)
    for _, rule := range r.rules {
        if strings.Contains(lower, strings.ToLower(rule.Keyword)) {
            return r.build(rule.Driver)
        }
    }
    if r.classifier != nil {
        if name, err := r.classifier.Classify(ctx, input, session.ConvertToLlm(sess.Messages())); err == nil && name != "" {
            return r.build(name)
        }
    }
    return r.build(r.fallback)
}
```

**三层流程**：

1. **规则匹配**（确定性）—— 大小写不敏感，关键词子串命中
2. **LLM 分类**（模糊）—— 调 classifier.Classify，传完整消息历史（通过 `session.ConvertToLlm` 转 `llm.Message`）
3. **fallback** —— `r.build(r.fallback)`

注释（router.go:105）：

> "LLM 分类器（模糊输入）：未命中规则时按意图分类"

## 3.7 与 session 的协作

`Select` 接收 `sess *session.Session` 参数，让 LLM 分类器能看到**完整消息历史**（不只当前 input）：

```go
session.ConvertToLlm(sess.Messages())  // 内部消息 → llm 标准消息
```

这是为了**多轮上下文** —— 用户在第二轮说"接着刚才的来"，classifier 能根据历史判断该用哪个驱动。

## 3.8 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **规则优先（确定性可审计）** | for 循环先跑 | 可解释性 |
| **LLM 分类兜底（模糊）** | classifier.Classify | 自然语言输入 |
| **fallback 三层兜底** | r.build(r.fallback) | 总能选出驱动 |
| **每次 Select 出新实例** | DriverFactory 模式 | 避免并发写 |
| **关键词大小写不敏感** | ToLower | 用户友好 |
| **驱动名来自 planner.Driver*** | NewHybrid 常量引用 | 跨包一致 |

## 3.9 自定义扩展姿势

### 注册自定义驱动

```go
r := router.NewHybrid(
    router.WithDriver("my-custom", &MyCustomDriver{}),
)
```

### 添加更多规则

```go
r := router.NewHybrid(
    router.WithRule("脚本", planner.DriverScripted),
    router.WithRule("反思", planner.DriverReflection),
    router.WithRule("审查", planner.DriverReflection),
)
```

### 注入 LLM 分类器

```go
r := router.NewHybrid(
    router.WithClassifier(myClassifier{model: chatModel}),
)
```

### 平台调度：单入口多驱动

```go
// 平台侧
r := router.NewHybrid()

// 用户输入
input := "先规划一下这个任务"
driver, _ := r.Select(ctx, input, sess)
agent := driver.New(rt)  // 裸实例 → 装配
reply, err := agent.Run(ctx, sess, input)
```

### 自定义兜底

```go
r := router.NewHybrid(
    router.WithFallback(planner.DriverReflection),  // 用 reflection 而不是 react 兜底
)
```

---

# 三包对照

| 维度 | executor | seam | router |
|---|---|---|---|
| 主题 | 并发执行原语 | 组件注册表 | 任务路由 |
| 行数 | ~100 | ~100 | ~120 |
| 核心抽象 | `Run[T, R]` | `SeamRegistry.Register/Resolve` | `HybridRouter.Select` |
| 复用方 | execpath | config | 平台调度 |
| 是否泛型 | ✅ | ❌ (用 any) | ❌ |
| 是否有 nil-safe | 否 | ✅ `s == nil` | 否 |
| 是否有错误返回 | 否（Err 进 Outcome） | ✅ | ✅ |
| 装配期 vs 运行期 | 运行期（每次 Run） | **装配期** | 运行期（每次 Select） |

---

# 一句话总结

**`executor` 是机制（怎么并发跑）、`seam` 是查找（按名找组件）、`router` 是调度（按输入选驱动）—— 三个包共同组成"agent 子系统的零碎基础设施"：executor 让 execpath 的三阶段模型有了干净的并发原语，seam 让 config 的 YAML 装配有了按名解析能力，router 让平台能用一个入口服务所有类型的智能体任务。它们都遵循"语义中立、零上层依赖"的共同原则，是 v2 §3.5 / §3.6 / §3.9 的最小落地。**
