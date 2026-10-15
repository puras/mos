// MoS chat Demo（S4a）：驱动层 ReAct 循环 + 直连 LLM 多轮对话。
//
// 装配：llm 协议注册表建模型 → planner.ReAct 决策器 → driver.RunContext
// → react 驱动。每一轮对话的 turn/user/assistant/turn-end 事件落会话 JSONL，
// 多轮上下文由事件投影累积，重启自动恢复续聊。
//
// 跑法：
//
//	cd examples/chat
//	cp .env.example .env   # 填入 API Key 与模型名
//	go run .               # 默认恢复上次会话；-new 开新会话
//
// 退出：输入 q。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/gemini"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

func main() {
	fresh := flag.Bool("new", false, "从全新会话开始（默认恢复上次会话）")
	flag.Parse()

	loadDotEnv(".env")

	model := mustModel()
	jl, sess := mustSession(*fresh)
	defer jl.Close()

	rt := &driver.RunContext{
		Planner:      planner.NewReAct(model),
		SystemPrompt: os.Getenv("MOS_SYSTEM_PROMPT"),
		Stream:       true,
		OnChunk:      render,
	}
	a := driver.New(rt, react.New())

	fmt.Printf("MoS chat 已就绪（provider=%s model=%s session=%s 已有 %d 条事件 / 累计 %d tokens）\n",
		provider(), os.Getenv("MOS_MODEL"), sess.ID, len(sess.Events()), sess.TotalUsage().TotalTokens)
	fmt.Println("输入 q 退出。")

	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n你 > ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if line == "q" || line == "quit" || line == "exit" {
			break
		}
		reply, err := a.Run(context.Background(), sess, line)
		if err != nil {
			fmt.Println()
			fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
			continue
		}
		// 回复已由 OnChunk 流式渲染（决策器回退非流式时也会一次性 emit 全文）；
		// 仅在关闭流式时才在此补打印，避免同一段回复显示两遍。
		if !rt.Stream {
			fmt.Println(reply)
		}
		u := sess.TotalUsage()
		fmt.Printf("  [tokens] 会话累计 输入 %d / 输出 %d / 共 %d（第 %d 轮）\n",
			u.PromptTokens, u.CompletionTokens, u.TotalTokens, sess.NextTurn()-1)
	}
}

// render 渲染流式增量：思维链用暗色，正文原样（打字机效果）。
func render(delta string, reasoning bool) {
	if reasoning {
		fmt.Printf("\x1b[2m%s\x1b[0m", delta)
		return
	}
	fmt.Print(delta)
}

// mustModel 经协议注册表构建模型对象，套一层重试装饰器。
func mustModel() llm.ChatModel {
	reg := llm.NewRegistry()
	reg.Register("openai", openai.New)
	reg.Register("anthropic", anthropic.New)
	reg.Register("gemini", gemini.New)

	keyEnv := map[string]string{
		"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY",
	}[provider()]
	if os.Getenv(keyEnv) == "" {
		fmt.Fprintf(os.Stderr, "缺少 %s（在 .env 或环境变量中配置）\n", keyEnv)
		os.Exit(1)
	}
	model, err := reg.Create(provider(), llm.Config{
		"api_key":  os.Getenv(keyEnv),
		"base_url": os.Getenv("MOS_BASE_URL"),
		"model":    os.Getenv("MOS_MODEL"),
	})
	check(err)
	return llm.WithRetry(model, 2, time.Second)
}

// mustSession 打开或恢复会话（JSONL 落 data/；中断的 turn 由 Restore 合成收尾）。
// 返回后端与会话：后端由调用方负责 Close。
func mustSession(fresh bool) (*session.JSONL, *session.Session) {
	jl, err := session.NewJSONL("data")
	check(err)
	id := "chat"
	if fresh {
		id = fmt.Sprintf("chat-%d", time.Now().Unix())
	}
	_ = jl.SaveHeader(id, session.Header{ID: id, CreatedAt: time.Now()})
	sess, err := session.Restore(id, jl)
	check(err)
	return jl, sess
}

func provider() string {
	if p := os.Getenv("MOS_PROVIDER"); p != "" {
		return p
	}
	return "openai"
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadDotEnv 极简 .env 加载器：忽略空行与 # 注释，KEY=VALUE 注入环境（已存在则不覆盖）。
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
