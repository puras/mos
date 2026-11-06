// MoS 技能示例：把一处技能目录接进来，模型按清单决定用哪个技能，正文按需现读。
//
// 装配只有三行关键代码（Skills + Dirs + 技能目录），其余都是宿主的活：
//   - 清单（name / description）在装配期渲染进静态系统提示词 —— 进门就看得见，
//     且不随调用变化（保 prompt cache）；
//   - 正文**不进**提示词：模型调 `skill` 工具时才从磁盘现读（改完正文下次调用即生效）；
//   - `/技能名 参数` 这种**宿主侧手势**由 ExpandSkill 就地展开正文，不经过模型判断。
//
// 跑法（示例不读 .env，环境变量自己导出——配置来源归宿主）：
//
//	cd examples/skills
//	export MOS_PROVIDER=openai ANTHROPIC_API_KEY=sk-...  # 按协议取对应的那把
//	export MOS_MODEL=gpt-4o-mini
//	go run . "帮我把 app 部署到测试环境"   # 模型自己决定要不要读技能正文
//	go run . "/deploy app 测试环境"        # 宿主手势：正文直接展开
//
// 技能目录里的 skills/deploy/SKILL.md 就是技能本体——改它、重跑，行为和提示词
// 都会跟着变，不需要动 Go 代码。
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

	// 工具：这一例只需要 shell 与 read（部署流程要跑命令、看日志）。
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
		// 技能目录列表**就是授权边界**：框架只读这里声明的目录，别的一概不碰。
		// 多个目录时按优先级从高到低，同名技能先到先得。
		Skills: &mos.SkillConfig{Dirs: []string{"skills"}},
		SystemPrompt: "你是一个部署助手。动手之前先看技能清单里有没有对口的技能；" +
			"有就先读它的正文，按里面写的步骤做。",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = a.Close() }()

	// Describe() 是装配快照：技能清单是否装上、有没有被跳过的（降级如实写出来）。
	fmt.Print(a.Describe())
	fmt.Println("技能清单：")
	for _, s := range a.Skills() {
		fmt.Printf("  - %s：%s\n", s.Name, s.Description)
	}

	q := strings.Join(os.Args[1:], " ")
	if q == "" {
		q = "帮我把 app 部署到测试环境"
	}
	// 宿主侧手势：`/deploy ...` 直接就地展开正文（模型不必先调 skill 工具）。
	// 正文本轮现读：技能目录里改完，下一次手势立刻生效。
	if strings.HasPrefix(strings.TrimSpace(q), "/") {
		expanded, ok, err := a.ExpandSkill(q)
		if err != nil {
			// 正文读不出来是**本轮失败**，不是"当普通消息发出去"。
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if ok {
			fmt.Println("[技能手势] 已展开正文")
			q = expanded
		}
	}

	sess, err := session.Open("data", "skills")
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
