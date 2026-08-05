# MoS

MoS stands for Sapient, representing a wisdom‑minded agent base.
**MoS 取自 Sapient，寓意面向心智智慧的智能体底座。**

MoS 是一个基于 Go 的智能体基础框架（Agent Base），提供统一的 LLM 接入、工具（函数调用）系统、会话记忆与可插拔的决策规划，帮助快速构建智能体应用。

## 特性

- **多协议 LLM 接入**：统一 `Model` 抽象（模型对象）+ 协议注册表，内置 OpenAI 兼容（DeepSeek / 通义千问 / GLM / Ollama 等）与 Anthropic Claude 原生协议适配器，支持函数调用与流式输出
- **声明式工具系统**：结构体 tag 自动生成 JSON Schema，模型函数调用参数自动校验与解析
- **可插拔规划**：默认 ReAct 循环，可通过 `planner.Planner` 接口替换为其他策略
- **会话记忆**：统一的 `memory.Memory` 接口，内置内存实现；长期向量记忆（sqlite-vec）规划中
- **事件观测**：`Observer` 回调暴露工具调用与完成事件，便于流式输出与可观测性扩展
- **Harness Engineering（工具安全防护）**：`agent.Policy` 在每次工具调用执行前拦截检查，内置默认策略可拦截危险 Shell 命令、路径越界与非 http 抓取
- **Loop Engineering（循环可观测）**：每轮循环暴露思考/工具调用/工具结果/完成事件（含迭代序号），完整还原推理过程
- **开箱即用的智能体**：`assistant` 包组装好模型对象 + 内置工具集 + 安全策略，附带交互式 CLI
- **零第三方依赖**：仅使用 Go 标准库

## 快速开始

```go
// 0. 创建模型对象（实现 llm.Model 即可，见下方"LLM 接入"）
model := ... 

// 1. 注册工具：结构体定义参数，tag 自动生成 JSON Schema
type calcParams struct {
	A int `json:"a" mos:"desc=加数"`
	B int `json:"b" mos:"desc=被加数"`
}

type calculator struct{}

func (calculator) Name() string        { return "calculator" }
func (calculator) Description() string { return "计算两个整数之和" }
func (calculator) Parameters() any     { return &calcParams{} }
func (calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

reg := tool.NewRegistry()
reg.Register(calculator{})

// 2. 创建 Agent（ReAct 循环 + 事件观测）
a := agent.New(model, reg,
	agent.WithSystemPrompt("你是助手"),
	agent.WithObserver(func(ev agent.Event) { /* 观测工具调用/完成 */ }),
)

// 3. 执行任务（多轮对话自动保留历史）
sess := agent.NewSession("demo")
reply, err := a.Run(ctx, sess, "请计算 1+2")
```

## 开箱即用的智能体（assistant + CLI）

`assistant` 包提供组装好的智能体：**模型对象注入** + 8 个内置工具 + 默认安全策略，可直接作为库集成到其他工程。框架只认 `llm.Model` 模型对象，不关心模型接入配置：

```go
// 1. 构建模型对象（可用 openai/anthropic 适配器，或任何自定义 llm.Model）
model, err := openai.New(llm.Config{
	"api_key": os.Getenv("OPENAI_API_KEY"),
	"model":   "gpt-4o",
})
if err != nil {
	// ...
}

// 2. 注入模型对象 + 运行配置
a, err := assistant.New(model, assistant.Config{
	WorkDir:      "./workspace",                          // 限制文件类工具的活动范围
	ShellAllowed: []string{"ls", "cat", "pwd"},           // Shell 命令前缀白名单
})
// ...
sess := a.NewSession()
reply, err := a.Run(ctx, sess, "帮我看看 workspace 里有什么文件")
```

运行配置可通过环境变量加载；模型对象从环境变量构建（`internal/config` 的应用层实现，无 API Key 时自动降级为 mock 离线模型对象 `assistant.Mock()`）：

| 环境变量 | 说明 |
| --- | --- |
| `MOS_PROVIDER` | openai / anthropic / mock（默认 openai，无 Key 自动降级 mock） |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | 对应协议的密钥 |
| `MOS_BASE_URL` | 自定义网关地址（兼容 OpenAI 协议） |
| `MOS_MODEL` | 模型名 |
| `MOS_SYSTEM_PROMPT` | 系统引导词 |
| `MOS_MAX_ITERATIONS` | 循环最大迭代次数 |
| `MOS_SHELL_ALLOW` | Shell 命令白名单，逗号分隔 |
| `MOS_WORKDIR` | 文件类工具的工作目录 |

内置工具：`calculator`（表达式求值）、`current_time`（当前时间）、`web_fetch`（网页抓取转纯文本）、`shell`（受限命令执行）、`read_file` / `write_file`（文件读写）、`find_files` / `grep`（文件搜索）。

交互式 CLI（同样支持环境变量配置，无 Key 时以 mock 演示完整工具调用循环）：

```bash
go run ./cmd/assistant
```

```
你 > 计算 (1+2)*3
  ⟳ [第1轮思考] 让我来处理
  ▶ [第1轮] 调用工具 calculator({"expr":"(1+2)*3"})
  ◀ [第1轮] 工具结果: 9
  ▶ [第2轮] 调用工具 current_time({})
  ⟳ [第3轮思考] 计算完成，结果为 9。...
智能体 > 计算完成，结果为 9。...
```

### Harness Engineering：工具安全防护

`agent.Policy` 接口在每次工具调用**执行前**拦截检查，返回错误即拒绝该次调用，并将拒绝原因回填给模型：

```go
type Policy interface {
	Allow(ctx context.Context, call *llm.ToolCall) error
}
```

默认策略 `assistant.NewDefaultPolicy(shellAllowed []string, workDir string)` 提供三层防护：

- **Shell 危险命令防御**：拦截 `rm -rf`、`mkfs`、`dd if=`、`shutdown`、`sudo`、`kill -9` 及管道交给 shell 执行的命令
- **命令前缀白名单**：配置 `ShellAllowed` 后仅允许白名单前缀的命令
- **路径越界防护**：基于 `filepath.Rel` 校验文件/目录操作必须落在 `WorkDir` 内，web_fetch 仅允许 http/https

### Loop Engineering：循环可观测

每轮循环依次发出 `EventStep`（思考）→ `EventToolCall` → `EventToolResult` →（循环）→ `EventDone`（完成），事件携带 `Iteration` 序号，方便流式输出与过程审计。

## LLM 接入

通过协议注册表创建模型对象，协议适配器均实现统一的 `llm.Model` 接口：

```go
reg := llm.NewRegistry()
reg.Register("openai", openai.New)      // OpenAI 兼容协议
reg.Register("anthropic", anthropic.New) // Anthropic 原生协议

model, err := reg.Create("openai", llm.Config{
	"api_key": os.Getenv("OPENAI_API_KEY"),
	"base_url": "https://api.deepseek.com/v1", // 可选，默认 OpenAI 官方地址
	"model":   "deepseek-chat",
})
```

配置键约定：`api_key` / `base_url` / `model`。

## 目录结构

基础库 + 完整智能体布局：**`pkg/` 对外公开 API，`internal/` 仅供仓库内部使用，依赖方向单向 `internal → pkg`**，外部工程只 import `pkg/` 即可集成。

```
mos/
├── pkg/                  # 基础库（对外 API，不依赖 internal）
│   ├── llm/              #   Model 抽象（模型对象）+ 协议注册表
│   │   ├── openai/       #     OpenAI 兼容协议适配器
│   │   └── anthropic/    #     Anthropic 原生协议适配器
│   ├── tool/             #   工具注册表、JSON Schema 生成、参数解析
│   │   └── builtin/      #     内置工具：shell / 文件 / 计算器 / 网页抓取等
│   ├── memory/           #   会话记忆抽象
│   │   └── inmemory/     #     内存实现
│   ├── agent/            #   智能体运行时：任务循环 / Policy / Session / Observer
│   ├── planner/          #   规划策略：Planner 接口 + ReAct 实现
│   └── assistant/        #   组装好的完整智能体：模型对象注入 / 默认策略 / 内置工具集
├── internal/
│   ├── config/            # 应用层：环境变量 → 模型对象 + 运行配置
│   └── cli/               # 应用层 CLI 交互逻辑（REPL）
├── cmd/
│   └── assistant/         # 完整智能体 CLI 薄入口
```

### pkg 与 internal 的划分准则

**`pkg/` 放框架层面的能力，`internal/` 放应用层面的能力。**

`internal/` 是 **Go 语言级约束**：其下包只能被父目录以内的代码 import，外部工程引用会直接编译报错（`use of internal package not allowed`）。判断归属时先问：

1. **这是框架层面的能力，还是应用层面的能力？** 框架层（通用、可复用、有 API 契约）→ `pkg/`；应用层（绑定具体产品的入口、配置约定、交互形态）→ `internal/`
2. **外部工程集成 MoS 时需要直接 import 它吗？** 是 → `pkg/`；仅本仓库自己用 → `internal/`
3. **它是稳定、带契约的公开 API，还是可随意重构的实现细节？** 前者 → `pkg/`；后者 → `internal/`

当前划分示例：

| 归属 | 包 | 理由 |
| --- | --- | --- |
| `pkg/` | `llm` / `tool` / `memory` / `agent` / `planner` | 框架层通用基础能力 |
| `pkg/` | `assistant` | 框架层组装智能体（模型对象注入，`Config` 仅含运行配置） |
| `pkg/` | `llm/openai` `llm/anthropic` `tool/builtin` | 框架层可选实现，外部按需选用 |
| `internal/` | `config` | 应用层：从 `MOS_*` 环境变量构建模型对象与运行配置 |
| `internal/` | `cli` | 应用层：REPL 绑定终端交互 |

注意：框架层 `assistant.Config` 只含运行配置（系统引导、迭代次数、工具安全），**模型以 `llm.Model` 对象形式注入**；"如何从环境变量/配置文件构建模型对象"属于应用层约定（放 `internal/config`）。外部工程可按自己的配置体系构建模型对象并注入。

## 测试

```bash
go test ./...
```

适配器测试通过 `httptest` 模拟协议服务，验证请求构造与响应解析的正确性，不依赖真实网络。

## 路线图

- [x] **M1**：Agent 任务循环、多协议 LLM 接入、工具系统、工作记忆、事件观测
- [x] **M1.5**：组装智能体（assistant 包 + CLI）、Harness Engineering 工具安全防护、Loop Engineering 循环可观测
- [ ] **M2**：长期记忆（sqlite-vec 向量检索）、RAG、流式输出透传
- [ ] **M3**：MCP 接入、多智能体编排
- [ ] **M4**：生产化（插件化、可观测性完善）
