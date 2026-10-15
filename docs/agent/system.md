# 系统提示词组装器（System Assembler）

> 来源：`agent/system.go`（M2.6 动态组装）+ `assistant/assistant.go:185` 默认装配入口。
> 目标读者：需要自定义/扩展系统提示词的开发者，以及想理解"每轮模型调用前发生了什么"的所有人。

---

## 1. 问题与方案

传统做法是把系统提示词写成一段大字符串，每次调用直接拼上去。在多轮 agent 里会遇到三类麻烦：

1. **prompt cache 命中低** —— 系统提示词每轮都在变，缓存复用率差，token 成本高。
2. **内容耦合** —— "你是谁"和"现在任务是什么"挤在一起，改一处就要重发整段。
3. **规模失控** —— 容易越写越长（Claude Code 数万 token），挤占真正的任务上下文。

`agent/system.go` 给出的方案是：**把系统提示词拆成"槽位（Slot）"，分而治之**。

---

## 2. 三层抽象

```
SystemSlot（槽位枚举，标识语义）
   ↓
SystemSection（分段：Title + Content，渲染时的最小单元）
   ↓
SystemAssembler（接口，每轮调一次，按需产出 sections）
```

- **接口极简**：只一个 `Assemble(ctx, sess, tools) ([]SystemSection, error)`（`agent/system.go:53`）
- **函数式适配器**：`SystemAssemblerFunc`（`agent/system.go:58`）让应用层用闭包实现自定义组装器，不必为每个变体写结构体
- **`RenderSystem`**（`agent/system.go:179`）：把 `[]SystemSection` 渲染为最终文本。空分段自动跳过，前后空白 `TrimSpace` 干净收口

这一层"接口 + 适配器 + 渲染器"的拆分，是为了**让"组装的策略"和"渲染的格式"解耦** —— 以后想换成 XML、Markdown 表格、或者发给不同厂商的不同格式，只动 `RenderSystem` 即可。

---

## 3. 七个槽位：固定 vs 动态的二分

七个槽位被刻意分成两类（`agent/system.go:17-37`）：

| 类型 | 槽位 | 特点 |
|---|---|---|
| **静态** | `SlotConstraints`（系统约束/身份） | 一旦设定，整轮不变 |
| **静态** | `SlotProject`（项目指令） | 启动加载一次后不变 |
| **静态** | `SlotSkills`（技能清单目录） | 启动加载一次后不变 |
| **动态** | `SlotTask`（任务定义） | 每轮来自 `sess.State.Goal` |
| **动态** | `SlotState`（当前状态） | 每轮来自 `TaskState.Snapshot()` |
| **动态** | `SlotTools`（工具签名） | 每轮来自 `tools []llm.Tool` |
| **可选** | `SlotSummary`（历史摘要） | 默认关闭，避免与 Compactor 重复 |

这个分类的妙处在源码注释里写得直白（`agent/system.go:14-16`）：

> "固定指令（系统约束）与运行时状态（任务定义 / 当前状态 / 工具签名）分离，后者每轮随 TaskState 进度重新组装，前者保持静态不变以利于 prompt cache。"

也就是说：**只有动态部分会变，静态部分跨轮复用 → 缓存友好 → 省 token**。

---

## 4. 克制主义：默认槽位集与显式控制

```go
// agent/system.go:41
var defaultEnableSlots = []SystemSlot{SlotConstraints, SlotTask, SlotState, SlotTools}
```

注释里点名参考 Pi 的"约 1K token"对比 Claude Code 数万 token —— 体现了**"少即是多"**的工程取向：默认只装最必要的四块，能不发的就不发。

更精巧的是 `NewDefaultSystemAssembler`（`agent/system.go:83`）的双轨语义：

- **`EnableSlots == nil`**：用默认集，并按目录是否配置**自动追加** `SlotProject` / `SlotSkills`。开箱即用，但绝不"配置了目录却不开槽位"。
- **`EnableSlots` 非 nil**：完全按调用方写的来，目录配置也忽略。给高级用户做"我只要这两块"。

这种"nil = 智能默认、非 nil = 严格控制"是 Go 里很地道的设计 —— 既不强迫用户填默认值，又给完整控制权。

---

## 5. `Assemble` 实现的工程细节

### 5.1 启动期一次性加载（`sync.Once`）

```go
// agent/system.go:123-129
a.once.Do(func() {
    a.project, a.loadErr = LoadProjectDocs(a.cfg.ProjectDocDir)
    if a.loadErr != nil {
        return
    }
    a.skills, a.loadErr = LoadSkills(a.cfg.SkillsDir)
})
```

项目指令和技能清单**只在第一次调用时读盘**，后续命中内存缓存。

**但失败信息会被缓存**（`loadErr`）—— 这意味着启动期错了，之后每轮都重新抛错，fail-fast 一致。

### 5.2 输出顺序固定

`Assemble` 里 `add` 的调用顺序就是最终渲染顺序（`agent/system.go:139-149`）：

```
约束 → 项目指令 → 任务 → 状态 → 工具 → 技能 → 摘要
```

这是有讲究的 —— 重要且稳定的（约束、项目）放前面，让模型先建立语境；易变的（状态、工具）放后面，方便局部变更。

### 5.3 空内容自动跳过

`add` 函数（`agent/system.go:132-137`）同时判两个条件：

```go
if !a.enabled[slot] || strings.TrimSpace(content) == "" {
    return
}
```

槽位关闭或内容空白都不产出 —— 这让"任务未设置时不分发任务段"成为隐式契约。

### 5.4 幂等判据为什么不用 `sync.Once`（重点）

最值得品的一段（注释见 `agent/system.go:151-156`）：

> "本组装器由 `buildRunContext` 装配后被同一 Assistant 的所有会话共享，once 只会让'第一个跑到这里的会话'拿到事件，其余会话永久缺失。**日志即事实** —— 扫已有事件是唯一对多会话与 Resume 都正确的判据。"

也就是说：

- **不能用 `sync.Once` 做"每会话只写一次 skill/load 事件"** —— 因为组装器是跨会话共享的，`once` 维度不对。
- **正确做法是查 `sess.Events`**（`hasSkillLoadEvent`，`agent/system.go:108`）—— 事件日志本身是"什么已经发生"的唯一可信源，对多会话、Resume、并发都自洽。
- **失败向上抛**（`agent/system.go:168`）—— 不静默吞错，与 `session.Restore` 的策略一致。

这处细节反映出"日志即事实"的设计哲学贯穿整个 agent 层。

---

## 6. 与其它模块的协作边界

从签名 `Assemble(ctx, *session.Session, []llm.Tool)` 能看出 System 模块**不依赖**：

- 具体的 LLM 厂商（`llm.Tool` 是抽象类型）
- 具体的工具实现
- 具体的存储（项目/技能文件读盘由 `LoadProjectDocs` / `LoadSkills` 负责）

它只依赖 `session.Session` 和 `llm.Tool`，加上事件日志（`event.SessionEventSkillLoad`）。这种**窄接口、强约束**让 System 既能在不同 Agent 配置下复用，也方便单测（mock 出一个最小 `Session` 即可）。

### 调用链

```
driver.ReActDriver.Run
  └─ rt.AssembleSystem(ctx)              // RunContext 持有 Assembler
       └─ SystemAssembler.Assemble(...)
            └─ 返回 []SystemSection
                 └─ RenderSystem → 最终 system 文本
                      └─ session.SystemMsg(text) → 注入本轮 messages
```

---

## 7. 自定义扩展：三种姿势

### 7.1 仅替换"系统约束"内容（最常见）

不需要自定义 `Assembler`，只需配 `SystemPrompt`：

```go
assistant.New(assistant.Config{
    SystemPrompt: "你是 MoS 助手，回答保持简洁。",
    // ...
})
```

内部走 `defaultSystemAssembler`，槽位结构不变，只是把"约束"槽位的内容换成你的字符串。

### 7.2 用闭包完全接管组装

适合一次性实验或快速定制：

```go
custom := agent.SystemAssemblerFunc(func(_ context.Context, sess *session.Session, _ []llm.Tool) ([]agent.SystemSection, error) {
    return []agent.SystemSection{
        {Title: "身份", Content: "你是专家"},
        {Title: "任务", Content: sess.State.Goal},
    }, nil
})

assistant.New(assistant.Config{
    Assembler: custom,
    // ...
})
```

### 7.3 自定义结构体（生产级）

当组装逻辑复杂、要单测、要复用时，写一个实现 `SystemAssembler` 接口的结构体（参考 `defaultSystemAssembler`，`agent/system.go:97`）。

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **静态段不重发** | `add` 函数 + `a.cfg.Constraints` 等 | 跨轮复用，prompt cache 命中 |
| **空段不出** | `strings.TrimSpace(content) == ""` 跳过 | 不浪费 token |
| **每会话只写一次 `skill/load`** | `hasSkillLoadEvent(sess)` | 日志即事实，跨会话/Resume 自洽 |
| **加载失败必报** | `loadErr` 缓存 + `return out, fmt.Errorf(...)` | fail-fast，不静默吞错 |
| **渲染与组装解耦** | `RenderSystem` 独立函数 | 改格式不动组装策略 |
| **nil = 智能默认、非 nil = 严格控制** | `NewDefaultSystemAssembler` 双轨语义 | 不强迫填默认值，又给完整控制权 |

---

## 9. 一句话总结

**`agent/system.go` 用"槽位化 + 静态/动态分离 + 启动期懒加载 + 日志即事实"四件套，把系统提示词从一团字符串改造成可缓存、可裁剪、可观测、可扩展的分段组件，本质是在用工程化手段压 token、提命中率、保可控。**