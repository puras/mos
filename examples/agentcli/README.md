# agentcli：七站式智能体运行时学习 CLI

`agentcli` 是 `agent/` 目录的教程式示例：把"由浅入深理解智能体运行时"的
学习路线落成 7 个可运行的站点，**站站并存**——`go run . -stage N` 改个数字
就能对比各站的装配差异。全部站点直接在 driver 层装配（不经 `assistant` 的一
行封装），共享同一个 REPL 骨架（[harness.go](harness.go)），每个站一个文件
（stage0.go … stage6.go），只写该站新增的概念。

## 准备

```bash
cd examples/agentcli
cp .env.example .env
# 编辑 .env，填入 OPENAI_* 或 ANTHROPIC_* 真实配置
```

| 站点 | 主题 | 一句话能力 |
|---|---|---|
| 0 | [最小装配](#第-0-站最小装配黑盒跑通) | RunContext + react.New，黑盒跑通 |
| 1 | [会话与事件](#第-1-站会话与事件日志即事实) | append-only 日志 = 唯一事实来源 |
| 2 | [驱动与循环](#第-2-站驱动与-react-循环打开黑盒) | 一轮循环的内部结构与状态机 |
| 3 | [钩子与守卫](#第-3-站钩子与守卫) | HookBus 可改写 vs GuardBus 不可绕过 |
| 4 | [上下文工程](#第-4-站上下文工程五件套) | 组装器/截断/压缩/审计/技能 |
| 5 | [审批挂起与恢复](#第-5-站审批挂起与恢复) | SuspendError → checkpoint → Resume |
| 6 | [装配与治理](#第-6-站装配与治理) | seam + YAML 装配 / 混合路由 / 遥测 |

通用开关：`-p openai|anthropic` 切协议，`-stream` 流式输出。

---

## 第 0 站：最小装配，黑盒跑通

```bash
go run . -stage 0
> 现在几点了？
> 看看 temp 目录里有什么
```

只做三件事：构造 `driver.RunContext`（工具 + 规划器 + 文件 jail）→
`react.New(rt)` 默认装配 → REPL 里 `agent.Run`。启动横幅打印装配清单。

**观察点**：模型自主决定是否调工具，最终只打印回复——中间过程是黑盒。

**自测**：`react.New` 默认装配了哪些钩子和守卫？（读 [../../agent/driver/react/react.go](../../agent/driver/react/react.go) 的 `InstallDefaults`）

## 第 1 站：会话与事件（日志即事实）

```bash
go run . -stage 1
> 帮我在 temp 里写一个 hello.txt
/events      # 事件日志：Seq / 类型 / 载荷摘要
/messages    # 投影出的模型消息历史（与 /events 对照）
/state       # TaskState：Goal / Status / Facts
/restore <id> # 从 jsonl 重放恢复另一个会话
```

会话挂 JSONL 持久化（`data/sessions/<id>.jsonl`，首行 header + 每行一条事件）。

**观察点**：消息历史是 `deriveMessages` 从事件**投影**出来的视图；
`assistant/chunk`（流式增量）同轮有 `assistant/message` 时被其消费。

**自测**：为什么"进模型面的消息必须能从事件重建"是不变式？`TaskState.Facts`
谁能写、谁不能写？

## 第 2 站：驱动与 ReAct 循环（打开黑盒）

```bash
go run . -stage 2
> 查看当前目录有哪些 go 文件，统计总数
go run . -stage 2 -max-turns 1   # 轮次保险丝：1 轮没做完就 blocked
```

`rt.Observer` 实时打印事件流（思考 → 工具调用/结果 → 完成），跑完打印任务
状态机状态。

**观察点**：一轮 = step/start → assistant/message → tool/call×N →
tool/result×N → step/end；`-max-turns` 触发后状态 pending→running→blocked。

**自测**：模型不调工具直接回答时，循环怎么终止？`turn/end` 的 reason 有哪些？

## 第 3 站：钩子与守卫

```bash
go run . -stage 3
> 帮我读取 temp/hello.txt            # 观察 pre-step 注入 + post-execute 前缀
> 用 shell 删除 temp 下的任意文件     # rm 被守卫拦截 → 失败回填，模型换路
go run . -stage 3 -doom 2            # 相同工具+参数连续 2 次 → blocked
```

本站注册了两个改写钩子（pre-step 注入消息、post-execute 加前缀）和一个单调
守卫（拦截 rm）。守卫走 `GuardBus`，上面的钩子**无法**把它改写掉。

**观察点**：守卫拦截不是终止任务——拒绝原因作为工具结果回填给模型；
钩子可短路可改写，守卫按注册顺序裁决且不可绕过。

**自测**：为什么守卫决策发生在 `tool/call` 日志**之前**？（提示：审批挂起
时不落日志）

## 第 4 站：上下文工程五件套

```bash
head -c 20000 /dev/urandom | base64 > temp/big.txt   # 造个大文件
go run . -stage 4
> 读一下 temp/big.txt                    # 观察 [截断] 与逃生通道提示
> 激活 demo 技能，然后做个自我介绍         # activate_skill → 约束生效
/system                                  # 打印当前轮组装的系统提示词分段
/facts                                   # 打印审计通过的已验证事实
```

- **组装器**：槽位式系统提示词（约束 + 任务/状态/工具签名 + 技能清单），每轮重组
- **截断器**：故意调低到 2KB/40 行；超限完整输出落 `data/escape/`，提示模型回读
- **压缩器**：保留最近 6 条；多轮长对话后 `/events` 里出现 `compaction/summary`
  （摘要取自 TaskState，不是 LLM 总结）
- **审计门**：只有只读工具输出才写入 Facts，模型自述不算数
- **技能**：`skills/demo/SKILL.md`（frontmatter 清单懒加载，激活后白名单生效）

**自测**：压缩摘要为什么不用 LLM 总结历史？激活技能的工具白名单靠钩子还是守卫执行？

## 第 5 站：审批挂起与恢复

```bash
go run . -stage 5
> 看看 temp 目录里有什么文件
── [审批] 工具=shell ... 批准? (y/N): y
```

本站写了一个 ~20 行的自定义策略：shell 一律返回 `*approval.RequiredError`
（软拒绝）。驱动据此发起审批并抛 `SuspendError` 挂起 → checkpoint 落盘 →
终端问答 → `Handle.Resume` 从挂起轮**直接续跑（跳过 LLM 调用）**。

**观察点**：`/events` 里 approval/request → checkpoint/save →
checkpoint/restore 的事件序列；选 n 时决策作为失败结果回填，模型换路继续。

**自测**：Resume 后为什么不需要重新调 LLM？未配置 `Approval` 时同样场景的
行为是什么？（fail-closed 拒绝）

## 第 6 站：装配与治理

```bash
go run . -stage 6
> 现在 temp 里有哪些文件          # 主 REPL 走 agentcli.yaml 装配的 assistant
/spans                           # span 树：driver.turn / llm.request / tool.execute
/route 先规划：调研 temp 再写摘要   # 规则命中 → plan-execute
/route 帮我写个冒泡排序            # 规则未命中 → LLM 分类兜底（react）
```

- **seam + config**：`agentcli.yaml` + `SeamRegistry` 按名解析组件 →
  `config.Assemble` 产出可运行智能体——换驱动只改 YAML 的 `driver` 字段
- **router**：`HybridRouter` 规则硬路由（"先规划/反思/脚本"前缀）优先，
  LLM 分类器兜底，`/route` 打印选中驱动的具体类型
- **telemetry**：内存 `Recorder` 记录 span 树，`/spans` 查看

**自测**：seam 工厂为什么返回 `any`？路由选出的裸驱动什么时候补齐默认装配？

---

## 目录结构

```
agentcli/
├── main.go        # flag 解析 + 站点分发
├── harness.go     # 共享骨架：双协议模型、RunContext 基座、REPL、事件打印
├── stage0..6.go   # 各站装配差异（自包含、可独立阅读）
├── agentcli.yaml  # 第 6 站的声明式装配配置
├── skills/demo/   # 第 4 站的示例技能
└── data/          # 运行产物（sessions / checkpoints / escape），不入仓
```
