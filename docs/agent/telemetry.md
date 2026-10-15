# 显式遥测 Span 契约（agent/telemetry）

> 来源：`agent/telemetry/` 整个包 —— `telemetry.go`（接口 + Noop + WithSpan 便捷函数）+ `recorder.go`（内存参考实现）+ `conformance.go`（契约测试工具）。
> 目标读者：想接入 OpenTelemetry / 自研可观测平台的开发者；想给 agent 行为做端到端追踪的 SRE。

---

## 1. 一句话定位

**`agent/telemetry` 是 MoS 的"显式遥测 span 契约"**：定义两个最小接口（`TelemetryContext` + `TelemetrySpan`）让 SDK 内部能**显式随 ctx 传递** span 树，**没有全局 current-span、没有 exporter 依赖** —— 适配器（OpenTelemetry / 自研）由平台注入，`nil` 时 SDK 自动降级 Noop（**零开销**）。

源码顶端的注释（telemetry.go:1-4）一段话讲清定位：

> "Package telemetry 提供显式传递的 span 契约（架构 v2 §3.11，对齐 PI pi-telemetry）：**无全局 current-span、无 exporter 依赖**，适配器由平台注入（OpenTelemetry / 自研）。SDK 在驱动层自动打点（driver 回合 / llm 请求 / 工具执行），nil 时内部降级 no-op。"

文件分布：

| 文件 | 主题 |
|---|---|
| `telemetry.go` | `TelemetryContext` / `TelemetrySpan` 接口 + 3 个 Span 名常量 + Noop + `WithSpan` 便捷函数 |
| `recorder.go` | 内存版参考实现（SpanRecord + Recorder + 父子树管理） |
| `conformance.go` | `Conformance(t, tc)` 契约测试工具（平台适配器接入时复用） |

---

## 2. 整体协作位置

```
驱动层 / LLM / 工具执行点
   ↓ telemetry.WithSpan(ctx, rt.Telemetry, "driver.turn", attrs, func(ctx) error {
   │     // 业务逻辑，自动 End（err 自动标记）
   │ })
   ↓ StartSpan 返回 (TelemetrySpan, 派生 ctx)
   ├─ OpenTelemetry 实现 → otel.Tracer.Start(...)
   ├─ 自研实现 → 内存 Recorder / 远端 exporter
   └─ NoopTelemetry → NoopSpan（直接返回）
```

**关键设计**：**所有打点都走 ctx 传递**，不让业务代码知道"我在哪个 span 里"是全局变量 —— 这是显式契约与全局 current-span（OTel 默认）的关键区别。

---

## 3. 两个接口（telemetry.go:16-30）

### 3.1 `TelemetryContext`：span 工厂

```go
type TelemetryContext interface {
    StartSpan(ctx context.Context, name string, attrs map[string]string) (TelemetrySpan, context.Context)
}
```

**两个关键点**：

1. **返回派生 ctx** —— 子 span 必须基于派生 ctx 开启才能形成 span 树
2. **`attrs` nil 允许** —— 调用方可能无起始属性

### 3.2 `TelemetrySpan`：进行中的 span

```go
type TelemetrySpan interface {
    SetAttribute(k, v string)
    AddEvent(name string, attrs map[string]string)
    End(err error)
}
```

**三条契约**：

| 方法 | 契约 |
|---|---|
| `SetAttribute` | 设置/覆盖属性（**End 后调用不保证生效**，实现应容忍） |
| `AddEvent` | 记录 span 内事件（attrs nil 允许） |
| `End` | 结束 span；`err != nil` 时实现应标记为错误状态；**幂等** |

注释（telemetry.go:24-29）逐条强调：
> "实现应容忍" + "End 后调用不保证生效" + "幂等"

—— 给适配器实现充分的容错空间（驱动层不必担心重复 End 的崩溃）。

---

## 4. 三个 Span 名常量（telemetry.go:9-13）

```go
const (
    SpanDriverTurn = "driver.turn"    // attrs: turn / driver
    SpanLLMRequest = "llm.request"    // attrs: turn / step / stream
    SpanToolExec   = "tool.execute"   // attrs: tool / call_id / turn
)
```

**SDK 在这三处自动打点**：

| Span | 打点位置 | 关键属性 |
|---|---|---|
| `driver.turn` | driver.Run 每轮循环 | `turn`, `driver` |
| `llm.request` | llm.ChatModel.Generate/Stream 入口 | `turn`, `step`, `stream` |
| `tool.execute` | execpath.ExecuteBatch 单条工具执行 | `tool`, `call_id`, `turn` |

—— 这三个 span 形成完整的"一次任务"的轨迹树：

```
driver.turn（turn=1, driver=react）
  ├─ llm.request（turn=1, step=1, stream=true）
  └─ tool.execute（turn=1, tool=read_file, call_id=abc, turn=1）
       └─ tool.execute（turn=1, tool=write_file, call_id=def, turn=1）
```

---

## 5. `NoopTelemetry` 与 `NoopSpan`（telemetry.go:32-48）

```go
var (
    NoopTelemetry TelemetryContext = noopContext{}
    NoopSpan      TelemetrySpan    = noopSpan{}
)

type noopContext struct{}
func (noopContext) StartSpan(ctx context.Context, _ string, _ map[string]string) (TelemetrySpan, context.Context) {
    return NoopSpan, ctx
}

type noopSpan struct{}
func (noopSpan) SetAttribute(string, string)        {}
func (noopSpan) AddEvent(string, map[string]string) {}
func (noopSpan) End(error)                          {}
```

**三个不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **返回原 ctx** | `return NoopSpan, ctx` | 不污染 ctx |
| **所有方法空实现** | 方法体空 | 零开销 |
| **永不返回 nil** | 全局变量 `NoopSpan` | 调用方无脑 defer End |

注释（telemetry.go:32）：

> "无操作实现：应用未提供遥测时使用，**调用零开销、永不失败**"

—— 这是 MoS 的"零依赖"哲学在遥测层的体现 —— 平台未注 Telemetry 时驱动打点代码照常运行，无性能损耗。

---

## 6. `WithSpan` 便捷函数（telemetry.go:50-61）

```go
func WithSpan(ctx context.Context, tc TelemetryContext, name string, attrs map[string]string, fn func(ctx context.Context) error) error {
    if tc == nil {
        return fn(ctx)        // nil tc → 直接执行 fn（零成本）
    }
    span, sctx := tc.StartSpan(ctx, name, attrs)
    err := fn(sctx)
    span.End(err)
    return err
}
```

**三大特性**（注释 telemetry.go:50-51）：

1. **回调式生命周期** —— 开启 span → 执行 fn（ctx 为派生 ctx）→ 自动 End（fn 返回错误时 span 标记错误并透传）
2. **`tc == nil` 直接执行 fn** —— 无遥测环境零成本（连 StartSpan 都不调）
3. **err 自动传递** —— fn 返回 err 时 span 标记为错误（`End(err)`）

**典型用法**：

```go
func (d *ReActDriver) runTurnInner(ctx context.Context, ...) error {
    return telemetry.WithSpan(ctx, d.rt.Telemetry, telemetry.SpanDriverTurn,
        map[string]string{"turn": strconv.Itoa(turn), "driver": "react"},
        func(ctx context.Context) error {
            // 业务逻辑，子 span 基于 ctx 自动树化
            ...
        })
}
```

---

## 7. `Recorder` 内存参考实现（recorder.go）

### 7.1 三个数据结构

```go
type EventRecord struct {
    Name       string
    Attributes map[string]string
    Time       time.Time
}

type SpanRecord struct {
    Name       string
    Attributes map[string]string
    Events     []EventRecord
    Start      time.Time
    End        time.Time
    Err        error
    Parent     *SpanRecord          // 父 span（nil = 根）
    Children   []*SpanRecord        // 子 span 列表
}

type Recorder struct {
    mu    sync.Mutex
    roots []*SpanRecord             // 根 span 列表
}
```

**SpanRecord 是不可变快照**（recorder.go:16 注释）："已结束 span 的不可变快照（参考实现导出，供测试断言/本地调试）"

### 7.2 父子关系建立（recorder.go:42-54）

```go
func (r *Recorder) StartSpan(ctx context.Context, name string, attrs map[string]string) (TelemetrySpan, context.Context) {
    rec := &SpanRecord{
        Name:       name,
        Attributes: cloneAttrs(attrs),
        Start:      time.Now(),
    }
    parent, _ := ctx.Value(spanKey{}).(*SpanRecord)    // 从 ctx 取父 span
    if parent != nil {
        rec.Parent = parent                            // 记到 record
    }
    s := &recorderSpan{rec: r, record: rec}
    return s, context.WithValue(ctx, spanKey{}, rec)   // 自己作为 ctx 里的"父"传给子
}
```

**关键点**：

- **`spanKey{}`** 是 ctx 键（recorder.go:108-109），私有类型防止外部碰撞
- **子 span 通过 ctx 取父** —— 子开 span 时把 ctx 里的 SpanRecord 当 Parent 挂上
- **`cloneAttrs(attrs)`** —— 防御性拷贝，避免外部修改污染记录

### 7.3 End 时落库（recorder.go:92-106）

```go
func (s *recorderSpan) End(err error) {
    s.rec.mu.Lock()
    defer s.rec.mu.Unlock()
    if s.ended { return }                       // 幂等
    s.ended = true
    s.record.End = time.Now()
    s.record.Err = err
    if s.record.Parent == nil {
        s.rec.roots = append(s.rec.roots, s.record)        // 根 → roots
        return
    }
    s.record.Parent.Children = append(s.record.Parent.Children, s.record)  // 子 → 父.Children
}
```

**三个不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **未结束 span 不落库** | `End` 才进 roots / Children | 测试断言时只看完整树 |
| **End 幂等** | `if s.ended { return }` | 适配器可放心重复 End |
| **End 后方法不 panic** | `if s.ended { return }` 在每个写方法 | 满足 Conformance |

### 7.4 `Spans()` 快照返回（recorder.go:56-65）

```go
func (r *Recorder) Spans() []SpanRecord {
    r.mu.Lock()
    defer r.mu.Unlock()
    out := make([]SpanRecord, 0, len(r.roots))
    for _, root := range r.roots {
        out = append(out, *cloneRecord(root))    // 深拷贝（含 Children / Attributes / Events）
    }
    return out
}
```

**返回深拷贝**（recorder.go:122-141 `cloneRecord`）—— 调用方修改返回的 SpanRecord 不影响 Recorder 内部状态。**Attributes / Events 都逐项拷贝**。

### 7.5 并发安全

- **`r.mu`** 保护 roots 与每个 SpanRecord 的属性/事件/children 列表
- **每个写操作（SetAttribute / AddEvent / End）都加锁**
- **`Spans()` 加锁后深拷贝** —— 拷贝期间不影响其他写

注释（recorder.go:28-30）：

> "并发安全；**span 记录在 End 时落库（未结束的 span 不对外可见）**"

—— 这是设计上的"两阶段"：进行中 + 已落库。Spans() 只看到已落库部分。

---

## 8. `Conformance` 契约测试工具（conformance.go）

### 8.1 一句话定位

**`Conformance(t, tc)` 是平台适配器的"接口契约验证器"**：跑四个子测试，覆盖所有 TelemetryContext/TelemetrySpan 必须满足的不变量。**OpenTelemetry / 自研适配器接入后第一件事就是调它**。

注释（conformance.go:9-16）讲清四个子测试：

> "1. StartSpan 返回非 nil span 与派生 ctx；2. span 方法（SetAttribute / AddEvent / End）可安全调用（no-op 亦不 panic），End 幂等；3. WithSpan 回调恰好执行一次、错误透传、自动 End（End 后再调用方法不 panic）；4. 嵌套 span 基于派生 ctx 可正常开启（span 树）。"

### 8.2 四个子测试拆解

| 子测试 | 验证点 |
|---|---|
| `start-span` | StartSpan 返回非 nil span + 派生 ctx；子 span 可基于派生 ctx 开启 |
| `with-span-success` | WithSpan 回调恰好执行 1 次 + 错误返回 nil |
| `with-span-error` | WithSpan(fn err) → 透传 sentinel 错误（`errors.Is`） |
| `with-span-nil-tc` | WithSpan(nil tc) → 直接执行 fn 1 次，不报错 |

### 8.3 平台适配器用法

```go
// 平台 OpenTelemetry 适配器的测试文件
func TestOTelAdapter(t *testing.T) {
    adapter := myOTelAdapter{...}
    telemetry.Conformance(t, adapter)        // ← 复用契约验证
    
    // 自己再加 OTel 特定的快照断言
    ...
}
```

### 8.4 SDK 自带的 `TestConformance`

```go
func TestConformance(t *testing.T) {
    t.Run("noop", func(t *testing.T) { Conformance(t, NoopTelemetry) })
    t.Run("recorder", func(t *testing.T) { Conformance(t, NewRecorder()) })
}
```

—— SDK 验证 Noop + Recorder 都通过契约。**Conformance 既验证 SDK 实现也作为平台实现的契约样板**。

---

## 9. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **无全局 current-span** | 接口契约（仅 ctx） | 显式传递，无副作用 |
| **End 幂等** | recorderSpan.ended 标志 | 重复 End 安全 |
| **End 后方法不 panic** | 各方法检查 ended | 满足 Conformance |
| **`tc == nil` 零成本** | WithSpan 早返回 | 无遥测零开销 |
| **Noop 永不返回 nil** | 全局变量 | 调用方无脑 defer |
| **子 span 基于派生 ctx** | TelemetryContext 契约 | span 树 |
| **未结束 span 不对外可见** | Recorder.roots 在 End 时填充 | 测试只看到完整树 |
| **Spans() 深拷贝返回** | cloneRecord | 防外部修改污染 |
| **`attrs == nil` 允许** | 各方法 nil 检查 | 调用方灵活 |
| **三个 Span 名是 SDK 契约** | SpanDriverTurn / SpanLLMRequest / SpanToolExec | 平台可按名聚合 |

---

## 10. 与 PI / OTel 的对齐

| 维度 | MoS telemetry | PI pi-telemetry | OpenTelemetry |
|---|---|---|---|
| span 形态 | TelemetrySpan（显式接口） | TelemetrySpan（同源） | Tracer/Trace |
| 父 span 传递 | ctx.Value(spanKey) | ctx | ctx |
| 全局 current | **无** | 无 | 有（otel.Tracer 默认） |
| exporter | 无 | 无 | 有（OTLP） |
| 适配器 | 平台注入 | 平台注入 | 内置 OTel SDK |
| Conformance | 内置 Conformance(t, tc) | 内置 | 无标准 |

**MoS 选择"显式接口 + 平台注入"**：让任何可观测系统（OTel / Jaeger / Zipkin / 自研）都能 30 行适配器接入，且 SDK 自身零依赖。

---

## 11. 自定义扩展姿势

### 11.1 OpenTelemetry 适配器（典型生产）

```go
type OTelAdapter struct {
    tracer trace.Tracer
}

func (o *OTelAdapter) StartSpan(ctx context.Context, name string, attrs map[string]string) (telemetry.TelemetrySpan, context.Context) {
    ctx, span := o.tracer.Start(ctx, name, trace.WithAttributes(toAttrs(attrs)...))
    return &otelSpan{span: span}, ctx
}

type otelSpan struct{ span trace.Span }

func (s *otelSpan) SetAttribute(k, v string) { s.span.SetAttributes(attribute.String(k, v)) }
func (s *otelSpan) AddEvent(name string, attrs map[string]string) {
    s.span.AddEvent(name, trace.WithAttributes(toAttrs(attrs)...))
}
func (s *otelSpan) End(err error) {
    if err != nil { s.span.RecordError(err) }
    s.span.End()
}

// 测试：复用契约验证
func TestOTelAdapter(t *testing.T) {
    adapter := &OTelAdapter{tracer: otel.Tracer("test")}
    telemetry.Conformance(t, adapter)
}
```

### 11.2 本地调试：用 Recorder

```go
rec := telemetry.NewRecorder()
agent, _ := assistant.New(model, tools, assistant.Config{
    // ... 业务配置
    // 不通过 Config 注 Telemetry（Config 暂无该字段，参考 RunContext 注入路径）
})

// 跑业务
agent.Run(ctx, sess, input")

// 断言：所有 span
for _, root := range rec.Spans() {
    t.Logf("root span: %s, err: %v, children: %d", root.Name, root.Err, len(root.Children))
    for _, c := range root.Children {
        t.Logf("  child: %s, err: %v", c.Name, c.Err)
    }
}
```

### 11.3 自研 exporter

```go
type MyExporter struct{ mu sync.Mutex; sink []SpanRecord }

func (m *MyExporter) Export(rec telemetry.SpanRecord) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.sink = append(m.sink, rec)
}

type MyAdapter struct{ exporter *MyExporter }

func (a *MyAdapter) StartSpan(ctx context.Context, name string, attrs map[string]string) (telemetry.TelemetrySpan, context.Context) {
    // ... 与 Recorder 类似但 End 时调 exporter.Export
}
```

### 11.4 在驱动层手动打点（自定义场景）

```go
func (d *ReActDriver) customStep(ctx context.Context, ...) error {
    return telemetry.WithSpan(ctx, d.rt.Telemetry, "my.custom.step",
        map[string]string{"foo": "bar"},
        func(ctx context.Context) error {
            // 业务逻辑
            return doStuff(ctx)
        })
}
```

—— `WithSpan` 接管整个 span 生命周期，开发者只关心 fn 内部。

---

## 12. 典型 span 轨迹示例

一次 `react` 驱动的两轮任务：

```
driver.turn (turn=1, driver=react)
├─ llm.request (turn=1, step=1, stream=true)
│   events: [first_token, last_token]
└─ tool.execute (tool=read_file, call_id=abc, turn=1)
    └─ （无子 span）

driver.turn (turn=2, driver=react)
├─ llm.request (turn=2, step=1, stream=true)
└─ tool.execute (tool=write_file, call_id=def, turn=2)
    events: [policy_require (Approval)]
    └─ （*approval.RequiredError 触发，挂起 — span End(err)）
```

**平台拿到这个轨迹能做什么**：

- **性能**：每轮平均耗时、LLM 等待占比、工具执行时间
- **错误**：哪些工具常超时、哪些 step 常触发 Approval
- **链路追踪**：Jaeger/Zipkin 关联 span 与请求 ID
- **计费**：按 turn / step 数 × token 计费

---

## 13. 一句话总结

**`agent/telemetry` 是 MoS 的"显式遥测 span 契约"：两个最小接口（TelemetryContext + TelemetrySpan）+ Noop + Recorder 参考实现 + WithSpan 便捷函数 + Conformance 契约测试工具，零依赖、全显式（无全局 current-span）、nil 降级零成本（不调 StartSpan）、三个标准 span 名（driver.turn / llm.request / tool.execute）让 SDK 自动打点。平台用 30 行适配器接入 OpenTelemetry / 自研可观测系统，Conformance(t, tc) 是适配器的"出厂验证"。** 是 v2 §3.11 "显式传递 span" 哲学的最朴素落地。