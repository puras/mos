// MoS 恢复示例（FR-G3）：进程被 kill -9 之后，宿主怎么把那一轮接回去。
//
// 演示的完整链路（**真进程硬杀**，不是模拟事件序列）：
//
//	子进程跑一轮 → 模型要调 note 工具 → 宿主规则判「需人工确认」→
//	审批应答方永远不答（模拟「人还没来得及答」）→ 父进程 SIGKILL 子进程
//	→ 父进程重开会话 → 从日志派生现场（未决审批 + 悬空调用 + 被中断的轮次）
//	→ 宿主显式指示续跑（这次有人批了）→ 工具真跑一遍 → 真实结果回填
//	→ 本轮正常收尾（沿用原轮次号）
//
// 三条口径（与 docs/ARCHITECTURE.md §3.17 一致）：
//   - 框架**不会自动**续跑：默认只报告，要不要续由宿主决定；
//   - 续跑**不替人回答**：原先那条未决审批仍留在未决列表里（问过了，没人答）；
//   - 续跑**会把那次调用再执行一遍**——外部副作用（shell 之类）可能重复，
//     宿主自己的工具要按幂等设计。
//
// 跑法（**不需要 API Key**：示例用一个假模型——恢复机制与真模型无关）：
//
//	cd examples/recovery
//	go run .
//
// 想接真模型：把 scriptedModel 换成 mos.NewModel(...)（其它代码不用动）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

const (
	// phaseFlag：父进程默认跑演练；子进程带 -phase=child。
	phaseFlag = "phase"
	// dirEnv：父进程把本轮的工作目录告诉子进程（会话日志也在这儿）。
	dirEnv = "MOS_RECOVERY_DIR"
	// toolName 是本例唯一的工具：记一条笔记（写文件）。
	toolName = "note"
	// markerName 是子进程进入「审批等待」的标记文件（父进程据此下刀）。
	markerName = "approval-asked"
	// notesName 是工具真正执行时写的文件（父进程最后打印它，证明真跑过了）。
	notesName = "notes.txt"
)

func main() {
	phase := flag.String(phaseFlag, "parent", "运行阶段：parent（默认，演练）/ child（子进程）")
	flag.Parse()

	switch *phase {
	case "child":
		child()
	case "parent":
		if err := parent(); err != nil {
			fmt.Fprintln(os.Stderr, "演练失败:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "未知阶段 %q\n", *phase)
		os.Exit(1)
	}
}

// parent 跑整场演练：起子进程 → 等它卡在审批等待 → SIGKILL → 重开会话 →
// 看清现场 → 显式续跑 → 看收尾。
func parent() error {
	dir, err := os.MkdirTemp("", "mos-recovery-")
	if err != nil {
		return fmt.Errorf("建临时目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	fmt.Printf("工作目录: %s\n", dir)

	// ① 起子进程：它跑一轮然后卡在审批等待里。
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取当前可执行文件: %w", err)
	}
	cmd := exec.Command(self, "-phase=child")
	cmd.Env = append(os.Environ(), dirEnv+"="+dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("起子进程: %w", err)
	}

	// ② 等它进入审批等待（标记文件出现），然后 SIGKILL——不给收尾的机会。
	marker := filepath.Join(dir, markerName)
	if err := waitFor(marker, 30*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	fmt.Println("子进程已进入审批等待 → SIGKILL（不给它收尾的机会）")
	if err := cmd.Process.Kill(); err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	_ = cmd.Wait() // 预期以「被信号杀掉」结束

	// ③ 重开会话（宿主视角）：现场全在日志里，重开即得。
	sess, err := session.Open(dir, "recovery")
	if err != nil {
		return fmt.Errorf("重开会话: %w", err)
	}
	pending := sess.PendingApprovals()
	hanging := sess.HangingCalls()
	fmt.Println("\n──── 重开后，从日志派生出来的现场 ────")
	fmt.Printf("未决审批: %d 条\n", len(pending))
	for _, p := range pending {
		fmt.Printf("  - %s（调用 %s）：%s\n", p.ToolName, p.CallID, p.Reason)
	}
	fmt.Printf("悬空调用: %d 条\n", len(hanging))
	for _, h := range hanging {
		fmt.Printf("  - %s（轮次 %d，调用 %s）：%s\n", h.ToolName, h.Turn, h.CallID, h.Args)
	}
	fmt.Printf("被中断的轮次: %d\n", sess.InterruptedTurn())

	// ④ 宿主显式指示续跑：这次有人批了（应答方直接批准）。
	reg := tool.NewRegistry()
	note := &noteTool{path: filepath.Join(dir, notesName)}
	if err := reg.Register(note); err != nil {
		return fmt.Errorf("注册工具: %w", err)
	}
	ans := &approvingAnswerer{}
	a, err := mos.New(mos.Config{
		Model:    &scriptedModel{reply: "续完了：笔记已记下。"},
		Tools:    reg,
		Rules:    []policy.Check{askRule},
		Approval: ans,
	})
	if err != nil {
		return fmt.Errorf("装配: %w", err)
	}
	defer func() { _ = a.Close() }()

	fmt.Println("\n──── 宿主显式指示续跑 ────")
	reply, err := a.Resume(context.Background(), sess, mos.ResumeOptions{})
	if err != nil {
		return fmt.Errorf("续跑: %w", err)
	}
	fmt.Printf("模型答复: %s\n", reply)

	// ⑤ 收尾核对：悬空清零、轮次正常收尾、工具真跑过一遍。
	fmt.Println("\n──── 续跑之后 ────")
	fmt.Printf("悬空调用: %d 条\n", len(sess.HangingCalls()))
	fmt.Printf("被中断的轮次: %d\n", sess.InterruptedTurn())
	fmt.Printf("工具执行次数: %d\n", note.calls)
	fmt.Printf("未决审批: %d 条（续跑**不替人回答**：原先那条仍留在未决列表里）\n", len(sess.PendingApprovals()))
	fmt.Printf("本次重新问了审批: %d 次\n", ans.asked)
	last := sess.Events()[len(sess.Events())-1]
	fmt.Printf("本轮收尾: %s / %s（沿用原轮次号 %d）\n", last.Kind, last.Reason, last.Turn)
	if b, err := os.ReadFile(filepath.Join(dir, notesName)); err == nil {
		fmt.Printf("笔记文件内容: %s", b)
	}
	return nil
}

// child 是子进程：跑一轮，卡在审批等待里等父进程 SIGKILL。
func child() {
	dir := os.Getenv(dirEnv)
	if dir == "" {
		fmt.Fprintln(os.Stderr, "子进程缺少 "+dirEnv)
		os.Exit(1)
	}
	sess, err := session.Open(dir, "recovery")
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开会话:", err)
		os.Exit(1)
	}
	reg := tool.NewRegistry()
	if err := reg.Register(&noteTool{path: filepath.Join(dir, notesName)}); err != nil {
		fmt.Fprintln(os.Stderr, "注册工具:", err)
		os.Exit(1)
	}
	a, err := mos.New(mos.Config{
		Model:    &scriptedModel{reply: "子进程不该走到这里。"},
		Tools:    reg,
		Rules:    []policy.Check{askRule},
		Approval: &blockingAnswerer{marker: filepath.Join(dir, markerName)},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "装配:", err)
		os.Exit(1)
	}
	defer func() { _ = a.Close() }()
	// 心跳 goroutine：审批等待里所有 goroutine 都阻塞时，Go 的死锁检测会报
	// "all goroutines are asleep"（示例专用：让子进程安静地等父进程来杀）。
	go func() {
		for {
			time.Sleep(time.Hour)
		}
	}()
	if _, err := a.Run(context.Background(), sess, "帮我记一条笔记"); err != nil {
		fmt.Fprintln(os.Stderr, "运行:", err)
		os.Exit(1)
	}
	// 走到这里说明审批没卡住——演练的前提不成立。
	fmt.Fprintln(os.Stderr, "审批应该一直等着，等父进程 SIGKILL")
	os.Exit(1)
}

// waitFor 等文件出现（子进程进入审批等待的信号）。
func waitFor(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等 %s 出现超时", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// askRule 是**宿主侧**的参数级规则：调 note 工具要人工确认。
//
// 为什么在示例里而不是框架里：什么算危险由宿主决定，框架只提供决策点
// （deny → 守卫，ask → 审批通道）。
func askRule(ctx context.Context, call policy.Call) (policy.Verdict, string) {
	return policy.VerdictAsk, "写文件之前要人工确认"
}

// blockingAnswerer 是子进程里的审批应答方：写标记文件，然后**永远不答**
// （模拟「人还没来得及答」，等父进程 SIGKILL）。
type blockingAnswerer struct{ marker string }

func (a *blockingAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	if err := os.WriteFile(a.marker, []byte("asked"), 0o644); err != nil {
		return approval.Unavailable, err
	}
	select {} // 永远等下去
}

// approvingAnswerer 是父进程里的应答方：这次有人批了。
type approvingAnswerer struct{ asked int }

func (a *approvingAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	a.asked++
	return approval.Approved, nil
}

// noteTool 是本例的工具：把一条笔记追加到文件里。
type noteTool struct {
	path  string
	calls int
}

func (t *noteTool) Name() string        { return toolName }
func (t *noteTool) Description() string { return "记一条笔记（追加到文件）" }
func (t *noteTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "笔记内容"},
		},
		"required": []string{"text"},
	}
}

func (t *noteTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	t.calls++
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "%s\n", a.Text); err != nil {
		return nil, err
	}
	return "已记下", nil
}

// scriptedModel 是**假模型**：第一次请求要调工具，拿到工具结果后作答。
//
// 用假模型是为了让示例不依赖 API Key、不依赖模型是否听话——恢复机制与
// 真模型无关。想接真模型就换成 mos.NewModel(...)。
type scriptedModel struct{ reply string }

func (m *scriptedModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.reply}}, nil
		}
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: toolName, Args: `{"text":"恢复演练的一条笔记"}`}},
	}}, nil
}

func (m *scriptedModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *scriptedModel) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming
}
