// Package main 演示 llm.ChatModel 统一协议抽象：同一份 REPL 代码
// 经 -p openai / -p anthropic 切换协议与 LLM 直接交互（不经 assistant / 工具层）。
//
// 启动时自动加载同目录 .env 文件（不存在时回退到系统环境变量）。
// 跑法：
//   cd examples/protocli
//   go run .                       # 协议取 .env 的 MOS_PROTOCOL，默认 openai
//   go run . -p anthropic          # 切 Anthropic 原生协议
//   go run . -p openai -stream     # 流式输出（ChatStream 增量打印）
//   go run . -p anthropic -think   # 开思考模式（ReasoningEffort 按协议映射）
//
// 运行中可切换（对下一轮生效）：
//   /stream   切换流式开/关
//   /think    切换思考模式开/关（档位取 -effort）
//   /clear    开新会话（清空多轮历史）
//   q         退出
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/openai"
)

func main() {
	protocol := flag.String("p", "", "协议：openai | anthropic（默认取 .env 的 MOS_PROTOCOL，再默认 openai）")
	stream := flag.Bool("stream", false, "流式输出（模型文本增量实时打印）")
	think := flag.Bool("think", false, "思考模式（ReasoningEffort 按协议映射：openai→reasoning_effort，anthropic→thinking budget_tokens）")
	effort := flag.String("effort", "medium", "思考投入档位：low | medium | high（-think 开启时生效）")
	flag.Parse()

	loadDotEnv(".env")

	if *protocol == "" {
		*protocol = os.Getenv("MOS_PROTOCOL")
	}
	if *protocol == "" {
		*protocol = "openai"
	}
	protocol = strPtr(strings.ToLower(*protocol))

	model, err := newModel(*protocol)
	check(err)

	fmt.Printf("协议 CLI 已就绪。\n")
	fmt.Printf("  协议: %s\n  模型: %s @ %s\n", *protocol,
		os.Getenv(envKey(*protocol, "MODEL")),
		displayBaseURL(*protocol))
	printSwitches(*stream, *think, *effort)
	fmt.Println("输入 q 退出；/stream /think 运行中切换；/clear 开新会话。")

	var history []llm.Message
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())
		switch line {
		case "":
			continue
		case "q", "quit":
			return
		case "/clear":
			history = nil
			fmt.Println("（已开新会话）")
			continue
		case "/stream":
			*stream = !*stream
			printSwitches(*stream, *think, *effort)
			continue
		case "/think":
			*think = !*think
			printSwitches(*stream, *think, *effort)
			continue
		}

		history = append(history, llm.Message{Role: llm.RoleUser, Content: line})
		req := &llm.Request{Messages: history}
		if *think {
			req.ReasoningEffort = effort // flag.String 返回的指针直接复用
		}

		var (
			resp *llm.Response
			err  error
		)
		if *stream {
			resp, err = model.ChatStream(context.Background(), req, func(c llm.StreamChunk) error {
				if c.Reasoning != "" {
					printDim(c.Reasoning)
				}
				if c.Delta != "" {
					fmt.Print(c.Delta)
				}
				return nil
			})
			if err == nil {
				fmt.Println()
			}
		} else {
			resp, err = model.Chat(context.Background(), req)
			if err == nil {
				if rc := resp.Message.ReasoningContent; rc != "" {
					printDim(rc + "\n")
				}
				fmt.Println(resp.Message.Content)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "chat error: %v\n", err)
			// 失败的用户输入撤回，避免损坏历史（下次重试该输入）。
			history = history[:len(history)-1]
			continue
		}
		history = append(history, resp.Message)
		fmt.Printf("[tokens] in=%d out=%d total=%d\n",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}
}

// printSwitches 打印当前开关状态（启动横幅与 /stream /think 切换后）。
func printSwitches(stream, think bool, effort string) {
	fmt.Printf("  开关: stream=%v think=%v", stream, think)
	if think {
		fmt.Printf("（effort=%s）", effort)
	}
	fmt.Println()
}

// printDim 以 ANSI 暗色输出思考内容（终端不支持时显示转义序列，示例可接受）。
func printDim(s string) {
	fmt.Print("\033[2m" + s + "\033[0m")
}

// newModel 按协议构造 llm.ChatModel。统一 Request/Response 结构在不同协议间
// 完全同形——这是 llm 层抽象的核心演示点。
func newModel(protocol string) (llm.ChatModel, error) {
	switch protocol {
	case "openai":
		return openai.New(llm.Config{
			"api_key":  os.Getenv("OPENAI_API_KEY"),
			"base_url": os.Getenv("OPENAI_BASE_URL"),
			"model":    os.Getenv("OPENAI_MODEL"),
		})
	case "anthropic":
		return anthropic.New(llm.Config{
			"api_key":  os.Getenv("ANTHROPIC_API_KEY"),
			"base_url": os.Getenv("ANTHROPIC_BASE_URL"),
			"model":    os.Getenv("ANTHROPIC_MODEL"),
		})
	default:
		return nil, fmt.Errorf("未知协议 %q（支持 openai | anthropic）", protocol)
	}
}

// envKey 拼协议侧环境变量名，如 envKey("anthropic", "MODEL") = ANTHROPIC_MODEL。
func envKey(protocol, field string) string {
	return strings.ToUpper(protocol) + "_" + field
}

// displayBaseURL 取展示用的 base_url（未配置时显示协议默认端点）。
func displayBaseURL(protocol string) string {
	if v := os.Getenv(envKey(protocol, "BASE_URL")); v != "" {
		return v
	}
	switch protocol {
	case "anthropic":
		return "https://api.anthropic.com"
	default:
		return "https://api.openai.com/v1"
	}
}

func strPtr(s string) *string { return &s }

// loadDotEnv 极简 .env 加载器：不引第三方依赖。
// 行为：读 path；忽略空行与 # 注释；解析 KEY=VALUE 注入 os.Setenv（已存在则不覆盖）。
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
