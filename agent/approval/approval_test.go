package approval

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInMemRequestResolveAwait(t *testing.T) {
	a := NewInMemApproval()
	ctx := context.Background()
	id, err := a.Request(ctx, ApprovalRequest{ToolName: "write_file", Command: "a.txt"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if id == "" {
		t.Fatal("Request 返回空 ID")
	}
	if a.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", a.Pending())
	}

	// 异步 Resolve 后 Await 取回
	dec := ApprovalDecision{Approved: true, Reason: "ok", DecidedAt: time.Now()}
	if err := a.Resolve(ctx, id, dec); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a.Pending() != 0 {
		t.Fatalf("pending after resolve = %d, want 0", a.Pending())
	}
	got, err := a.Await(ctx, id)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !got.Approved {
		t.Fatal("await decision should be approved")
	}
}

func TestInMemOneShot(t *testing.T) {
	a := NewInMemApproval()
	id, _ := a.Request(context.Background(), ApprovalRequest{ToolName: "shell"})
	_ = a.Resolve(context.Background(), id, ApprovalDecision{Approved: true})
	// 二次 Resolve → ErrAlreadyResolved
	if err := a.Resolve(context.Background(), id, ApprovalDecision{Approved: false}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolve err = %v, want ErrAlreadyResolved", err)
	}
}

func TestInMemUnknownID(t *testing.T) {
	a := NewInMemApproval()
	if err := a.Resolve(context.Background(), "nope", ApprovalDecision{}); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("resolve unknown err = %v, want ErrUnknownID", err)
	}
	if _, err := a.Await(context.Background(), "nope"); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("await unknown err = %v, want ErrUnknownID", err)
	}
}

func TestInMemAwaitTimeout(t *testing.T) {
	a := NewInMemApproval()
	id, _ := a.Request(context.Background(), ApprovalRequest{ToolName: "shell"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Await(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("await timeout err = %v, want DeadlineExceeded", err)
	}
}

// stubConfirm 是同步 Confirmer 测试桩。
type stubConfirm struct{ allow bool }

func (c stubConfirm) Confirm(context.Context, ConfirmRequest) (bool, error) { return c.allow, nil }

func TestConfirmerAdapterImmediate(t *testing.T) {
	ad := NewConfirmerAdapter(stubConfirm{allow: true})
	id, err := ad.Request(context.Background(), ApprovalRequest{ToolName: "write_file"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// 立即可用决策（Immediate）：不挂起
	dec, ok := ad.Decision(context.Background(), id)
	if !ok || !dec.Approved {
		t.Fatalf("Decision = %+v, ok=%v; want approved immediate", dec, ok)
	}
	// Resolve 是 no-op（决策在 Request 已同步确认）
	if err := ad.Resolve(context.Background(), id, ApprovalDecision{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestConfirmerAdapterDenied(t *testing.T) {
	ad := NewConfirmerAdapter(stubConfirm{allow: false})
	id, err := ad.Request(context.Background(), ApprovalRequest{ToolName: "shell"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	dec, ok := ad.Decision(context.Background(), id)
	if !ok || dec.Approved {
		t.Fatalf("Decision = %+v; want immediate denied", dec)
	}
}

func TestRequiredError(t *testing.T) {
	e := &RequiredError{Request: ApprovalRequest{ToolName: "shell"}}
	if e.Error() == "" {
		t.Fatal("RequiredError.Error empty")
	}
	var target *RequiredError
	if !errors.As(e, &target) {
		t.Fatal("errors.As should match *RequiredError")
	}
	if target.Request.ToolName != "shell" {
		t.Fatal("Request not preserved")
	}
}

func TestSuspendError(t *testing.T) {
	e := &SuspendError{
		ID:         "abc",
		Request:    ApprovalRequest{ToolName: "shell"},
		CallID:     "c1",
		ReplayTurn: 2,
	}
	if s := e.Error(); s == "" || s != "agent: suspended awaiting approval abc (tool=shell, hook=, turn=2)" {
		t.Fatalf("Error = %q", s)
	}
}
