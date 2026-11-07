// MoS 子代理示例：把一处子代理目录接进来，主模型按清单点名派活。
//
// 装配只有三行关键代码（SubAgents + Dirs + 子代理目录），其余都是宿主的活：
//   - 清单（name / description）装配期渲染进静态系统提示词（进门就看得见，
//     且不随调用变化 —— 保 prompt cache）；
//   - 每个子代理注册成一个普通工具 `subagent__<name>`，主模型用 function
//     calling 点名调用；
//   - 子代理在**自己的会话**里用**收窄过的工具面**干活（声明白名单 ∩ 父可见集，
//     再恒定扣掉 `subagent__*` 与 `tool_search`），只回一段固定结构的回执
//     （`{status, summary, findings[]}`）。
//
// 跑法（示例不读 .env，环境变量自己导出——配置来源归宿主）：
//
//	cd examples/subagent
//	export MOS_PROVIDER=openai ANTHROPIC_API_KEY=sk-...  # 按协议取对应的那把
//	export MOS_MODEL=gpt-4o-mini
//	go run . "看一下 agent/skill 这个包有没有 bug"
//
// 子代理目录里的 agents/reviewer/AGENT.md 就是子代理本体——改它、重跑，行为
// 和提示词都会跟着变，不需要动 Go 代码。
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

	// 工具：这一例主模型自己也能用 shell / read 等（子代理另按白名单收窄）。
	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	a, err := mos.New(mos.Config{
		Model: model,
		Tools: reg,
		// 子代理目录列表**就是授权边界**：框架只读这里声明的目录，别的一概不碰。
		// 目录形态与 Skills 同构：`<子代理目录>/<名字>/AGENT.md`。
		SubAgents: &mos.SubAgentConfig{Dirs: []string{"agents"}},
		SystemPrompt: "你是一个主助手。清单里列了可派的子代理：" +
			"任务对口就点名派给它（工具名 subagent__<名字>），" +
			"子代理回的是一段结论与线索清单——那是它的自述，采纳前自己复核。",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = a.Close() }()

	// Describe() 是装配快照：装了几个子代理、每个的白名单与模型覆盖、
	// 有没有被跳过的（目录读不动、声明不合法等诊断都在里面 —— 降级不静默）。
	fmt.Print(a.Describe())

	q := strings.Join(os.Args[1:], " ")
	if q == "" {
		q = "看一下 agent/skill 这个包有没有 bug"
	}
	sess, err := session.Open("data", "subagent")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	reply, err := a.Run(context.Background(), sess, q)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(reply)
	fmt.Printf("[tokens] 输入 %d / 输出 %d\n",
		sess.TotalUsage().FreshInput(), sess.TotalUsage().CompletionTokens)
}

// env 读环境变量，空则用默认值。
func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
