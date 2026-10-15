// stage3.go —— 第 3 站：钩子与守卫（工具执行安全通道）。
//
// 学习目标：区分两类链——HookBus 瀑布钩子（可改写/可短路，平台扩展点）与
// GuardBus 单调守卫（不可被普通钩子改写绕过，安全边界）。
//
//	go run . -stage 3
//	> 帮我读取 temp/hello.txt          # 观察 pre-step 注入 + post-execute 前缀
//	> 用 shell 删除 temp 下的任意文件   # rm 类命令被守卫拦截 → 显式失败回填给模型
//	go run . -stage 3 -doom 2          # DoomLoop 守卫：相同工具+参数连续 2 次即 blocked
//
// 观察点：
//   - post-execute 钩子给每个工具结果加了前缀 → 模型"看到"的内容被改写
//   - pre-step 钩子往模型输入追加了一条注入消息 → 输入被改写
//   - 守卫拦截不是终止任务：拒绝原因作为工具结果回填，模型会换路继续
//   - 守卫注册顺序即裁决顺序：Policy → 技能白名单 → DoomLoop（本站自定
//     义守卫排在最后）
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/execpath"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

func runStage3(model llm.ChatModel, protocol string) *harness {
	rt, reg := baseRunContext(model)
	rt.Observer = printObserver()

	// react.New 内部先走 InstallDefaults（压缩/执行/截断/审计钩子 + Policy 守卫），
	// 之后注册的钩子排在链尾（瀑布顺序 = 注册顺序）。
	h := &harness{
		protocol: protocol,
		rt:       rt,
		sess:     newSession(),
	}
	h.agent = react.New(rt)

	// 改写钩子 1：pre-step 往模型输入追加一条注入消息（演示"可改写环节"）。
	rt.HookBus.On(hook.HookPreStep, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev2, err := next(ctx, ev)
		if err != nil || ev2 == nil {
			return ev2, err
		}
		ev2.Messages = append(ev2.Messages,
			session.SystemMsg("（pre-step 钩子注入：本轮输入已被改写，忽略本条内容）"))
		return ev2, nil
	})

	// 改写钩子 2：post-execute 给模型可见的工具结果加前缀。
	rt.HookBus.On(hook.HookToolPostExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev2, err := next(ctx, ev)
		if err != nil || ev2 == nil {
			return ev2, err
		}
		if ev2.ResultText != "" {
			ev2.ResultText = "[hook·改写] " + ev2.ResultText
		}
		return ev2, nil
	})

	// 单调守卫：拦截 shell 命令里带 rm 的调用（ToolDeniedError = 非致命，
	// 拒绝原因显式回填给模型，任务继续）。守卫不走 HookBus，无法被上面的
	// 改写钩子绕过。
	rt.GuardBus.Add(func(ctx context.Context, ev *hook.HookEvent) error {
		if ev.ToolCall == nil || ev.ToolCall.Name != "shell" {
			return nil
		}
		if strings.Contains(ev.ToolCall.Args, "rm") {
			return &execpath.ToolDeniedError{Reason: "演示守卫：禁止 rm 类破坏性命令"}
		}
		return nil
	})

	if flagDoom > 0 {
		h.extraBanner = append(h.extraBanner,
			"DoomLoop 守卫已启用: 相同工具+参数连续 "+itoa(flagDoom)+" 次调用 → 任务 blocked")
	}
	h.extraBanner = append(h.extraBanner,
		"钩子: pre-step 注入 + post-execute 前缀（HookBus，可改写）",
		"守卫: rm 拦截（GuardBus，单调不可绕过）",
		"工具: "+strings.Join(reg.Names(), ", "),
	)
	return h
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
