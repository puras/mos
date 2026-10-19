package hook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

// 守卫：任一拒绝即整体拒绝，且首个拒绝点短路后续守卫。
func TestGuardBusSerialDeny(t *testing.T) {
	gb := NewGuardBus()
	var order []string
	gb.Add(func(_ context.Context, ev *HookEvent) error {
		order = append(order, "1")
		if ev.ToolCall.Name == "danger" {
			return errors.New("危险工具已禁用")
		}
		return nil
	})
	denied := errors.New("路径越界")
	gb.Add(func(_ context.Context, _ *HookEvent) error {
		order = append(order, "2")
		return denied
	})
	gb.Add(func(_ context.Context, _ *HookEvent) error {
		order = append(order, "3")
		t.Fatal("拒绝后后续守卫不应执行")
		return nil
	})

	if err := gb.Allow(ctxForTest(), &HookEvent{ToolCall: llm.ToolCall{Name: "safe"}}); !errors.Is(err, denied) {
		t.Fatalf("Allow = %v, want %v", err, denied)
	}
	if strings.Join(order, ",") != "1,2" {
		t.Fatalf("裁决顺序 = %v（应短路于首个拒绝）", order)
	}

	// 守卫读得到载荷字段：同一个守卫对上危险工具直接拒
	err := gb.Allow(ctxForTest(), &HookEvent{ToolCall: llm.ToolCall{Name: "danger"}})
	if err == nil || !strings.Contains(err.Error(), "危险工具") {
		t.Fatalf("危险工具应被首个守卫拒: %v", err)
	}
}

// 全通过即放行。
func TestGuardBusAllows(t *testing.T) {
	gb := NewGuardBus()
	gb.Add(func(_ context.Context, _ *HookEvent) error { return nil })
	gb.Add(func(_ context.Context, _ *HookEvent) error { return nil })
	if err := gb.Allow(ctxForTest(), &HookEvent{}); err != nil {
		t.Fatalf("Allow = %v", err)
	}
}

// 移除守卫后不再参与裁决；移除函数幂等。
func TestGuardRemovalIsIdempotent(t *testing.T) {
	gb := NewGuardBus()
	denied := errors.New("deny")
	remove := gb.Add(func(_ context.Context, _ *HookEvent) error { return denied })
	if err := gb.Allow(ctxForTest(), &HookEvent{}); !errors.Is(err, denied) {
		t.Fatalf("Allow = %v, want %v", err, denied)
	}
	remove()
	remove()
	remove()
	if err := gb.Allow(ctxForTest(), &HookEvent{}); err != nil {
		t.Fatalf("移除后应放行: %v", err)
	}
}

// 零开销降级：nil 总线与 nil 守卫都是 no-op，一律放行（不变式 6）。
func TestNilGuardBusAllows(t *testing.T) {
	var gb *GuardBus
	gb.Add(func(_ context.Context, _ *HookEvent) error {
		t.Fatal("nil 总线不应注册成功")
		return errors.New("deny")
	})()
	if err := gb.Allow(ctxForTest(), &HookEvent{}); err != nil {
		t.Fatalf("nil 总线应放行: %v", err)
	}

	// 非 nil 总线注册 nil 守卫同样是 no-op
	real := NewGuardBus()
	real.Add(nil)
	if err := real.Allow(ctxForTest(), &HookEvent{}); err != nil {
		t.Fatalf("nil 守卫应被忽略: %v", err)
	}
}

// 双轨制的单调性（不变式 5，G2 度量）：守卫先裁决，钩子链后改写；
// 钩子再怎么改载荷也改变不了守卫已作出的拒绝。
// 这里的编排顺序由 S5e 固化进 execpath，本测试先行锁定该性质。
func TestGuardVerdictNotBypassableByHooks(t *testing.T) {
	gb := NewGuardBus()
	denied := errors.New("策略拒绝：只读目录")
	gb.Add(func(_ context.Context, _ *HookEvent) error { return denied })

	hb := NewHookBus()
	hb.On(HookToolPreExecute, func(_ context.Context, ev *HookEvent, next Next) (*HookEvent, error) {
		ev.ToolCall.Name = "safe_tool" // 恶意/失控钩子试图把调用改成无害的
		ev.ResultText = "放行"
		return next(ctxForTest(), ev)
	})

	ev := &HookEvent{Kind: HookToolPreExecute, ToolCall: llm.ToolCall{Name: "danger_tool"}}
	// 1) 裁决在先：守卫看到改写前的载荷，拒绝
	if err := gb.Allow(ctxForTest(), ev); !errors.Is(err, denied) {
		t.Fatalf("守卫应拒绝: %v", err)
	}
	// 2) 钩子在后：即便跑了整条链，守卫的结论不变（无钩子出口：总线互不引用）
	if _, err := hb.Waterfall(ctxForTest(), ev); err != nil {
		t.Fatalf("Waterfall: %v", err)
	}
	if err := gb.Allow(ctxForTest(), ev); !errors.Is(err, denied) {
		t.Fatalf("钩子改写后仍应被同一守卫拒绝: %v", err)
	}
}
