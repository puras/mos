package handle

import (
	"context"
	"errors"
	"testing"

	"github.com/puras/mos/agent/checkpoint"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

// testNewAgent 装配最小 Agent：ReActDriver + read_file 工具。
func testNewAgent(sess *session.Session) (*driver.Agent, *driver.RunContext, error) {
	reg := tool.NewRegistry()
	if err := reg.Register(builtin.ReadFile{}); err != nil {
		return nil, nil, err
	}
	rt := &driver.RunContext{Tools: reg, Session: sess}
	return driver.New(rt, react.NewReAct(rt)), rt, nil
}

func TestRegistryCreateGetDispose(t *testing.T) {
	reg := NewRegistry(testNewAgent, checkpoint.NewMemStore(), nil)
	ctx := context.Background()

	h, err := reg.Create(ctx, session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "s1" {
		t.Fatalf("handle id = %q", h.ID)
	}
	if got, ok := reg.Get("s1"); !ok || got != h {
		t.Fatalf("Get(s1) = %v, %v", got, ok)
	}
	if err := h.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	// 幂等
	if err := h.Dispose(ctx); err != nil {
		t.Fatalf("Dispose idempotent: %v", err)
	}
}

func TestRegistryResume(t *testing.T) {
	reg := NewRegistry(testNewAgent, checkpoint.NewMemStore(), nil)
	ctx := context.Background()

	sess := session.NewSession("r1")
	// 模拟会话事件（read_file 调用——工具存在，校验通过）
	_ = sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: 1, Reason: "user"})
	_ = sess.Append(event.SessionEventUserMessage, event.UserMessageData{Message: "hi"})
	_ = sess.Append(event.SessionEventToolCall, event.ToolCallData{Turn: 1, CallID: "c1", Name: "read_file", Args: `{"path":"a.txt"}`})

	h, err := reg.Resume(ctx, sess)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if h.Session.ID != "r1" {
		t.Fatalf("handle session id = %q", h.Session.ID)
	}
}

func TestValidateMissing(t *testing.T) {
	reg := NewRegistry(testNewAgent, checkpoint.NewMemStore(), nil)
	ctx := context.Background()

	sess := session.NewSession("m1")
	// 会话引用了不存在于 Tools 的工具（grep 未注册）
	_ = sess.Append(event.SessionEventToolCall, event.ToolCallData{Turn: 1, CallID: "c1", Name: "grep", Args: `{}`})

	_, err := reg.Resume(ctx, sess)
	if err == nil {
		t.Fatal("Resume 应因 missing 工具报错")
	}
	var m Missing
	if !errors.As(err, &m) {
		t.Fatalf("err = %v, want Missing", err)
	}
	if len(m.Tools) != 1 || m.Tools[0] != "grep" {
		t.Fatalf("Missing.Tools = %v", m.Tools)
	}
}

func TestRegistryDisposeAll(t *testing.T) {
	reg := NewRegistry(testNewAgent, checkpoint.NewMemStore(), nil)
	ctx := context.Background()
	if _, err := reg.Create(ctx, session.NewSession("a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := reg.Create(ctx, session.NewSession("b")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := reg.DisposeAll(ctx); err != nil {
		t.Fatalf("DisposeAll: %v", err)
	}
	// Dispose 后 Get 仍返回句柄（标记 disposed），重复 DisposeAll 幂等
	if err := reg.DisposeAll(ctx); err != nil {
		t.Fatalf("DisposeAll idempotent: %v", err)
	}
}
