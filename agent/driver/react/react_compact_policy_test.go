package react

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
)

// 压缩失败但本轮继续（宿主选了 skip-and-continue）：记一条提示、跳过压缩、
// 本轮照常回答——上下文可能已超预算，这个风险由宿主自担。
func TestCompactionFailureSkipAndContinue(t *testing.T) {
	sess := session.New("t", nil)
	prefillSessions(t, sess, 5)
	comp := &stubCompactor{planOK: true, cut: 2, sumErr: errors.New("模型不可用")}
	var notified error
	rt := &driver.RunContext{
		Planner:                 &scriptedPlanner{acts: []planner.Action{answer("继续回答")}},
		Compactor:               comp,
		SystemPrompt:            "sys",
		CompactionFailurePolicy: driver.CompactionSkipAndContinue,
		OnCompactionFailure:     func(err error) { notified = err },
	}
	a := driver.New(rt, New())

	reply, err := a.Run(context.Background(), sess, "新问题")
	if err != nil {
		t.Fatalf("跳过压缩时本轮应继续: %v", err)
	}
	if reply != "继续回答" {
		t.Fatalf("回复不对: %q", reply)
	}
	if notified == nil || !strings.Contains(notified.Error(), "模型不可用") {
		t.Fatalf("应通知宿主压缩失败: %v", notified)
	}

	// 日志里有一条 compaction 事件：没有摘要（= 没压缩成），原因在 failed 里
	var compEv *session.Event
	evs := sess.Events()
	for i := range evs {
		if evs[i].Kind == session.EventCompaction {
			compEv = &evs[i]
		}
	}
	if compEv == nil {
		t.Fatal("应落一条 compaction 事件（提示压缩失败）")
	}
	if compEv.Compaction.Summary != "" {
		t.Fatalf("失败的压缩不该有摘要: %q", compEv.Compaction.Summary)
	}
	if !strings.Contains(compEv.Compaction.Failed, "模型不可用") {
		t.Fatalf("失败原因应记在 failed 里: %q", compEv.Compaction.Failed)
	}

	// 投影不受影响：失败的压缩等于没发生（不注入摘要、不遮蔽历史）
	for _, m := range sess.Messages() {
		if strings.HasPrefix(m.Content, session.CompactionSummaryPrefix) {
			t.Fatal("失败的压缩不该往投影里注入摘要")
		}
	}
}
