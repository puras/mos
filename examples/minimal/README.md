# minimal —— 装配层的最小可跑示例

这个示例存在意义是**证明装配层够用**：一个真实可跑的智能体，装配只要十几行；
其余都是宿主自己的事（选工具、策略、审批问人）。

对比 `examples/chat`：那边是完整演示（策略、审批、shell、压缩、截断全开），
这边只留"最小可用"，方便看清装配层到底替你做了什么。

## 跑法

```bash
cd examples/minimal
export MOS_PROVIDER=anthropic          # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...         # 按协议取对应的那把
export MOS_MODEL=agnes-2.5-flash        # 模型名
export MOS_BASE_URL=http://192.168.4.20 # 可选：网关地址
go run . "只回答：1+1=?"
```

这个示例**不读 `.env`**（框架不碰环境变量，配置来源归宿主）——环境变量自己导出。
`examples/chat` 里有自己的 `.env` 加载器，可作参考。

## 装配做了什么（`mos.New`）

```
MoS chat 已就绪
  已注册工具: current_time, read_file, write_file, edit_file
  模型可见工具: current_time, read_file, write_file, edit_file
  并发上限: 4（仅对声明可并发的工具生效）
  决策轮次上限: 不限（上下文增长由压缩接管）
  工作目录: ...
  shell: 未装配（模型看不到这个能力）
  上下文压缩: 已装配（超预算时模型生成结构化摘要落日志）
  结果截断: 32768 字节（全文落 .../.mos-trunc，read_file 可回读）
  参数级规则: 无
  名字级策略: 未装配（名字级名单不收紧）
```

这份横幅是 `a.Describe()` 给的——**装了什么、没装什么都是事实**（"未装配"/
"一律拒绝"都明说），宿主不用各写一份。

## 宿主的四件事

| 宿主负责 | 代码位置 |
| --- | --- |
| 选模型与凭据 | `mos.NewModel(mos.ModelConfig{...})`（重试次数、日志回调也是宿主的参数） |
| 选工具 | `tool.NewRegistry()` + `builtin.Default()` + `builtin.FileToolsIn(".")` |
| 会话与落盘 | `session.Open("data", "minimal")` |
| 策略与审批 | `mos.Config.Policy` / `Rules` / `Approval`（本示例没配） |
