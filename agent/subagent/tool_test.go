package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos/tool"
)

// newCatalog 造一个只有一个子代理的清单（正文现读）。
func newCatalog(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 只读代码审查\ntools: [read]", "你是严格的代码审查者。")
	return Load(Options{Dirs: []string{root}})
}

// 工具名与描述：subagent__<name>，描述里带「自述要复核」的口径。
func TestToolNameAndDescription(t *testing.T) {
	cat := newCatalog(t)
	tools := cat.Tools(nil, ToolOptions{})
	if len(tools) != 1 {
		t.Fatalf("应造 1 个工具，实际 %d", len(tools))
	}
	if tools[0].Name() != "subagent__reviewer" {
		t.Fatalf("工具名不对：%q", tools[0].Name())
	}
	desc := tools[0].Description()
	if !strings.Contains(desc, "只读代码审查") {
		t.Fatalf("描述应含声明描述：%q", desc)
	}
	if !strings.Contains(desc, "复核") {
		t.Fatalf("描述应写明自述要复核：%q", desc)
	}
}

// 参数 schema：task 必填，context 可选。
func TestToolParameters(t *testing.T) {
	cat := newCatalog(t)
	tools := cat.Tools(nil, ToolOptions{})
	params, ok := tools[0].Parameters().(*TaskParams)
	if !ok {
		t.Fatalf("参数类型不对：%T", tools[0].Parameters())
	}
	_ = params // 零值示例（框架只反射类型）
	// 框架反射出的 schema 要把 task 标成必填。
	reg := tool.NewRegistry()
	if err := reg.Register(tools[0]); err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("注册表里应有 1 个工具：%d", len(list))
	}
	schema, ok := list[0].Parameters.(map[string]any)
	if !ok {
		t.Fatalf("schema 类型不对：%T", list[0].Parameters)
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "task" {
		t.Fatalf("task 应是必填：%v", schema)
	}
}

// 子代理 = 普通工具：不实现 ConcurrencySafe → 独占（默认保守）。
func TestToolIsExclusiveByDefault(t *testing.T) {
	cat := newCatalog(t)
	tools := cat.Tools(nil, ToolOptions{})
	if _, ok := tools[0].(tool.ConcurrencySafe); ok {
		t.Fatal("子代理工具不该声明可并发（内部要发模型请求）")
	}
}

// 正常路径：回执原样回填父上下文。
func TestToolExecuteReturnsReceipt(t *testing.T) {
	cat := newCatalog(t)
	var got Spec
	runner := RunnerFunc(func(_ context.Context, spec Spec) (Result, error) {
		got = spec
		return Result{
			Status:  StatusCompleted,
			Summary: "看完了",
			Findings: []Finding{{
				Ref:     "main.go:12",
				Summary: "这里有个空指针风险",
			}},
		}, nil
	})
	tools := cat.Tools(runner, ToolOptions{})
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"看看这段代码","context":"main.go"}`))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	res, ok := out.(Result)
	if !ok {
		t.Fatalf("回执类型不对：%T", out)
	}
	if res.Status != StatusCompleted || res.Summary != "看完了" || len(res.Findings) != 1 {
		t.Fatalf("回执不对：%+v", res)
	}
	if got.Task != "看看这段代码" || got.Context != "main.go" {
		t.Fatalf("参数没传到执行体：%+v", got)
	}
	if got.Instructions != "你是严格的代码审查者。" {
		t.Fatalf("指令正文没传到执行体：%+v", got)
	}
	if len(got.Tools) != 1 || got.Tools[0] != "read" {
		t.Fatalf("白名单没传到执行体：%+v", got)
	}
}

// 子代理失败 → status: error 的**正常结果**（父轮次不失败）。
func TestToolFailureIsNormalResult(t *testing.T) {
	cat := newCatalog(t)
	runner := RunnerFunc(func(context.Context, Spec) (Result, error) {
		return Result{}, errors.New("模型服务不可用")
	})
	tools := cat.Tools(runner, ToolOptions{})
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"干活"}`))
	if err != nil {
		t.Fatalf("子代理失败不该让工具报错: %v", err)
	}
	res := out.(Result)
	if res.Status != StatusError {
		t.Fatalf("状态该是 error：%+v", res)
	}
	if !strings.Contains(res.Error, "模型服务不可用") {
		t.Fatalf("回执要带失败原因：%+v", res)
	}
}

// 回执不合法（状态不在三档里）→ 投影成 status: error（fail-closed）。
func TestToolRejectsInvalidReceipt(t *testing.T) {
	cat := newCatalog(t)
	runner := RunnerFunc(func(context.Context, Spec) (Result, error) {
		return Result{Status: "done", Summary: "好了"}, nil
	})
	tools := cat.Tools(runner, ToolOptions{})
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"干活"}`))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	res := out.(Result)
	if res.Status != StatusError || !strings.Contains(res.Error, "回执不合法") {
		t.Fatalf("非法回执应被挡住：%+v", res)
	}
}

// 参数层错误 → Go error（这次调用压根没跑起来）。
func TestToolRejectsBadArgs(t *testing.T) {
	cat := newCatalog(t)
	tools := cat.Tools(RunnerFunc(func(context.Context, Spec) (Result, error) {
		return Result{Status: StatusCompleted, Summary: "不该被调用"}, nil
	}), ToolOptions{})
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺 task 应报错")
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":""}`)); err == nil {
		t.Fatal("空 task 应报错")
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{不是 json`)); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// 超时 → status: timeout 的正常结果（工具级超时，同 MCP callTimeout）。
func TestToolTimeout(t *testing.T) {
	cat := newCatalog(t)
	runner := RunnerFunc(func(ctx context.Context, _ Spec) (Result, error) {
		<-ctx.Done() // 尊重 ctx 的执行体
		return Result{}, ctx.Err()
	})
	tools := cat.Tools(runner, ToolOptions{Timeout: 30 * time.Millisecond})
	start := time.Now()
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"干活"}`))
	if err != nil {
		t.Fatalf("超时不报错: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("超时不该等太久：%s", d)
	}
	res := out.(Result)
	if res.Status != StatusTimeout {
		t.Fatalf("状态该是 timeout：%+v", res)
	}
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("回执要写明超时：%+v", res)
	}
}

// 执行体不理会 ctx 时，超时照样返回（工具不等它）。
func TestToolTimeoutEvenIfRunnerIgnoresContext(t *testing.T) {
	cat := newCatalog(t)
	runner := RunnerFunc(func(context.Context, Spec) (Result, error) {
		time.Sleep(300 * time.Millisecond)
		return Result{Status: StatusCompleted, Summary: "晚了"}, nil
	})
	tools := cat.Tools(runner, ToolOptions{Timeout: 20 * time.Millisecond})
	start := time.Now()
	out, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"干活"}`))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("超时不该等执行体：%s", d)
	}
	if res := out.(Result); res.Status != StatusTimeout {
		t.Fatalf("状态该是 timeout：%+v", res)
	}
}

// 父轮次取消 → 子代理跟着取消（ctx 透传）。
func TestParentCancelCancelsSubagent(t *testing.T) {
	cat := newCatalog(t)
	observed := make(chan error, 1)
	runner := RunnerFunc(func(ctx context.Context, _ Spec) (Result, error) {
		<-ctx.Done()
		observed <- ctx.Err()
		return Result{}, ctx.Err()
	})
	tools := cat.Tools(runner, ToolOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	out, err := tools[0].Execute(ctx, json.RawMessage(`{"task":"干活"}`))
	if err != nil {
		t.Fatalf("取消不该报错: %v", err)
	}
	res := out.(Result)
	if res.Status != StatusError || !strings.Contains(res.Error, "取消") {
		t.Fatalf("回执该说明被取消：%+v", res)
	}
	select {
	case e := <-observed:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("子代理该收到取消：%v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("子代理没收到取消")
	}
}

// 没有 Runner → 框架层错误（这次调用没跑起来）。
func TestToolWithoutRunner(t *testing.T) {
	cat := newCatalog(t)
	tools := cat.Tools(nil, ToolOptions{})
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{"task":"干活"}`)); err == nil {
		t.Fatal("没有 Runner 应报错")
	}
}
