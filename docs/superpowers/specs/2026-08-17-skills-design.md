# Skills 强化 设计文档（M2.6 follow-up）

> 日期：2026-08-17 ｜ 对应章节：v2 架构 §3.3（系统提示词动态组装）+ §3.6（Skill 注入）
> 状态：草案 v1，待用户评审
> 调研：docs/research/skills-survey.md（Claude Skills / pi-coding-agent / Codex / Cursor / Aider / Gemini CLI / LangChain Hub / LlamaIndex）

## 1. 目标

MoS 当前 Skills 能力仅解析 `name` / `description`，模型自由 `read_file` 读 SKILL.md，无 frontmatter 声明式配置、无显式激活工具、无工具子集限制、无激活观测。本设计在不引入新执行机制（不替代 `SeamRegistry` / `SubAgent` / `HookRegistry`）的前提下，**把 Skills 升级为带 frontmatter 契约 + 显式激活 + 工具子集限制 + 事件可观测的"知识包"语义**，对齐 Claude Skills 的渐进式披露哲学。

## 2. 不做什么（YAGNI）

- **不实现 `model` / `agent` / `hooks` 字段**——与现有 `llm/wrap` 装饰器 / `SubAgent` 原语（M3）/ `HookRegistry` 职责重叠。Skills 保持"知识包"纯语义。
- **不实现 marketplace / 安装 / 认证 / 索引**——架构 §9.3 已划归平台层。
- **不实现 `user_invocable` 斜杠命令面**——SDK 不暴露 CLI；字段可声明，由平台层消费。
- **不实现跨会话 Skill 状态**——激活态仅单轮生效（与 `request_human_input` 同生命周期）。
- **不实现 Skill 间依赖锁 / 版本管理**——YAGNI，路径管理留给平台层。

## 3. frontmatter 字段契约（v1）

```yaml
---
name: git-commit-helper            # 必需，唯一标识 + 目录名
description: 辅助提交 Git commit   # 必需，触发条件一句话
schema_version: "1"                # 必需，便于未来字段演进
when_to_use: 用户请求提交时        # 可选：触发场景细化（description 超限时分离）
allowed-tools:                     # 可选：硬限制工具子集（未声明 = 不限制）
  - git
  - read_file
references:                        # 可选：references/ 子文档路径（模型按需 Read）
  - references/commit-types.md
  - references/conventional-commits.md
paths:                             # 可选：glob 路径过滤（仅影响 SlotSkills 清单）
  - "*.go"
  - "**/*.proto"
source: local                      # 可选：local / github:owner/repo 审计用
user-invocable: true               # 可选：是否允许斜杠命令调用（v1 仅声明，CLI 不接）
---

正文 markdown（懒加载正文，模型经 activate_skill 触发后读取）。
```

| 字段 | 类型 | 必需 | YAML 形式 |
| --- | --- | --- | --- |
| `name` | string | ✅ | 标量 |
| `description` | string | ✅ | 标量 |
| `schema_version` | string | ✅ | 标量（v1 固定为 `"1"`） |
| `when_to_use` | string | — | 标量 |
| `allowed-tools` | string list | — | inline `["a", "b"]` 或多行 `- a` |
| `references` | string list | — | 同上 |
| `paths` | string list | — | 同上 |
| `source` | string | — | 标量 |
| `user-invocable` | bool | — | 标量（默认 false） |

**解析方式**：使用 `gopkg.in/yaml.v3`（已在 go.mod 声明）。保留 `parseFrontmatter` 旧路径（仅 `name` / `description`），新路径解析完整 8 字段。

## 4. 数据结构

```go
// agent/skills.go
type Skill struct {
    Name          string   // 必需
    Description   string   // 必需
    SchemaVersion string   // 必需
    WhenToUse     string   // 可选
    AllowedTools  []string // 可选
    References    []string // 可选
    Paths         []string // 可选
    Source        string   // 可选
    UserInvocable bool     // 可选，默认 false

    // 运行时元数据
    Dir          string // SKILL.md 所在目录（references 拼接用）
    PathMatch    bool   // 当前 working dir 是否命中 paths glob
}
```

向后兼容：现有 `LoadSkills` 调用方字段访问 `Name` / `Description` 不变；新增字段为零值时按"未声明"处理。

## 5. `activate_skill` 工具契约

### 5.1 注册与作用域

- **自动注册**：当 `assistant.Config.SkillsDir != ""` 时 `assistant.New` 自动注册
- **可关闭**：`assistant.Config.DisableActivateSkill bool` 默认 false
- **作用域**：单轮生效（与 `request_human_input` 同模式）

### 5.2 工具签名

```go
package builtin

type ActivateSkill struct {
    Skills []agent.Skill // 注入式：装配层传入
}

type activateSkillArgs struct {
    Name string `json:"name" jsonschema:"required"`
}

type SkillActivationResult struct {
    Name         string   `json:"name"`
    Body         string   `json:"body"`           // SKILL.md 正文（frontmatter 之外）
    References   []string `json:"references"`     // 子文档相对路径
    AllowedTools []string `json:"allowed_tools"`  // 本次激活的工具子集
}
```

### 5.3 行为

1. 解析 args（`tool.Parse` 自动校验 `name` 必需）
2. 在 `t.Skills` 中按 `name` 查找；未找到返回错误
3. 写事件 `skill/activate`（durable）载荷 `SkillActivateData`
4. sess 注入：`agent.AddTurnSkill(sess, skill)` — 把 Skill 投影为
   `sess.TurnSkills`（最小切片 session.ActivatedSkill）并合并 AllowedTools 到
   `sess.TurnToolSubset`（Set semantics，去重保序）
5. （保留步骤位）sess 替代 ctx：本轮工具白名单 = `sess.TurnToolSubset`
6. 返回 `SkillActivationResult`（body = markdown body + references 路径列表）

### 5.4 并发与互斥

- **同时激活多个 Skill**：`AllowedTools` 取并集；每个 Skill 各自写 `skill/activate` 事件
- **轮结束清空**：`react.runTurnInner` 末尾 emit `skill/deactivate` 载荷本轮所有 name，
  并清零 `sess.TurnSkills` / `sess.TurnToolSubset`（防御性轮首 reset 在 runTurnInner 顶部再次兜底）

## 6. 工具子集硬限制

### 6.1 实现路径

`react.executeTool` 调用前（policyGuard 已有的拦截点）增加：
```go
if subset := toolSubsetFromCtx(ctx); subset != nil {
    if !contains(subset, call.Name) {
        return tool.Result{Error: fmt.Errorf("skill allow-list blocks tool %q", call.Name)}
    }
}
```

复用现有 `policyGuard` 拦截点（fail-closed），不新增层级。

### 6.2 语义

- 未声明 `allowed-tools` = 不收窄（保持现状）
- 声明后 = 本轮所有激活 Skill 的 `AllowedTools` 并集
- 模型返回未声明的 `ToolCall` → 返回 tool error（不进 SuspendError，不挂起）

## 7. 事件载荷

```go
// agent/event/events.go 新增 3 个常量 + 3 个 payload
const (
    SessionEventSkillLoad       = "skill/load"       // 装配时扫描
    SessionEventSkillActivate   = "skill/activate"   // activate_skill 调用
    SessionEventSkillDeactivate = "skill/deactivate" // 轮结束清空
)

type SkillLoadData struct {
    Skills []SkillMeta `json:"skills"` // 仅 frontmatter 摘要（避免大字段）
}

type SkillMeta struct {
    Name          string   `json:"name"`
    Description   string   `json:"description"`
    SchemaVersion string   `json:"schema_version,omitempty"`
    WhenToUse     string   `json:"when_to_use,omitempty"`
    AllowedTools  []string `json:"allowed_tools,omitempty"`
    References    []string `json:"references,omitempty"`
    Paths         []string `json:"paths,omitempty"`
    Source        string   `json:"source,omitempty"`
    UserInvocable bool     `json:"user_invocable,omitempty"`
}

type SkillActivateData struct {
    Name         string   `json:"name"`
    AllowedTools []string `json:"allowed_tools,omitempty"`
    References   []string `json:"references,omitempty"`
    // 注意：durable 日志仅记元数据；正文 (Body) 由 activate_skill 工具直接
    // 返回给模型（不进 Session Event Log，避免正文重复落盘）。
}

type SkillDeactivateData struct {
    Names []string `json:"names"` // 本轮所有激活过的 skill name
}
```

**`skill/load` 触发时机**：`defaultSystemAssembler.once.Do` 首次加载时写入一次（durable，append-only 日志）。

**`skill/activate` 触发时机**：`activate_skill` 工具 Execute 成功后。

**`skill/deactivate` 触发时机**：每轮 `runTurnInner` 末尾（无论成功/失败/挂起）。

## 8. SlotSkills 渲染升级

`renderSkills` 增强输出：

```
以下技能按需触发（用 activate_skill 工具激活；激活后正文 + references 路径返回给你）：

可用技能：
- git-commit-helper: 辅助提交 Git commit（*.go）
- proto-migration: proto 文件迁移（**/*.proto）

未命中路径（模型可手动 activate_skill 调用）：
- deployment-runbook: 部署运维（路径未命中）
```

- `PathMatch = true` 的 Skill 列入"可用技能"
- `PathMatch = false` 的 Skill 列入"未命中路径"（仍可手动激活）
- 不命中 paths glob 的 Skill 仍可被模型主动调 `activate_skill` 加载

## 9. driver 集成点

### 9.1 `react.runTurnInner`

- 每轮开始：`sess.TurnSkills = nil`，`sess.TurnToolSubset = nil`（防御性 reset，
  避免上一轮 early-return 路径未走 emitSkillDeactivate 导致状态泄漏）
- `activate_skill` 工具执行后：`agent.AddTurnSkill(sess, target)` 累积
- 每轮结束（成功 / SuspendError / 失败）：
  ```go
  if len(sess.TurnSkills) > 0 {
      sess.Append(event.SkillDeactivate, SkillDeactivateData{Names: ...})
      sess.TurnSkills = nil
      sess.TurnToolSubset = nil
  }
  ```

### 9.2 `executeTool` 前置校验

```go
if subset := agent.ToolSubsetFromSession(rt.Session); subset != nil && !contains(subset, call.Name) {
    return tool.Result{Error: fmt.Errorf("skill allow-list blocks tool %q", call.Name)}
}
```

注：`skillAllowListGuard` 签名在 driver 层升级为 `skillAllowListGuard(rt *driver.RunContext) hook.Guard`，
捕获 rt 后闭包内访问 `rt.Session.TurnToolSubset`（hook.Guard 仅接受 ctx，无法传入 rt）。

### 9.3 `policyGuard` 不变

不修改 `policyGuard`（已通过 fail-closed 拦截路径 / 危险命令）。Skill 子集限制走 `executeTool` 前置校验，不冲突。

## 10. 装配入口

```go
// assistant/config.go 新增字段
type Config struct {
    // ...
    SkillsDir            string // 已有：技能目录
    DisableActivateSkill bool   // 新增：默认 false；true 时不注册 activate_skill 工具
}
```

`assistant.New` 内部：
- `SkillsDir` 非空时：扫描生成 `Skill` 列表（含新字段）
- 当 `DisableActivateSkill == false`：注册 `ActivateSkill{Skills: skills}`
- `SystemAssemblerConfig.SkillsDir` 同路径

## 11. 测试策略

### 11.1 单元测试（`agent/skills_test.go`）

- `TestParseFrontmatter_Full` — 完整 8 字段解析
- `TestParseFrontmatter_Minimal` — 仅 `name` / `description` / `schema_version`（向后兼容）
- `TestParseFrontmatter_BadYAML` — 格式错误不 panic，返回 ok=false
- `TestLoadSkills_SubDir` — 子目录递归（保留旧行为）
- `TestSkill_PathMatch` — glob 匹配当前 working dir

### 11.2 单元测试（`agent/skills_activate_test.go`）

- `TestActivateSkill_NotFound` — 未知 name 返回 error
- `TestActivateSkill_Success` — 写入事件 + 返回 body
- `TestActivateSkill_ToolSubset_Union` — 多次激活后 AllowedTools 取并集

### 11.3 driver 集成测试（`agent/driver/react/react_skills_test.go`）

- `TestReact_SkillAllowListBlocks` — 工具未声明时返回 tool error
- `TestReact_SkillDeactivateOnTurnEnd` — 轮结束自动 deactivate
- `TestReact_AllowedToolsUnion` — 多 Skill 激活后工具并集生效

### 11.4 端到端（`assistant/e2e_skills_test.go`）

- `TestE2E_ActivateSkillFlow` — mock 模型第一轮调 `activate_skill`，第二轮按 Skill 指引调 `git`，断言 `skill/load` → `skill/activate` → 工具调用 → `skill/deactivate` 顺序
- `TestE2E_AllowedToolBlocked` — 声明 `allowed-tools: [read_file]` 后模型调 `shell` 返回错误

## 12. 文档同步

- `docs/architecture-v2.md` §3.3 修订 Skills 描述：补 frontmatter 契约 + activate_skill 工具 + 事件 + 工具子集
- `FEATURES.md` §3.3 "系统提示词动态组装" 项后新增独立段："Skills 强化（v1）" 列 8 字段 + 工具 + 事件
- `README.md` Skills 章节：补 activate_skill 工具示例

## 13. 兼容性

- 旧 SKILL.md（仅 `name` / `description`）：新解析路径把缺失字段置为零值（`SchemaVersion` 留空 → 视为不兼容字段但仍可加载，向下兼容）
- 旧 `LoadSkills` 调用方：返回的 `Skill` 新增字段为零值，旧访问方式不变
- 旧 `renderSkills`：替换为新实现（同时支持 `Skill.AllowedTools` / `Skill.References` 为空时降级为旧输出）

## 14. 关键文件

```
agent/skills.go              # 解析 + 数据结构扩展
agent/skills_test.go         # 单元测试扩展
agent/event/events.go        # +3 常量 +3 payload
agent/observer.go            # +EventSkillLoad/Activate/Deactivate 观测枚举
agent/driver/react/react.go  # 轮结束 deactivate + executeTool 前置校验
agent/driver/react/react_skills_test.go  # driver 集成测试
assistant/config.go          # +DisableActivateSkill
assistant/assistant.go       # 自动注册 activate_skill
assistant/builtin/activate_skill.go  # 新工具实现
assistant/e2e_skills_test.go # E2E 测试
docs/architecture-v2.md      # §3.3 修订
FEATURES.md                  # §3.3 增段
README.md                    # Skills 章节更新
```

## 15. 不在本设计范围

- `model` / `agent` / `hooks` 字段（与现有机制重叠）
- marketplace / 安装 / 认证（平台层）
- Skill 间依赖锁 / 版本（YAGNI）
- 跨会话 Skill 状态（仅单轮）
- 用户斜杠命令面（CLI 不在 SDK 范围）
- Skill 嵌套（一个 Skill 引用另一个 Skill）