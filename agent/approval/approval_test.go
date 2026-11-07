package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAnswerer struct {
	out  Outcome
	err  error
	seen []Request
}

func (f *fakeAnswerer) Decide(ctx context.Context, req Request) (Outcome, error) {
	f.seen = append(f.seen, req)
	return f.out, f.err
}

type fakeAudit struct {
	asked   []Request
	decided []Outcome
	err     error
}

func (f *fakeAudit) Asked(req Request) error {
	f.asked = append(f.asked, req)
	return f.err
}

func (f *fakeAudit) Decided(req Request, out Outcome) error {
	f.decided = append(f.decided, out)
	return f.err
}

// 正常路径：签发 id → 记"问了" → 问人 → 记"答了"。
func TestServiceDecide(t *testing.T) {
	ans := &fakeAnswerer{out: Approved}
	aud := &fakeAudit{}
	svc := NewService(ans, aud)

	out, err := svc.Decide(context.Background(), Request{
		ToolName: "shell", CallID: "c1", Reason: "递归删除", Turn: 2,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out != Approved {
		t.Fatalf("outcome = %v", out)
	}
	if len(aud.asked) != 1 || len(aud.decided) != 1 {
		t.Fatalf("审计应各记一条: asked=%d decided=%d", len(aud.asked), len(aud.decided))
	}
	if aud.decided[0] != Approved {
		t.Fatalf("审计结果 = %v", aud.decided[0])
	}
	// 请求 id 由服务签发，且"问了"与"答了"用同一个 id 配对
	if aud.asked[0].ID == "" {
		t.Fatal("应签发请求 id")
	}
	if aud.asked[0].ID != ans.seen[0].ID {
		t.Fatalf("应答方看到的 id 应与审计一致: %q vs %q", ans.seen[0].ID, aud.asked[0].ID)
	}
	if aud.asked[0].ToolName != "shell" || aud.asked[0].Reason != "递归删除" {
		t.Fatalf("请求内容丢了: %+v", aud.asked[0])
	}
}

// 未装配应答方 → Unavailable（fail-closed）。
func TestServiceWithoutAnswererFailsClosed(t *testing.T) {
	svc := NewService(nil, nil)
	out, err := svc.Decide(context.Background(), Request{ToolName: "shell"})
	if out != Unavailable {
		t.Fatalf("outcome = %v, want Unavailable", out)
	}
	if err == nil {
		t.Fatal("应说明原因")
	}
	var s *Service
	out, err = s.Decide(context.Background(), Request{})
	if out != Unavailable || err == nil {
		t.Fatalf("nil 服务应 fail-closed: %v %v", out, err)
	}
}

// 应答方报错 → 归一到 Unavailable（通道坏了 ≠ 人拒绝）。
func TestServiceNormalizesError(t *testing.T) {
	ans := &fakeAnswerer{err: errors.New("审批窗口崩了")}
	aud := &fakeAudit{}
	svc := NewService(ans, aud)

	out, err := svc.Decide(context.Background(), Request{ToolName: "shell"})
	if out != Unavailable {
		t.Fatalf("outcome = %v, want Unavailable", out)
	}
	if err == nil || !strings.Contains(err.Error(), "审批窗口崩了") {
		t.Fatalf("错误原因应带出来: %v", err)
	}
	// 审计仍要记"答了"（结果是 unavailable）
	if len(aud.decided) != 1 || aud.decided[0] != Unavailable {
		t.Fatalf("审计 = %+v", aud.decided)
	}
}

// 非法结果值归一到 Unavailable（不能凭空变成"批准"）。
func TestServiceNormalizesUnknownOutcome(t *testing.T) {
	ans := &fakeAnswerer{out: Outcome(99)}
	svc := NewService(ans, &fakeAudit{})
	out, _ := svc.Decide(context.Background(), Request{})
	if out != Unavailable {
		t.Fatalf("outcome = %v, want Unavailable", out)
	}
}

// 审计写失败 → 中止（不执行）：审计链断了就不知道谁批的。
func TestServiceAuditFailureAborts(t *testing.T) {
	ans := &fakeAnswerer{out: Approved}
	aud := &fakeAudit{err: errors.New("磁盘满了")}
	svc := NewService(ans, aud)

	out, err := svc.Decide(context.Background(), Request{ToolName: "shell"})
	if out != Unavailable {
		t.Fatalf("outcome = %v, want Unavailable", out)
	}
	if err == nil || !strings.Contains(err.Error(), "磁盘满了") {
		t.Fatalf("错误应带出审计失败原因: %v", err)
	}
	if len(ans.seen) != 0 {
		t.Fatal("审计写失败就不该问人")
	}
}

// 先验命中 → 复用决定、不问人；记新 id + reusedFrom（可审计）。
func TestServicePriorHitReusesDecision(t *testing.T) {
	ans := &fakeAnswerer{out: Rejected}
	aud := &fakeAudit{}
	svc := NewService(ans, aud)
	svc.SetPrior(func(ctx context.Context, req Request) (PriorDecision, bool) {
		if req.Turn != 3 || req.CallID != "c1" {
			return PriorDecision{}, false
		}
		return PriorDecision{Outcome: Approved, ReusedFrom: "ap-old"}, true
	})

	out, err := svc.Decide(context.Background(), Request{ToolName: "shell", CallID: "c1", Turn: 3})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out != Approved {
		t.Fatalf("outcome = %v, want Approved（复用先验）", out)
	}
	if len(ans.seen) != 0 {
		t.Fatalf("命中先验就不该问人，asked %d 次", len(ans.seen))
	}
	// 复用也要记审计：新 id + reusedFrom 指回被复用的那次决定。
	if len(aud.asked) != 1 || len(aud.decided) != 1 {
		t.Fatalf("审计应各记一条: asked=%d decided=%d", len(aud.asked), len(aud.decided))
	}
	if aud.asked[0].ID == "" || aud.asked[0].ID == "ap-old" {
		t.Fatalf("复用应记新 id，got %q", aud.asked[0].ID)
	}
	if aud.asked[0].ReusedFrom != "ap-old" {
		t.Fatalf("复用应指回原决定: %+v", aud.asked[0])
	}
	if aud.decided[0] != Approved {
		t.Fatalf("审计结果 = %v", aud.decided[0])
	}
}

// 先验没命中 → 照常问人。
func TestServicePriorMissAsksNormally(t *testing.T) {
	ans := &fakeAnswerer{out: Approved}
	aud := &fakeAudit{}
	svc := NewService(ans, aud)
	svc.SetPrior(func(ctx context.Context, req Request) (PriorDecision, bool) {
		return PriorDecision{}, false
	})

	out, err := svc.Decide(context.Background(), Request{ToolName: "shell", CallID: "c1", Turn: 3})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out != Approved {
		t.Fatalf("outcome = %v", out)
	}
	if len(ans.seen) != 1 {
		t.Fatalf("没命中就该问人，asked %d 次", len(ans.seen))
	}
	if aud.asked[0].ReusedFrom != "" {
		t.Fatalf("不是复用就不该带 reusedFrom: %+v", aud.asked[0])
	}
}

// 先验返回的不是「真决定」（撤回 / 无人应答）→ 不复用，照常问人。
func TestServicePriorNonDecisionFallsBackToAsking(t *testing.T) {
	for _, prior := range []Outcome{Cancelled, Unavailable} {
		ans := &fakeAnswerer{out: Approved}
		aud := &fakeAudit{}
		svc := NewService(ans, aud)
		svc.SetPrior(func(ctx context.Context, req Request) (PriorDecision, bool) {
			return PriorDecision{Outcome: prior, ReusedFrom: "ap-old"}, true
		})

		out, err := svc.Decide(context.Background(), Request{ToolName: "shell", CallID: "c1", Turn: 3})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if out != Approved {
			t.Fatalf("outcome = %v, want Approved（应回落问人）", out)
		}
		if len(ans.seen) != 1 {
			t.Fatalf("先验 outcome=%v 不该复用，asked %d 次", prior, len(ans.seen))
		}
		if aud.asked[0].ReusedFrom != "" {
			t.Fatalf("没复用就不该带 reusedFrom: %+v", aud.asked[0])
		}
	}
}

// 结果名解析（与 Outcome.String 互逆）。
func TestParseOutcome(t *testing.T) {
	for _, o := range []Outcome{Approved, Rejected, Cancelled, Unavailable} {
		got, ok := ParseOutcome(o.String())
		if !ok || got != o {
			t.Fatalf("%q 解析回 %v ok=%v", o.String(), got, ok)
		}
	}
	if _, ok := ParseOutcome("approved-ish"); ok {
		t.Fatal("不认识的结果名不该解析成功")
	}
}
