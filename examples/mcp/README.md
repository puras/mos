# mcp —— 接一台 MCP 服务器，工具经 `tool_search` 搜到才进清单

MCP 工具和平常工具**没有区别**：进同一张注册表，策略、审批、守卫、并发、截断
全都照常管它们。区别只在"什么时候声明给模型"：

| 暴露档 | 模型清单 | `tool_search` 搜得到 | 程序化调用 |
| --- | --- | --- | --- |
| `deferred`（默认） | 搜到之后 | ✅ | ✅ |
| `direct` | 一直有 | — | ✅ |
| `hidden` | 从来没有 | ❌ | ✅ |

默认 `deferred` 的理由是工具面：一台服务器常见几十个工具，全量声明会挤爆提示词。

## 跑法

```bash
cd examples/mcp
export MOS_PROVIDER=openai                  # openai / anthropic / gemini
export ANTHROPIC_API_KEY=sk-...              # 按协议取对应的那把
export MOS_MODEL=gpt-4o-mini

go run .                                     # 默认起 npx 的参考服务器（需要 Node）
go run . -- npx -y @modelcontextprotocol/server-everything
go run . -- /path/to/your-mcp-server --flag  # `--` 之后是服务器命令
```

`--` 之后的内容原样当作子进程命令。示例**不读 `.env`**（配置来源归宿主）。

没装 Node 也能跑：连不上只会让这个例子没有 MCP 工具，**装配照样成功**，快照里
会有一行降级说明 —— 单台服务器连不上不影响其余服务器，也不炸装配。

## 关键装配（四行）

```go
a, err := mos.New(mos.Config{
    Model: model,
    Tools: reg,
    MCP: &mos.MCPConfig{
        Servers: []mcp.ServerConfig{{
            Name:    "demo",            // 命名空间：公开工具名 mcp__demo__<tool>
            Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-everything"},
            // Exposure 留空 = deferred；ExposureDirect = 全量声明
        }},
    },
})
defer a.Close()                          // stdio 是子进程：用完必须收掉
```

## 跑起来会看到什么

1. `a.Describe()` 的装配快照：连上哪台、装了多少工具、有谁降级；
2. `Tools.DeferredTools()`：已注册但**还没声明给模型**的工具（MCP 工具默认都在这里）；
3. 一次真实对话：模型先 `tool_search` 搜到工具，再调用。search 命中即激活，
   **下一次请求**才会把这些工具声明给模型。

## 还可以换的连接方式

| 想做的 | 改哪里 |
| --- | --- |
| streamable-http 远端服务器 | `ServerConfig{Transport: mcp.TransportHTTP, URL: "https://..."}` |
| 带上宿主的凭据 | `Headers`（静态）或 `TokenProvider`（每次请求取一次，不缓存） |
| 单独藏起某个工具 | `ToolExposure: map[string]mcp.Exposure{"danger_tool": mcp.ExposureHidden}` |
| 换掉连接实现（走自己的网关） | `MCPConfig.Dial` |
| 服务器说明超预算时的行为 | `MCPConfig.MaxInstructionBytes`（默认 32KB，超了整段省略并写明） |

## 参考

- 包：`tool/mcp`（`Manager` / `Client` / 三个共享资源工具）
- 装配层：`mos.Config.MCP`（`mos.go` 的 `MCPConfig`）
- 一页说明：[docs/s8.2-mcp.md](../../docs/s8.2-mcp.md)；设计与 PI / DSH 对账：
  `docs/ARCHITECTURE.md` §3.16