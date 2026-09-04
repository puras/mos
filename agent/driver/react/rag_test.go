package react

import (
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/memory"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
)

// mockRetriever 固定返回预设分块的长期记忆检索器。
type mockRetriever struct {
	docs []memory.Document
	// 记录最近一次检索的 query 与 limit，供断言"按任务目标检索"。
	query string
	limit int
}

func (m *mockRetriever) Retrieve(_ context.Context, query string, limit int) ([]memory.Document, error) {
	m.query, m.limit = query, limit
	return m.docs, nil
}

// directModel 不调用工具，直接作答；捕获收到的消息供断言注入内容。
type directModel struct {
	captured []llm.Message
}

func (m *directModel) Capabilities() llm.Capability { return 0 }
func (m *directModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.captured = req.Messages
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "好的"}}, nil
}
func (m *directModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func newReActRT(model llm.ChatModel, reg *tool.Registry, cfg *driver.RunContext) *driver.RunContext {
	rt := &driver.RunContext{Tools: reg, Planner: planner.NewReAct(model), SystemPrompt: "你是助手"}
	if cfg != nil {
		rt.Retriever = cfg.Retriever
		rt.RetrieverCfg = cfg.RetrieverCfg
		rt.Observer = cfg.Observer
	}
	return rt
}

func TestAgentRetrieverInjection(t *testing.T) {
	ret := &mockRetriever{docs: []memory.Document{
		{Source: "doc-a", Content: "Go 并发模型基于 goroutine。", Score: 0.8},
		{Source: "doc-b", Content: "SQLite 支持 FTS5 全文检索。", Score: 0.6},
	}}
	m := &directModel{}
	a := New(newReActRT(m, tool.NewRegistry(), &driver.RunContext{
		Retriever:    ret,
		RetrieverCfg: agent.RetrieverConfig{MaxChunks: 3, Limit: 8},
	}))

	sess := session.NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "讲解 Go 并发")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "好的" {
		t.Fatalf("reply = %q", reply)
	}

	// 检索 query 取自任务目标（首轮目标 = 输入），limit 透传配置
	if !strings.Contains(ret.query, "Go 并发") {
		t.Fatalf("retrieve query = %q, want 按任务目标检索", ret.query)
	}
	if ret.limit != 8 {
		t.Fatalf("retrieve limit = %d, want 8", ret.limit)
	}

	// 注入消息经翻译边界转 user 角色并标 [rag] 前缀，携带来源与得分
	last := m.captured[len(m.captured)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("injected message role = %v, want user", last.Role)
	}
	for _, want := range []string{"[rag]", "检索到的相关记忆", "[来源: doc-a] (score 0.80)", "Go 并发模型基于 goroutine"} {
		if !strings.Contains(last.Content, want) {
			t.Fatalf("injected content missing %q:\n%s", want, last.Content)
		}
	}

	// 注入消息不写入会话日志（每轮重检，不留痕）
	for _, msg := range sess.Messages() {
		if strings.Contains(msg.Content, "检索到的相关记忆") || msg.Source == "rag" {
			t.Fatalf("rag injection leaked into session log: %+v", sess.Messages())
		}
	}
	if len(sess.Messages()) != 2 {
		t.Fatalf("messages len = %d, want 2 (user + assistant)", len(sess.Messages()))
	}
}

func TestKnowledgeGapEvent(t *testing.T) {
	// 检索有候选但全部低于知识守门最低分 → 判定知识差距，广播事件且不注入
	ret := &mockRetriever{docs: []memory.Document{
		{Source: "doc-a", Content: "与查询无关的内容", Score: 0.1},
	}}
	m := &directModel{}
	var events []agent.Event
	a := New(newReActRT(m, tool.NewRegistry(), &driver.RunContext{
		Retriever:    ret,
		RetrieverCfg: agent.RetrieverConfig{MaxChunks: 3, Limit: 8, MinScore: 0.5},
		Observer:     func(ev agent.Event) { events = append(events, ev) },
	}))

	sess := session.NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "数据库事务隔离级别")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "好的" {
		t.Fatalf("reply = %q", reply)
	}

	var gap *agent.Event
	for i := range events {
		if events[i].Type == agent.EventKnowledgeGap {
			gap = &events[i]
			break
		}
	}
	if gap == nil {
		t.Fatalf("no agent.EventKnowledgeGap in events: %+v", events)
	}
	if gap.Content != "数据库事务隔离级别" {
		t.Fatalf("gap content = %q, want 查询文本", gap.Content)
	}
	if gap.SessionID != "s1" {
		t.Fatalf("gap session = %q", gap.SessionID)
	}
	// 知识差距时不注入任何 rag 内容
	for _, msg := range m.captured {
		if strings.Contains(msg.Content, "[rag]") {
			t.Fatalf("knowledge gap still injected rag content: %+v", msg)
		}
	}
}

func TestAgentRetrieverConflictResolution(t *testing.T) {
	// 冲突裁决：同一来源保留最高分分块；MaxChunks 截断注入条数
	ret := &mockRetriever{docs: []memory.Document{
		{Source: "doc-a", Content: "甲版本-低分", Score: 0.3},
		{Source: "doc-a", Content: "甲版本-高分", Score: 0.9},
		{Source: "doc-b", Content: "乙版本-中分", Score: 0.5},
		{Source: "doc-c", Content: "丙版本-高分", Score: 0.85},
		{Source: "doc-d", Content: "丁版本-高分", Score: 0.95},
	}}
	rt := &driver.RunContext{Retriever: ret, RetrieverCfg: agent.RetrieverConfig{MaxChunks: 3, Limit: 8}, Session: session.NewSession("s1")}

	extras, err := retrieveAndInject(context.Background(), rt, 1, "查询")
	if err != nil {
		t.Fatalf("retrieveAndInject: %v", err)
	}
	if len(extras) != 1 {
		t.Fatalf("extras = %d, want 1", len(extras))
	}
	content := extras[0].Content
	// 同源冲突：只保留高分版本（doc-a 高分），低分版本被丢弃
	if strings.Contains(content, "甲版本-低分") {
		t.Fatalf("low-score chunk should be dropped:\n%s", content)
	}
	if !strings.Contains(content, "甲版本-高分") {
		t.Fatalf("high-score chunk missing:\n%s", content)
	}
	// 注入条数 = 同源合并后（4 源）截断到 MaxChunks=3
	if strings.Count(content, "- [来源:") != 3 {
		t.Fatalf("injected chunks = %d, want 3 (MaxChunks):\n%s", strings.Count(content, "- [来源:"), content)
	}
}

func TestAgentRetrieverNotConfigured(t *testing.T) {
	// 未配置检索器时行为与既往一致：不注入、不检索
	m := &directModel{}
	a := New(newReActRT(m, tool.NewRegistry(), nil))
	sess := session.NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "你好"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range m.captured {
		if strings.Contains(msg.Content, "[rag]") {
			t.Fatalf("rag injected without retriever: %+v", msg)
		}
	}
}
