package mos_test

// kill -9 恢复演练（FR-G3 / S9.3）：**真进程**硬杀 → 重开会话 → 宿主看清现场
// （未决审批 + 悬空调用）→ 显式指示 → 续完 → 本轮正常收尾。
//
// 演练用真进程（不是模拟）：子进程 = 本测试二进制自身（`-test.run` 指向子进程
// 用例），父进程在子进程**卡在审批等待**时 SIGKILL 它（应答方永远不答），
// 然后按宿主的流程走一遍：重开会话 → 派生未决审批与悬空调用 → 显式续跑。
//
// 口径提醒（与 §3.17 一致）：
//   - 续跑会把那次调用**再执行一遍**——父进程这边的同名工具是个**立即返回**
//     的实现（真实场景里是「这次真的能跑完」；跑不完的话，幂等边界按诚实
//     口径：文件类靠观察门 CAS，shell 副作用可能重复）；
//   - 原先那条**未决审批仍留在未决列表里**（日志是事实：问过了，没人答）——
//     续跑**不替宿主回答人**（不写 decided），宿主自行按「那次调用是否还
//     悬空」决定要不要把它从面板上撤下。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

const (
	kill9SessionID = "kill9"
	kill9DirEnv    = "MOS_KILL9_DIR"
	kill9ToolName  = "blocker"
)

// kill9Model：历史里没有工具结果时请求工具调用，有了就作答。
type kill9Model struct{ reply string }

func (m *kill9Model) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.reply}}, nil
		}
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: kill9ToolName, Args: "{}"}},
	}}, nil
}

func (m *kill9Model) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *kill9Model) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming
}

// kill9AskRule 让工具走「需人工确认」档（ask → 审批通道）。
func kill9AskRule(ctx context.Context, call policy.Call) (policy.Verdict, string) {
	return policy.VerdictAsk, "这一步要人工确认"
}

// kill9BlockingAnswerer 是子进程里的审批应答方：写标记文件（让父进程知道
// 「已进入审批等待」），然后**永远不答**（等父进程 SIGKILL）。
type kill9BlockingAnswerer struct{ marker string }

func (a *kill9BlockingAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	if err := os.WriteFile(a.marker, []byte("asked"), 0o644); err != nil {
		return approval.Unavailable, err
	}
	// 永远不答（父进程来杀）。
	select {}
}

// kill9ApprovingAnswerer 是父进程里的应答方：批准（这次真的有人批了）。
type kill9ApprovingAnswerer struct{ asked int }

func (a *kill9ApprovingAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	a.asked++
	return approval.Approved, nil
}

// kill9Tool 是子进程里的那个工具：真跑起来的话也**永远不返回**（双保险——
// 正常路径下它根本轮不到执行，审批等待就把子进程卡住了）。
type kill9Tool struct{ marker string }

func (t *kill9Tool) Name() string        { return kill9ToolName }
func (t *kill9Tool) Description() string { return "卡住不返回的工具" }
func (t *kill9Tool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *kill9Tool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	if err := os.WriteFile(t.marker, []byte("executed"), 0o644); err != nil {
		return nil, err
	}
	select {}
}

// kill9QuickTool 是父进程里的同名工具：立即返回（真实场景的「这次真跑完了」）。
type kill9QuickTool struct{ calls int }

func (t *kill9QuickTool) Name() string        { return kill9ToolName }
func (t *kill9QuickTool) Description() string { return "立即返回的工具" }
func (t *kill9QuickTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *kill9QuickTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	t.calls++
	return "跑完了", nil
}

// TestKill9Child 是子进程入口：跑一轮，卡在审批等待里等父进程 SIGKILL。
func TestKill9Child(t *testing.T) {
	dir := os.Getenv(kill9DirEnv)
	if dir == "" {
		t.Skip("只在子进程里跑")
	}
	sess, err := session.Open(dir, kill9SessionID)
	if err != nil {
		t.Fatalf("打开会话: %v", err)
	}
	reg := tool.NewRegistry()
	if err := reg.Register(&kill9Tool{marker: filepath.Join(dir, "tool-executed")}); err != nil {
		t.Fatalf("注册: %v", err)
	}
	a, err := mos.New(mos.Config{
		Model:    &kill9Model{reply: "子进程的答复"},
		Tools:    reg,
		Rules:    []policy.Check{kill9AskRule},
		Approval: &kill9BlockingAnswerer{marker: filepath.Join(dir, "approval-asked")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 走到这里说明审批没卡住——演练的前提不成立。
	t.Fatal("审批应该一直等着，等父进程 SIGKILL")
}

// TestKill9ResumeE2E：kill -9 → 重开会话 → 看清现场 → 显式续跑 → 本轮收尾。
func TestKill9ResumeE2E(t *testing.T) {
	if os.Getenv(kill9DirEnv) != "" {
		t.Skip("这是子进程")
	}
	dir := t.TempDir()

	// 起子进程。
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=TestKill9Child", "-test.v")
	cmd.Env = append(os.Environ(), kill9DirEnv+"="+dir)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}

	// 等它进入审批等待（标记文件出现）。
	marker := filepath.Join(dir, "approval-asked")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("子进程没进入审批等待:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// kill -9（SIGKILL：不给收尾的机会）。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	cmd.Wait() // 预期以「被信号杀掉」结束

	// 重开会话（宿主视角）。
	sess, err := session.Open(dir, kill9SessionID)
	if err != nil {
		t.Fatalf("重开会话: %v", err)
	}

	// 宿主看清现场：一个未决审批 + 一个悬空调用 + 被中断的那一轮。
	pending := sess.PendingApprovals()
	if len(pending) != 1 {
		t.Fatalf("应派生出一个未决审批，got %d: %+v", len(pending), pending)
	}
	if pending[0].CallID != "c1" || pending[0].ToolName != kill9ToolName {
		t.Fatalf("未决审批内容不对: %+v", pending[0])
	}
	hanging := sess.HangingCalls()
	if len(hanging) != 1 {
		t.Fatalf("应派生出一个悬空调用，got %d: %+v", len(hanging), hanging)
	}
	if hanging[0].ToolName != kill9ToolName || hanging[0].CallID != "c1" {
		t.Fatalf("悬空调用内容不对: %+v", hanging[0])
	}
	if turn := sess.InterruptedTurn(); turn != 1 {
		t.Fatalf("被中断的轮次应是 1，got %d", turn)
	}

	// 显式指示续跑（宿主这边有人批了）。
	reg := tool.NewRegistry()
	quick := &kill9QuickTool{}
	if err := reg.Register(quick); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ans := &kill9ApprovingAnswerer{}
	a, err := mos.New(mos.Config{
		Model:    &kill9Model{reply: "续完了"},
		Tools:    reg,
		Rules:    []policy.Check{kill9AskRule},
		Approval: ans,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reply, err := a.Resume(context.Background(), sess, mos.ResumeOptions{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if reply != "续完了" {
		t.Fatalf("reply = %q", reply)
	}
	if quick.calls != 1 {
		t.Fatalf("悬空调用应执行一次，got %d", quick.calls)
	}
	if ans.asked != 1 {
		t.Fatalf("续跑应重新问一次审批（原先那条没人答），got %d", ans.asked)
	}
	if n := len(sess.HangingCalls()); n != 0 {
		t.Fatalf("续跑后不应还有悬空调用，got %d", n)
	}

	// 本轮正常收尾（沿用原轮次号）。
	last := sess.Events()[len(sess.Events())-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "completed" {
		t.Fatalf("续跑后应正常收尾: %+v", last)
	}
	if last.Turn != 1 {
		t.Fatalf("收尾轮次应沿用原轮次 1，got %d", last.Turn)
	}

	// 日志里回填的是**真实**结果（不是投影里的合成失败回执）。
	var real bool
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil && ev.ToolResult.CallID == "c1" {
			real = true
			if ev.ToolResult.Content == "" || ev.ToolResult.IsError {
				t.Fatalf("回填应是成功的真实结果: %+v", ev.ToolResult)
			}
		}
	}
	if !real {
		t.Fatalf("日志里应有真实 tool/result: %+v", sess.Events())
	}

	// 落盘的日志也要对：重开一次，悬空调用清零、轮次正常收尾。
	again, err := session.Open(dir, kill9SessionID)
	if err != nil {
		t.Fatalf("再次重开会话: %v", err)
	}
	if n := len(again.HangingCalls()); n != 0 {
		t.Fatalf("落盘后不应还有悬空调用，got %d", n)
	}
	if turn := again.InterruptedTurn(); turn != 0 {
		t.Fatalf("落盘后不该还有被中断的轮次，got %d", turn)
	}
}
