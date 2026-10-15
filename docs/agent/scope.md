# 资源作用域（agent/scope）

> 来源：`agent/scope/scope.go`（架构 v2 §3.7，对齐 Cordis "registrations are reversible effects"）。
> 目标读者：理解"Agent 怎么管理临时资源生命周期"的工程师；想给自定义组件做自动清理的开发者。

---

## 1. 一句话定位

**`agent/scope` 是 per-agent/per-session 的"资源回收容器"**：把"资源注册"和"资源清理"配对，**Close 时按注册逆序自动回滚**（LIFO），单注册项可单独撤销（On 返回值），已关闭后注册立即执行。Agent 生命周期管理（`agent/handle.Dispose`）就靠它。

源码顶端的注释（scope.go:1-4）一段话讲清定位：

> "提供 per-agent/per-session 注册作用域（架构 v2 §3.7）：注册到作用域的钩子/工具/覆盖在生命周期结束时按注册逆序自动回滚（**对齐 Cordis 'registrations are reversible effects'**）"

文件只有一个 `scope.go`，70 行，**整个包就是 `Scope` 一个类型 + `On` + `Close` 两个方法**。

---

## 2. 核心数据结构

```go
type Scope struct {
    mu        sync.Mutex
    disposers []func()
    closed    bool
}
```

**三个字段，缺一不可**：

| 字段 | 作用 |
|---|---|
| `mu` | 并发安全 |
| `disposers` | 按注册顺序累积的回收回调 |
| `closed` | 关闭后注册的早期 return 判据 |

**没有错误返回，没有复杂状态机** —— 整个包的设计哲学就是"简单到极致"。

---

## 3. `On` 注册（scope.go:23-47）

```go
func (s *Scope) On(dispose func()) func() {
    if s == nil || dispose == nil {
        return func() {}                         // nil scope / nil dispose → no-op 撤销器
    }
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.closed {
        dispose()                                // 已关闭 → 立即执行
        return func() {}
    }
    s.disposers = append(s.disposers, dispose)
    i := len(s.disposers) - 1
    var once sync.Once
    return func() {
        once.Do(func() {
            s.mu.Lock()
            defer s.mu.Unlock()
            if i >= len(s.disposers) || s.disposers[i] == nil {
                return                          // 已被撤销 / 已 Close
            }
            s.disposers[i] = nil                // 标记撤销（不删除，保留索引稳定）
        })
    }
}
```

### 3.1 三条不变量

1. **`s == nil` / `dispose == nil` 安全** —— 返回 no-op 撤销器，调用方无脑 defer
2. **已关闭后注册立即执行** —— 防止"注册了但 Close 已过"导致资源泄露
3. **返回的撤销器幂等** —— `sync.Once` 包裹，多次调用只生效一次

### 3.2 「标记 nil」而非「切片删除」

撤销单个注册项时**不是** `s.disposers = append(s.disposers[:i], s.disposers[i+1:]...)`，而是 `s.disposers[i] = nil`：

| 方案 | 撤销复杂度 | Close 复杂度 | 索引稳定 |
|---|---|---|---|
| 切片删除 | O(n) | O(n) | ❌ 已撤销项后面的索引会变 |
| **标记 nil** | **O(1)** | O(n)（要 nil 跳过） | ✅ |

**索引稳定的好处**：撤销函数捕获的是 `i := len(s.disposers) - 1`，如果后续再注册/撤销别的项，这个 `i` 不会变 —— 闭包捕获稳定。

**Close 时的 nil 跳过**（scope.go:64-66）：

```go
for i := len(disposers) - 1; i >= 0; i-- {
    if disposers[i] != nil {
        disposers[i]()
    }
}
```

### 3.3 已关闭后立即执行

```go
if s.closed {
    dispose()
    return func() {}
}
```

注释（scope.go:30-31）：

> "作用域已关闭：立即执行并返回 no-op 撤销器"

这是关键设计：**业务代码 `defer scope.On(cleanup)` 时不关心 scope 状态，如果已经关闭就立刻清理资源**。避免"注册了但 Close 已过"导致资源泄露。

---

## 4. `Close` 关闭（scope.go:50-69）

```go
func (s *Scope) Close() {
    if s == nil { return }
    s.mu.Lock()
    if s.closed {
        s.mu.Unlock()
        return                                  // 幂等
    }
    s.closed = true
    disposers := s.disposers
    s.disposers = nil                           // 释放引用
    s.mu.Unlock()

    for i := len(disposers) - 1; i >= 0; i-- {  // 逆序
        if disposers[i] != nil {
            disposers[i]()
        }
    }
}
```

### 4.1 三步走

1. **检查幂等** —— `s.closed` 已置则 return
2. **快照 + 释放引用** —— 把 `disposers` 置 nil，Close 内回调时锁外执行不会死锁
3. **逆序执行** —— `for i := len-1; i >= 0; i--`，**LIFO**（后注册先清理）

### 4.2 锁外执行

注释（scope.go:60-61）解释：

> "快照 disposers + 释放引用后立即 unlock"

**为什么不持锁执行回调？** 防止回调内又调 `On` 注册新资源导致死锁。

### 4.3 LIFO 语义

注释（scope.go:3）：

> "**注册到作用域的钩子/工具/覆盖在生命周期结束时按注册逆序自动回滚**"

**为什么是逆序？** 资源依赖关系 —— 后注册的资源往往依赖先注册的（比如：先注册 FS 后注册工具，关闭时应该先关工具再关 FS）。

### 4.4 nil-safe

`s == nil` 时 Close 直接返回，让调用方无脑调 `defer scope.Close()`。

---

## 5. 在 AgentHandle.Dispose 中的用法

源码（handle.go:98-100）：

```go
if h.Scope != nil {
    h.Scope.Close()
}
```

整个 AgentHandle 的资源清理流程：

```
AgentHandle.Dispose(ctx)
   │
   ├─ h.disposed = true                              // 幂等保护
   │
   ├─ h.Scope.Close()                                 // ← 关键：Scope 逆序回滚
   │     │
   │     └─ 比如注册过：
   │           - Hook 移除函数（按 hook.On 返回值）
   │           - Bus 监听器移除（按 bus.On 返回值）
   │           - 文件系统清理（fs capability dispose）
   │           - HTTP 连接关闭
   │           - 等等
   │
   ├─ h.RT.Notify(EventDisposed)
   └─ sess.Append(SessionEventAgentDisposed)
```

**这就是 scope 存在的全部理由**：让 AgentHandle 不用关心"我注册了什么资源"，调一个 `Scope.Close()` 全部清理。

---

## 6. 设计哲学：Cordis 的"registrations are reversible effects"

注释明确提到对齐 **Cordis**（一个 Node.js 框架）：

> "对齐 Cordis 'registrations are reversible effects'"

**核心思想**：

| 概念 | 含义 |
|---|---|
| Registration | 注册 = 副作用（添加钩子、订阅事件、分配资源） |
| Reversible | 每次注册都伴随"撤销函数" |
| Scope | 把一组相关的 registration 绑在一起，统一撤销 |
| Effect | 注册和撤销是镜像配对的 |

**应用到 MoS**：

- 注册钩子到 `HookBus.On(...)` 时返回的移除函数 → 包成 `scope.On(dispose)` 的 dispose
- 注册监听器到 `EventBus.On(...)` 时返回的移除函数 → 同样包成 `scope.On(dispose)`
- 创建 HTTP 连接 → close 函数包成 dispose
- 等等

**一个 Scope 管理整个 Agent 生命周期内所有的临时注册**，Dispose 一次清空。

---

## 7. 与 HookBus/EventBus 的对比

| 维度 | HookBus.On / EventBus.On | Scope.On |
|---|---|---|
| 注册对象 | 钩子/监听器（业务回调） | 任意 dispose 函数（统一回收） |
| 返回值 | 移除函数（按 id 移除） | 撤销函数（按索引标记 nil） |
| 批量管理 | 单个移除 | **批量 Close**（一次清全部） |
| 顺序保证 | 注册顺序 | **逆序 Close**（LIFO） |
| 已关闭行为 | 仍可注册（不影响当前链） | **立即执行 dispose**（防泄露） |

**Scope 不替代 HookBus/EventBus**，而是它们的**生命周期管理层**：

```
业务代码
  ├─ hook.On(...)           → 返回 removeFn
  └─ scope.On(removeFn)     → 把 removeFn 注册到 scope
                             Dispose 时 scope.Close() 自动调所有 removeFn
```

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **Close 幂等** | `s.closed` 检查 | 多次 Close 安全 |
| **LIFO 回滚顺序** | Close 倒序循环 | 资源依赖正确 |
| **On 返回的撤销函数幂等** | sync.Once | 多次撤销安全 |
| **已关闭后 On 立即执行** | `if s.closed { dispose() }` | 防资源泄露 |
| **nil scope/dispose 安全** | 早返回 no-op | 装配友好 |
| **Close 锁外执行回调** | 快照 + unlock | 防回调内 On 死锁 |
| **撤销用标记 nil 而非切片删** | `disposers[i] = nil` | 索引稳定 |
| **Close 跳过 nil 项** | `if disposers[i] != nil` | 已撤销项不重复执行 |

---

## 9. 自定义扩展姿势

### 9.1 注册带清理的资源

```go
// 业务代码
removeHook := hookBus.On(hook.HookPreStep, myHook)
defer scope.On(removeHook)()    // 注意：scope.On 返回的撤销器不调，依赖 Close 批量清

// 或：
scope.On(func() {
    removeHook()                // 等价写法
})

// AgentHandle.Dispose() 时 → scope.Close() → removeHook 自动调
```

### 9.2 HTTP 连接管理

```go
conn, _ := httpClient.Get(url)
scope.On(func() {
    conn.Close()                // Dispose 时自动关连接
})
```

### 9.3 文件句柄 / 临时文件

```go
tmpFile, _ := os.CreateTemp("", "agent-*")
scope.On(func() {
    tmpFile.Close()
    os.Remove(tmpFile.Name())
})
```

### 9.4 嵌套 scope（手动实现）

```go
parent := scope.New()
child := scope.New()

parent.On(child.Close)           // parent 关时自动关 child

child.On(...)                    // child 注册的资源
child.Close()                    // 单独关 child
// 或
parent.Close()                   // 全部关
```

### 9.5 已关闭后注册的语义

```go
s := scope.New()
s.Close()                        // 已关闭

s.On(func() {                    // 立即执行！
    fmt.Println("immediate")
})
// 打印 "immediate"
```

这让"延迟注册"的代码模式（如 `init()` 函数、跨包协作）安全。

---

## 10. 一句话总结

**`agent/scope` 是 70 行代码的极简资源回收容器：用一个 `disposers []func()` 切片 + 互斥锁 + closed 标志实现"注册 / 单独撤销 / 批量逆序 Close"三件套，与 HookBus/EventBus 的"按 id 移除"机制互补，专门服务于"Agent 生命周期结束时统一清理所有临时资源"这个场景。**它的全部价值就是让 AgentHandle.Dispose 不用关心"我注册了什么"，调一次 Close 全部清空 —— 是 v2 §3.7 "registrations are reversible effects" 哲学的最朴素落地。