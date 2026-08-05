# MoS

MoS stands for Sapient, representing a wisdom‑minded agent base.
**MoS 取自 Sapient，寓意面向心智智慧的智能体底座。**

MoS 是一个基于 Go 的智能体基础框架（Agent Base），提供统一的 LLM 接入、工具（函数调用）系统、会话记忆与可插拔的决策规划，帮助快速构建智能体应用。

## 特性

- **多协议 LLM 接入**：统一 `Provider` 抽象 + 协议注册表，内置 OpenAI 兼容（DeepSeek / 通义千问 / GLM / Ollama 等）与 Anthropic Claude 原生协议适配器，支持函数调用与流式输出
- **声明式工具系统**：结构体 tag 自动生成 JSON Schema，模型函数调用参数自动校验与解析
- **可插拔规划**：默认 ReAct 循环，可通过 `planner.Planner` 接口替换为其他策略
- **会话记忆**：统一的 `memory.Memory` 接口，内置内存实现；长期向量记忆（sqlite-vec）规划中
- **事件观测**：`Observer` 回调暴露工具调用与完成事件，便于流式输出与可观测性扩展
- **零第三方依赖**：仅使用 Go 标准库

## 快速开始

```go
// 0. 创建 LLM Provider（实现 llm.Provider 即可，见下方"LLM 接入"）
provider := ... 

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
a := agent.New(provider, reg,
	agent.WithSystemPrompt("你是助手"),
	agent.WithObserver(func(ev agent.Event) { /* 观测工具调用/完成 */ }),
)

// 3. 执行任务（多轮对话自动保留历史）
sess := agent.NewSession("demo")
reply, err := a.Run(ctx, sess, "请计算 1+2")
```

完整可运行示例见 [examples/demo](./examples/demo/main.go)（内置 mock Provider，无需 API Key）：

```bash
go run ./examples/demo
```

## LLM 接入

通过协议注册表创建 Provider，协议适配器均实现统一的 `llm.Provider` 接口：

```go
reg := llm.NewRegistry()
reg.Register("openai", openai.New)      // OpenAI 兼容协议
reg.Register("anthropic", anthropic.New) // Anthropic 原生协议

provider, err := reg.Create("openai", llm.Config{
	"api_key": os.Getenv("OPENAI_API_KEY"),
	"base_url": "https://api.deepseek.com/v1", // 可选，默认 OpenAI 官方地址
	"model":   "deepseek-chat",
})
```

配置键约定：`api_key` / `base_url` / `model`。

## 目录结构

```
mos/
├── llm/                  # Provider 抽象 + 协议注册表
│   ├── openai/           #   OpenAI 兼容协议适配器
│   └── anthropic/        #   Anthropic 原生协议适配器
├── tool/                 # 工具注册表、JSON Schema 生成、参数解析
├── memory/               # 会话记忆抽象
│   └── inmemory/         #   内存实现
├── agent/                # 智能体运行时：任务循环 / Session / Observer
├── planner/              # 规划策略：Planner 接口 + ReAct 实现
└── examples/demo/        # 完整链路示例
```

## 测试

```bash
go test ./...
```

适配器测试通过 `httptest` 模拟协议服务，验证请求构造与响应解析的正确性，不依赖真实网络。

## 路线图

- [x] **M1**：Agent 任务循环、多协议 LLM 接入、工具系统、工作记忆、事件观测
- [ ] **M2**：长期记忆（sqlite-vec 向量检索）、RAG、流式输出透传
- [ ] **M3**：MCP 接入、多智能体编排
- [ ] **M4**：生产化（插件化、可观测性完善）
