// stage2.go —— 第 2 站：驱动与 ReAct 循环（打开黑盒）。
//
// 学习目标：观察一轮（turn）循环的内部结构——每个 turn = 一次模型调用 +
// 其触发的全部工具执行；驱动以事件形式广播每个环节。
//
//	go run . -stage 2
//	> 查看当前目录有哪些 go 文件，统计总数
//	go run . -stage 2 -max-turns 1     # MaxTurns 保险丝：1 轮没做完就 blocked
//
// 观察点：
//   - 实时事件流：思考（EventStep）→ 工具调用/结果 → 完成（EventDone）
//   - -max-turns 1 时复杂任务到轮数上限即 blocked（任务状态机：
//     pending → running → blocked/completed）
//   - 会话事件流（/events，继承自第 1 站）里同一轮的 step/start…step/end
package main

import (
	"context"
	"fmt"

	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/llm"
)

func runStage2(model llm.ChatModel, protocol string) *harness {
	rt, _ := baseRunContext(model)
	rt.Observer = printObserver() // 实时事件流：本站主角

	h := &harness{
		protocol: protocol,
		rt:       rt,
		sess:     newSession(),
	}
	h.agent = react.New(rt)
	if flagMaxTurns > 0 {
		h.extraBanner = append(h.extraBanner,
			fmt.Sprintf("MaxTurns 保险丝: %d（达到上限任务置 blocked）", flagMaxTurns))
	}

	// 接管默认输入：跑完后打印任务状态机的当前状态。
	h.handleInput = func(ctx context.Context, input string) (string, error) {
		out, err := h.agent.Run(ctx, h.sess, input)
		fmt.Printf("—— 任务状态: %s（turn 总数见上方事件流）\n", h.sess.State.Status)
		return out, err
	}
	return h
}
