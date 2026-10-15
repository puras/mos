# 声明式配置加载（agent/config）

> 来源：`agent/config/config.go`（架构 v2 §1.1 / §3.5）。
> 目标读者：想用 YAML 装配智能体的平台开发者；想理解"配置如何变成可运行 Agent"的所有人。

---

## 1. 一句话定位

**`agent/config` 是"声明式装配层"**：把 YAML 配置（驱动名、模型名、工具集名、策略名、参数）通过 `SeamRegistry` 按名解析为真实组件，最终调用 `assistant.New` 装配成可运行的智能体。**平台只需注册 provider，业务只需写 YAML**。

源码顶端的注释（config.go:1-4）一段话讲清定位：

> "YAML 配置 → 组件按名解析（SeamRegistry）→ 组装好的可运行智能体（assistant）。**平台（如 SaaS 智能体平台）注册 provider 后，仅凭 YAML 即可完成装配**"

文件只有一个 `config.go`，按职责分三块：

| 块 | 行号 | 职责 |
|---|---|---|
| 类型 | 23-54 | `Config` 结构（YAML 字段） |
| 装配入口 | 68-133 | `Load` / `Assemble` / `AssembleConfig` |
| 解析 helper | 136-237 | `resolveModel/Tools/Policy/PlanPlanner/Reflector/ScriptPlanner` |

---

## 2. 整体装配链路

```
YAML 配置
   ↓ Load / yaml.Unmarshal
Config（结构体）
   ↓ AssembleConfig
   ├─ resolveModel(reg, name)         → llm.ChatModel
   ├─ resolveTools(reg, names)        → []tool.Tool
   ├─ resolvePolicy(reg, name)        → driver.Policy
   ├─ resolvePlanPlanner(reg, name)   → planner.PlanPlanner (可选)
   ├─ resolveReflector(reg, name)     → planner.Reflector (可选)
   ├─ resolveScriptPlanner(reg, name) → planner.ScriptPlanner (可选)
   │
   └─ WorkspaceRoot 兜底 → os.Getwd()
   ↓
assistant.Config + model + tools
   ↓ assistant.New
*assistant.Assistant
```

**关键点**：所有解析走 `SeamRegistry.Resolve(kind, name)` —— 业务侧只关心名字，**实际类型由平台注册时决定**。

---

## 3. `Config` 数据结构（config.go:23-54）

### 3.1 字段分组

| 字段 | YAML 名 | 缺省 | 说明 |
|---|---|---|---|
| `Driver` | `driver` | `react` | react / plan-execute / reflection |
| `Model` | `model` | **必填** | 模型 provider 名 |
| `Tools` | `tools` | `builtin` | 工具集（`["builtin"]` = 内置包） |
| `Policy` | `policy` | `default` | 安全策略 provider 名 |
| `PlanPlanner` | `plan_planner` | 空 | PlanExecute 驱动专用 |
| `Reflector` | `reflector` | 空 | Reflection 驱动专用 |
| `ScriptPlanner` | `script_planner` | 空 | Scripted 驱动专用 |
| `SystemPrompt` | `system_prompt` | 空 | 系统引导词 |
| `MaxTurns` | `max_turns` | 0 = 不限 | 保险丝 |
| `ContextBudget` | `context_budget` | 0 = 关闭 | 上下文利用率监控 |
| `DoomLoopMaxRepeat` | `doom_loop_max_repeat` | 0 = 关闭 | Doom Loop 检测阈值 |
| `ProjectDocDir` | `project_doc_dir` | 空 | 项目指令目录 |
| `SkillsDir` | `skills_dir` | 空 | 技能清单目录 |
| `WorkspaceRoot` | `workspace_root` | `os.Getwd()` 兜底 | 文件工具 jail 根目录 |

### 3.2 字段语义层级

**1. 必填核心** —— `Model` 一个字段必填，缺则 `AssembleConfig` 返回 `"model is required"`。

**2. 可选核心** —— `Driver`、`Tools`、`Policy` 都有合理缺省，零配置可跑。

**3. 驱动专属** —— `PlanPlanner` / `Reflector` / `ScriptPlanner` 与 driver 类型耦合：

| Driver | 必需字段 |
|---|---|
| `react` | 无 |
| `plan-execute` | `PlanPlanner` |
| `reflection` | `Reflector` |
| `scripted` | `ScriptPlanner` |

注释（config.go:34-37）解释："Driver=plan-execute 时使用"、"reflection 时必需"、"scripted 时必需（opt-in）"。

**4. 调优参数** —— `MaxTurns`、`ContextBudget`、`DoomLoopMaxRepeat`、`ProjectDocDir`、`SkillsDir`，0/空 = 关闭/回退默认。

**5. 文件安全根** —— `WorkspaceRoot`，缺省回退到 `os.Getwd()`。注释（config.go:50-52）说明兜底理由：

> "空值由 assistant.New 校验拒绝；本字段为空时 Assemble 自动用 cwd 兜底，**避免单元测试等'无项目上下文'场景无法启动**"

### 3.3 注释里的"自动构建默认组装器"

`ProjectDocDir` / `SkillsDir` 不是给 config 用的，是传给 `assistant.Config` 让它**自动构建默认系统组装器**（参见 `docs/agent/system.md §4`）：

> "`EnableSlots == nil`：用默认集，并按目录是否配置自动追加 `SlotProject` / `SlotSkills`"

也就是说这两个字段是"提示"，让 assistant.New 知道该装哪些槽位。

---

## 4. `Load` / `Assemble` / `AssembleConfig` 三入口

### 4.1 `Load`（config.go:57-63）：纯解析

```go
func Load(data []byte) (*Config, error)
```

注释（config.go:56）：

> "未做组件解析，纯结构校验"

只做 YAML → Config 解析，不查 Registry。适合业务想拿到 Config 再自定义处理的场景。

### 4.2 `Assemble`（config.go:68-74）：从字节开始

```go
func Assemble(reg *seam.SeamRegistry, raw []byte) (*assistant.Assistant, error)
```

Load + AssembleConfig 的组合，最常用入口。

### 4.3 `AssembleConfig`（config.go:77-133）：从 Config 开始

```go
func AssembleConfig(reg *seam.SeamRegistry, cfg *Config) (*assistant.Assistant, error)
```

注释（config.go:76）：

> "按已解析的 Config 装配智能体（供编程式配置复用）"

让业务可以用代码构造 Config（不走 YAML），复用同一套解析逻辑。

### 4.4 `WorkspaceRoot` 兜底

```go
workspaceRoot := cfg.WorkspaceRoot
if workspaceRoot == "" {
    var cwdErr error
    workspaceRoot, cwdErr = os.Getwd()
    if cwdErr != nil {
        return nil, fmt.Errorf("config: WorkspaceRoot 缺省且无法获取 cwd: %w", cwdErr)
    }
}
```

这是**显式兜底**而非 fail-fast —— 与 "model is required" 的强校验形成对比。

注释（config.go:115-116）解释：

> "WorkspaceRoot 缺省且无法获取 cwd"

`os.Getwd()` 失败的情况极少见（除非 chdir 到不存在的目录），但仍然做错误传播不静默吞。

---

## 5. 解析 helper：6 个按名解析（config.go:136-237）

### 5.1 统一模式

每个 `resolveXxx` 函数结构一致：

```go
func resolveXxx(reg *seam.SeamRegistry, name string) (T, error) {
    if name == "" { return zeroValue, nil }   // 可选字段未配 → nil
    v, err := reg.Resolve(seam.KindXxx, name)
    if err != nil { return zeroValue, fmt.Errorf("config: xxx: %w", err) }
    actual, ok := v.(T)
    if !ok { return zeroValue, fmt.Errorf("config: xxx provider %q is not T (%T)", name, v) }
    return actual, nil
}
```

**三层错误信息**：

1. `reg.Resolve` 失败（未注册）→ `config: xxx: <原错误>`
2. 类型断言失败（注册了但类型不对）→ `config: xxx provider "name" is not T (actualType)`
3. 包外错误（未配置但缺省失败）→ 各 helper 自己处理

### 5.2 5 个解析 + 1 个特殊

| 函数 | 必需 | 缺省 | 类型断言目标 |
|---|---|---|---|
| `resolveModel` | ✅ | 报错 | `llm.ChatModel` |
| `resolveTools` | ❌ | `builtin.Default()` | `tool.Tool` |
| `resolvePolicy` | ❌ | `assistant.NewDefaultPolicy(nil, "")` | `driver.Policy` |
| `resolvePlanPlanner` | ❌ | nil | `planner.PlanPlanner` |
| `resolveReflector` | ❌ | nil | `planner.Reflector` |
| `resolveScriptPlanner` | ❌ | nil | `planner.ScriptPlanner` |

### 5.3 `resolveTools` 的特殊形态（config.go:149-170）

```go
func resolveTools(reg *seam.SeamRegistry, names []string) ([]tool.Tool, error) {
    if len(names) == 0 {
        return builtin.Default(), nil   // 空数组 = 内置工具包
    }
    var out []tool.Tool
    for _, name := range names {
        if name == "builtin" {
            out = append(out, builtin.Default()...)  // "builtin" 字符串也展开
            continue
        }
        v, err := reg.Resolve(seam.KindTool, name)
        ...
    }
}
```

**两个特殊点**：

1. **空数组 = 内置工具包** —— `Tools: []` 不配置时使用 `builtin.Default()`
2. **`"builtin"` 字符串关键字** —— 可以 `Tools: ["builtin"]`（等价于不配）或 `Tools: ["builtin", "my_custom"]`（内置 + 自定义混合）

注释（config.go:27-28）：

> "Tools 工具集：`["builtin"]` = 内置工具包（缺省）；其余名称走 seam tool provider"

### 5.4 `resolvePolicy` 的"default 关键字"（config.go:172-189）

```go
func resolvePolicy(reg *seam.SeamRegistry, name string) (driver.Policy, error) {
    if name == "" { name = "default" }            // 空 → "default"
    if name == "default" {
        return assistant.NewDefaultPolicy(nil, "")  // 不走 registry，直接构造
    }
    v, err := reg.Resolve(seam.KindPolicy, name)
    ...
}
```

**`"default"` 是不走 registry 的硬编码 fallback** —— 因为大多数场景不需要自定义 Policy，让缺省路径**省一次 registry 查找**。

### 5.5 `resolvePlanPlanner` 和 `resolveScriptPlanner` 共用 `KindPlanner`

```go
// resolvePlanPlanner
v, err := reg.Resolve(seam.KindPlanner, name)

// resolveScriptPlanner  
v, err := reg.Resolve(seam.KindPlanner, name)
```

**两者共用同一个 Kind** —— registry 用 `KindPlanner` 一个分类管理所有 planner 实现。区分靠**类型断言**（`planner.PlanPlanner` vs `planner.ScriptPlanner`）和**驱动在 driver.Run 里的失败处理**。

---

## 6. 与 `assistant.New` 的桥接（config.go:118-132）

```go
ac := assistant.Config{
    Driver:            cfg.Driver,
    PlanPlanner:       planPlanner,
    Reflector:         reflector,
    ScriptPlanner:     scriptPlanner,
    SystemPrompt:      cfg.SystemPrompt,
    MaxTurns:          cfg.MaxTurns,
    ContextBudget:     cfg.ContextBudget,
    DoomLoopMaxRepeat: cfg.DoomLoopMaxRepeat,
    ProjectDocDir:     cfg.ProjectDocDir,
    SkillsDir:         cfg.SkillsDir,
    Policy:            policy,
    FileAccess:        assistant.FileAccessConfig{WorkspaceRoot: workspaceRoot},
}
return assistant.New(model, tools, ac)
```

**字段对齐 `assistant.Config`**，但**不做实际装配**，只做"翻译 + 转发"：

- `model` / `tools` 是 `assistant.New` 的位置参数（必填核心）
- 其他字段扁平化复制
- `FileAccess` 嵌套字段单独构造

也就是说 **`config` 包只是 `assistant` 包的 YAML 适配器**，不引入新的装配语义。

---

## 7. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **YAML → Config → Assistant 单向流** | AssembleConfig 不回头改 Config | 不可变转换 |
| **Model 必填** | `cfg.Model == ""` 报错 | 核心组件不能缺 |
| **WorkspaceRoot 兜底到 cwd** | `os.Getwd()` | 测试场景可启动 |
| **`"builtin"` 工具关键字** | resolveTools | 内置包展开 |
| **`"default"` 策略关键字** | resolvePolicy | 硬编码 fallback |
| **Type assertion 错误明确** | `%T` 打印实际类型 | 调试友好 |
| **所有 Resolve 错误加 `config:` 前缀** | fmt.Errorf wrap | 错误可追溯 |
| **可选字段未配 → nil 不报错** | PlanPlanner/Reflector/ScriptPlanner | 零配置可跑 |

---

## 8. 自定义扩展姿势

### 8.1 写一个 YAML 配置（典型 SaaS 平台）

```yaml
driver: react
model: claude-sonnet-4-5
tools:
  - builtin
  - my_custom_search
policy: default
system_prompt: |
  你是 MoS 助手，回答简洁。
max_turns: 20
context_budget: 100000
doom_loop_max_repeat: 4
project_doc_dir: ./.claude
skills_dir: ./skills
workspace_root: /home/user/project
```

### 8.2 注册自定义 provider

```go
reg := seam.NewSeamRegistry()
seam.RegisterModel(reg, "my-gpt", myChatModel{})        // kind=Model
seam.RegisterTool(reg, "my_custom_search", mySearchTool{}) // kind=Tool
seam.RegisterPolicy(reg, "my_policy", myPolicy{})         // kind=Policy

agent, err := config.Assemble(reg, yamlBytes)
// → agent.Run(ctx, sess, "你好")
```

具体注册 API 详见 `seam` 包文档（后续包）。

### 8.3 编程式构造 Config（不用 YAML）

```go
cfg := &config.Config{
    Driver: "react",
    Model:  "claude-sonnet-4-5",
    Tools:  []string{"builtin"},
}
agent, err := config.AssembleConfig(reg, cfg)
```

### 8.4 装配后调 assistant.Run

```go
agent, err := config.Assemble(reg, yamlBytes)
if err != nil { return err }

sess := session.NewSession("user-123")
reply, err := agent.Run(ctx, sess, "你好")
```

`agent.Run` 内部走 `driver.ReactDriver.Run`，与 `agent/driver-react.md` 衔接。

### 8.5 平台侧动态切换模型

```yaml
# 生产环境
model: claude-sonnet-4-5
# 测试环境
model: mock-echo
```

只需切换注册表里的实现名，**业务 YAML 不变**。

---

## 9. 错误信息范式

config 包的错误都带 `config:` 前缀 + 上下文：

```
config: parse yaml: yaml: line 5: ...
config: model is required
config: model: seam: ...
config: model provider "x" is not llm.ChatModel (*myType)
config: WorkspaceRoot 缺省且无法获取 cwd: ...
```

**好处**：

- 上层 `errors.Is` / `fmt.Errorf("%w")` 可追溯到 config 层
- 业务错误（YAML 错）vs 系统错误（Registry 错）vs 类型错误（断言错）通过前缀分层
- 平台 UI 可针对性提示"修改 YAML 第几行"

---

## 10. 一句话总结

**`agent/config` 是 MoS 的"声明式装配适配器"：把 YAML 配置通过 `SeamRegistry` 按名解析为真实组件（model / tools / policy / 各 planner），用 6 个 `resolve*` helper 统一解析模式（Registry 查 + 类型断言），最终调用 `assistant.New` 扁平化转发。Model 必填强校验、WorkspaceRoot 兜底到 cwd、可选字段（PlanPlanner/Reflector/ScriptPlanner）未配返 nil 让零配置可跑、`"builtin"`/`"default"` 是硬编码关键字 —— 一切设计都围绕"平台注册 provider + 业务写 YAML"这个最小职责分工。**