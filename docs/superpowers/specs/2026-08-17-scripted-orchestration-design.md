# 脚本化编排（Scripted Orchestration）设计

- 日期：2026-08-17
- 状态：设计稿（brainstorming 产出，待实施计划）
- 关联：README 路线图 M3 LLMCompiler 的先行单智能体版；复用 `agent/executor`、`agent/approval`、`agent/checkpoint`

## 1. 背景与动机

DSH（DeepSeek Harness）四种模式分析指出 MoS 现状缺失 **PTC（程序化工具调用）**：模型一次性产出"多步工具编排脚本"，机器按依赖图一次性执行。相比 ReAct 的逐步循环（每步一轮模型调用），脚本化把轮次压到 **2 次模型调用**（写脚本 + 汇报结果），适合步骤多但逻辑清晰的任务（批量重命名、跑一整套流水线），是纯效率特性。

MoS 现状对照：

| 现有能力 | 位置 | 与脚本化的关系 |
| --- | --- | --- |
| `executor.Run` 有界并发批量 | `agent/executor` | 处理"模型一次返回多个 ToolCall"；脚本化每波复用其三阶段 |
| `planexecute` 线性计划 + 每步内嵌 ReAct | `agent/driver/planexecute` | 脚本化是其推广：**带依赖的执行图 + 纯工具调用**（每步不调用模型） |
| `execBatchTools` 批量执行通道 | `agent/driver/react` | 提取为共享通道，脚本化各波复用（见 §4） |

经澄清确定的三条核心语义：

1. **依赖 DAG**：步骤带 `depends_on`，独立步骤有界并发，失败向下游传播（下游跳过、旁支照常）。
2. **纯工具调用**：每步 = 一个 tool call（带命名输出），脚本产出后 **0 次额外模型调用**（区别于 planexecute 的每步内嵌 ReAct）。
3. **合成最终回复**：脚本执行完再做一次模型调用，用结果合成用户可读回复。全程共 2 次模型调用。

## 2. 脚本模型（`planner/script.go`）

```go
// ScriptStep 是脚本中的一步（直接工具调用）。
type ScriptStep struct {
	ID        string         `json:"id"`                   // 步骤唯一标识，供 depends_on / output 引用
	Tool      string         `json:"tool"`                 // 工具名（必须在 Registry 中）
	Args      map[string]any `json:"args,omitempty"`       // 参数，可用 ${id} / ${id.field} 引用上游输出
	DependsOn []string       `json:"depends_on,omitempty"` // 依赖的步骤 ID
	Output    string         `json:"output,omitempty"`     // 命名输出，供下游引用（可选）
}

// ScriptedPlan 是脚本阶段的产出：目标 + 带依赖的步骤图。
type ScriptedPlan struct {
	Goal  string       `json:"goal"`
	Steps []ScriptStep `json:"steps"`
}

// ScriptPlanner 生成执行脚本（Scripted 驱动，架构 v2 §3.3 的第三种计划器）。
// tools 为当前注册的工具清单：模型必须先知道存在哪些工具才能写对 Tool 名，
// 故注入清单（名称 + 一句话描述）防模型编造工具名。
type ScriptPlanner interface {
	Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error)
}
```

- 实现 `planner.NewScript(model)`：提示模型只输出 JSON 脚本；工具名限清单内、每步单意图、`depends_on` 只在有真实依赖时声明、`output` 命名供下游引用。
- 解析回退：直接 JSON → 剥 ```` ```json ```` 围栏 → **失败即报错**（带原始输出）。DAG 无法像步骤列表那样行拆分回退，宁要诚实的解析错误（上层可回退 ReAct），不做猜测性解析。

## 3. 脚本校验（确定性安全闸，`agent/driver/scripted`）

fail-closed：**任一校验不过 → 整份脚本拒绝执行**（不做部分执行），错误连同原因回填给模型。

- ID 唯一且非空
- `Tool` 在 `rt.Tools` 中存在
- `Args` 经 `tool.Parse` 可解析为对应工具的参数
- `DependsOn` 引用已知 ID，无自依赖，无重复边
- 无环（拓扑排序）
- `Output` 命名合法且唯一
- 步数 ≤ 上限（默认 16，可配置；防脚本爆炸 + 防提示词膨胀）

## 4. 共享执行通道（新包 `agent/execpath`）

从 `agent/driver/react` 提取 `execBatchTools` + `executeTool` 为导出函数：

```go
// agent/execpath：单波/单批工具执行的统一安全通道（行为保持搬移）。
// 语义中立于驱动：react 批量与 scripted 各波共用，保证 Policy/审批/日志/审计/截断
// 语义全库唯一。
func ExecuteBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session,
	turn, step int, calls []llm.ToolCall) ([]executor.Outcome[*llm.ToolCall, ToolOutcome], *approval.SuspendError)
```

通道内容（与现状一致）：GuardBus 单调决策（含 Policy 与审批挂起）→ `AppendToolCall` 落日志 + 观测 → 钩子链（pre-execute → execute → post-execute，含截断与审计）→ Outcome 按源序回填。react 的 `execBatchTools` 改为薄封装调用本函数。

- 这是**唯一触及现有代码**的改动，属行为保持的搬移，现有 `batch_test.go` 全量回归覆盖。
- 收益：脚本路径与 react 路径的工具执行语义完全一致，不存在"换驱动行为就变"的裂缝。

## 5. 分波调度（DAG 执行器，`scripted` 驱动内）

```
状态 : status[stepID] ∈ {pending, running, done, skipped, failed, denied}
     outputs[stepID] → any（命名结果）
每轮：
  ready = 满足 status==pending 且 DependsOn ⊆ done 的步骤
  ready 为空 → 跳出；剩余 pending 全部置 skipped（传递依赖失败，不可达）
  ready → 插值 args（${id} / ${id.field}）→ 构 []llm.ToolCall（ID 前缀 "step:<id>"）
  execpath.ExecuteBatch(整波，有界并发，复用 executor 三阶段)
  回填：done → 记 outputs[id]；denied/failed → 其传递依赖全部 skipped
  SuspendError → 存档脚本状态后冒泡（§7）
```

- **每波 = 一个拓扑层**：依赖已满足的步骤天然互不依赖，整波并发正确且简单。
- 波数有界（≤ 步数），循环有安全兜底；ctx 取消贯穿（未开始步骤回填 ctx 错误，下游跳过）。
- **失败隔离**：单步失败不中断整脚本——失败步骤的下游被跳过，但旁支照常执行（DAG 相对线性 planexecute 的核心收益）。
- **插值**：`${id}` → 整个结果（非字符串则 JSON 序列化）；`${id.field}` → 结果 JSON 单层字段查找；引用不存在的结果 → 该步 fail-fast 报错（进失败传播）。

## 6. 驱动流程与合成回复（`agent/driver/scripted/scripted.go`）

遵循生命周期规则"一个 Turn 至多一次模型调用"，全程 2 个 Turn：

```
Turn N   （脚本轮）
  turn/start → user/message
  assistant/message：脚本文本（渲染为可读步骤列表或 JSON，入库保持历史完整）
  script/start 事件 → 逐波执行（tool/call + tool/result 经共享通道入库）→ script/end 事件
  step/end → turn/end（reason: done）
Turn N+1 （合成轮）
  planner.NextAction：模型看到全部工具结果，产出最终回复
  若模型仍想调用工具 → 不执行，取 Content 为回复；Content 为空则报错
    （v1 语义：脚本驱动要求合成轮直接作答，不进入多脚本循环）
  turn/end（reason: done）→ 状态 completed
```

- 新增事件（对齐 planexecute 的 plan 系列）：`script/start`、`script/step`（每步状态迁移）、`script/end`（含执行报告：每步状态 + 命名输出摘要）。
- 日志即事实：脚本执行过程中的每次工具调用照常 `tool/call` + `tool/result` 入库，Seq 连续可重放；会话持久化 / 恢复 / 压缩与现有机制天然兼容。
- 脚本步骤进入 TaskState 的事实仍经审计门（共享通道的 post-execute 审计钩子），模型自述不算数。

## 7. M2.8 审批与断点（复用 `agent/approval` + `agent/checkpoint`）

- 脚本中某步触发审批（如受限危险命令 `rm`）→ 共享通道返回 `*approval.SuspendError`。
- 驱动存档**脚本级状态**：`ScriptedPlan` + `outputs` + `status` + 挂起波的工具调用 → 冒泡 SuspendError。
- Resume：`rt.ResumeCheckpoint` 注入后恢复 `outputs`/`status`，从就绪步骤继续调度——已跑步骤**不重来**。
- **向后兼容**：现有 `checkpoint.Checkpoint` 增加可选字段 `ScriptState *ScriptState`（脚本状态载荷：`ScriptedPlan` + `outputs` + `status` + 挂起波的工具调用）；老 checkpoint 无此字段，照常可读、恢复语义不变。
- 未配置 `rt.Checkpoints` 时：仅记录 approval/request 事件，SuspendError 仍冒泡（无法跨进程恢复，与 react 现状一致）。

## 8. 装配（opt-in，默认不启用）

- `RunContext` 新增字段 `ScriptPlanner planner.ScriptPlanner`（对齐 `PlanPlanner`/`Reflector` 的注入模式；nil 时 scripted 驱动拒绝运行，对齐 reflection 的 fail-closed）。
- `scripted.New(rt)` 默认装配：`NewReAct` 的钩子/守卫 + **不注册 DoomLoopGuard**（脚本中故意重复同一工具调用是合法语义，不应被误判死循环；预算失控由步数上限 + ctx 取消兜底）。
- `router`：可选规则（如输入含"批量/重命名/脚本"→ `DriverScripted`）；规则为显式配置，**不加则行为不变**。
- `agent/config` + `agent/seam`：支持 `driver: scripted` + `script_planner:` 注册装配。

## 9. 影响面（新增特性对现有能力的影响）

- **纯增量**：scripted 驱动 / `NewScript` / script 事件 / `execpath` 包均为新代码；`RunContext` 仅加一个字段，现有驱动忽略之。现有 `react.New` / `planexecute.New` / `reflection.New` 装配一行不改。
- **唯一动旧代码处**：`execpath` 提取（§4），行为保持搬移，`batch_test.go` 回归覆盖。
- **唯一配置性行为变化**：router 规则将匹配输入从 react 改道 scripted；由使用方显式配置，默认不启用。
- **共享通道收益**：脚本路径与 react 路径工具执行语义一致，无行为分叉。

## 10. 测试策略

| 层 | 内容 |
| --- | --- |
| `planner.NewScript` | JSON 解析回退（直接 / 围栏 / 失败报错）；工具清单注入提示词 |
| 脚本校验 | 环 / 未知工具 / 自依赖 / 重复边 / 步数超限 / Output 重复，逐项单测 |
| 分波调度 | 假工具：波序、并发上限、失败传播（下游跳过 / 旁支照常）、插值、插值失败 |
| 集成 | 假模型：产脚本 → 执行 → 合成回复 全链路；DoomLoop 守卫未注册验证 |
| 审批恢复 | 步骤触发审批 → 挂起 → 决策表 resume → 从断点续跑、已跑步骤不重跑 |
| 回归 | `go test ./...` 全量（execpath 重构行为不变） |

## 11. 范围外（v1 明确不做）

- 条件 / foreach 循环 / retry 控制流（依赖 DAG 已覆盖主要效率收益，后续可按需加 `when`）
- 每步内嵌 ReAct（那是 planexecute 的语义，不是 PTC）
- 多脚本循环（合成轮直接作答；"交还模型决定"是后续增强）
