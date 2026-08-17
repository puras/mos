package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
)

type stubTool struct{ name, desc string }

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return t.desc }
func (t stubTool) Parameters() any     { return nil }
func (t stubTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "ok:" + t.name, nil
}

func TestExecuteBatchBasic(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(stubTool{name: "t1"})
	sess := session.NewSession("s")
	rt := &driver.RunContext{Session: sess, Tools: reg}
	rt.Hooks = hook.NewHookBus()
	rt.Guards = hook.NewGuardBus()
	rt.Truncator = agent.NewDefaultTruncator(agent.TruncatorConfig{})
	rt.Hooks.On(hook.HookToolExecute, ExecuteHook(rt))
	rt.Hooks.On(hook.HookToolPostExecute, TruncateHook(rt))
	calls := []llm.ToolCall{{ID: "c1", Name: "t1", Args: "{}"}}
	outs, suspend := ExecuteBatch(context.Background(), rt, sess, 1, 1, calls)
	if suspend != nil {
		t.Fatalf("unexpected suspend: %v", suspend)
	}
	if len(outs) != 1 || outs[0].Output.Err != nil || outs[0].Output.Text == "" {
		t.Fatalf("unexpected outcomes: %+v", outs)
	}
	if err := ApplyOutcomes(context.Background(), rt, sess, 1, calls, outs); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(sess.Events) == 0 {
		t.Fatal("no events logged")
	}
}

func TestExecuteBatchGuardDenies(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(stubTool{name: "t1"})
	sess := session.NewSession("s")
	rt := &driver.RunContext{Session: sess, Tools: reg}
	rt.Hooks = hook.NewHookBus()
	rt.Guards = hook.NewGuardBus()
	rt.Guards.Add(func(_ context.Context, _ *hook.HookEvent) error {
		return errors.New("denied by guard")
	})
	calls := []llm.ToolCall{{ID: "c1", Name: "t1", Args: "{}"}}
	outs, _ := ExecuteBatch(context.Background(), rt, sess, 1, 1, calls)
	if !outs[0].Denied {
		t.Fatal("expected denied outcome")
	}
	var denied *ToolDeniedError
	if !errors.As(outs[0].Err, &denied) {
		t.Fatalf("expected ToolDeniedError, got %v", outs[0].Err)
	}
}
