# Agent 顶层能力面（agent 包零散文件）

> 来源：`agent/` 顶层零散 `.go` —— `auditor.go`（审计器）+ `compact.go`（压缩器）+ `observer.go`（事件类型）+ `projectdoc.go`（CLAUDE.md 加载）+ `rag.go`（检索注入配置）+ `skills.go`（技能加载与渲染）+ `skills_activate.go`（技能激活态生命周期）+ `truncate.go`（结果截断器）。
> 不在本文档：`system.go` / `driver/` 子包 / `session/` / `hook/` / `event/` / `approval/` / `checkpoint/` / `handle/` / `telemetry/` / `executor/` / `scope/` / `seam/` / `router/` / `config/` —— 各有专文。
> 目标读者：理解"agent 顶层包还有哪些独立能力"的所有人；想自定义上下文工程 / 审计 / 技能系统 / 项目指令的开发者。

---

## 1. 一句话定位

**`agent` 顶层包是 MoS 智能体的"零碎能力面"**：把不属于 driver/hook/session/handle 的独立能力聚合成一组 **"接口 + 默认实现 + 平台扩展点"**：审计（Auditor）/ 压缩（Compactor）/ 截断（Truncator）/ 检索注入（Retriever）/ 系统提示组装（Assembler，详见 `agent/system.md`）/ 项目指令加载（ProjectDoc）/ 技能加载（Skill）+ 事件类型（EventType / Observer）。每个能力都有"接口 + 默认实现 + 平台注入位"的三段式模板。

源码顶端的注释（observer.go:1-4）讲清 agent 顶层包边界：

> "Package agent 提供智能体运行时概念：**事件/观测定义与扩展点接口**（Audit/Compaction/Truncation/RAG/SystemAssembler）；**任务循环与驱动见子包 agent/driver、agent/driver/react**。"

文件清单（agent 包所有 `.go`）：

| 文件 | 主题 | 关联文档 |
|---|---|---|
| `auditor.go` | Auditor 接口 + DefaultAuditor（白名单审计） | 本文 §2 |
| `compact.go` | Compactor 接口 + defaultCompactor（fresh-context 压缩） | 本文 §3 |
| `observer.go` | EventType + Event + Observer（运行时事件类型） | 本文 §4 |
| `projectdoc.go` | LoadProjectDocs + renderProjectDocs（CLAUDE.md 加载） | 本文 §5 |
| `rag.go` | RetrieverConfig（Agentic RAG 注入配置） | 本文 §6 |
| `skills.go` | Skill + LoadSkills + renderSkills + AddTurnSkill（技能加载） | 本文 §7 |
| `skills_activate.go` | WithSession / SessionFromCtx + ResetRunSkills / FinalizeRunSkills（技能激活态生命周期） | 本文 §8 |
| `truncate.go` | ResultTruncator + defaultTruncator（工具结果截断 + 逃生通道） | 本文 §9 |
| `system.go` | Assembler / 7 槽位 | `docs/agent/system.md` |

---

## 2. Auditor —— 审计器（auditor.go）

### 2.1 一句话定位

**`Auditor` 是 MoS 的"事实审计门"**：决定哪些工具调用结果**可被提升为任务状态中的"已验证事实"**。**只有被环境独立验证过的事实才能写入 TaskState，模型"自称完成"的声明一律不算数 —— 环境说了算。**

注释（auditor.go:10-12）讲清哲学：

> "审计门（Audit Gate）：只有被环境独立验证过的事实才能更新任务状态，模型'自称完成'的声明一律不算数 —— **环境说了算**。"

### 2.2 接口

```go
type Auditor interface {
    Audit(ctx context.Context, call *llm.ToolCall, result any) ([]session.Fact, error)
}
```

返回值是 `[]session.Fact` —— 0 条 = 没有可写入的事实，1+ 条 = 已审计通过的事实。

### 2.3 `DefaultAuditor`：只读白名单（auditor.go:21-48）

```go
type DefaultAuditor struct {
    Trusted map[string]bool
}

func defaultTrustedTools() map[string]bool {
    return map[string]bool{
        "current_time": true,
        "web_fetch":    true,
        "web_search":   true,
        "read_file":    true,
        "find_files":   true,
        "grep":         true,
    }
}
```

**两条不变量**：

| 工具类型 | 是否审计 | 原因 |
|---|---|---|
| **只读/自证**（current_time、read_file、web_fetch、web_search、find_files、grep） | ✅ 审计通过 | 输出本身就是环境回答 |
| **变更类**（shell、write_file 等） | ❌ 拒绝 | "未核实声明"，需后续只读工具二次取证 |

**`Trust(name)` 平台扩展**（auditor.go:31-36）：业务可以把"自证工具"加入白名单（如平台自定义的只读 API 查询工具）。

### 2.4 `Audit` 实现（auditor.go:51-61）

```go
func (d *DefaultAuditor) Audit(_ context.Context, call *llm.ToolCall, result any) ([]session.Fact, error) {
    if d == nil || d.Trusted == nil || !d.Trusted[call.Name] {
        return nil, nil
    }
    evidence := session.FormatToolResult(result)
    return []session.Fact{{
        Summary:  call.Name + " → " + truncate(evidence, 120),
        Evidence: truncate(evidence, 512),
        Tool:     call.Name,
    }}, nil
}
```

**三条不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **白名单外返回 nil, nil** | `if !Trusted { return nil, nil }` | fail-closed（不审计 = 不写事实） |
| **Summary 截断到 120 rune** | truncate(s, 120) | 防大字段污染 |
| **Evidence 截断到 512 rune** | truncate(s, 512) | 详细证据有边界 |
| **rune 截断** | truncate 函数（auditor.go:64-70） | 避免切断多字节字符 |

---

## 3. Compactor —— 压缩器（compact.go）

### 3.1 一句话定位

**`Compactor` 是 MoS 的"fresh-context 压缩器"**（M2.5）：两轮之间历史超过阈值时，把**已验证事实摘要**注入 + 保留最近消息，**防止跨轮累积的上下文腐化**（40% Dumb Zone 之后质量陡降）。

注释（compact.go:8-15）一段话讲清哲学：

> "压缩发生在两轮之间：历史超过阈值时，将旧消息压缩为摘要注入，保留最近消息，**防止跨轮累积的上下文腐化（40% Dumb Zone 之后质量陡降）**。压缩摘要直接采用审计写入的 TaskState.Snapshot()，而非 LLM 重新总结 —— **已被环境独立验证过的事实才进入压缩摘要，模型自述不算数**。"

### 3.2 接口

```go
type Compactor interface {
    Compact(history []session.InternalMessage, state *session.TaskState) ([]session.InternalMessage, bool)
}
```

第二返回值表示"是否发生了压缩"（`false` 时第一返回值无效）。

### 3.3 `defaultCompactor.Compact` 三步走（compact.go:43-72）

```go
// 步骤 1：定位可压缩区（排除开头的 system 引导，system 由 Agent 每轮注入）
start := 0
if len(history) > 0 && history[0].Role == llm.RoleSystem {
    start = 1
}
msgs := history[start:]
if len(msgs) <= c.cfg.KeepRecent {
    return history, false
}

// 步骤 2：从尾部保留 KeepRecent 条；孤儿 tool 结果前移切割点
cut := len(msgs) - c.cfg.KeepRecent
for cut > 0 && msgs[cut].Role == llm.RoleTool {
    cut--
}
kept := msgs[cut:]

// 步骤 3：摘要注入：采用审计写入的 TaskState（已验证事实）
summary := session.SummaryMsg(state.Snapshot())

out := make([]session.InternalMessage, 0, len(kept)+2)
if start > 0 { out = append(out, history[0]) }
out = append(out, summary)
out = append(out, kept...)
return out, true
```

### 3.4 关键设计点

**孤儿 tool 结果前移**（compact.go:57-59）：如果保留区首条是 `tool` 结果（即对应的 assistant ToolCall 在切割点前），前移切割点使成对保留。注释：

> "先移动 cut 指针、最后一次拷贝，**避免循环内反复复制 kept 切片（O(n²)）**"

**摘要来自 TaskState 不是 LLM**（compact.go:15 + 63）：`state.Snapshot()` 经 `session.SummaryMsg(...)` 转 user 角色注入。**核心哲学**：只有审计通过的事实才能进摘要 —— 模型的"我完成了"声明不会污染压缩。

**默认值**（compact.go:26）：`DefaultKeepRecent = 12`（约 3-4 个 Turn）。

### 3.5 配置

```go
type CompactorConfig struct {
    KeepRecent int   // 保留最近消息条数；<=0 时回退 DefaultKeepRecent
}

const DefaultKeepRecent = 12
```

### 3.6 与 Truncator 的区别

| 维度 | Truncator | Compactor |
|---|---|---|
| 触发时机 | 单次工具结果 > 阈值 | 历史 > 阈值（两轮之间） |
| 作用对象 | 模型可见的 **单条结果文本** | 整个 **消息历史** |
| 策略 | 截断 + 落盘（逃生通道） | 摘要（TaskState）+ 保留最近 |
| 哲学 | 防单条大结果爆窗口 | 防跨轮上下文腐化 |

---

## 4. EventType + Observer —— 运行时事件（observer.go）

### 4.1 一句话定位

**`Observer` 是 MoS 的"运行时事件回调"**：所有需要被业务感知的事件（步骤 / 工具调用 / 完成 / 事实 / 流式 / 压缩 / 上下文利用率 / 知识差距 / 计划 / 评审 / 脚本 / 恢复 / 释放 / 技能加载 / 激活 / 取消）都通过统一的 `Event` 结构 + `Observer(Event)` 回调广播。

### 4.2 17 个 EventType（observer.go:16-50）

| EventType | 触发点 | 关键字段 |
|---|---|---|
| `EventStep` | 模型一轮中间推理（思考文本 + 工具调用） | Content（思考）/ ToolCall |
| `EventToolCall` | 模型请求调用工具 | ToolCall |
| `EventToolResult` | 工具执行完成 | ToolResult / ToolCall |
| `EventDone` | 任务完成 | Content（最终回复） |
| `EventFact` | 审计通过，新增已验证事实 | Fact |
| `EventStream` | 模型流式文本增量 | Delta / Reasoning |
| `EventCompacted` | 历史被 fresh-context 压缩（M2.5） | Content（压缩摘要） |
| `EventContextUsage` | 上下文利用率观测（M2.5） | Content "N%" + Usage |
| `EventKnowledgeGap` | 检索候选全低于阈值（知识差距） | Content（查询） |
| `EventPlan` | 计划产出（Plan&Execute） | Content（步骤摘要） |
| `EventReflection` | 评审结果（Basic Reflection） | Content（问题摘要） |
| `EventScript` | 脚本产出（Scripted） | Content（脚本摘要） |
| `EventResume` | checkpoint 恢复（M2.8） | Content（恢复说明） |
| `EventDisposed` | 句柄释放（M2.8） | — |
| `EventSkillLoad` | 技能清单装载（skill/load） | — |
| `EventSkillActivate` | 技能激活（skill/activate） | — |
| `EventSkillDeactivate` | 技能取消激活（skill/deactivate） | — |

### 4.3 `Event` 结构（observer.go:54-65）

```go
type Event struct {
    Type       EventType
    SessionID  string
    Turn       int       // 当前 Turn 序号（从 1 开始）
    Content    string    // EventStep 思考 / EventDone 回复 / EventCompacted 摘要 / EventContextUsage 利用率
    Delta      string    // 预留：流式文本增量
    Reasoning  string    // 流式推理内容增量（EventStream）
    ToolCall   *llm.ToolCall
    ToolResult any
    Fact       *session.Fact      // EventFact：审计通过的已验证事实
    Usage      llm.Usage          // EventContextUsage：模型 token 用量
}
```

注释（observer.go:53-54）讲清"Turn"语义：

> "Loop Engineering：通过 Turn 追踪循环轮次（**一个 Turn = 一次模型调用 + 其触发的所有工具执行**），Step 事件暴露每轮思考。"

### 4.4 `Observer` 类型

```go
type Observer func(Event)
```

—— 函数式回调，业务可以挂多个 Observer（多播）。

### 4.5 与 event.EventBus 的区别

| 维度 | `Observer func(Event)` | `event.EventBus` |
|---|---|---|
| 抽象层级 | agent 顶层 | agent/event 子包 |
| 事件来源 | driver/模型层循环打点 | Session 事件日志 + 多源 |
| 形态 | 单个回调函数 | 4 种总线模式（emit/waterfall/parallel/serial） |
| 用途 | 平台可观测（CLI 进度条 / Web UI） | 会话内部 hook 链 / 拦截 |

`Observer` 是**单点回调**，`EventBus` 是**多监听器总线**。两者可以并存：Observer 走单点简化链路，EventBus 走多监听器扩展。

---

## 5. ProjectDoc —— 项目指令加载（projectdoc.go）

### 5.1 一句话定位

**`LoadProjectDocs` 是 MoS 的"项目指令扫描器"**：从工作目录向上递归查找 `CLAUDE.md`（对齐 Claude Code 约定），返回**"顶层在前、最近在后"** 排序的文档列表 —— 父目录指令先声明，子目录就近覆盖。

注释（projectdoc.go:21-23）讲清语义：

> "从 dir 开始向上递归查找 CLAUDE.md，返回按**'顶层在前、最近在后'**排序的文档列表 —— 父目录指令先声明，子目录就近覆盖。"

### 5.2 `LoadProjectDocs` 实现（projectdoc.go:24-43）

```go
func LoadProjectDocs(dir string) ([]ProjectDoc, error) {
    var docs []ProjectDoc
    cur := dir
    for cur != "" {
        path := filepath.Join(cur, ProjectDocFile)
        if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
            b, err := os.ReadFile(path)
            if err != nil { return nil, fmt.Errorf("agent: read project doc %s: %w", path, err) }
            docs = append([]ProjectDoc{{Path: path, Content: string(b)}}, docs...)  // 前插
        }
        parent := filepath.Dir(cur)
        if parent == cur { break }
        cur = parent
    }
    return docs, nil
}
```

**关键点**：

| 设计 | 含义 |
|---|---|
| `docs = append([]ProjectDoc{...}, docs...)` | **前插**保证"顶层在前、最近在后" |
| `parent == cur` 退出 | 防止根目录（`/`）无限循环 |
| 目录不存在 / 无 CLAUDE.md | 返回 `nil, nil`（非错误） |
| `os.Stat` 后 `os.ReadFile` | 二次错误检查，文件存在但读不到时显式报错 |

### 5.3 `renderProjectDocs` 渲染（projectdoc.go:45-62）

```go
func renderProjectDocs(docs []ProjectDoc) string {
    var b strings.Builder
    for _, d := range docs {
        content := strings.TrimSpace(d.Content)
        if content == "" { continue }
        if b.Len() > 0 { b.WriteString("\n\n") }
        b.WriteString("## ")
        b.WriteString(d.Path)
        b.WriteString("\n")
        b.WriteString(content)
    }
    return b.String()
}
```

**渲染格式**：

```markdown
## /workspace/CLAUDE.md
（父级 CLAUDE.md 内容）

## /workspace/subproject/CLAUDE.md
（子目录 CLAUDE.md 内容，就近覆盖）
```

—— 每份标注来源路径（调试友好），按"顶层在前"顺序拼接。

### 5.4 与 Config 装配的衔接

`assistant.New` 通过 `Config.ProjectDocDir` 字段触发自动装配（assistant.go:184-189）：

```go
case cfg.ProjectDocDir != "" || cfg.SkillsDir != "":
    rt.Assembler = agent.NewDefaultSystemAssembler(agent.SystemAssemblerConfig{
        Constraints:   cfg.SystemPrompt,
        ProjectDocDir: cfg.ProjectDocDir,
        SkillsDir:     cfg.SkillsDir,
    })
```

`NewDefaultSystemAssembler` 内部会调 `LoadProjectDocs` + `LoadSkills`（详见 `docs/agent/system.md`）。

---

## 6. RAG 检索注入配置（rag.go）

### 6.1 一句话定位

**`RetrieverConfig` 是 MoS 的"Agentic RAG 注入配置"**（M2.7）：定义"每轮模型调用前按任务目标检索相关知识 + 知识守门"的配置；实际检索经 `RunContext.Retriever + RetrieverCfg` 注入，由 react 包的 `ragInjectHook` 执行。

注释（rag.go:17-19）讲清注入路径：

> "Retriever 配置经 RunContext.Retriever + RetrieverCfg 注入（见 react 包 ragInjectHook）；**未注入 Retriever 时跳过检索注入，行为不变**。"

### 6.2 `RetrieverConfig` 三字段

```go
type RetrieverConfig struct {
    MaxChunks int       // 每次注入上下文的最大分块数（默认 3）
    Limit     int       // 每次检索从后端取回的分块数（默认 8，为裁决留出候选）
    MinScore  float64   // 知识守门最低分（Agentic RAG 知识差距检测）
}
```

### 6.3 `MinScore` 知识守门

> "MinScore > 0 时低于该阈值的分块不注入，**全部低于阈值则判定'知识差距' —— 广播 EventKnowledgeGap**（Content 为查询），交由模型经外部工具补齐。"

**关键设计**：知识差距不是错误，是 **"信号"** —— 告诉模型"内部知识库答不了你，去用外部工具（web_search）补"。注释强调"得分尺度取决于检索后端（hybrid 路径为 [0,1] 融合分）；0 表示不过滤"。

### 6.4 平台注入位

`assistant.Config`（config.go:78-83）：
```go
Retriever       memory.Retriever
RetrieverMinScore float64   // → retrieverCfg.MinScore
```

—— `RetrieverMinScore` 直接镜像到 `RetrieverConfig.MinScore`，其他字段（MaxChunks / Limit）保留默认值。

---

## 7. Skills 加载（skills.go）

### 7.1 一句话定位

**`LoadSkills` 是 MoS 的"技能清单扫描器"**（M2.6）：扫描 `SkillsDir` 下的 `SKILL.md`（对齐 Agent Skills 规范），只加载 frontmatter 元数据，**正文按需懒读** —— 框架只注入清单（name + description），正文由模型触发 `activate_skill` 工具时经 `read_file` 按需读取。

注释（skills.go:13-18）讲清懒加载语义：

> "技能正文与 references/ 目录由模型在触发时经 read_file 按需读取 —— 框架只注入懒加载清单，正文不占常驻上下文（对齐 Claude Code / Agent Skills 的懒加载语义）。"

### 7.2 `Skill` 数据结构（skills.go:19-39）

```go
type Skill struct {
    Name          string    // frontmatter name
    Description   string    // frontmatter description（触发条件 + 能力描述）
    SchemaVersion string    // frontmatter schema_version（约定为字符串，向后兼容）
    WhenToUse     string    // frontmatter when_to_use
    AllowedTools  []string  // frontmatter allowed-tools（白名单）
    References    []string  // frontmatter references
    Source        string    // frontmatter source（local/repository）
    UserInvocable bool      // frontmatter user-invocable
    Dir           string // SKILL.md 所在目录（懒读时定位）
}
```

### 7.3 `LoadSkills` 扫描（skills.go:47-68）

```go
func LoadSkills(dir string) ([]Skill, error) {
    entries, err := os.ReadDir(dir)
    if err != nil {
        if os.IsNotExist(err) { return nil, nil }   // 目录不存在 = 空列表（非错误）
        return nil, err
    }
    var out []Skill
    if s, ok := parseSkillFile(filepath.Join(dir, SkillFile)); ok {
        out = append(out, s)               // dir 直接包含的 SKILL.md
    }
    for _, e := range entries {
        if !e.IsDir() { continue }
        if s, ok := parseSkillFile(filepath.Join(dir, e.Name(), SkillFile)); ok {
            out = append(out, s)           // dir 下每个一级子目录中的 SKILL.md
        }
    }
    return out, nil
}
```

**扫描规则**：

| 位置 | 形态 |
|---|---|
| `dir/SKILL.md` | 单个根级技能 |
| `dir/<子目录>/SKILL.md` | 子目录技能 |

—— 简化扫描策略（不深递归），避免无限子目录树。

### 7.4 `parseSkillFile` frontmatter 解析（skills.go:84-110）

`extractFrontmatter` 先抽出 `---` 之间的 YAML 块（避免正文被 yaml 误吃）：

```yaml
---
name: build-project
description: 项目脚手架构建
allowed-tools:
  - read_file
  - shell
references:
  - references/build-process.md
---

# 正文 Markdown（不解析）
...
```

注释（skills.go:90-91）：

> "先抽出 `---` 之间的 YAML 块，再交给 yaml.Unmarshal 解析（避免正文被 yaml 误吃）"

### 7.5 `renderSkills` 单段列表渲染（skills.go:129-142）

```go
func renderSkills(skills []Skill) string {
    var b strings.Builder
    b.WriteString("以下技能按需触发（用 activate_skill 工具激活；激活后正文 + references 路径返回给你）：\n\n")
    for _, s := range skills {
        b.WriteString("- ")
        b.WriteString(s.Name)
        if d := strings.TrimSpace(s.Description); d != "" {
            b.WriteString(": ")
            b.WriteString(d)
        }
        b.WriteString("\n")
    }
    return strings.TrimSpace(b.String())
}
```

**v2.21 起 `paths` 字段移除**（注释 skills.go:127-128）：

> "清单过滤属业务层职责，业务可经 Config.Assembler 自定义组装器实现；**全部技能统一列出，模型按需 activate_skill**"

### 7.6 `AddTurnSkill` 累积激活态（skills.go:144-172）

```go
func AddTurnSkill(sess *session.Session, sk Skill) {
    if sess == nil { return }
    sess.TurnSkills = append(sess.TurnSkills, session.ActivatedSkill{
        Name:         sk.Name,
        AllowedTools: sk.AllowedTools,
    })
    if len(sk.AllowedTools) == 0 {
        return  // nil / 空子集：保持 nil，不做"空集合 ≠ 不限制"的语义破坏
    }
    seen := make(map[string]bool, len(sess.TurnToolSubset)+len(sk.AllowedTools))
    for _, n := range sess.TurnToolSubset { seen[n] = true }
    for _, n := range sk.AllowedTools {
        if !seen[n] {
            seen[n] = true
            sess.TurnToolSubset = append(sess.TurnToolSubset, n)
        }
    }
}
```

**关键设计**：

- **AllowedTools 是安全控制** —— 合并到 `sess.TurnToolSubset`（Set semantics，去重保序）
- **空 AllowedTools 保持 nil** —— 不做"空集合 = 不允许任何工具"的语义破坏（spec §6.2 "未声明 = 不收窄"）
- **包级自由函数而非方法** —— 避免 session 反向依赖 agent 包造成循环依赖

---

## 8. Skills 激活态生命周期（skills_activate.go）

### 8.1 一句话定位

**`skills_activate.go` 提供"激活态在 Session 上的持久化 + ctx 注入 + Run 级生命周期"**：因为 `tool.Registry.Call` 不返回 ctx（累积状态对调用方不可见），激活态必须放在 `*session.Session` 这种 caller 可持有的对象上；`WithSession` / `SessionFromCtx` 给工具按 ctx 取用。

注释（skills_activate.go:10-23）讲清设计动机：

> "激活态持久化：本次 Run 已激活的 Skill 列表 + 工具子集并集改存在 *session.Session 上（替代 ctx 方案）。原因：tool.Registry.Call 仅返回 (any, error)，**ctx 内累积状态对调用方不可见 —— 必须放到 Session 这种可被 caller 持有的对象上**。生命周期边界 = driver.Agent.Run / RunStream（所有驱动的统一入口）：起始 ResetRunSkills、收尾 defer FinalizeRunSkills。"

### 8.2 四个核心函数

| 函数 | 职责 |
|---|---|
| `SkillsFromSession(sess)` | 读 TurnSkills 列表 |
| `ToolSubsetFromSession(sess)` | 读 TurnToolSubset（白名单） |
| `ResetRunSkills(sess)` | Run 起始清零 |
| `FinalizeRunSkills(sess)` | Run 收尾写 deactivate 事件 + 清零 |

### 8.3 `FinalizeRunSkills` 写事件（skills_activate.go:58-69）

```go
func FinalizeRunSkills(sess *session.Session) {
    if sess == nil || len(sess.TurnSkills) == 0 { return }
    names := make([]string, len(sess.TurnSkills))
    for i, s := range sess.TurnSkills { names[i] = s.Name }
    _ = sess.Append(event.SessionEventSkillDeactivate, event.MarshalData(event.SkillDeactivateData{Names: names}))
    sess.TurnSkills = nil
    sess.TurnToolSubset = nil
}
```

—— **Run 收尾强制写 `skill/deactivate` 事件**（不论 Run 正常完成 / 出错 / 挂起冒泡）。注释强调"白名单是安全控制，跨 Run 必须清零"。

### 8.4 `WithSession` / `SessionFromCtx` ctx 注入（skills_activate.go:74-89）

```go
type sessionKey struct{}

func WithSession(ctx context.Context, sess *session.Session) context.Context {
    return context.WithValue(ctx, sessionKey{}, sess)
}

func SessionFromCtx(ctx context.Context) *session.Session {
    if ctx == nil { return nil }
    v, _ := ctx.Value(sessionKey{}).(*session.Session)
    return v
}
```

**装配层调用点**（注释 skills_activate.go:72-78）：

> "driver（execpath.ExecuteHook）在 tool/execute 钩子前调用此函数。ActivateSkill 等工具经 SessionFromCtx 取用（写事件、读本次 Run 的激活态）。"

### 8.5 作用域边界 = 本次 Run 而非单轮

注释（skills_activate.go:24-26）：

> "作用域是本次 Run 而非单轮。已知边界：**TurnSkills 不进 checkpoint（json:\"-\"），跨挂起-恢复不保留** —— resume 重放挂起轮工具批时批内 activate_skill 会重新激活。"

—— checkpoint 不保留激活态是有意设计：避免 stale 白名单污染新挂起点。

---

## 9. Truncator —— 工具结果截断（truncate.go）

### 9.1 一句话定位

**`ResultTruncator` 是 MoS 的"工具结果瘦身器"**（M2.3）：对"给模型看的工具结果文本"做**双重限制（行数 + 字节） + 按工具类型的智能截断 + 逃生通道**（完整结果落盘，模型可经 `read_file` 按偏移回读）。

注释（truncate.go:11-13）讲清边界：

> "ResultTruncator 对'给模型看的工具结果文本'做上下文瘦身（M2.3）。**截断只作用于模型可见的文本，工具原始返回值（含审计）不受影响**。"

### 9.2 接口

```go
type ResultTruncator interface {
    Truncate(toolName, content string) (result string, truncated bool)
}
```

### 9.3 默认上限与工具分类（truncate.go:30-50）

| 常量 | 值 | 来源 |
|---|---|---|
| `DefaultTruncateMaxBytes` | 64 KB | Claude Code 50K 字符 / OpenClaw / Pi 50KB |
| `DefaultTruncateMaxLines` | 2000 | 双层防护（shell 沙箱默认输出上限同为 64KB） |

**三类工具**：

| 类别 | 工具 | 策略 |
|---|---|---|
| `headTools` | read_file, web_fetch, web_search, grep, find_files, current_time | 保留头部 |
| `tailTools` | shell | 保留尾部（报错在最后） |
| 其他 | 默认 | head+tail 各保留 keep/2 |

### 9.4 `defaultTruncator.Truncate` 四步走（truncate.go:74-104）

```go
// 步骤 1：双重限制检测
overBytes := d.cfg.MaxBytes > 0 && len(content) > d.cfg.MaxBytes
lines := 1 + strings.Count(content, "\n")
overLines := d.cfg.MaxLines > 0 && lines > d.cfg.MaxLines
if !overBytes && !overLines {
    return content, false
}

// 步骤 2：按工具类型选截断策略
keep := d.cfg.MaxLines
var out string
switch {
case headTools[toolName]: out = headTruncate(content, keep)
case tailTools[toolName]: out = tailTruncate(content, keep)
default:                  out = bothTruncate(content, keep)
}

// 步骤 3：行数截断后再做字节截断（UTF-8 安全）
if d.cfg.MaxBytes > 0 && len(out) > d.cfg.MaxBytes {
    out = byteTruncate(out, d.cfg.MaxBytes)
}

// 步骤 4：逃生通道 —— 完整结果落盘
spillPath := d.spill(toolName, content)
note := buildTruncateNote(toolName, len(content), lines, spillPath)
return out + note, true
```

### 9.5 三个截断函数（truncate.go:140-190）

| 函数 | 行为 |
|---|---|
| `headTruncate(s, keep)` | 保留开头 keep 行，加 `... [中间省略 N 行] ...` |
| `tailTruncate(s, keep)` | 保留结尾 keep 行（报错在尾部） |
| `bothTruncate(s, keep)` | head+tail 各 keep/2，中间省略 |

**`byteTruncate` UTF-8 安全**（truncate.go:179-190）：

```go
for n > 0 && !utf8.RuneStart(s[n]) {
    n--
}
```

—— 退字节直到 UTF-8 rune 边界，避免切断多字节字符。

### 9.6 逃生通道 spill（truncate.go:106-118）

```go
func (d *defaultTruncator) spill(toolName, content string) string {
    dir := filepath.Join(d.cfg.Dir, "mos-trunc")
    if err := os.MkdirAll(dir, 0o700); err != nil { return "" }
    name := filepath.Join(dir, "mos-"+toolName+"_"+itoa(int(d.seq.Add(1)))+".txt")
    if err := os.WriteFile(name, []byte(content), 0o600); err != nil { return "" }
    return name
}
```

**关键设计**：

| 设计 | 含义 |
|---|---|
| `os.MkdirAll(dir, 0o700)` | 懒创建（NewDefaultTruncator 不创建） |
| `0o600` 文件权限 | 只有当前用户可读（安全） |
| `atomic.Uint64` 序列号 | 防文件名碰撞 |
| 写入失败 → 返回 `""` | **降级为"无路径"提示**，截断本体不受影响 |
| `Dir = os.TempDir()` 兜底 | 不显式配则用系统临时目录 |

### 9.7 截断提示模板（truncate.go:120-137）

```
[工具输出已截断：完整 87654 字节 / 3500 行。完整输出已保存至 /tmp/mos-trunc/mos-read_file_42.txt，
需要完整内容请用 read_file(path="/tmp/mos-trunc/mos-read_file_42.txt") 按偏移回读。]
```

—— 把 spillPath 显式给模型（让它知道在哪 + 如何回读）。

### 9.8 配置

```go
type TruncatorConfig struct {
    MaxBytes int       // 0 = 不限制字节
    MaxLines int       // 0 = 不限制行数
    Dir      string    // 落盘目录（空 = os.TempDir()）
}
```

零值时使用默认上限（64KB / 2000 行）。

---

## 10. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **白名单外不审计** | DefaultAuditor.Audit | fail-closed |
| **rune 截断多字节字符** | truncate 函数 | 防乱码 |
| **压缩用 TaskState 不用 LLM** | defaultCompactor | 只压缩"已验证事实" |
| **保留区首条 tool 结果前移** | Compact cut 指针前移 | 防孤儿 tool 结果 |
| **system 消息由 Agent 每轮注入** | Compact 排除首条 system | 不重复注入 |
| **Turn 在 Event 中从 1 开始** | Event.Turn 注释 | Loop Engineering 一致 |
| **Event vs EventBus 双轨** | Observer + EventBus | 简化 + 扩展并存 |
| **ProjectDoc 顶层在前** | docs 前插 | 父指令先声明 |
| **目录不存在 = 空列表** | LoadProjectDocs / LoadSkills | 非错误 |
| **RAG 未注入 Retriever = 跳过** | rag.go 注释 | 行为不变 |
| **MinScore = 0 不过滤** | RetrieverConfig.MinScore | 显式语义 |
| **知识差距不是错误是信号** | EventKnowledgeGap | 引导模型外部补 |
| **Skill 正文懒读** | LoadSkills 只读 frontmatter | 不占常驻上下文 |
| **Skill 路径过滤业务层** | renderSkills paths 字段移除 | 框架中立 |
| **激活态 nil = 不收窄** | AddTurnSkill 保持 nil | spec §6.2 |
| **激活态在 Session 不在 ctx** | tool.Registry.Call 不返 ctx | caller 可见 |
| **Run 收尾强制写 deactivate** | FinalizeRunSkills defer | 白名单不跨 Run 泄漏 |
| **TurnSkills 不进 checkpoint** | json:"-" | 跨挂起-恢复不保留 |
| **截断只影响模型可见文本** | Truncate 返回值 | 审计不受影响 |
| **spill 失败不阻断截断** | spill 返回 "" | 降级 |
| **UTF-8 安全字节截断** | byteTruncate | 防乱码 |
| **空 ToolSubset 保持 nil** | AddTurnSkill len==0 | 不破坏"未声明 = 不收窄" |

---

## 11. 跨能力协作关系

```
一次完整 Run 流程：

Run 起始
  ├─ ResetRunSkills(sess)               ← skills_activate.go
  ├─ LoadProjectDocs(ProjectDocDir)     ← projectdoc.go
  └─ LoadSkills(SkillsDir)              ← skills.go
      ↓
Driver.Run（react / plan-execute / ...）
  │
  ├─ 每轮循环
  │   ├─ SystemAssembler 组装系统提示词（system.go）
  │   │     ├─ Constraints 槽位
  │   │     ├─ renderProjectDocs(ProjectDocDir)
  │   │     └─ renderSkills(SkillsDir)
  │   ├─ LLM.Call
  │   │     ├─ WithSpan(ctx, rt.Telemetry, "llm.request", ...)
  │   │     └─ RAG 注入（rag.go 配置）
  │   ├─ Truncate（如果工具结果 > 阈值）   ← truncate.go
  │   ├─ 执行工具
  │   │     ├─ WithSession 注入 sess     ← skills_activate.go
  │   │     ├─ Auditor.Audit → Facts     ← auditor.go
  │   │     │     └─ 写 TaskState.Facts（已验证事实）
  │   │     └─ 工具执行（如 activate_skill）
  │   │           └─ AddTurnSkill(sess, sk) ← skills.go
  │   └─ 下一轮
  │
  ├─ 两轮之间
  │     └─ Compactor.Compact              ← compact.go
  │           └─ 用 TaskState.Snapshot() 作摘要源
  │
  └─ Run 收尾
        └─ FinalizeRunSkills(sess)       ← skills_activate.go
              └─ 写 skill/deactivate 事件
```

**关键依赖关系**：

| 能力 | 依赖其他能力 | 被依赖 |
|---|---|---|
| Auditor | session.Fact / llm.ToolCall | Compactor（摘要来源） |
| Compactor | session.InternalMessage / TaskState / SummaryMsg | 无（顶层） |
| Observer | session.Fact / llm.ToolCall / llm.Usage | 无（顶层） |
| ProjectDoc | 无 | system Assembler |
| RAG Config | memory.Retriever | react ragInjectHook |
| Skills Load | 无 | system Assembler + activate_skill 工具 |
| Skills Activate | session.ActivatedSkill / event.SkillDeactivateData | driver.Agent.Run |
| Truncator | os + atomic.Uint64 | execpath（工具执行钩子） |

---

## 12. 自定义扩展姿势

### 12.1 自定义 Auditor（平台风控）

```go
type RestrictedAuditor struct {
    *agent.DefaultAuditor
    blockedTools map[string]bool
}

func (r *RestrictedAuditor) Audit(ctx context.Context, call *llm.ToolCall, result any) ([]session.Fact, error) {
    if r.blockedTools[call.Name] {
        return nil, fmt.Errorf("工具 %q 已被风控屏蔽", call.Name)
    }
    return r.DefaultAuditor.Audit(ctx, call, result)
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Auditor: &RestrictedAuditor{...},
    ...
})
```

### 12.2 自定义 Compactor（保 20 条不压）

```go
type ConservativeCompactor struct{}

func (c *ConservativeCompactor) Compact(history []session.InternalMessage, state *session.TaskState) ([]session.InternalMessage, bool) {
    return history, false  // 永不压缩（让历史自由累积）
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Compactor: &ConservativeCompactor{},
    ...
})
```

### 12.3 自定义 Truncator（更宽松）

```go
type PermissiveTruncator struct {
    *agent.DefaultTruncator
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Truncator: agent.NewDefaultTruncator(agent.TruncatorConfig{
        MaxBytes: 256 * 1024,    // 256KB（比默认 64KB 宽）
        MaxLines: 5000,
    }),
    ...
})
```

### 12.4 多个 Observer 多播

```go
observers := []agent.Observer{
    func(e agent.Event) { log.Printf("[%s] turn=%d", e.Type, e.Turn) },
    func(e agent.Event) { metrics.RecordEvent(e.Type, e.SessionID) },
    func(e agent.Event) { ws.Broadcast(e) },          // WebSocket 推前端
}

// 平台包装：让所有 Observer 收到
combined := func(e agent.Event) {
    for _, obs := range observers { obs(e) }
}
agent, _ := assistant.New(model, tools, assistant.Config{
    Observer: combined,
    ...
})
```

### 12.5 自定义 Skill（写在 SKILL.md）

```markdown
---
name: build-project
description: 构建项目脚手架
when_to_use: 用户要求新建项目时
allowed-tools:
  - read_file
  - shell
  - write_file
references:
  - references/build-process.md
---

# Build Project

创建项目时按以下流程：
1. 读取模板目录
2. 替换占位符
3. 初始化 git
...
```

放在 `SkillsDir/build-project/SKILL.md`，框架自动扫描。

### 12.6 自定义项目指令

`/workspace/CLAUDE.md`：

```markdown
# 项目指令

- 所有代码必须跑通 `make test`
- 提交前必须更新 CHANGELOG.md
- ...
```

`assistant.Config.ProjectDocDir = "/workspace"`，框架自动加载并按"顶层在前"顺序注入系统提示词。

---

## 13. 一句话总结

**`agent` 顶层包是 MoS 智能体的"零碎能力面"：8 个独立文件提供 8 组"接口 + 默认实现 + 平台扩展点"——Auditor（白名单审计）/ Compactor（fresh-context 压缩 + TaskState 作摘要源）/ Truncator（双重限制 + 智能截断 + 逃生通道 spill）/ Observer（17 种 EventType 多播）/ ProjectDoc（向上递归 CLAUDE.md）/ Skills（frontmatter 扫描 + 懒加载正文 + 激活态生命周期）/ RAG Config（MaxChunks / Limit / MinScore 知识守门）。每个能力都遵循"环境说了算 + fail-closed + 平台可注入 + 零核心依赖"的统一哲学，是 v2 §3.5 "扩展点可注入"架构的最细粒度落地。**