// Command assistant 展示基于 MoS assistant 包接入真实模型的最小集成方式：
// 读取同目录 config.json 中的模型配置，组装智能体后进入交互式问答。
//
// 运行方式：
//
//	cd examples/assistant
//	go run .
//
// 模型信息配置在 config.json 中：填入 api_key 后即可连接真实模型；
// api_key 留空时自动降级为 mock 离线模式，无需联网即可体验完整工具循环。
// 也可用环境变量覆盖配置：OPENAI_API_KEY / ANTHROPIC_API_KEY、
// MOS_BASE_URL、MOS_MODEL、MOS_PROVIDER 等（优先级高于配置文件）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"hegq.net/mos/pkg/agent"
	"hegq.net/mos/pkg/assistant"
)

// fileConfig 对应 config.json 的模型信息配置，字段与 assistant.Config 对齐。
type fileConfig struct {
	Provider      string   `json:"provider"`       // openai / anthropic / mock
	BaseURL       string   `json:"base_url"`       // 协议服务地址
	Model         string   `json:"model"`          // 模型名称
	APIKey        string   `json:"api_key"`        // 服务密钥
	SystemPrompt  string   `json:"system_prompt"`  // 系统引导词
	MaxIterations int      `json:"max_iterations"` // 循环最大迭代次数
	ShellAllowed  []string `json:"shell_allowed"`  // shell 命令前缀白名单
	WorkDir       string   `json:"workdir"`        // 文件类工具根目录
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	// 无 API Key 时降级为 mock，保证离线可运行
	if cfg.Provider != "mock" && cfg.APIKey == "" {
		fmt.Println("⚠ 未配置 api_key，降级为 mock 离线模式（请在 config.json 或环境变量中配置后重启）")
		cfg.Provider = "mock"
	}

	cfg.Observer = observer // 挂载任务循环观测（思考/工具调用过程）

	a, err := assistant.New(*cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败:", err)
		os.Exit(1)
	}

	fmt.Println("======================================")
	fmt.Println(" MoS Assistant — 真实模型联调示例")
	fmt.Println("======================================")
	fmt.Printf(" Provider : %s\n", cfg.Provider)
	if cfg.Model != "" {
		fmt.Printf(" Model    : %s\n", cfg.Model)
	}
	if cfg.APIKey != "" {
		fmt.Println(" API Key  : 已配置")
	}
	fmt.Printf(" Tools    : %s\n", strings.Join(a.ToolNames(), ", "))
	fmt.Println(" 输入 /exit 退出，/reset 新会话")
	fmt.Println("--------------------------------------")

	sess := a.NewSession()
	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()

	for {
		fmt.Print("\n你 > ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit", "/q":
			fmt.Println("再见！")
			return
		case "/reset", "/new":
			sess = a.NewSession()
			fmt.Println("（已开启新会话）")
			continue
		}

		reply, err := a.Run(ctx, sess, line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\n执行出错:", err)
			continue
		}
		fmt.Printf("\n智能体 > %s\n", reply)
	}
}

// observer 输出任务循环中的中间过程（Loop/Harness Engineering 观测点）。
func observer(ev agent.Event) {
	switch ev.Type {
	case agent.EventStep:
		fmt.Printf("  ⟳ [第%d轮思考] %s\n", ev.Iteration, ev.Content)
	case agent.EventToolCall:
		fmt.Printf("  ▶ [第%d轮] 调用工具 %s(%s)\n", ev.Iteration, ev.ToolCall.Name, ev.ToolCall.Args)
	case agent.EventToolResult:
		fmt.Printf("  ◀ [第%d轮] 工具结果: %v\n", ev.Iteration, ev.ToolResult)
	}
}

// loadConfig 读取 config.json，并用环境变量覆盖其中可配置项。
func loadConfig() (*assistant.Config, error) {
	raw, err := os.ReadFile("config.json")
	if err != nil {
		return nil, fmt.Errorf("读取 config.json 失败: %w", err)
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}

	cfg := &assistant.Config{
		Provider:      orEnv("MOS_PROVIDER", fc.Provider),
		BaseURL:       orEnv("MOS_BASE_URL", fc.BaseURL),
		Model:         orEnv("MOS_MODEL", fc.Model),
		APIKey:        fc.APIKey,
		SystemPrompt:  orEnv("MOS_SYSTEM_PROMPT", fc.SystemPrompt),
		ShellAllowed:  fc.ShellAllowed,
		WorkDir:       orEnv("MOS_WORKDIR", fc.WorkDir),
		MaxIterations: fc.MaxIterations,
	}
	// API Key 优先取环境变量，避免密钥写入配置文件
	if v := firstEnv("OPENAI_API_KEY", "ANTHROPIC_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("MOS_SHELL_ALLOW"); v != "" {
		cfg.ShellAllowed = splitCSV(v)
	}
	if v := os.Getenv("MOS_MAX_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MOS_MAX_ITERATIONS 无效: %w", err)
		}
		cfg.MaxIterations = n
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 10
	}
	return cfg, nil
}

func orEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
