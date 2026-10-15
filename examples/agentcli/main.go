// main.go：agentcli 入口 —— .env 加载、协议选择、站点分发。
//
// agentcli 是"由浅入深学习 agent/ 目录"的教程式 CLI：每个 -stage N 对应
// 学习路线中的一站，站站并存（改一个数字即可对比各站装配差异）。
//
//	cd examples/agentcli
//	cp .env.example .env   # 填入真实 key
//	go run . -stage 0      # 最小装配，黑盒跑通
//	go run . -stage 1      # + 会话与事件（日志即事实）
//	go run . -stage 2      # + 驱动循环细节（实时事件流 / MaxTurns）
//	go run . -stage 3      # + 钩子与守卫（改写 / 拦截 / DoomLoop）
//	go run . -stage 4      # + 上下文工程（组装器/截断/压缩/技能）
//	go run . -stage 5      # + 审批挂起与恢复（Suspend → checkpoint → Resume）
//	go run . -stage 6      # + 装配与治理（seam/config/router/telemetry）
//
// 各站的设计说明与自测题见本目录 README.md。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/puras/mos/llm"
)

// flagMaxTurns / flagDoom 是跨站可用的保险丝开关（stage2/3 演示用，
// 其他站点置零即不生效）。
var (
	flagMaxTurns int
	flagDoom     int
)

type stageRunner func(model llm.ChatModel, protocol string) *harness

var stages = map[int]struct {
	name  string
	brief string
	run   stageRunner
}{
	0: {"minimal", "最小装配：黑盒跑通", runStage0},
	1: {"session", "会话与事件：日志即事实", runStage1},
	2: {"driver", "驱动与循环：实时事件流", runStage2},
	3: {"hooks", "钩子与守卫：改写与拦截", runStage3},
	4: {"context", "上下文工程：组装/截断/压缩/技能", runStage4},
	5: {"approval", "审批挂起与恢复", runStage5},
	6: {"assembly", "装配与治理：seam/config/router/telemetry", runStage6},
}

func main() {
	stage := flag.Int("stage", 0, "站点编号 0-6（见 README.md）")
	protocol := flag.String("p", "", "协议：openai | anthropic（默认取 .env 的 MOS_PROTOCOL，再默认 openai）")
	stream := flag.Bool("stream", false, "流式输出（模型文本增量实时打印）")
	flag.IntVar(&flagMaxTurns, "max-turns", 0, "MaxTurns 保险丝（>0 生效；stage2 演示）")
	flag.IntVar(&flagDoom, "doom", 0, "DoomLoop 阈值（>0 生效；stage3 演示）")
	flag.Parse()

	loadDotEnv(".env")

	if *protocol == "" {
		*protocol = os.Getenv("MOS_PROTOCOL")
	}
	if *protocol == "" {
		*protocol = "openai"
	}
	*protocol = strings.ToLower(*protocol)

	model, err := newModel(*protocol)
	check(err)

	s, ok := stages[*stage]
	if !ok {
		fmt.Fprintf(os.Stderr, "未知站点 %d；可用：", *stage)
		for i := 0; i < len(stages); i++ {
			fmt.Fprintf(os.Stderr, " %d=%s", i, stages[i].name)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	h := s.run(model, *protocol)
	h.protocol, h.stream = *protocol, *stream

	fmt.Printf("agentcli · 第 %d 站「%s」\n", *stage, s.brief)
	fmt.Printf("  协议: %s    模型: %s\n", *protocol, os.Getenv(envKey(*protocol)))
	checkStageFlags(*stage)
	h.repl()
}

// envKey 返回展示用的模型环境变量名。
func envKey(protocol string) string {
	return strings.ToUpper(protocol) + "_MODEL"
}

// checkStageFlags 提示与站点无关的开关（教学友好）。
func checkStageFlags(stage int) {
	if flagMaxTurns > 0 && stage != 2 {
		fmt.Printf("  提示: -max-turns 在第 2 站演示效果最佳\n")
	}
	if flagDoom > 0 && stage != 3 {
		fmt.Printf("  提示: -doom 在第 3 站演示效果最佳\n")
	}
}
