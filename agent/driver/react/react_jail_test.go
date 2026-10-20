package react

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

// 端到端：文件 jail 在执行通道里真的拦住越界访问。
//
// 这是 S5d 的核心不变式——jail 不是装饰：越界路径的工具不执行、拒绝理由回填
// 模型、tool/call 仍留痕、事件形状与正常调用一致。
func TestFileJailDeniesThroughDriver(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	// jail 内有一个正常文件，一个指向 jail 外的 symlink。
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	})
	reg := tool.NewRegistry()
	for _, tl := range builtin.FileTools(fsys) {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}

	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		{Content: "读两个文件", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read_file", Args: `{"path":"../../etc/passwd"}`},
			{ID: "c2", Name: "read_file", Args: `{"path":"link.txt"}`},
		}, FinishReason: "tool_calls"},
		answer("都在 jail 里，不给读"),
	}}

	d := driver.New(&driver.RunContext{
		Planner: p,
		Tools:   reg,
	}, New())

	reply, err := d.Run(context.Background(), sess, "读一下")
	if err != nil {
		t.Fatalf("拒绝不应中断 turn: %v", err)
	}
	if reply != "都在 jail 里，不给读" {
		t.Fatalf("reply = %q", reply)
	}

	evs := sess.Events()
	// 两次调用 → 两条 tool/call + 两条 tool/result
	var results []*session.ToolResult
	for _, ev := range evs {
		if ev.Kind == session.EventToolResult {
			results = append(results, ev.ToolResult)
		}
	}
	if len(results) != 2 {
		t.Fatalf("tool/result 数 = %d", len(results))
	}
	for i, res := range results {
		if !res.IsError {
			t.Fatalf("result[%d] 应为错误: %+v", i, res)
		}
		if !strings.Contains(res.Content, "不在允许的根内") {
			t.Fatalf("result[%d] 应回填拒绝理由: %+v", i, res)
		}
	}
	// CallID 仍一一对应（结果须对回原调用）
	if results[0].CallID != "c1" || results[1].CallID != "c2" {
		t.Fatalf("CallID = %q/%q", results[0].CallID, results[1].CallID)
	}
	// 越界读没有产生任何副作用
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("jail 外文件不应被动过")
	}
}

// 对照：jail 内的正常读取放行，工具真的执行。
func TestFileJailAllowsInsideThroughDriver(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	})
	reg := tool.NewRegistry()
	for _, tl := range builtin.FileTools(fsys) {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}

	sess := session.New("t", nil)
	p := &scriptedPlanner{acts: []planner.Action{
		{Content: "读", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read_file", Args: `{"path":"ok.txt"}`},
		}, FinishReason: "tool_calls"},
		answer("读到了"),
	}}
	d := driver.New(&driver.RunContext{Planner: p, Tools: reg}, New())
	if _, err := d.Run(context.Background(), sess, "读一下"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult {
			if ev.ToolResult.IsError {
				t.Fatalf("jail 内读取不应报错: %+v", ev.ToolResult)
			}
			if !strings.Contains(ev.ToolResult.Content, "inside") {
				t.Fatalf("内容未回填: %+v", ev.ToolResult)
			}
			return
		}
	}
	t.Fatal("没有 tool/result 事件")
}
