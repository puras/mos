package scripted

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// order 记录假工具执行顺序（并发时经互斥保护）。
type orderTool struct{ name string }

func (o orderTool) Name() string        { return o.name }
func (o orderTool) Description() string { return o.name }
func (o orderTool) Parameters() any     { return struct{}{} }
func (o orderTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "result-" + o.name, nil
}

// failTool 是 fail-closed 测试用工具：Execute 返回普通 error（非 context 取消）
// 供 execpath 标记为 failed 而不触发 ctx 取消的特殊路径。
type failTool struct{ name string }

func (f *failTool) Name() string        { return f.name }
func (f *failTool) Description() string { return f.name }
func (f *failTool) Parameters() any     { return struct{}{} }
func (f *failTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, errBoom
}

// errBoom 是 failTool 的非上下文错误（避免 execpath 对 ctx.Err 特殊路径误判）。
var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// newTestRT 是 dag_test 的 RunContext 构造助手：会话 + 工具注册 + 默认钩子/守卫
// + 无遥测（nil）。默认钩子经 react.InstallDefaults 注册（含 execpath.ExecuteHook
// 链尾执行器），否则 execpath 瀑布链无执行终端，工具不会真正运行。
func newTestRT(t *testing.T, tools ...tool.Tool) *driver.RunContext {
	t.Helper()
	reg := tool.NewRegistry()
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	rt := &driver.RunContext{Session: session.NewSession("s"), Tools: reg}
	react.InstallDefaults(rt)
	rt.Telemetry = telemetry.NoopTelemetry
	return rt
}

// TestInterpolateWholeAndField 验证 Args 插值：整串 ${id} → 上游完整结果；
// ${id.field} → 单层字段；含文本片段 → 字符串替换。
func TestInterpolateWholeAndField(t *testing.T) {
	outputs := map[string]any{
		"a": map[string]any{"count": 3, "label": "x"},
		"b": "plain",
	}
	args := map[string]any{
		"whole": "${a}",
		"field": "${a.count}",
		"plain": "${b}",
		"mix":   "got ${b} here",
	}
	got, err := interpolateArgs(args, outputs)
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got["field"] != float64(3) {
		t.Fatalf("field: %#v", got["field"])
	}
	if got["whole"] == nil || !reflect.DeepEqual(got["whole"], outputs["a"]) {
		t.Fatalf("whole: %#v", got["whole"])
	}
	if got["plain"] != "plain" {
		t.Fatalf("plain: %#v", got["plain"])
	}
	if got["mix"] != "got plain here" {
		t.Fatalf("mix: %#v", got["mix"])
	}
}

// TestInterpolateUnresolved 验证未解析的引用 → 报错。
func TestInterpolateUnresolved(t *testing.T) {
	if _, err := interpolateArgs(map[string]any{"v": "${ghost}"}, map[string]any{}); err == nil {
		t.Fatal("expected error for unresolved ref")
	}
}

// TestExecuteWavesOrderAndSkip 验证分波调度：
//   - a 失败 → c 跳过（依赖 a）；b、d 照常；
//   - a、b 同波（无依赖）；c、d 下一波。
func TestExecuteWavesOrderAndSkip(t *testing.T) {
	a := step("a", "fail_tool")
	b := step("b", "ok_tool")
	c := step("c", "ok_tool")
	c.DependsOn = []string{"a"}
	d := step("d", "ok_tool")
	d.DependsOn = []string{"b"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a, b, c, d}}

	rt := newTestRT(t,
		orderTool{name: "ok_tool"},
		&failTool{name: "fail_tool"},
	)
	st := newState(plan)
	report, suspend, err := executeWaves(context.Background(), rt, rt.Session, 1, plan, st, nil)
	if err != nil {
		t.Fatalf("executeWaves: %v", err)
	}
	if suspend != nil {
		t.Fatalf("unexpected suspend: %v", suspend)
	}
	want := map[string]string{
		"a": "failed", "b": "done", "c": "skipped", "d": "done",
	}
	for id, w := range want {
		if report.Status[id] != w {
			t.Fatalf("step %s status = %q, want %q", id, report.Status[id], w)
		}
	}
	if report.Outputs["d"] != "result-ok_tool" {
		t.Fatalf("outputs[d] = %#v", report.Outputs["d"])
	}
}

// TestRestoreStateDeepCopyOutputs 钉住 Task 5 的有意设计选择（Task 3 复审遗留）：
// MemStore.Load 返回浅拷贝（cp := *ckpt），恢复路径若直接复用原 maps，将与存储的
// ScriptState 共享内存。resume 续跑时 mutate outputs/status 会改写 checkpoint，
// 跨进程 Resume 读取旧值时丢状态。restoreState 必须深拷贝 Outputs 与 Status。
//
// 走 FileStore（真实 JSON 路径）保证读路径与会话持载路径一致。
func TestRestoreStateDeepCopyOutputs(t *testing.T) {
	dir := t.TempDir()
	store, err := checkpoint.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	plan := &planner.ScriptedPlan{
		Steps: []planner.ScriptStep{{ID: "a", Tool: "noop"}},
	}
	ss := &planner.ScriptState{
		Plan:    *plan,
		Outputs: map[string]any{"a": "v1"},
		Status:  map[string]string{"a": "done"},
	}
	ckpt := &checkpoint.Checkpoint{SessionID: "s", ScriptState: ss}
	if err := store.Save(context.Background(), ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := store.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ScriptState == nil {
		t.Fatal("loaded ScriptState nil")
	}
	st := restoreState(plan, loaded.ScriptState)
	// 续跑期 mutate 状态
	st.Outputs["a"] = "v2"
	st.Status["a"] = "failed"

	reloaded, err := store.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.ScriptState.Outputs["a"]; got != "v1" {
		t.Fatalf("Outputs[a] mutated across loads: got %#v, want %q", got, "v1")
	}
	if got := reloaded.ScriptState.Status["a"]; got != "done" {
		t.Fatalf("Status[a] mutated across loads: got %q, want %q", got, "done")
	}
}

// TestRestoreStateDeepCopyNestedOutputsMemStore 钉住 restoreState 对 Outputs 中
// 嵌套容器（map[string]any）的深拷贝：MemStore.Load 是 cp := *ckpt 浅拷贝，路径上
// 复用 maps 会与存储的 ScriptState 共享内存，resume 续跑时 mutate 嵌套元素会
// 改写 checkpoint，跨进程 Resume 读到旧值。本测试走 MemStore + 嵌套 map（不走
// JSON 反序列化的 FileStore），并 mutate Outputs["a"].(map[string]any)["k"]，
// 然后重新 Load 断言存储值未被污染。去掉 restoreState 内的 deepCopyValue 调用
// 后本测试必 RED（顶标量替换路径 FileStore 测试无法区分）。
func TestRestoreStateDeepCopyNestedOutputsMemStore(t *testing.T) {
	store := checkpoint.NewMemStore()
	plan := &planner.ScriptedPlan{
		Steps: []planner.ScriptStep{{ID: "a", Tool: "noop"}},
	}
	originalNested := map[string]any{"k": "v1"}
	ss := &planner.ScriptState{
		Plan: *plan,
		Outputs: map[string]any{
			"a": originalNested,
		},
		Status: map[string]string{"a": "done"},
	}
	ckpt := &checkpoint.Checkpoint{SessionID: "s", ScriptState: ss}
	if err := store.Save(context.Background(), ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := store.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ScriptState == nil {
		t.Fatal("loaded ScriptState nil")
	}
	st := restoreState(plan, loaded.ScriptState)
	// 续跑期 mutate 嵌套容器：必须切掉与 checkpoint 共享的引用。
	nested, ok := st.Outputs["a"].(map[string]any)
	if !ok {
		t.Fatalf("Outputs[a] is not a map: %T", st.Outputs["a"])
	}
	nested["k"] = "mutated"

	// 重新 Load：存储的 Outputs["a"]["k"] 仍应为 "v1"（深拷贝守住内存隔离）。
	reloaded, err := store.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloadedNested, ok := reloaded.ScriptState.Outputs["a"].(map[string]any)
	if !ok {
		t.Fatalf("reloaded Outputs[a] is not a map: %T", reloaded.ScriptState.Outputs["a"])
	}
	if got := reloadedNested["k"]; got != "v1" {
		t.Fatalf("nested Outputs[a][k] mutated across loads: got %q, want %q", got, "v1")
	}
	// 哨兵：原 constructed map 也未被续跑侧污染（与 reloaded 不再是同一对象）。
	if originalNested["k"] != "v1" {
		t.Fatalf("original nested map leaked alias: got %q, want %q", originalNested["k"], "v1")
	}
}
