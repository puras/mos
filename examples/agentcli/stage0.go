// stage0.go —— 第 0 站：最小装配，黑盒跑通。
//
// 学习目标：用最少的代码把 driver 层智能体跑起来，建立可观察的黑盒。
// 只做三件事：构造 RunContext（工具 + 规划器 + 文件 jail）→ react.New 默认装配
// → REPL 里 agent.Run。
//
//	go run . -stage 0
//	> 现在几点了？
//	> 看看 temp 目录里有什么
//
// 观察点：模型会自主决定是否调用工具（current_time / shell / read_file…），
// 最终只把回复打印出来——中间过程是黑盒，第 2 站会打开它。
package main

import (
	"fmt"
	"strings"

	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/llm"
)

func runStage0(model llm.ChatModel, protocol string) *harness {
	rt, reg := baseRunContext(model)

	h := &harness{
		protocol: protocol,
		rt:       rt,
		sess:     newSession(),
	}
	h.agent = react.New(rt)
	h.extraBanner = append(h.extraBanner,
		"驱动: ReActDriver（react.New 默认装配：压缩/RAG 钩子 + 执行/截断/审计链 + Policy 守卫）",
		"工具: "+strings.Join(reg.Names(), ", "),
		"工作区(jail): "+workspaceRoot(),
	)
	h.on("/tools", func(string) {
		fmt.Println("已注册工具:", strings.Join(reg.Names(), ", "))
	})
	return h
}
