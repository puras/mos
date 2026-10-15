// 直连 LLM 的多轮对话 Demo（S2 里程碑）：不经 agent 循环，
// registry 按协议选适配器 → 流式打字机输出 → 会话事件日志 JSONL 持久化。
// 重启自动恢复上次会话继续聊（日志即事实）；中断的 turn 由 Restore 合成 interrupted 收尾。
//
// 跑法：
//
//	cd examples/chat
//	cp .env.example .env   # 填入 API Key 与模型名
//	go run .
//
// 退出：输入 q；-new 从全新会话开始（默认恢复上次会话）。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/gemini"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/session"
)

func main() {
	fresh := flag.Bool("new", false, "从全新会话开始（默认恢复上次会话）")
	flag.Parse()

	loadDotEnv(".env")
	model := mustModel()
	jl := mustJSONL()
	defer jl.Close()

	id := "chat"
	if *fresh {
		id = fmt.Sprintf("chat-%d", time.Now().Unix())
	}
	_ = jl.SaveHeader(id, session.Header{ID: id, CreatedAt: time.Now()})
	sess, err := session.Restore(id, jl)
	check(err)

	fmt.Printf("MoS chat 已就绪（provider=%s model=%s session=%s 已有 %d 条事件）\n",
		provider(), os.Getenv("MOS_MODEL"), sess.ID, len(sess.Events()))
	fmt.Println("输入 q 退出。")

	turn := lastTurn(sess.Events())
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
		turn++
		chatTurn(sess, model, turn, line)
	}
}

// chatTurn 跑一轮：turn/start → user → 模型（流式）→ assistant → turn/end。
// 每步都落会话日志；模型调用失败以 reason=error 收尾，不留孤儿 turn。
func chatTurn(sess *session.Session, model llm.ChatModel, turn int, input string) {
	appendEvent(sess, session.Event{Kind: session.EventTurnStart, Turn: turn})
	appendEvent(sess, session.Event{Kind: session.EventUser, Text: input, Turn: turn})

	fmt.Print("\nMoS > ")
	text, usage, err := stream(context.Background(), model, &llm.Request{Messages: messagesFor(sess)})
	if err != nil {
		fmt.Println()
		fmt.Fprintf(os.Stderr, "模型调用失败: %v\n", err)
		appendEvent(sess, session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: "error"})
		return
	}

	assistant := session.Event{Kind: session.EventAssistant, Text: text, Turn: turn}
	if usage.TotalTokens > 0 {
		assistant.Usage = &usage // llm.Usage 同形直存
	}
	appendEvent(sess, assistant)
	appendEvent(sess, session.Event{Kind: session.EventTurnEnd, Turn: turn, Reason: "completed"})
	if usage.TotalTokens > 0 {
		fmt.Printf("\n  [tokens] 本轮 输入 %d / 输出 %d / 共 %d ｜ 会话累计 %d\n",
			usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, sess.TotalUsage().TotalTokens)
	}
}

// stream 优先流式（打字机效果），协议不支持时回退非流式。
// 推理增量（思维链）以暗色文本透出，正文增量累积为完整回复。
func stream(ctx context.Context, model llm.ChatModel, req *llm.Request) (string, llm.Usage, error) {
	var sb strings.Builder
	resp, err := model.ChatStream(ctx, req, func(c llm.StreamChunk) error {
		if c.Reasoning != "" {
			fmt.Printf("\x1b[2m%s\x1b[0m", c.Reasoning)
		}
		if c.Delta != "" {
			sb.WriteString(c.Delta)
			fmt.Print(c.Delta)
		}
		return nil
	})
	if errors.Is(err, llm.ErrUnsupported) {
		resp, err = model.Chat(ctx, req)
		if err != nil {
			return "", llm.Usage{}, err
		}
		fmt.Print(resp.Message.Content)
		return resp.Message.Content, resp.Usage, nil
	}
	if err != nil {
		return "", llm.Usage{}, err
	}
	text := sb.String()
	if text == "" && resp != nil {
		text = resp.Message.Content
	}
	if resp == nil {
		return text, llm.Usage{}, nil
	}
	return text, resp.Usage, nil
}

// messagesFor 组装请求消息：可选系统引导 + 会话投影。
// 会话投影直接就是 []llm.Message——日志与协议同形，零转换；
// 多轮上下文随会话事件自然累积，这就是 S1 投影的用武之地。
func messagesFor(sess *session.Session) []llm.Message {
	var msgs []llm.Message
	if sys := os.Getenv("MOS_SYSTEM_PROMPT"); sys != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: sys})
	}
	return append(msgs, sess.Messages()...)
}

// mustModel 经协议注册表构建模型对象，并套一层重试装饰器（展示 llm/wrap 用法）。
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

func provider() string {
	if p := os.Getenv("MOS_PROVIDER"); p != "" {
		return p
	}
	return "openai"
}

func mustJSONL() *session.JSONL {
	jl, err := session.NewJSONL("data") // 会话日志落 examples/chat/data/（已 gitignore）
	check(err)
	return jl
}

func appendEvent(s *session.Session, ev session.Event) {
	if _, err := s.Append(ev); err != nil {
		fmt.Fprintf(os.Stderr, "日志写入失败（fail-fast）: %v\n", err)
		os.Exit(1)
	}
}

func lastTurn(events []session.Event) int {
	n := 0
	for _, ev := range events {
		if ev.Turn > n {
			n = ev.Turn
		}
	}
	return n
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

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
