// Package main 演示用 mos.Assistant 接 OpenAI 兼容模型跑 CLI。
//
// 启动时自动加载同目录 .env 文件（不存在时回退到系统环境变量）。
// 跑法：
//   cd examples/cli
//   go run .
//
// 工具：4 个最常用内置（时间 / 读 / 写 / shell）。
// 会话：单会话多轮，同一 *session.Session 累积历史。
// 退出：输入 q。
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/puras/mos/assistant"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

func main() {
	loadDotEnv(".env")

	root := mustAbs(filepath.Join(filepath.Dir(mustAbs(".")), "temp"))

	// OpenAI 兼容协议（go.hegq.net/v1 是 MoGo 网关，签名同 OpenAI Chat Completions）。
	model, err := openai.New(llm.Config{
		"api_key":  os.Getenv("OPENAI_API_KEY"),
		"base_url": os.Getenv("MOS_BASE_URL"),
		"model":    os.Getenv("MOS_MODEL"),
	})
	check(err)

	tools := []tool.Tool{
		builtin.CurrentTime{},
		builtin.ReadFile{},
		builtin.WriteFile{},
		builtin.Shell{},
	}

	a, err := assistant.New(
		model,
		tools,
		assistant.Config{
			FileAccess: assistant.FileAccessConfig{WorkspaceRoot: root},
		},
	)
	check(err)

	fmt.Printf("Assistant 已就绪。\n")
	fmt.Printf("  模型: %s @ %s\n", os.Getenv("MOS_MODEL"), os.Getenv("MOS_BASE_URL"))
	fmt.Printf("  工具: %v\n", a.ToolNames())
	fmt.Printf("  工作区: %s\n", root)
	fmt.Println("输入 q 退出。")

	sess := a.NewSession()
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if line == "q" || line == "quit" {
			break
		}
		out, err := a.Run(context.Background(), sess, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Run error: %v\n", err)
			continue
		}
		fmt.Println(out)
	}
}

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

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
