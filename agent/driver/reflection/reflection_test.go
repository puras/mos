package reflection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
)

// ---- mock 模型：离线演示用 ----

// genModel 按调用次序产出初稿/修订稿（ReAct 决策器）。
type genModel struct {
	replies []string
	n       int
}

func newGen(replies ...string) *genModel { return &genModel{replies: replies} }

func (m *genModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *genModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	m.n++
	reply := "初稿"
	if m.n-1 < len(m.replies) {
		reply = m.replies[m.n-1]
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: reply}}, nil
}
func (m *genModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// reviewModel 按次序返回评审结果（Reflector）。
type reviewModel struct {
	reviews []string // 每轮评审的模型输出
	n       int
	err     error
}

func newReview(reviews ...string) *reviewModel { return &reviewModel{reviews: reviews} }

func (m *reviewModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *reviewModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.n++
	reply := `{"issues":[],"score":90}`
	if m.n-1 < len(m.reviews) {
		reply = m.reviews[m.n-1]
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: reply}}, nil
}
func (m *reviewModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// failModel 生成阶段直接报错。
type failModel struct{}

func (m *failModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *failModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return nil, errors.New("mock model failed")
}
func (m *failModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// ---- 工具 ----

type noopTool struct{}

func (t noopTool) Name() string        { return "noop" }
func (t noopTool) Description() string { return "noop" }
func (t noopTool) Parameters() any     { return map[string]any{} }
func (t noopTool) Execute(context.Context, json.RawMessage) (any, error) {
	return "ok", nil
}

func newRT(gen, review llm.ChatModel) *driver.RunContext {
	reg := tool.NewRegistry()
	_ = reg.Register(noopTool{})
	return &driver.RunContext{
		Tools:     reg,
		Planner:   planner.NewReAct(gen),
		Reflector: planner.NewReflector(review),
	}
}

func reflectionEvents(sess *session.Session) []session.SessionEvent {
	var out []session.SessionEvent
	for _, ev := range sess.Events {
		if strings.HasPrefix(ev.Type, "reflection/") {
			out = append(out, ev)
		}
	}
	return out
}

// TestReflectionPassFirstRound 验证首轮评审通过：只生成一次，无修订。
func TestReflectionPassFirstRound(t *testing.T) {
	rt := newRT(newGen("最终产出"), newReview())
	a := New(rt)
	sess := session.NewSession("s1")

	reply, err := a.Run(context.Background(), sess, "写一段话")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "最终产出" {
		t.Fatalf("reply = %q", reply)
	}
	if sess.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed", sess.State.Status)
	}
	evs := reflectionEvents(sess)
	if len(evs) != 3 { // start + review + end
		t.Fatalf("reflection events = %d, want 3: %+v", len(evs), evs)
	}
	if len(sess.Messages()) != 2 { // user(目标) + assistant(产出)
		t.Fatalf("messages = %d, want 2", len(sess.Messages()))
	}
}

// TestReflectionReviseThenPass 验证一轮修订后评审通过。
func TestReflectionReviseThenPass(t *testing.T) {
	gen := newGen("初稿", "修订稿")
	rt := newRT(gen, newReview(`{"issues":["缺少细节"],"score":60}`, `{"issues":[],"score":95}`))
	a := New(rt)
	sess := session.NewSession("s1")

	reply, err := a.Run(context.Background(), sess, "写一段话")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "修订稿" {
		t.Fatalf("reply = %q, want 修订稿", reply)
	}
	if gen.n != 2 {
		t.Fatalf("generator calls = %d, want 2 (生成 + 修订)", gen.n)
	}
	evs := reflectionEvents(sess)
	if len(evs) != 4 { // start + review + review + end
		t.Fatalf("reflection events = %d, want 4: %+v", len(evs), evs)
	}
	if len(sess.Messages()) != 5 { // user + assistant + user(评审) + user(修订) + assistant(修订稿)
		t.Fatalf("messages = %d, want 5: %+v", len(sess.Messages()), sess.Messages())
	}
	// 评审意见以 reflection 来源写入日志（Model-visible means logged）
	var sawReview bool
	for _, m := range sess.Messages() {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "缺少细节") {
			sawReview = true
		}
	}
	if !sawReview {
		t.Fatal("review issues should be logged as user message")
	}
}

// TestReflectionMaxIterations 验证评审始终不通过时按最大迭代收尾。
func TestReflectionMaxIterations(t *testing.T) {
	rt := newRT(newGen("v1", "v2", "v3"), newReview(`{"issues":["问题"]}`, `{"issues":["问题"]}`))
	a := New(rt, WithMaxIterations(2))
	sess := session.NewSession("s1")

	reply, err := a.Run(context.Background(), sess, "目标")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "v3" {
		t.Fatalf("reply = %q, want 最后修订稿（生成 1 次 + 修订 2 次）", reply)
	}
	evs := reflectionEvents(sess)
	last := evs[len(evs)-1]
	if last.Type != event.SessionEventReflectionEnd {
		t.Fatalf("last event = %s", last.Type)
	}
	var end event.ReflectionEndData
	if err := json.Unmarshal(last.Data, &end); err != nil {
		t.Fatalf("unmarshal end: %v", err)
	}
	if end.Status != "max_iterations" {
		t.Fatalf("end status = %q, want max_iterations", end.Status)
	}
	if sess.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed（达到迭代上限仍交付最后产出）", sess.State.Status)
	}
}

// TestReflectionNoReflector 验证未配置评审器时拒绝运行。
func TestReflectionNoReflector(t *testing.T) {
	rt := newRT(newGen("产出"), nil)
	rt.Reflector = nil
	a := New(rt)
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "目标"); err == nil {
		t.Fatal("Run should fail without reflector")
	}
}

// TestReflectionFailOnGenerate 验证生成阶段失败 → blocked。
func TestReflectionFailOnGenerate(t *testing.T) {
	rt := newRT(&failModel{}, newReview())
	a := New(rt)
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "目标"); err == nil {
		t.Fatal("Run should fail on generate")
	}
	if sess.State.Status != session.StatusBlocked {
		t.Fatalf("status = %v, want blocked", sess.State.Status)
	}
	evs := reflectionEvents(sess)
	last := evs[len(evs)-1]
	if last.Type != event.SessionEventReflectionEnd {
		t.Fatalf("last event = %s", last.Type)
	}
}
