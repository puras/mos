// stage5.go —— 第 5 站：审批挂起与恢复（Suspend → checkpoint → Resume）。
//
// 学习目标：跑通"异步审批"完整闭环——策略返回 *approval.RequiredError（软
// 拒绝）→ 驱动发起 Approval.Request 并抛 SuspendError 挂起 → checkpoint 落盘
// → 人工决策 → Handle.Resume 从挂起轮直接续跑（跳过 LLM 调用）。
//
//	go run . -stage 5
//	> 看看 temp 目录里有什么文件
//	（模型调 shell → 策略要求确认 → 终端出现 [审批] 提示，输入 y 或 n）
//
// 观察点：
//   - 事件流（/events 继承自第 1 站）：approval/request → checkpoint/save
//     → （决策后）checkpoint/restore → 工具执行
//   - 选 y：工具真正执行；选 n：决策作为失败结果回填，模型换路继续
//   - 挂起进程退出后重新 go run . -stage 5 再问同样的问题也能续（checkpoint
//     在 data/checkpoints/<会话id>.checkpoint.json）
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/checkpoint"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/handle"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

// shellApprovalPolicy 是本站的最小自定义安全策略：shell 一律要求人工确认
// （返回 *approval.RequiredError，软拒绝——区别于硬拒绝的普通 error），
// 其余工具放行。学习点在 RequiredError 契约本身。
type shellApprovalPolicy struct{}

func (shellApprovalPolicy) Allow(_ context.Context, call *llm.ToolCall) error {
	if call.Name != "shell" {
		return nil
	}
	var p struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(call.Args), &p)
	return &approval.RequiredError{Request: approval.ApprovalRequest{
		ToolName: call.Name,
		Command:  p.Command,
		Reason:   "演示策略：shell 命令需要人工确认",
		Args:     string(call.Args),
	}}
}

func runStage5(model llm.ChatModel, protocol string) *harness {
	// 共享设施：审批通道（内存版）+ checkpoint 文件存储 + 会话持久化。
	ap := approval.NewInMemApproval()
	store, err := checkpoint.NewFileStore("data/checkpoints")
	check(err)
	persist, err := session.NewJSONL(sessionsDir)
	check(err)

	// buildAgent 为指定会话重建绑定的 agent + RunContext（句柄恢复时复用，
	// 与 assistant.newAgentForSession 同构）。
	buildAgent := func(sess *session.Session) (*driver.Agent, *driver.RunContext, error) {
		rt, _ := baseRunContext(model)
		rt.Observer = printObserver()
		rt.Policy = shellApprovalPolicy{}
		rt.Approval = ap
		rt.Checkpoints = store
		sess.Persistence = persist
		rt.Session = sess
		return react.New(rt), rt, nil
	}

	registry := handle.NewRegistry(buildAgent, store, nil)

	sess := newSession()
	hdl, err := registry.Create(context.Background(), sess)
	check(err)

	h := &harness{
		protocol: protocol,
		rt:       hdl.RT,
		agent:    hdl.Agent,
		sess:     sess,
	}
	h.extraBanner = append(h.extraBanner,
		"策略: shell → 需人工确认（RequiredError 软拒绝）",
		"审批: InMemApproval + checkpoint.FileStore(data/checkpoints)",
		"流程: 模型调 shell → SuspendError 挂起 → [审批] y/N → Resume 续跑",
		"命令: /events 查看审批与 checkpoint 事件",
	)

	// 接管默认输入：Run 返回 SuspendError 时进入"终端问答 → Resume"循环。
	h.handleInput = func(ctx context.Context, input string) (string, error) {
		out, err := hdl.Run(ctx, input)
		for {
			var se *approval.SuspendError
			if !errors.As(err, &se) {
				return out, err
			}
			approved := askApproval(se.Request)
			out, err = hdl.Resume(ctx, approval.ApprovalDecision{
				Approved:  approved,
				Reason:    map[bool]string{true: "CLI 批准", false: "CLI 拒绝"}[approved],
				DecidedBy: "agentcli",
				DecidedAt: time.Now(),
			})
		}
	}

	// /events 复用第 1 站的事件打印（挂起-恢复的事件序列是本站观察点）。
	h.on("/events", func(string) {
		for _, ev := range h.sess.Events {
			fmt.Printf("  %3d  %s\n", ev.Seq, ev.Type)
		}
	})
	return h
}

// askApproval 在终端打印审批请求并读取 y/N 决策。
func askApproval(req approval.ApprovalRequest) bool {
	fmt.Printf("── [审批] 工具=%s\n    命令: %s\n    原因: %s\n    批准? (y/N): ",
		req.ToolName, req.Command, req.Reason)
	var line string
	fmt.Scanln(&line) // REPL 正阻塞等待本轮结果，这里直接读 stdin 是安全的
	line = strings.TrimSpace(line)
	fmt.Println()
	return line == "y" || line == "Y"
}
