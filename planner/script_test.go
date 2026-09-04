package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

// fakeModel 返回固定响应（供 Script/ReAct 规划器测试）。
type fakeModel struct {
	replies []llm.Response // 按调用顺序弹出；耗尽后复用最后一个
	i       int
}

func (f *fakeModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	r := f.replies[len(f.replies)-1]
	if f.i < len(f.replies) {
		r = f.replies[f.i]
	}
	f.i++
	return &r, nil
}

func (f *fakeModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return f.Chat(context.Background(), nil)
}

func (f *fakeModel) Capabilities() llm.Capability { return 0 }

func scriptReply(json string) llm.Response {
	return llm.Response{Message: llm.Message{Content: json}}
}

func TestNewScriptParsesPlan(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(
		`{"goal":"改名","steps":[{"id":"a","tool":"find_files","args":{"dir":"."},"output":"files"}]}`)}}
	p := NewScript(m)
	plan, err := p.Script(context.Background(), "改名", nil, []llm.Tool{{Name: "find_files", Description: "找文件"}})
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if plan.Goal != "改名" || len(plan.Steps) != 1 || plan.Steps[0].Tool != "find_files" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestNewScriptStripsFence(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(
		"```json\n{\"goal\":\"g\",\"steps\":[{\"id\":\"a\",\"tool\":\"current_time\"}]}\n```")}}
	p := NewScript(m)
	plan, err := p.Script(context.Background(), "g", nil, []llm.Tool{{Name: "current_time"}})
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestNewScriptErrorsOnGarbage(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply("not json at all")}}
	p := NewScript(m)
	if _, err := p.Script(context.Background(), "g", nil, nil); err == nil {
		t.Fatal("expected error on non-JSON reply")
	}
}

func TestScriptPromptIncludesToolNames(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(`{"steps":[{"id":"a","tool":"current_time"}]}`)}}
	// 通过 captureModel 抓取本次请求的消息文本
	got := ""
	wrap := &captureModel{inner: m, onReq: func(req *llm.Request) {
		for _, msg := range req.Messages {
			got += msg.Content
		}
	}}
	p := NewScript(wrap)
	if _, err := p.Script(context.Background(), "g", nil, []llm.Tool{{Name: "current_time", Description: "时间"}}); err != nil {
		t.Fatalf("Script: %v", err)
	}
	if !strings.Contains(got, "current_time") {
		t.Fatalf("prompt should list tool names, got: %s", got)
	}
}

type captureModel struct {
	inner *fakeModel
	onReq func(*llm.Request)
}

func (c *captureModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.onReq(req)
	return c.inner.Chat(ctx, req)
}
func (c *captureModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return c.inner.ChatStream(ctx, req, emit)
}
func (c *captureModel) Capabilities() llm.Capability { return 0 }
