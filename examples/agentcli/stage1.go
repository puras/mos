// stage1.go —— 第 1 站：会话与事件（"日志即事实"）。
//
// 学习目标：理解 agent/session 的事件日志设计——append-only 事件是唯一事实
// 来源，模型消息历史只是从事件投影出来的视图（deriveMessages）。
//
//	go run . -stage 1
//	> 帮我在 temp 里写一个 hello.txt
//	/events      # 事件日志：Seq / 类型 / 摘要
//	/messages    # 投影出的模型消息历史（对比 /events 的对应关系）
//	/state       # 外部可信任务状态 TaskState（Goal / Status / Facts）
//	/restore <会话ID>   # 从 jsonl 重放恢复另一个会话（重启后也可用）
//
// 观察点：事件落盘在 data/sessions/<id>.jsonl（首行 header，之后每行一条
// 事件）；assistant/chunk 是流式增量，同轮有 assistant/message 时被其消费。
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

const sessionsDir = "data/sessions"

func runStage1(model llm.ChatModel, protocol string) *harness {
	rt, _ := baseRunContext(model)

	// 挂载 JSONL 持久化：Append 同步落盘（header 首次写入，事件逐条追加）。
	persist, err := session.NewJSONL(sessionsDir)
	check(err)

	h := &harness{
		protocol: protocol,
		rt:       rt,
		sess:     newSession(),
	}
	h.sess.Persistence = persist
	h.agent = react.New(rt)
	h.extraBanner = append(h.extraBanner,
		"持久化: "+sessionsDir+"/"+h.sess.ID+".jsonl（日志即事实，恢复 = 重放）",
		"命令: /events /messages /state /restore <id>",
	)

	// /events 打印事件日志：Seq + 类型 + 载荷摘要。
	h.on("/events", func(string) {
		fmt.Printf("事件日志（共 %d 条）：\n", len(h.sess.Events))
		for _, ev := range h.sess.Events {
			fmt.Printf("  %3d  %-22s %s\n", ev.Seq, ev.Type, eventSummary(ev))
		}
	})

	// /messages 打印投影出的模型消息历史（与 /events 对照看"投影"关系）。
	h.on("/messages", func(string) {
		msgs := h.sess.Messages()
		fmt.Printf("模型消息历史（投影，共 %d 条）：\n", len(msgs))
		for i, m := range msgs {
			fmt.Printf("  %2d  %-9s %s\n", i+1, m.Role, short(m.Content, 80))
		}
	})

	// /state 打印外部可信任务状态（独立于消息历史存在）。
	h.on("/state", func(string) {
		st := h.sess.State
		fmt.Printf("任务状态：\n  目标: %s\n  状态: %s\n  已验证事实: %d 条\n",
			st.Goal, st.Status, len(st.Facts))
		for _, f := range st.Facts {
			fmt.Printf("    - [%s] %s\n", f.Tool, f.Summary)
		}
	})

	// /restore 从持久化重放恢复指定会话（崩溃恢复语义见 session.Restore）。
	h.on("/restore", func(arg string) {
		if arg == "" {
			fmt.Println("用法: /restore <会话ID>（ID 见启动横幅或 data/sessions 目录）")
			return
		}
		sess, err := session.Restore(persist, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "恢复失败: %v\n", err)
			return
		}
		h.sess = sess
		// driver.Agent.Run 每次都会 rt.Session = sess，替换即生效。
		fmt.Printf("已切换到会话 %s（%d 条事件，状态 %s）\n", sess.ID, len(sess.Events), sess.State.Status)
	})
	return h
}

// eventSummary 摘要一条会话事件的载荷（按类型解出关键字段）。
func eventSummary(ev session.SessionEvent) string {
	switch ev.Type {
	case event.SessionEventTurnStart, event.SessionEventTurnEnd:
		var d event.TurnData
		_ = json.Unmarshal(ev.Data, &d)
		if d.Reason != "" {
			return "turn=" + fmt.Sprint(d.Turn) + " reason=" + d.Reason
		}
		return "turn=" + fmt.Sprint(d.Turn)
	case event.SessionEventUserMessage:
		var d event.UserMessageData
		_ = json.Unmarshal(ev.Data, &d)
		return short(d.Message, 60)
	case event.SessionEventAssistantMessage:
		var d event.AssistantMessageData
		_ = json.Unmarshal(ev.Data, &d)
		s := short(d.Content, 60)
		if len(d.ToolCalls) > 0 {
			s += fmt.Sprintf("（+%d 个工具调用）", len(d.ToolCalls))
		}
		return s
	case event.SessionEventToolCall:
		var d event.ToolCallData
		_ = json.Unmarshal(ev.Data, &d)
		return d.Name + "(" + short(d.Args, 48) + ")"
	case event.SessionEventToolResult:
		var d event.ToolResultData
		_ = json.Unmarshal(ev.Data, &d)
		if d.Error != "" {
			return d.Name + " 错误: " + short(d.Error, 48)
		}
		return d.Name + " → " + short(d.Text, 48)
	case event.SessionEventAssistantChunk:
		return "（流式增量）"
	default:
		return ""
	}
}
