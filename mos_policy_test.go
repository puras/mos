package mos_test

import (
	"context"
	"strings"
	"testing"

	mos "github.com/puras/mos"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// TestConfigPolicyDenyBlocksExecution：装配期传进来的 Config.Policy，其拒绝
// 名单必须**同时**管「模型看不见」与「调不动」——两份语义读同一份名单是
// policy 包的承诺，只做到一半就是 fail-open。
//
// 这是一条回归守卫：SetPolicy 把守卫挂到 a.rt.Guards 上，而装配中途的 a.rt
// 是个空壳（Guards 为 nil，Add 是 no-op）。曾经因此把守卫**静默丢掉**，只剩
// 可见性生效——工具从模型清单里消失，但模型点名调用照样跑到底。
func TestConfigPolicyDenyBlocksExecution(t *testing.T) {
	reg := newTestRegistry(t)
	reg.Register(stubTool{name: "danger"})

	model := &scriptedModel{steps: []scriptStep{
		// 模型点名调一个它（本轮清单里）看不到的工具
		func(*llm.Request) *llm.Response { return toolCallResponse("c1", "danger", `{}`) },
		func(*llm.Request) *llm.Response { return textResponse("那我不调") },
	}}
	a, err := mos.New(mos.Config{
		Model:  model,
		Tools:  reg,
		Policy: &policy.Config{Deny: []string{"danger"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	sess := session.New("s-policy-deny", nil)
	if _, err := a.Run(context.Background(), sess, "试试危险工具"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if hasToolName(model.seenAt(0), "danger") {
		t.Fatalf("拒绝名单里的工具不该进模型清单（看不见）：%v", model.seenAt(0))
	}

	// 调不动：守卫在 preflight 就短路，工具本体没被执行（执行了就会返回 ok）。
	res, ok := toolResult(sess, "c1")
	if !ok {
		t.Fatal("缺被拒调用的回填")
	}
	if !res.IsError {
		t.Fatalf("拒绝名单里的工具不该执行成功：%s", res.Content)
	}
	if !strings.Contains(res.Content, "安全策略拒绝") {
		t.Fatalf("回填应说明是策略拒绝：%s", res.Content)
	}
}

// TestSetPolicyHotSwapUpdatesGuard：运行期热替换策略同样要**立刻**换掉守卫
// （与装配期走同一条路）：先放行、再拒绝，同一进程内两次调用结论不同。
func TestSetPolicyHotSwapUpdatesGuard(t *testing.T) {
	reg := newTestRegistry(t)
	reg.Register(stubTool{name: "danger"})

	model := &scriptedModel{steps: []scriptStep{
		func(*llm.Request) *llm.Response { return toolCallResponse("c1", "danger", `{}`) },
		func(*llm.Request) *llm.Response { return textResponse("第一次跑了") },
		func(*llm.Request) *llm.Response { return toolCallResponse("c2", "danger", `{}`) },
		func(*llm.Request) *llm.Response { return textResponse("第二次被拦") },
	}}
	a, err := mos.New(mos.Config{Model: model, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	sess := session.New("s-policy-hot", nil)
	if _, err := a.Run(context.Background(), sess, "第一次"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first, ok := toolResult(sess, "c1")
	if !ok || first.IsError {
		t.Fatalf("未装策略时应放行：%+v", first)
	}

	a.SetPolicy(&policy.Config{Deny: []string{"danger"}})

	if _, err := a.Run(context.Background(), sess, "第二次"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	second, ok := toolResult(sess, "c2")
	if !ok {
		t.Fatal("缺回填")
	}
	if !second.IsError || !strings.Contains(second.Content, "安全策略拒绝") {
		t.Fatalf("热替换后应被守卫拦下：%s", second.Content)
	}
}
