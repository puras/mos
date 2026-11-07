package react

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// reasoningOnly 是「只吐思维链就收手」的典型产出：正文空、工具调用也空。
func reasoningOnly() planner.Action {
	return planner.Action{
		ReasoningContent: "下一步该读配置文件……",
		FinishReason:     "stop",
		Usage: llm.Usage{
			PromptTokens: 10, CompletionTokens: 320, TotalTokens: 330,
			Details: llm.UsageDetails{Output: llm.OutputTokenDetails{Reasoning: 217}},
		},
	}
}

// 空回复不是「完成」：既无正文也无工具调用时，本轮以失败收尾
// （fail-closed）——记成 completed 的话，宿主与用户都以为这一轮正常结束，
// 而日志里查不到任何错误。
func TestBlankAnswerFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		act    planner.Action
		detail string // turn/end 详情里应出现的片段
	}{
		{"只有思维链", reasoningOnly(), "只有思维链"},
		{"空白正文", planner.Action{Content: "  \n\t", FinishReason: "stop"}, "既无正文"},
		{"截断且正文为空", planner.Action{ReasoningContent: "想了很久", FinishReason: "length"}, "finish_reason=length"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess := session.New("t", nil)
			p := &scriptedPlanner{acts: []planner.Action{c.act}}
			a := driver.New(&driver.RunContext{Planner: p}, New())

			reply, err := a.Run(context.Background(), sess, "继续")
			if err == nil {
				t.Fatalf("空回复应以错误收尾，reply = %q", reply)
			}
			if !errors.Is(err, ErrNoAnswer) {
				t.Fatalf("错误应认得出 ErrNoAnswer: %v", err)
			}

			evs := sess.Events()
			end := evs[len(evs)-1]
			if end.Kind != session.EventTurnEnd || end.Reason != "error" {
				t.Fatalf("末条事件应是以失败收尾的 turn/end: %+v", end)
			}
			if !strings.Contains(end.Error, c.detail) {
				t.Fatalf("turn/end 详情应写明「%s」: %q", c.detail, end.Error)
			}

			// 事实留痕：那一轮的 assistant 事件（思维链 + 用量）仍在日志里——
			// 只是这一轮不冒充完成。
			var asst *session.Event
			for i := range evs {
				if evs[i].Kind == session.EventAssistant {
					asst = &evs[i]
				}
			}
			if asst == nil {
				t.Fatal("assistant 事件应留痕")
			}
			if asst.Reasoning != c.act.ReasoningContent {
				t.Fatalf("思维链应留痕: %+v", asst)
			}
			// 停止原因一并留痕：截断的正文照样交付，但日志要能分出「说完的」
			// 与「被切开的」。
			if asst.FinishReason != c.act.FinishReason {
				t.Fatalf("停止原因应留痕: %+v", asst)
			}
			if c.act.Usage.TotalTokens > 0 && (asst.Usage == nil || asst.Usage.TotalTokens != c.act.Usage.TotalTokens) {
				t.Fatalf("用量应留痕: %+v", asst.Usage)
			}
		})
	}
}

// 正常作答照旧记 completed（回归：fail-closed 不许误伤正常轮次）。
func TestAnswerStillCompletes(t *testing.T) {
	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{answer("北京")}}
	a := driver.New(&driver.RunContext{Planner: p}, New())

	reply, err := a.Run(context.Background(), sess, "现在几点")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "北京" {
		t.Fatalf("reply = %q", reply)
	}
	evs := sess.Events()
	end := evs[len(evs)-1]
	if end.Kind != session.EventTurnEnd || end.Reason != "completed" {
		t.Fatalf("末条事件应是 completed 的 turn/end: %+v", end)
	}
}
