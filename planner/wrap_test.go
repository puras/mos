package planner

import (
	"context"
	"errors"
	"testing"
	"time"

	"hegq.net/mos/llm"
)

// fakePlanner 是测试用的可编程规划器。
type fakePlanner struct {
	calls  int
	err    error
	action Action
}

func (p *fakePlanner) NextAction(context.Context, []llm.Message, []llm.Tool) (Action, error) {
	p.calls++
	if p.err != nil {
		return Action{}, p.err
	}
	return p.action, nil
}

func TestPlannerWithLogging(t *testing.T) {
	base := &fakePlanner{action: Action{Content: "hi"}}
	var got []LogResult
	p := WithLogging(base, func(r LogResult) { got = append(got, r) })
	history := []llm.Message{{Role: llm.RoleUser, Content: "q"}}
	tools := []llm.Tool{{Name: "calc"}}
	action, err := p.NextAction(context.Background(), history, tools)
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if action.Content != "hi" {
		t.Fatalf("content = %q", action.Content)
	}
	if len(got) != 1 || len(got[0].History) != 1 || len(got[0].Tools) != 1 || got[0].Err != nil {
		t.Fatalf("log result = %+v", got)
	}
}

func TestPlannerWithRetry(t *testing.T) {
	base := &fakePlanner{action: Action{Content: "ok"}}
	p := WithRetry(base, 3, time.Millisecond)
	base.err = errors.New("boom")
	if _, err := p.NextAction(context.Background(), nil, nil); err == nil {
		t.Fatal("should fail after exhausted retries")
	}
	if base.calls != 3 {
		t.Fatalf("calls = %d, want 3", base.calls)
	}
	base.err = nil
	if _, err := p.NextAction(context.Background(), nil, nil); err != nil {
		t.Fatalf("NextAction after recovery: %v", err)
	}
}
