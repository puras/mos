# MoS v0.2.0 — Skills 与 MCP 接入

v0.1.0 交付了「装配一次就能跑」的骨架：统一 LLM 接入、声明式工具系统、事件日志会话、可插拔驱动、四层安全纵深。**v0.2.0 补上两个最缺的能力面——Skills（把知识装进模型）与 MCP（把外部工具接进来）**，并修掉若干处「看起来能用、实际失效」的缺陷。

> 标签：`v0.2.0`（该标签处 `mos.Version == "0.2.0"`；`main` 上的开发版本为 `0.3.0`）。

## 新增

### Skills 重做（S8.1 / S8.4-C / S8.4-D）

- **独立包 `agent/skill`**：目录发现 + frontmatter（`name` / `description`）解析；模型侧**只暴露一个专用 `skill` 工具**，正文按需加载（不预塞进上下文）
- **清单进静态系统提示词**（保 prompt cache），**正文调用时现读**——改了 `SKILL.md` 立刻生效，不必重渲染提示词
- **`Agent.ReloadSkills()`**：显式、幂等、全量重扫（目录结构变了才需要扫）
- 技能目录列表 = **宿主授权边界**（不再要求把技能目录登记成文件读根）；读操作走注入的 `Reader`
- `skill/activate` 事件留痕；装配入口 `mos.Config.Skills`
- `allowed-tools` **求交诊断**（S8.4-D）：声明的白名单与当前可见工具集求交，缺失写进新事件 `skill/allowed-tools-mismatch` 与 `Describe()` 快照——**只诊断、不收窄**（工具面不变，诊断也不进模型可见的返回值）

### MCP 接入（S8.2 / S8.3 + S8.4-A / B / E）

- **官方 Go SDK v1.8.0**（拍板：不手写协议层），协议基线 `2026-07-28`
- **新包 `tool/mcp`**：stdio 与 streamable-http 两个 transport；SDK 类型不外泄
- 工具命名 `mcp__<server>__<tool>`，schema 直通、结果投影；二阶段**原子换代**（换代失败保留上一代）
- 装配入口 `mos.Config.MCP`：一开即装、**默认 deferred**（先经 `tool_search` 激活再声明）、hidden 档、**失败隔离**（单台连不上只记 warning，不拖垮其余服务器）
- **服务器摘要进静态系统提示词**（S8.4-A）：`mcp_servers` 节，整节 ≤4KB、单条 ≤250 字节，渲染层自己排序保证确定性
- **认证可诊断**（S8.4-B）：`ServerConfig.TokenProvider`（每次请求取一次，不缓存不刷新）、凭据注入 HTTP RoundTripper、新错误分类 `ErrorKind: auth`（HTTP 401/403 与取凭据失败）
- **资源三件套**（S8.4-E）：`mcp_resource_list` / `mcp_resource_templates` / `mcp_resource_read`（显式 `server` 参数；工具面恒为 0 或 3，不随服务器数增长；hidden 服务器的资源不泄露）

### 其他

- **工具层前置**：`Registry.Replace` 原子换代、`SchemaProvider`、可见性叠加
- **摘要调用**：独立超时、取消原因可分辨、压缩失败策略可选
- **沙箱路径强制后端**：Linux Landlock + Windows 低完整性 token
- **新示例**：[examples/mcp](https://github.com/puras/mos/tree/v0.2.0/examples/mcp)（stdio 与 streamable-http）、[examples/skills](https://github.com/puras/mos/tree/v0.2.0/examples/skills)

## 修正

1. **装配期 `Config.Policy` 的守卫从未挂上（fail-open）**——装配中途 `a.rt` 还是空壳（`Guards == nil`，`GuardBus.Add` 是 no-op），拒绝名单里的工具「模型看不见却调得动」。现在策略在 `a.rt` 装好之后再挂，与运行期热替换走同一条路。
2. **被策略挡住的 deferred 工具仍能被 `tool_search` 搜到**——搜出来、激活，最后被守卫拦死。`Registry.DeferredTools()` 与 `List()` 现在同口径：**「看不见」与「搜不到」一致**。
3. **deferred 工具激活后要多等一轮才可见**——驱动每轮重取声明清单，改为**同一轮的下一次请求**即声明（原先要等下一个 turn）。
4. **没配凭据时 401/403 不归认证类**——认证层改为无条件装上；分类按 HTTP 状态码判定，不解析错误文本。
5. **MCP 服务器串行连接**——`Manager.Start` 每台一个 goroutine，慢的不再拖住全部就绪；单台失败仍然隔离。
6. **技能清单预算漏接线**——`MaxCatalogBytes` 之前没接到装配层，超预算时描述不会被裁掉。

## 破坏性变更与迁移

- **`agent/skill`**
  - `Skill.Body`（字符串）→ `Skill.BodyPath`（路径，正文现读）
  - `Agent.ExpandSkill(text)` / `ExpandInvocation` **增返回 `error`**：正文读不出时 fail-closed（不再降级成普通消息）
  - `Agent.ReloadSkills()` 增返回 `error`（幂等、全量重扫）
- **`tool`**：`Registry` 增 `Replace` / `SchemaProvider` / `Visible`（新增 API，既有调用不受影响）
- **依赖**：直接依赖 3 → 6；`github.com/google/uuid` 不再需要（沙箱路径强制后端换掉了它）；新增官方 MCP SDK v1.8.0 及其传递依赖（`go.sum` 涉及的模块由 4 个增至 18 个；`go list -m all` 为 20——多出的是主模块本身与本地 `replace` 的 `moproto`）

## 依赖

- [`github.com/puras/moproto`](https://github.com/puras/moproto) v0.1.0 — 协议类型 + 用量统计
- [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) v1.8.0 — 官方 MCP 客户端（仅在 `tool/mcp` 内部使用）
- [`github.com/landlock-lsm/go-landlock`](https://github.com/landlock-lsm/go-landlock) v0.11.0 — Linux 路径强制
- [`github.com/SivanCola/windows-sandbox`](https://github.com/SivanCola/windows-sandbox) — Windows 路径强制
- `golang.org/x/sys` v0.48.0、`gopkg.in/yaml.v3` v3.0.1

## 快速开始

```bash
go get github.com/puras/mos@v0.2.0
```

最小示例见 [examples/minimal](https://github.com/puras/mos/tree/v0.2.0/examples/minimal)，完整演示（策略、审批、shell、压缩、截断全开）见 [examples/chat](https://github.com/puras/mos/tree/v0.2.0/examples/chat)，MCP 与 Skills 各自的可跑示例见 [examples/mcp](https://github.com/puras/mos/tree/v0.2.0/examples/mcp) 与 [examples/skills](https://github.com/puras/mos/tree/v0.2.0/examples/skills)。

```bash
cd examples/mcp        # 或 examples/skills
export MOS_PROVIDER=openai
export OPENAI_API_KEY=sk-...
export MOS_MODEL=your-model-name
go run . "你的任务"
```

## 文档

- [docs/PRD.md](https://github.com/puras/mos/tree/v0.2.0/docs/PRD.md) — 产品需求（要什么）
- [docs/ARCHITECTURE.md](https://github.com/puras/mos/tree/v0.2.0/docs/ARCHITECTURE.md) — 目标架构与不变式，含每个子系统契约
- 一页说明：[Skills](https://github.com/puras/mos/tree/v0.2.0/docs/s8.1-skills.md) ｜ [MCP](https://github.com/puras/mos/tree/v0.2.0/docs/s8.2-mcp.md) ｜ [可用性优化](https://github.com/puras/mos/tree/v0.2.0/docs/s8.4-usability.md) ｜ [SubAgent](https://github.com/puras/mos/tree/v0.2.0/docs/s8.5-subagent.md)

## 许可证

[MIT](https://github.com/puras/mos/blob/v0.2.0/LICENSE)

## 尚未包含（路线图）

- **SubAgent 实现**（FR-D3）：设计与六条拍板已定（agents-as-tools + 声明式 frontmatter、同步交付、工具名 `subagent__<name>`、白名单与父策略取交集 deny 优先、模型覆盖缺失装配期报错、深度 0），实现待做（S8.5-A/B/C/D）
- **挂起-恢复**（FR-G3）：已重裁为「**日志即 checkpoint**」——从会话日志派生未决审批与悬空调用，宿主显式指示后续跑那一次工具调用；实现待做（S9.1~S9.3）
- **M3b**：AgentTeam（FR-D6）、并行 DAG（FR-D4/D5）、合约 + Elo（FR-D7/D10）
- **M3a 其余**：FR-D1 / FR-D2 / FR-D9
- **M4**：FR-E1~E10（含 `runs/<run-id>` 可恢复运行目录）
- Docker 强隔离后端（当前路径强制 + 进程沙箱仍属纵深防御的一层，不是强隔离边界）
