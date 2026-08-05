// Command demo 演示 MoS 智能体底座的完整链路：
// Provider → Planner(ReAct) → Agent 任务循环 → 工具调用 → 事件观测。
//
// 本示例使用内置 mock Provider，无需真实 API Key 即可运行：
//
//	go run ./examples/demo
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// ---- 工具：两数相加 ----

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

// ---- mock Provider：模拟模型行为 ----

type mockProvider struct{}

func (mockProvider) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming
}

func (mockProvider) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		// 已拿到工具结果，给出最终答案
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "1+2 的结果是 " + last.Content,
		}}, nil
	}
	// 首轮：请求调用计算器工具
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "让我用计算器算一下",
		ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}

func (mockProvider) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func main() {
	// 1. 注册工具
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		panic(err)
	}

	// 2. 创建 Agent：ReAct 循环 + 系统引导 + 事件观测
	a := agent.New(mockProvider{}, reg,
		agent.WithSystemPrompt("你是一个乐于助人的助手，需要计算时使用计算器工具。"),
		agent.WithObserver(func(ev agent.Event) {
			switch ev.Type {
			case agent.EventToolCall:
				fmt.Printf("  ▶ 调用工具 %s(%s)\n", ev.ToolCall.Name, ev.ToolCall.Args)
			case agent.EventToolResult:
				fmt.Printf("  ◀ 工具结果: %v\n", ev.ToolResult)
			case agent.EventDone:
				fmt.Println("  ✓ 任务完成")
			}
		}),
	)

	// 3. 执行任务
	sess := agent.NewSession("demo")
	reply, err := a.Run(context.Background(), sess, "请计算 1+2")
	if err != nil {
		panic(err)
	}

	fmt.Println("Agent:", reply)
	fmt.Printf("会话历史共 %d 条消息\n", len(sess.History))
}
