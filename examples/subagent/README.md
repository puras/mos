# subagent —— 子代理目录接进来，主模型点名派活

子代理 = 一个子代理目录里的 `AGENT.md`（frontmatter：`name` / `description` /
可选 `model` / `tools` + 人设正文）。这个示例演示五件事：

1. **清单进提示词、正文不进**：`name` 与 `description` 在装配期渲染进静态系统
   提示词（进门就看得见，且不随调用变化 —— 保 prompt cache）；人设正文在**调用
   时现读**，改完 `AGENT.md` 下一次调用立刻生效，不用重启。
2. **子代理 = 一个普通工具**：每个子代理注册成 `subagent__<名字>`，主模型用
   function calling 点名调用；回执是**固定结构** `{status, summary, findings[]}`，
   作为普通工具结果回填父上下文 —— 子代理失败不会让父轮次失败。
3. **工具面收窄**：子代理实际能用的工具 = **声明白名单 ∩ 父可见工具集**
   （再恒定扣掉 `subagent__*` 与 `tool_search`：深度 0，不许再委派、工具面
   不经 `tool_search` 自己扩容）。声明**不能放大**宿主策略。
4. **独立子会话**：子代理的中间推理与工具调用留在**自己的会话**里（同一持久化
   后端，id 形如 `<父会话>-sub-<名字>-<序号>`），不进父上下文 —— 父只拿到那段
   回执。
5. **子代理自述 ≠ 事实**：`summary` / `findings` 是子代理自己说的，父模型要自己
   复核（NFR-5）。

## 跑法

```bash
cd examples/subagent
export MOS_PROVIDER=openai                  # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...              # 按协议取对应的那把
export MOS_MODEL=gpt-4o-mini
go run . "看一下 agent/skill 这个包有没有 bug"
```

示例**不读 `.env`**（配置来源归宿主），环境变量自己导出。

## 关键装配（三行）

```go
a, err := mos.New(mos.Config{
    Model:     model,
    Tools:     reg,
    SubAgents: &mos.SubAgentConfig{Dirs: []string{"agents"}}, // ← 授权边界
})
```

子代理目录里的 `agents/reviewer/AGENT.md` 就是子代理本体。改它、重跑，行为
和提示词都会跟着变，不需要动 Go 代码。

## 模型覆盖（可选）

frontmatter 里写 `model: <模型 id>`，子代理就**不走父的当前模型**。宿主模型
目录里没有这个 id → **装配期报错**（声明了却没装 = 假能力，不静默降级到父模型）。
这一例是单模型模式，所以两个子代理都没写 `model`（跟随父的模型）。

## 快照里能看到什么

`a.Describe()` 会写明装了几个子代理、每个的名字 / 描述 / 白名单 / 模型覆盖、
拆分判据诊断（instructions / tools / model 三者全同 → 「拆了只是多一层开销」）
与装配期告警 —— 降级不静默。

## 参考

- 子代理包：`agent/subagent`（`Definition` / `Catalog` / `Runner` / `Result` /
  `ChildRegistry`）
- 装配层：`mos.Config.SubAgents`（`mos.go` 的 `SubAgentConfig`）
- 设计与 PI / DSH 对账：`docs/ARCHITECTURE.md` §3.18；一页说明
  [docs/s8.5-subagent.md](../../docs/s8.5-subagent.md)
