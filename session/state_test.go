package session

import (
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

// 事件构造的小工具：Seq 由 Append 分配，这里直接手工造（派生逻辑不依赖 Seq）。
func userEv(turn int, text string) Event {
	return Event{Kind: EventUser, Turn: turn, Text: text}
}

func callEv(turn int, id, name string) Event {
	return Event{Kind: EventToolCall, Turn: turn, ToolCall: &llm.ToolCall{ID: id, Name: name}}
}

func resultEv(turn int, id, content string, isErr bool) Event {
	return Event{Kind: EventToolResult, Turn: turn, ToolResult: &ToolResult{CallID: id, Content: content, IsError: isErr}}
}

func turnEnd(turn int, reason string) Event {
	return Event{Kind: EventTurnEnd, Turn: turn, Reason: reason}
}

func always(string) bool { return true }

func TestDeriveTaskStateGoalAndFacts(t *testing.T) {
	events := []Event{
		userEv(1, "统计 lab 下的测试数"),
		callEv(1, "c1", "read_file"),
		resultEv(1, "c1", "{\"path\":\"lab/a_test.go\"}\n第二行证据", false),
		callEv(1, "c2", "shell"),
		resultEv(1, "c2", "ok", false),
		callEv(1, "c3", "write_file"),
		resultEv(1, "c3", "{\"created\":true}", false),
	}
	// 只信 read_file：shell（变更类）与 write_file 的输出都不配进状态。
	st := DeriveTaskState(events, func(name string) bool { return name == "read_file" })

	if st.Goal != "统计 lab 下的测试数" {
		t.Fatalf("Goal = %q", st.Goal)
	}
	if len(st.Facts) != 1 {
		t.Fatalf("Facts = %d 条, want 1（未信任工具的输出不是事实）: %+v", len(st.Facts), st.Facts)
	}
	f := st.Facts[0]
	if f.Tool != "read_file" {
		t.Fatalf("Fact.Tool = %q", f.Tool)
	}
	if !strings.HasPrefix(f.Summary, "read_file → ") || strings.Contains(f.Summary, "\n") {
		t.Fatalf("Summary 应为单行且带工具名前缀: %q", f.Summary)
	}
	if !strings.Contains(f.Evidence, "第二行证据") {
		t.Fatalf("Evidence 应保留环境输出原文: %q", f.Evidence)
	}
}

func TestDeriveTaskStateErrorResultIsNotFact(t *testing.T) {
	events := []Event{
		userEv(1, "g"),
		callEv(1, "c1", "read_file"),
		resultEv(1, "c1", "被守卫拒绝", true),
	}
	st := DeriveTaskState(events, always)
	if len(st.Facts) != 0 {
		t.Fatalf("失败结果不该成为事实: %+v", st.Facts)
	}
}

func TestDeriveTaskStateDedup(t *testing.T) {
	events := []Event{
		userEv(1, "g"),
		callEv(1, "c1", "read_file"),
		resultEv(1, "c1", "same", false),
		callEv(2, "c2", "read_file"),
		resultEv(2, "c2", "same", false), // 同工具 + 同证据：去重
		callEv(3, "c3", "read_file"),
		resultEv(3, "c3", "different", false),
	}
	st := DeriveTaskState(events, always)
	if len(st.Facts) != 2 {
		t.Fatalf("Facts = %d 条, want 2（相同证据去重）: %+v", len(st.Facts), st.Facts)
	}
}

func TestDeriveTaskStateStatus(t *testing.T) {
	cases := []struct {
		name   string
		events []Event
		want   TaskStatus
	}{
		{"空会话", nil, StatusPending},
		{"只有输入", []Event{userEv(1, "g")}, StatusPending},
		{"末轮未收尾", []Event{userEv(1, "g"), callEv(1, "c", "read_file")}, StatusRunning},
		{"正常收尾", []Event{userEv(1, "g"), turnEnd(1, "completed")}, StatusCompleted},
		{"出错收尾不算完成", []Event{userEv(1, "g"), turnEnd(1, "error")}, StatusRunning},
		{"前轮完成本轮开着", []Event{userEv(1, "g"), turnEnd(1, "completed"), userEv(2, "继续")}, StatusRunning},
		{"前轮完成本轮出错", []Event{userEv(1, "g"), turnEnd(1, "completed"), turnEnd(2, "terminated")}, StatusRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveTaskState(tc.events, always).Status; got != tc.want {
				t.Fatalf("Status = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeriveTaskStateNilTrusted(t *testing.T) {
	events := []Event{
		userEv(1, "g"),
		callEv(1, "c1", "read_file"),
		resultEv(1, "c1", "x", false),
	}
	st := DeriveTaskState(events, nil)
	if len(st.Facts) != 0 || st.Goal != "g" {
		t.Fatalf("trusted=nil 应全不信任: %+v", st)
	}
}

func TestTaskStateSnapshot(t *testing.T) {
	st := &TaskState{
		Goal:    "修复测试",
		Status:  StatusRunning,
		Pending: []string{"跑 go test"},
		Blocked: []string{"等审批"},
		Facts:   []Fact{{Tool: "read_file", Summary: "read_file → 40 行"}},
	}
	s := st.Snapshot()
	for _, want := range []string{"任务目标: 修复测试", "任务状态: running", "待办: 跑 go test", "阻塞: 等审批", "- read_file → 40 行"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Snapshot 缺 %q:\n%s", want, s)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	// 多字节字符不切半
	s := strings.Repeat("汉", 600)
	got := truncateRunes(s, 512)
	if len([]rune(got)) != 513 || !strings.HasSuffix(got, "…") {
		t.Fatalf("rune 截断越界: %d runes", len([]rune(got)))
	}
	if truncateRunes("短", 10) != "短" {
		t.Fatalf("短串不该被截断")
	}
	if got := firstLine("a\nb", 10); got != "a" {
		t.Fatalf("firstLine = %q", got)
	}
}

// Goal 取最后一条 user：TaskState 描述进行中的任务，已收尾的旧主题不该
// 永久霸占目标位（否则每次压缩注入的摘要都把模型钉死在第一个问题上）。
func TestDeriveTaskStateGoalTracksLatestUser(t *testing.T) {
	events := []Event{
		userEv(1, "当前系统提示词是啥？"),             // t1：早已 completed 的旧主题
		userEv(2, "做一个 mos-web 要不要先做 TUI？"), // 当前主题
	}
	st := DeriveTaskState(events, func(string) bool { return false })
	if st.Goal != "做一个 mos-web 要不要先做 TUI？" {
		t.Fatalf("Goal = %q, want 当前主题（旧目标不该霸占目标位）", st.Goal)
	}
}
