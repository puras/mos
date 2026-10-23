package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/agent/approval"
)

// 终端审批：y = 允许，n = 拒绝，**空回车 = 拒绝**（默认答案必须是安全的那个）。
func TestTerminalAnswerer(t *testing.T) {
	cases := []struct {
		in   string
		want approval.Outcome
	}{
		{"y\n", approval.Approved},
		{"Y\n", approval.Approved},
		{"yes\n", approval.Approved},
		{"n\n", approval.Rejected},
		{"no\n", approval.Rejected},
		{"\n", approval.Rejected}, // 空回车 = 拒绝
		{"whatever\n", approval.Rejected},
	}
	for _, c := range cases {
		var out bytes.Buffer
		a := terminalAnswerer{in: bufio.NewScanner(strings.NewReader(c.in)), out: &out}
		got, err := a.Decide(context.Background(), approval.Request{
			ToolName: "shell", Reason: "递归删除", Turn: 1,
		})
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("输入 %q → %v, want %v", c.in, got, c.want)
		}
		// 提示里要说清"批什么、为什么"
		if !strings.Contains(out.String(), "shell") || !strings.Contains(out.String(), "递归删除") {
			t.Fatalf("提示内容不足: %q", out.String())
		}
	}
}

// 终端关闭（EOF）→ 无人应答（fail-closed），不是"批准"。
func TestTerminalAnswererEOF(t *testing.T) {
	var out bytes.Buffer
	a := terminalAnswerer{in: bufio.NewScanner(strings.NewReader("")), out: &out}
	got, err := a.Decide(context.Background(), approval.Request{ToolName: "shell"})
	if got != approval.Unavailable {
		t.Fatalf("outcome = %v, want Unavailable", got)
	}
	if err == nil {
		t.Fatal("应说明没人应答")
	}
}
