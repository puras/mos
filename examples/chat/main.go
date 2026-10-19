// MoS chat Demo（S4b）：驱动层 ReAct 循环 + 直连 LLM 多轮对话。
//
// 装配：llm 协议注册表建模型（套 WithRetry / WithLogging 装饰器）
// → planner.ReAct 决策器 → driver.RunContext（含 tool.Registry）
// → react 驱动。每一轮对话的 turn/user/assistant/tool/turn-end 事件落会话 JSONL，
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
	"sync/atomic"
	"time"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/gemini"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

func main() {
	fresh := flag.Bool("new", false, "从全新会话开始（默认恢复上次会话）")
	denyFlag := flag.String("deny", "", "禁用指定工具（逗号分隔），演示策略拦截")
	policyFlag := flag.String("policy", "", "策略配置文件（YAML），给出后忽略 -deny")
	flag.Parse()

	loadDotEnv(".env")

	model := mustModel()
	jl, sess := mustSession(*fresh)
	defer jl.Close()

	// 内置工具集：模型可调用的工具在此登记（未登记的工具不进 function calling 清单）
	reg := tool.NewRegistry()
	for _, tl := range builtin.Default() {
		if err := reg.Register(tl); err != nil {
			check(fmt.Errorf("注册工具 %q: %w", tl.Name(), err))
		}
	}

	// 单调守卫：可改写环节走钩子，安全约束走守卫，两者不可互相绕过。
	// 策略默认放行本示例已注册的内置工具，再叠加 -deny 拒绝名单。
	policyPath := *policyFlag
	if policyPath == "" {
		policyPath = os.Getenv("MOS_POLICY")
	}
	deny := list(*denyFlag + "," + os.Getenv("MOS_DENY_TOOLS"))
	cfg := policyConfig(policyPath, reg.Names(), deny)
	guards := mustGuards(cfg)

	rt := &driver.RunContext{
		Planner:      planner.NewReAct(model),
		Tools:        reg,
		Guards:       guards,
		SystemPrompt: os.Getenv("MOS_SYSTEM_PROMPT"),
		Stream:       true,
		OnChunk:      render,
	}
	agent := driver.New(rt, react.New())

	fmt.Printf("MoS chat 已就绪（provider=%s model=%s session=%s 已有 %d 条事件 / 累计 %d tokens）\n",
		provider(), os.Getenv("MOS_MODEL"), sess.ID, len(sess.Events()), sess.TotalUsage().TotalTokens)
	fmt.Printf("  可用工具: %s\n", strings.Join(reg.Names(), ", "))
	if guards == nil {
		fmt.Println("  策略: 未装配（nil 总线 = 零开销降级，全部放行）")
	} else {
		src := "默认（放行已注册工具）+ -deny"
		if policyPath != "" {
			src = "文件 " + policyPath
		}
		fmt.Printf("  策略[%s]: 放行 [%s] 拒绝 [%s]（未列名的工具一律拒绝）\n",
			src, strings.Join(cfg.Allow, ", "), strings.Join(cfg.Deny, "（空）"))
	}
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
		reply, err := agent.Run(context.Background(), sess, line)
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
		flushLine() // 模型回复未必以换行结尾，先收尾再打统计，避免两行黏在一起
		u := sess.TotalUsage()
		fmt.Printf("  [tokens] 会话累计 输入 %d / 输出 %d / 共 %d（第 %d 轮）\n",
			u.PromptTokens, u.CompletionTokens, u.TotalTokens, sess.NextTurn()-1)
	}
}

// render 渲染流式增量：思维链用暗色，正文原样（打字机效果）。
func render(delta string, reasoning bool) {
	if reasoning {
		fmt.Printf("\x1b[2m%s\x1b[0m", delta)
	} else {
		fmt.Print(delta)
	}
	if delta != "" {
		pendingLine.Store(true)
	}
}

// pendingLine 记录终端上是否还有没换行的半截输出（模型回复不以换行结尾）。
// 随后的 token 统计与模型调用日志都靠 flushLine 另起一行，避免插进回复中间。
var pendingLine atomic.Bool

func flushLine() {
	if pendingLine.Swap(false) {
		fmt.Println()
	}
}

// mustModel 经协议注册表构建模型对象，套「重试 + 调用日志」两层装饰器。
//
// 装饰器嵌套顺序即执行顺序（外层先进）：日志刻意套在重试之外——重试失败再试
// 会各打一行，重试了几次、共耗时多久一目了然；若把日志放里面，只会看到最后
// 一次成功，失败被静默吞掉。
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

	var m llm.ChatModel = llm.WithRetry(model, 2, time.Second)
	if logLLM() {
		// 模型名等日志内容由闭包持有，装饰器只管「调用结束时回调一次」
		name := os.Getenv("MOS_MODEL")
		m = llm.WithLogging(m, func(r llm.CallResult) { logCall(name, r) })
	}
	return m
}

// mustGuards 构建单调守卫链：安全策略（Policy）作为第一道关卡。
//
// cfg 为空时返回 nil——nil 总线的 Allow 直接放行，零开销降级，不装守卫的
// 用户不付任何代价。拒绝是「结果」不是「故障」：工具不执行，拒绝理由经
// tool/result 回填给模型，让它自行纠正。
//
// 这里在 Policy 外包一层只为演示可见性（打印被拒的工具名与轮次）——裁决
// 本身全部委托 Policy.Check，示例不自己重写规则。GuardBus 尚无观测通道，
// 正经日志是 S5c 之后的事。
func mustGuards(cfg policy.Config) *hook.GuardBus {
	if len(cfg.Allow) == 0 && len(cfg.Deny) == 0 {
		return nil
	}
	p := policy.New(cfg)
	gb := hook.NewGuardBus()
	gb.Add(func(ctx context.Context, ev *hook.HookEvent) error {
		err := p.Check(ctx, ev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[guard] 第 %d 轮：拒绝 %s——工具不会执行（%v）\n",
				ev.Turn, ev.ToolCall.Name, err)
		}
		return err
	})
	return gb
}

// policyConfig 决定生效的策略：
//   - 指定 -policy 文件：完全以该 YAML 为准（平台纯配置装配，G3）；
//   - 否则：默认放行本示例已注册的内置工具，再叠加 -deny / MOS_DENY_TOOLS 的拒绝名单。
//
// 注意策略本身是 fail-closed 的（未列入 allow 即拒绝）——示例默认放行全部
// 内置工具只是因为它是 Demo，真平台必须显式声明。
func policyConfig(policyPath string, allow, deny []string) policy.Config {
	if policyPath != "" {
		cfg, err := policy.LoadFile(policyPath)
		check(err)
		return cfg
	}
	return policy.Config{Allow: allow, Deny: deny}
}

// list 按逗号切分并去空白（命令行与 .env 共用）。
func list(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// logLLM 是模型调用观测日志的开关：默认开启，MOS_LOG_LLM=0 关闭。
func logLLM() bool { return os.Getenv("MOS_LOG_LLM") != "0" }

// llmCalls 是模型调用计数，仅用于给日志行编号，便于与会话事件对号入座。
var llmCalls atomic.Int64

// logCall 打印一次模型调用的观测行：模式 / 耗时 / 请求规模 / 停止原因 / token。
//
// 只观测不改写——要改写模型输入或拦下某次调用，属 S5 的 HookBus（可短路可改写），
// 那是另一套机制。本 Demo 把日志写 stderr：stdout 是流式渲染的地盘。
func logCall(model string, r llm.CallResult) {
	flushLine()
	mode := "chat  "
	if r.Stream {
		mode = "stream"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[llm #%d] %s %6.2fs model=%s", llmCalls.Add(1), mode, r.Elapsed.Seconds(), model)
	if r.Req != nil {
		fmt.Fprintf(&b, " msgs=%d", len(r.Req.Messages))
		if n := len(r.Req.Tools); n > 0 {
			fmt.Fprintf(&b, " tools=%d", n)
		}
	}
	switch {
	case r.Err != nil:
		fmt.Fprintf(&b, " 错误=%v", r.Err)
	case r.Resp == nil:
		b.WriteString(" 空响应")
	default:
		finish := r.Resp.FinishReason
		if finish == "" {
			finish = "-"
		}
		u := r.Resp.Usage
		fmt.Fprintf(&b, " finish=%s tokens=%d/%d/%d",
			finish, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		// 缓存命中与推理 token：非零才打印，避免噪声
		if d := u.Details; d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 || d.Output.Reasoning > 0 {
			if d.Input.CacheRead > 0 || d.Input.CacheWrite > 0 {
				fmt.Fprintf(&b, " cache=%d/%d", d.Input.CacheRead, d.Input.CacheWrite)
			}
			if d.Output.Reasoning > 0 {
				fmt.Fprintf(&b, " reasoning=%d", d.Output.Reasoning)
			}
		}
		if calls := r.Resp.Message.ToolCalls; len(calls) > 0 {
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				names = append(names, c.Name)
			}
			fmt.Fprintf(&b, " → 请求调用 %s", strings.Join(names, ", "))
		}
	}
	fmt.Fprintln(os.Stderr, b.String())
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
