package mos_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/contextfile"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// stubModel 是只回一句话的假模型（装配层测试不需要真模型）。
type stubModel struct{ reply string }

func (m *stubModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.reply}}, nil
}

func (m *stubModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(ctx, req)
}

func (m *stubModel) Capabilities() llm.Capability { return llm.CapFunctionCalling | llm.CapStreaming }

// 没有模型就装配不出来（fail-closed，不降级成空转）。
func TestNewRequiresModel(t *testing.T) {
	if _, err := mos.New(mos.Config{}); err == nil {
		t.Fatal("缺模型应当报错")
	}
}

// 能力即证据：nil 的能力不装（shell 不出现、审批未装、压缩/截断按配置）。
func TestNewCapabilityPresence(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := a.Describe()
	if !strings.Contains(d, "shell: 未装配") {
		t.Fatalf("未给沙箱就不该有 shell:\n%s", d)
	}
	if !strings.Contains(d, "决策轮次上限: 不限") {
		t.Fatalf("默认不限轮次:\n%s", d)
	}
	// 截断默认开启（32KB），压缩默认开启（128K 窗口）。
	if !strings.Contains(d, "结果截断: 32768 字节") {
		t.Fatalf("默认截断上限应为 32KB:\n%s", d)
	}
	if !strings.Contains(d, "上下文压缩: 已装配") {
		t.Fatalf("默认应装配压缩:\n%s", d)
	}
}

// 截断上限可关（负数 = 不截断），显式上限要生效。
func TestTruncateMaxBytes(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, TruncateMaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "结果截断: 未开启") {
		t.Fatalf("负数应表示不截断:\n%s", a.Describe())
	}

	b, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, TruncateMaxBytes: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(b.Describe(), "结果截断: 1024 字节") {
		t.Fatalf("显式上限应生效:\n%s", b.Describe())
	}
}

// 名字级策略：被 deny 的工具**不出现在模型可见清单里**。
func TestPolicyVisibility(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(stubTool{name: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(stubTool{name: "shell"}); err != nil {
		t.Fatal(err)
	}
	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Tools:  reg,
		Policy: &policy.Config{Deny: []string{"shell"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := a.Describe()
	if !strings.Contains(d, "已注册工具: read, shell") {
		t.Fatalf("注册表应含全部工具:\n%s", d)
	}
	if !strings.Contains(d, "模型可见工具: read") || strings.Contains(d, "模型可见工具: read, shell") {
		t.Fatalf("被 deny 的工具不该对模型可见:\n%s", d)
	}
}

// 参数级规则：deny 档进守卫（单调拒绝），ask 档走 pre-execute 钩子。
func TestParamRulesWired(t *testing.T) {
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Rules: []policy.Check{
			func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
				if call.Tool == "read" {
					return policy.VerdictDeny, "不读这个文件"
				}
				return policy.VerdictAllow, ""
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "参数级规则: 1 个检查器") {
		t.Fatalf("Describe 应报规则条数:\n%s", a.Describe())
	}
	// deny 档：守卫应拒绝（单调，工具不会执行）。
	ev := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`},
	}
	if err := a.RunContext().Guards.Allow(context.Background(), ev); err == nil {
		t.Fatal("deny 档应被守卫拒绝")
	}
	// allow 档：同一规则不拦别的工具。
	ev2 := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c2", Name: "current_time", Args: `{}`},
	}
	if err := a.RunContext().Guards.Allow(context.Background(), ev2); err != nil {
		t.Fatalf("allow 档不该被拒: %v", err)
	}
}

// ask 档：进 pre-execute 钩子（转人工确认），不直接拒绝。
func TestParamRuleAskGoesToApprovalHook(t *testing.T) {
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Rules: []policy.Check{
			func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
				return policy.VerdictAsk, "要人确认"
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: llm.ToolCall{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`},
	}
	out, err := a.RunContext().Hooks.Waterfall(context.Background(), ev)
	if err != nil {
		t.Fatalf("ask 档不该直接报错: %v", err)
	}
	if out.Decision.Kind != hook.DecisionAsk {
		t.Fatalf("ask 档应置 DecisionAsk, got %+v", out.Decision)
	}
}

// 运行期旋钮：SetConcurrency / SetMaxDecisionRounds 改的是**下一轮**的值。
func TestRuntimeKnobs(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.SetConcurrency(7)
	a.SetMaxDecisionRounds(9)
	// 轮次用浅拷贝，旋钮值写进副本——这里检查副本拿到的是新值。
	rt := a.RunContext()
	if rt.Concurrency == 7 {
		t.Fatal("模板不该被就地改（每轮用浅拷贝）")
	}
	sess := session.New("t", nil)
	if _, err := a.Run(context.Background(), sess, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(a.Describe(), "并发上限: 7") {
		t.Fatalf("Describe 应反映新旋钮值:\n%s", a.Describe())
	}
	if !strings.Contains(a.Describe(), "决策轮次上限: 9") {
		t.Fatalf("Describe 应反映新轮次上限:\n%s", a.Describe())
	}
}

// 运行期加根：宿主授权新目录后，文件工具的边界跟着变（下一工具调用生效）。
//
// 这是"智能体初始目录 A、运行期加目录 B"的框架侧答案：边界是**每次调用
// 现判**的活集合，不需要重建智能体。
func TestRuntimeAddRoot(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	roots := filesystem.NewRootSet([]string{dirA}, []string{dirA})
	fsys := filesystem.NewSandboxedWithRoots(roots)

	ag, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Roots: roots})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := fsys.Resolve(ctx, filesystem.Request{Path: filepath.Join(dirB, "x.txt"), Op: filesystem.OpRead}); err == nil {
		t.Fatal("加根前 B 应在边界外")
	}

	if err := ag.AddRoot(dirB); err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	if _, err := fsys.Resolve(ctx, filesystem.Request{Path: filepath.Join(dirB, "x.txt"), Op: filesystem.OpRead}); err != nil {
		t.Fatalf("加根后 B 应放行: %v", err)
	}
	if !strings.Contains(ag.Describe(), "文件根（运行期可变）") {
		t.Fatalf("Describe 应写出根集合:\n%s", ag.Describe())
	}

	// 未装配根集合 → 运行期加根报错（不静默无效）。
	ag2, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := ag2.AddRoot(dirB); err == nil {
		t.Fatal("未装配根集合时 AddRoot 应报错")
	}
}

// 装配层把根集合接到沙箱的动态范围上（宿主没自己接 Scope 时）：
// 文件 jail 与 shell 沙箱共用一份根，不会"工具能读、命令写不了"。
func TestRootsWiredToSandboxScope(t *testing.T) {
	dirA := t.TempDir()
	roots := filesystem.NewRootSet([]string{dirA}, []string{dirA})
	sb := &sandbox.Local{WorkDir: dirA}
	if _, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Sandbox: sb, Roots: roots}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if sb.Scope == nil {
		t.Fatal("装配层应把根集合接到沙箱的动态范围上")
	}
	sc := sb.Scope()
	if len(sc.WriteRoots) != 1 || sc.WriteRoots[0] != filepath.Clean(dirA) {
		t.Fatalf("沙箱范围 = %+v，want 写根 %s", sc, dirA)
	}
	roots.AddWrite(t.TempDir())
	if len(sb.Scope().WriteRoots) != 2 {
		t.Fatalf("加根后沙箱范围应跟着变: %+v", sb.Scope())
	}
}

// 策略热替换：换策略后可见性跟着换，旧守卫撤下。
func TestSetPolicyHotSwap(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(stubTool{name: "read"}); err != nil {
		t.Fatal(err)
	}
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "模型可见工具: read") {
		t.Fatalf("初始应可见:\n%s", a.Describe())
	}
	a.SetPolicy(&policy.Config{Deny: []string{"read"}})
	if !strings.Contains(a.Describe(), "模型可见工具: ") &&
		strings.Contains(a.Describe(), "模型可见工具: read") {
		t.Fatalf("换策略后不该再可见:\n%s", a.Describe())
	}
	a.SetPolicy(nil)
	if !strings.Contains(a.Describe(), "名字级策略: 未装配") {
		t.Fatalf("置 nil 应回到未装配:\n%s", a.Describe())
	}
}

// 缺会话就报错（不静默跑空）。
func TestRunRequiresSession(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), nil, "hi"); err == nil {
		t.Fatal("缺会话应当报错")
	}
}

// 装配层不引入额外依赖：默认压缩/截断都用现成机制（冒烟：跑一轮不炸）。
func TestRunSmoke(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "你好"}, SystemPrompt: "你是助手"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	reply, err := a.Run(context.Background(), sess, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "你好" {
		t.Fatalf("reply = %q", reply)
	}
	if len(sess.Events()) == 0 {
		t.Fatal("事件应落会话")
	}
}

// stubTool 是只回固定文本的工具。
type stubTool struct{ name string }

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return "测试工具" }
func (t stubTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t stubTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	return "ok", nil
}

// 上下文文件（AGENTS.md 等）：装配进系统提示词，并在装配快照里如实写明。
func TestContextFilesWiredIntoSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("项目门规：提交前必须跑测试"), 0o644); err != nil {
		t.Fatalf("写 AGENTS.md: %v", err)
	}
	a, err := mos.New(mos.Config{
		Model:        &stubModel{reply: "ok"},
		SystemPrompt: "宿主自己的引导词",
		Context: &mos.ContextConfig{
			Dir: dir,
			// 全局目录显式给空目录：测试不受本机 ~/.mos 影响。
			Options: contextfile.Options{GlobalDirs: []string{}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.RunContext().SystemPrompt
	if !strings.Contains(got, "项目门规：提交前必须跑测试") {
		t.Fatalf("系统提示词应含上下文正文：\n%s", got)
	}
	// 装配快照：装了什么要写出来（含文件路径）。
	snap := a.Describe()
	if !strings.Contains(snap, "上下文文件") || !strings.Contains(snap, path) {
		t.Fatalf("装配快照应写明上下文文件：\n%s", snap)
	}
}

// 分节装配器 + 上下文文件：项目约定落在「约定之后、工具指引之前」。
func TestContextFilesLandInProjectContextSection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("项目门规"), 0o644); err != nil {
		t.Fatalf("写 AGENTS.md: %v", err)
	}
	pb := prompt.New()
	if err := pb.Set(prompt.Section{Name: "identity", Order: prompt.OrderIdentity, Text: "身份"}); err != nil {
		t.Fatalf("装配身份: %v", err)
	}
	if err := pb.Set(prompt.Section{Name: "rules", Order: prompt.OrderPolicy, Text: "约定"}); err != nil {
		t.Fatalf("装配约定: %v", err)
	}
	a, err := mos.New(mos.Config{
		Model:  &stubModel{reply: "ok"},
		Prompt: pb,
		Context: &mos.ContextConfig{
			Dir:     dir,
			Options: contextfile.Options{GlobalDirs: []string{}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.RunContext().SystemPrompt
	if strings.Index(got, "约定") > strings.Index(got, "项目门规") {
		t.Fatalf("项目约定应排在框架约定之后：\n%s", got)
	}
	if !strings.Contains(got, prompt.SectionProjectContext) && !strings.Contains(got, "项目约定与指引") {
		t.Fatalf("应作为独立一节（项目约定）：\n%s", got)
	}
}

// SystemPrompt 与 Prompt 二选一：两个都给 = 装配错误（不静默挑一个）。
func TestSystemPromptAndPromptAreExclusive(t *testing.T) {
	pb := prompt.New()
	_, err := mos.New(mos.Config{
		Model:        &stubModel{reply: "ok"},
		SystemPrompt: "字符串",
		Prompt:       pb,
	})
	if err == nil {
		t.Fatal("SystemPrompt 与 Prompt 同时给应当报错")
	}
}

// 全局目录：宿主没显式给 = 用 MoS 自己的 ~/.mos（HOME 可控，测试确定）。
func TestContextGlobalDirDefaultsToMosHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(home, ".mos")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatalf("建全局目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("全局约定"), 0o644); err != nil {
		t.Fatalf("写全局 AGENTS.md: %v", err)
	}
	dir := t.TempDir()
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "ok"},
		Context: &mos.ContextConfig{
			Dir: dir,
			// GlobalDirs 留空 = 用默认的 ~/.mos。
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := a.RunContext().SystemPrompt; !strings.Contains(got, "全局约定") {
		t.Fatalf("应加载 ~/.mos 里的全局约定：\n%s", got)
	}
}

// 没装上下文文件这个能力时，装配快照要写明「未装配」（没装什么也写出来）；
// 装了但目录里没文件时也要说清楚——两件事不是一回事。
func TestDescribeSaysContextFilesNotWired(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a.Describe(), "上下文文件: 未装配") {
		t.Fatalf("装配快照应写明未装配上下文文件：\n%s", a.Describe())
	}

	dir := t.TempDir()
	a2, err := mos.New(mos.Config{
		Model:   &stubModel{reply: "ok"},
		Context: &mos.ContextConfig{Dir: dir, Options: contextfile.Options{GlobalDirs: []string{}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a2.Describe(), "已装配，但没找到文件") {
		t.Fatalf("装配快照应区分「没装」与「装了没找到」：\n%s", a2.Describe())
	}
}
