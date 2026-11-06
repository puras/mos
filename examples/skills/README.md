# skills —— 技能目录接进来，正文按需现读

技能 = 一个目录里的 `SKILL.md`（frontmatter：`name` / `description` / 可选
`when-to-use`）。这个示例演示三件事：

1. **清单进提示词、正文不进**：`name` 与 `description` 在装配期渲染进静态系统
   提示词（进门就看得见，且不随调用变化 —— 保 prompt cache）；正文只在模型调
   `skill` 工具时从磁盘现读，改完正文下一次调用立刻生效，不用重启。
2. **目录列表就是授权边界**：`SkillConfig.Dirs` 声明的目录框架才读，别的一概不碰。
3. **宿主侧手势**：`/技能名 参数` 由宿主调 `ExpandSkill` 就地展开正文，不经过
   模型判断（省一次往返，也省掉"模型没读出该用哪个技能"的不确定性）。正文读
   不出来时 `ExpandSkill` 返回错误 —— 那是**本轮失败**，不是"当普通消息发出去"。

## 跑法

```bash
cd examples/skills
export MOS_PROVIDER=openai                  # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...              # 按协议取对应的那把
export MOS_MODEL=gpt-4o-mini
go run . "帮我把 app 部署到测试环境"          # 模型自己决定要不要读技能正文
go run . "/deploy app 测试环境"               # 宿主手势：正文直接展开
```

示例**不读 `.env`**（配置来源归宿主），环境变量自己导出。

## 关键装配（三行）

```go
a, err := mos.New(mos.Config{
    Model:  model,
    Tools:  reg,
    Skills: &mos.SkillConfig{Dirs: []string{"skills"}}, // ← 授权边界
})
```

技能目录里的 `skills/deploy/SKILL.md` 就是技能本体。改它、重跑，行为和提示词
都会跟着变，不需要动 Go 代码。

## 快照里能看到什么

`a.Describe()` 会写明装了几个技能、清单占了多少字节、有没有被跳过的（目录不存在、
正文超限、`allowed-tools` 与工具面对不上等诊断都在里面 —— 降级不静默）。

`a.Skills()` 给出**已装技能**的清单（名字 + 描述）；技能集是装配期固定的，
运行期只重新读正文（`a.ReloadSkills()` 会刷新清单与诊断，但不改已发出的系统提示词）。

## 参考

- 技能包：`agent/skill`（`Skill` / `Reader` / `Load` / `Reload`）
- 装配层：`mos.Config.Skills`（`mos.go` 的 `SkillConfig`）
- 设计与 PI / DSH 对账：`docs/ARCHITECTURE.md` §3.15；可用性优化（正文现读、
  `allowed-tools` 诊断）见 [docs/s8.4-usability.md](../../docs/s8.4-usability.md)