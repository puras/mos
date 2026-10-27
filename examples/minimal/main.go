// MoS 最小示例：一行装配出一个能跑的智能体。
//
// 这个文件的存在意义是**证明装配层够用**：一个真实可跑的智能体，装配只要
// 十几行；其余都是宿主自己的事（选工具、策略、审批问人）。
//
// 跑法：
//
//	cd examples/minimal
//	cp ../chat/.env.example .env   # 填入 API Key 与模型名
//	go run . "只回答：1+1=?"
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/puras/mos"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

func main() {
	provider := env("MOS_PROVIDER", "openai")
	keyEnv := mos.APIKeyEnv(provider)
	if keyEnv == "" {
		fmt.Fprintf(os.Stderr, "未知协议 %q\n", provider)
		os.Exit(1)
	}

	// ① 模型：协议 + 凭据 + 重试（参数是宿主的，机制是框架的）。
	model, err := mos.NewModel(mos.ModelConfig{
		Provider: provider,
		APIKey:   os.Getenv(keyEnv),
		BaseURL:  env("MOS_BASE_URL", ""),
		Model:    env("MOS_MODEL", "gpt-4o-mini"),
		Retry:    llm.RetryPolicy{MaxAttempts: 3},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// ② 工具：一套文件工具（jail 根 = 当前目录，写前观察门开启）。
	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fileTools, err := builtin.FileToolsIn(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, tl := range fileTools {
		if err := reg.Register(tl); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// ③ 装配：一次装好模型、工具、安全、上下文工程。
	a, err := mos.New(mos.Config{
		Model: model,
		Tools: reg,
		// 路径强制范围：只能收窄，不能放宽（默认从工作目录派生）。
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(a.Describe())

	// ④ 会话 + 跑一轮。
	sess, err := session.Open("data", "minimal")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	q := strings.Join(os.Args[1:], " ")
	if q == "" {
		q = "只回答：1+1=?"
	}
	reply, err := a.Run(context.Background(), sess, q)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(reply)
	fmt.Printf("[tokens] 输入 %d / 缓存读 %d / 输出 %d / 共 %d\n",
		sess.TotalUsage().FreshInput(),
		sess.TotalUsage().Details.Input.CacheRead,
		sess.TotalUsage().CompletionTokens,
		sess.TotalUsage().TotalTokens)
}

// env 读环境变量，空则用默认值。
func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
