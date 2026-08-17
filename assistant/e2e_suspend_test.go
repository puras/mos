package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
	"hegq.net/mos/tool/builtin"
)

// suspendWriteModel 模拟审批流：第一次模型调用发起 write_file（触发审批挂起），
// 恢复后的第二次调用（历史含工具结果）直接作答。
func suspendWriteModel(workDir string) llm.ChatModel {
	return suspendWriteModelImpl{workDir: workDir}
}

type suspendWriteModelImpl struct{ workDir string }

func (m suspendWriteModelImpl) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m suspendWriteModelImpl) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	var toolMsgs int
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			toolMsgs++
		}
	}
	if toolMsgs == 0 {
		// 第一次：发起 write_file（路径在 WorkDir 内 → 需要人工确认）
		path := filepath.Join(m.workDir, "out.txt")
		args := `{"path":"` + path + `","content":"hello"}`
		return &llm.Response{Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "我来写文件",
			ToolCalls: []llm.ToolCall{{ID: "w1", Name: "write_file", Args: args}},
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "文件已写入完成",
	}}, nil
}
func (suspendWriteModelImpl) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// newSuspendAssistant 构建带异步审批 + checkpoint + 持久化的智能体。
func newSuspendAssistant(t *testing.T, workDir string) (*Assistant, *approval.InMemApproval, *checkpoint.MemStore, session.Persistence) {
	t.Helper()
	persist, err := session.NewJSONL(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	ap := approval.NewInMemApproval()
	store := checkpoint.NewMemStore()
	pol := NewDefaultPolicy(nil, workDir)
	pol.Approval = ap

	a, err := New(suspendWriteModel(workDir), []tool.Tool{builtin.WriteFile{}}, Config{
		Policy:         pol,
		Approval:       ap,
		CheckpointStore: store,
		Persistence:    persist,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, ap, store, persist
}

// TestE2E_SuspendResumeApproval 验证 M2.8 端到端链路：
// 模型发起 write_file → 策略需人工确认 → Approval.Request → 驱动抛 SuspendError 挂起
// （checkpoint 落盘）→ 平台 Resolve → Handle.Resume(approved) → 从挂起轮续跑 →
// 工具执行 → 下一轮作答完成。
func TestE2E_SuspendResumeApproval(t *testing.T) {
	workDir := t.TempDir()
	a, ap, store, persist := newSuspendAssistant(t, workDir)

	sess := a.NewSession()
	_, err := a.Run(context.Background(), sess, "写一个文件")
	if err == nil {
		t.Fatal("Run 应返回 SuspendError，got nil")
	}
	var suspend *approval.SuspendError
	if !errors.As(err, &suspend) {
		t.Fatalf("err = %v, want *approval.SuspendError", err)
	}
	if suspend.Request.ToolName != "write_file" {
		t.Fatalf("suspend.Request.ToolName = %q", suspend.Request.ToolName)
	}

	// 挂起后：approval/request + checkpoint/save 事件已落库，checkpoint 已存
	if !hasEvent(sess.Events, event.SessionEventApprovalRequest) {
		t.Fatal("缺少 approval/request 事件")
	}
	if !hasEvent(sess.Events, event.SessionEventCheckpointSave) {
		t.Fatal("缺少 checkpoint/save 事件")
	}
	ckpt, err := store.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("checkpoint 未落库: %v", err)
	}
	if ckpt.ApprovalID != suspend.ID || ckpt.SuspendedCallID != "w1" {
		t.Fatalf("ckpt = %+v", ckpt)
	}
	if ap.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", ap.Pending())
	}

	// 平台 Resolve（one-shot）+ Handle.Resume 续跑
	if err := ap.Resolve(context.Background(), suspend.ID, approval.ApprovalDecision{Approved: true, DecidedAt: time.Now()}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	h, err := a.Handle(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply, err := h.Resume(context.Background(), approval.ApprovalDecision{Approved: true, DecidedAt: time.Now()})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !strings.Contains(reply, "文件已写入完成") {
		t.Fatalf("reply = %q", reply)
	}

	// 恢复后：approval/resolve + checkpoint/restore 事件；任务完成状态
	if !hasEvent(h.Session.Events, event.SessionEventApprovalResolve) {
		t.Fatal("缺少 approval/resolve 事件")
	}
	if !hasEvent(h.Session.Events, event.SessionEventCheckpointRestore) {
		t.Fatal("缺少 checkpoint/restore 事件")
	}
	if h.Session.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed", h.Session.State.Status)
	}
	// checkpoint 已清理（任务完成）
	if _, err := store.Load(context.Background(), sess.ID); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("checkpoint 应已清理, err = %v", err)
	}
	// write_file 实际执行（文件落盘）
	if _, err := os.Stat(filepath.Join(workDir, "out.txt")); err != nil {
		t.Fatalf("write_file 未实际执行: %v", err)
	}
	// 持久化可重放：从 JSONL 重放的事件能重建出相同轨迹
	restored, err := session.Restore(persist, sess.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !hasEvent(restored.Events, event.SessionEventApprovalResolve) {
		t.Fatal("重放后缺少 approval/resolve 事件")
	}
}

// TestE2E_SuspendResumeDenied 验证拒绝路径：Resume(approved=false) 时
// 工具被显式拒绝回填（用户拒绝原因反馈给模型），任务继续完成。
func TestE2E_SuspendResumeDenied(t *testing.T) {
	workDir := t.TempDir()
	a, _, _, _ := newSuspendAssistant(t, workDir)

	sess := a.NewSession()
	_, err := a.Run(context.Background(), sess, "写一个文件")
	var suspend *approval.SuspendError
	if !errors.As(err, &suspend) {
		t.Fatalf("err = %v, want SuspendError", err)
	}

	h, err := a.Handle(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply, err := h.Resume(context.Background(), approval.ApprovalDecision{
		Approved: false, Reason: "不允许写文件", DecidedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !strings.Contains(reply, "文件已写入完成") {
		t.Fatalf("reply = %q（拒绝后模型应继续作答）", reply)
	}
	// 工具未实际执行：文件不存在
	if _, err := os.Stat(filepath.Join(workDir, "out.txt")); err == nil {
		t.Fatal("write_file 不应在拒绝路径执行")
	}
}

// hasEvent 检查事件流是否包含指定类型。
func hasEvent(events []session.SessionEvent, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// containsStr 报告 slice 是否包含指定元素。
func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// humanInputModel 模拟 request_human_input 场景：第一次调用发起 request_human_input，
// 恢复后的第二次调用（历史含工具结果回答）复述回答。
type humanInputModel struct{ wantAnswer string }

func (m humanInputModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m humanInputModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	var toolMsgs int
	var answer string
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			toolMsgs++
			answer = msg.Content
		}
	}
	if toolMsgs == 0 {
		args := `{"question":"目标目录里最大的文件是哪个？","context":"需要人类查看"}`
		return &llm.Response{Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "我来问人类",
			ToolCalls: []llm.ToolCall{{ID: "h1", Name: "request_human_input", Args: args}},
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "人类回答是: " + answer,
	}}, nil
}
func (humanInputModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestE2E_RequestHumanInput 验证 request_human_input 工具端到端：
// 模型主动请求人类输入 → 工具发起审批 → 挂起（checkpoint 落库）→ 平台作答 →
// Handle.Resume 续跑 → 工具返回人类回答 → 模型基于回答继续。
func TestE2E_RequestHumanInput(t *testing.T) {
	persist, err := session.NewJSONL(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	ap := approval.NewInMemApproval()
	store := checkpoint.NewMemStore()
	pol := NewDefaultPolicy(nil, t.TempDir())
	pol.Approval = ap

	a, err := New(humanInputModel{wantAnswer: "X"}, nil, Config{
		Policy:         pol,
		Approval:       ap,
		CheckpointStore: store,
		Persistence:    persist,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// request_human_input 自动注册（配置 Approval 时）
	if !containsStr(a.ToolNames(), "request_human_input") {
		t.Fatal("request_human_input 未自动注册")
	}

	sess := a.NewSession()
	_, err = a.Run(context.Background(), sess, "查目标目录里最大的文件")
	var suspend *approval.SuspendError
	if !errors.As(err, &suspend) {
		t.Fatalf("err = %v, want *approval.SuspendError", err)
	}
	if suspend.Request.ToolName != "request_human_input" || suspend.Request.Command == "" {
		t.Fatalf("suspend = %+v", suspend)
	}

	// 人类作答：Content 即回答
	if err := ap.Resolve(context.Background(), suspend.ID, approval.ApprovalDecision{
		Approved: true, Content: "report.tar.gz (2.4GB)", DecidedBy: "human@console",
		DecidedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	h, err := a.Handle(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply, err := h.Resume(context.Background(), approval.ApprovalDecision{
		Approved: true, Content: "report.tar.gz (2.4GB)", DecidedBy: "human@console",
		DecidedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !strings.Contains(reply, "report.tar.gz") {
		t.Fatalf("reply = %q，应包含人类回答", reply)
	}
	// 工具结果含回答（模型可见）
	var toolResultSeen bool
	for _, msg := range h.Session.Messages() {
		if msg.Role == llm.RoleTool && strings.Contains(msg.Content, "report.tar.gz") {
			toolResultSeen = true
		}
	}
	if !toolResultSeen {
		t.Fatal("工具结果应包含人类回答")
	}
	if h.Session.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed", h.Session.State.Status)
	}
}
