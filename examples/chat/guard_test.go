package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// 宿主参数级规则的判定（deny / ask / allow 三档）。
func TestHostPolicyVerdicts(t *testing.T) {
	hp := hostPolicy()
	verdict := func(name, args string) policy.Verdict {
		if err := hp.Guard()(context.Background(), &hook.HookEvent{
			ToolCall: llm.ToolCall{Name: name, Args: args},
		}); err != nil {
			return policy.VerdictDeny
		}
		// 守卫只认 deny；ask 档要看钩子
		ev := &hook.HookEvent{ToolCall: llm.ToolCall{Name: name, Args: args}}
		_, _ = hp.Approval()(context.Background(), ev,
			func(ctx context.Context, e *hook.HookEvent) (*hook.HookEvent, error) { return e, nil })
		if ev.Decision.Kind == hook.DecisionAsk {
			return policy.VerdictAsk
		}
		return policy.VerdictAllow
	}

	if got := verdict("read_file", `{"path":"/tmp/.env"}`); got != policy.VerdictDeny {
		t.Fatalf("读 .env 应 deny, got %v", got)
	}
	if got := verdict("write_file", `{"path":"/tmp/id_rsa","content":"x"}`); got != policy.VerdictDeny {
		t.Fatalf("写 id_rsa 应 deny, got %v", got)
	}
	// 删除命令一律 ask——不看 flag 里有没有字母 r。
	// （2026-10 事故：规则键的是"含 r 的 flag"，`git status --short` 误触发，
	// 而逐个 `git rm` 漏过，被拒后模型拆解命令绕过了拦截。）
	for _, cmd := range []string{
		"rm -rf build",                             // 递归删除
		"git rm -r --quiet lab",                    // git 递归删除
		"git rm --quiet lab/.gitignore lab/go.mod", // git 逐个删除（事故里漏过的那条）
		"git rm lab/go.mod",                        // git 删除单文件
		"unlink /tmp/x",                            // unlink
	} {
		if got := verdict("shell", fmt.Sprintf(`{"command":%q}`, cmd)); got != policy.VerdictAsk {
			t.Fatalf("%q 应 ask, got %v", cmd, got)
		}
	}
	// 非删除命令不该误触发
	for _, cmd := range []string{
		"git status --short", // --short 含字母 r，但不能因为字母就拦
		"go test ./...",
		"ls -la",
		"rmdir build", // 只能删空目录，豁免
	} {
		if got := verdict("shell", fmt.Sprintf(`{"command":%q}`, cmd)); got != policy.VerdictAllow {
			t.Fatalf("%q 应放行, got %v", cmd, got)
		}
	}
	if got := verdict("shell", `{"command":"go test ./..."}`); got != policy.VerdictAllow {
		t.Fatalf("普通命令应放行, got %v", got)
	}
	if got := verdict("read_file", `{"path":"/tmp/notes.md"}`); got != policy.VerdictAllow {
		t.Fatalf("普通文件应放行, got %v", got)
	}
	// 参数坏掉 → fail-closed
	if got := verdict("shell", `{坏 JSON}`); got != policy.VerdictDeny {
		t.Fatalf("参数坏掉应 deny, got %v", got)
	}
}

// 端到端：deny 档拦住工具，理由经 tool/result 回填给模型。
func TestHostPolicyDenyBlocksTool(t *testing.T) {
	reg := tool.NewRegistry()
	ft := &fakeTool{name: "read_file"}
	if err := reg.Register(ft); err != nil {
		t.Fatalf("注册: %v", err)
	}
	hp := hostPolicy()
	guards := hook.NewGuardBus()
	guards.Add(hp.Guard())
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, hp.Approval())

	sess := session.New("t", nil)
	p := &scriptPlanner{acts: []planner.Action{{
		ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "read_file", Args: `{"path":"/tmp/.env"}`,
		}},
	}, {Content: "好"}}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg, Guards: guards, Hooks: hooks,
	}, react.New())

	if _, err := d.Run(context.Background(), sess, "读一下 .env"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := sess.Events()
	var result string
	for _, ev := range evs {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
			result = ev.ToolResult.Content
		}
	}
	if !strings.Contains(result, "敏感文件") {
		t.Fatalf("拒绝理由应回填给模型: %s", result)
	}
	if ft.executed {
		t.Fatal("被拒的工具不该执行")
	}
}

// 端到端：ask 档在没有审批通道时 fail-closed（拒绝），理由同样回填给模型。
func TestHostPolicyAskFailsClosed(t *testing.T) {
	reg := tool.NewRegistry()
	ft := &fakeTool{name: "shell"}
	if err := reg.Register(ft); err != nil {
		t.Fatalf("注册: %v", err)
	}
	hp := hostPolicy()
	guards := hook.NewGuardBus()
	guards.Add(hp.Guard())
	hooks := hook.NewHookBus()
	hooks.On(hook.HookToolPreExecute, hp.Approval())

	sess := session.New("t", nil)
	p := &scriptPlanner{acts: []planner.Action{{
		ToolCalls: []llm.ToolCall{{
			ID: "c1", Name: "shell", Args: `{"command":"rm -rf build"}`,
		}},
	}, {Content: "好"}}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg, Guards: guards, Hooks: hooks,
	}, react.New())

	if _, err := d.Run(context.Background(), sess, "删掉 build"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var result string
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
			result = ev.ToolResult.Content
		}
	}
	if !strings.Contains(result, "人工确认") {
		t.Fatalf("ask 档理由应回填给模型: %s", result)
	}
	if ft.executed {
		t.Fatal("未审批的命令不该执行")
	}
}

// fakeTool 是一个假工具（记录是否被执行）。
type fakeTool struct {
	name     string
	executed bool
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "假工具" }
func (f *fakeTool) Parameters() any     { return map[string]any{} }
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	f.executed = true
	return map[string]any{"ok": true}, nil
}

// scriptPlanner 按预设序列返回动作。
type scriptPlanner struct {
	acts []planner.Action
	i    int
}

func (p *scriptPlanner) NextAction(context.Context, []llm.Message, []llm.Tool) (planner.Action, error) {
	if p.i >= len(p.acts) {
		return planner.Action{Content: "结束"}, nil
	}
	a := p.acts[p.i]
	p.i++
	return a, nil
}

// 启动横幅：没装名字级名单（cfg == nil）是**正常路径**，不能崩。
func TestPolicyBannerNilConfig(t *testing.T) {
	got := policyBanner(nil, "")
	if !strings.Contains(got, "未装配") {
		t.Fatalf("横幅 = %q", got)
	}
	got = policyBanner(&policy.Config{Allow: []string{"read_file"}, Deny: []string{"shell", "write_file"}}, "p.yaml")
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "shell, write_file") {
		t.Fatalf("横幅 = %q", got)
	}
	// Allow 非空 → 未列名的工具一律拒绝；Allow 未配置 → 默认放行
	if !strings.Contains(got, "未列名的工具一律拒绝") {
		t.Fatalf("横幅语义不对: %q", got)
	}
	got = policyBanner(&policy.Config{}, "")
	if !strings.Contains(got, "放行 不限制") || !strings.Contains(got, "未列名的工具默认放行") {
		t.Fatalf("空配置的横幅 = %q", got)
	}
	if strings.Contains(got, "（空）write_file") {
		t.Fatalf("占位符不该混进列表: %q", got)
	}
	if strings.Contains(got, "拒绝 无") && strings.Contains(got, "放行 [") {
		t.Fatalf("空列表不该显示成有值: %q", got)
	}
}

// 端到端：宿主规则的 ask 档 → 终端问人 → 批准才执行。
func TestHostPolicyAskReachesTerminalApproval(t *testing.T) {
	run := func(answer string) (executed bool, result string) {
		reg := tool.NewRegistry()
		ft := &fakeTool{name: "shell"}
		if err := reg.Register(ft); err != nil {
			t.Fatalf("注册: %v", err)
		}
		hp := hostPolicy()
		guards := hook.NewGuardBus()
		guards.Add(hp.Guard())
		hooks := hook.NewHookBus()
		hooks.On(hook.HookToolPreExecute, hp.Approval())

		var out bytes.Buffer
		ans := terminalAnswerer{
			in:  bufio.NewScanner(strings.NewReader(answer)),
			out: &out,
		}
		sess := session.New("t", nil)
		p := &scriptPlanner{acts: []planner.Action{{
			ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: "shell", Args: `{"command":"rm -rf build"}`,
			}},
		}, {Content: "好"}}}
		d := driver.New(&driver.RunContext{
			Planner: p, Tools: reg, Guards: guards, Hooks: hooks, Answerer: ans,
		}, react.New())

		if _, err := d.Run(context.Background(), sess, "删掉 build"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, ev := range sess.Events() {
			if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
				result = ev.ToolResult.Content
			}
		}
		return ft.executed, result
	}

	// 批准 → 执行
	executed, _ := run("y\n")
	if !executed {
		t.Fatal("批准后应执行")
	}
	// 拒绝 → 不执行，理由回填
	executed, result := run("n\n")
	if executed {
		t.Fatal("拒绝后不该执行")
	}
	if !strings.Contains(result, "保持拒绝状态") {
		t.Fatalf("拒绝理由应含制止绕过的语义: %s", result)
	}
}
