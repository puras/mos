# 脚本化编排（Scripted Orchestration）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增 `agent/driver/scripted` 驱动：模型一次产出依赖 DAG 脚本 → 机器按拓扑分波有界并发执行（纯工具调用，0 次额外模型调用）→ 合成最终回复（全程 2 次模型调用）。

**Architecture:** 新 `planner.NewScript` 规划器产出 `ScriptedPlan`；新 `agent/driver/scripted` 驱动负责脚本校验（fail-closed）、分波调度（复用 `agent/executor.Run` 三阶段）、M2.8 审批断点（复用 `agent/approval` + `agent/checkpoint`）；从 `agent/driver/react` 提取 `agent/execpath` 共享执行通道，react 批量与脚本各波共用同一 GuardBus/钩子/日志/审计语义。opt-in 装配（RunContext.ScriptPlanner + router 规则 + config/seam），默认不影响现有能力。

**Tech Stack:** Go 1.26，module `hegq.net/mos`。复用：`agent/executor`、`agent/approval`、`agent/checkpoint`、`agent/hook`、`agent/session`、`agent/event`、`agent/telemetry`、`agent/driver/react`、`planner`、`tool`、`llm`。

## Global Constraints

- 设计文档：`docs/superpowers/specs/2026-08-17-scripted-orchestration-design.md`（实现前先读）。
- 每步都是直接工具调用，**不执行任意代码**（不引入 `python3 -c` / `bash -c` 等解释器入口）；Policy 对解释器入口的拦截保持不变。
- 脚本校验 fail-closed：任一校验不过 → 整份脚本拒绝执行，不做部分执行。
- 脚本驱动**不注册** DoomLoopGuard（脚本中故意重复同一工具调用是合法语义）。
- 默认装配 = `react.NewReAct` 的钩子/守卫，仅剔除 Doom Loop；脚本驱动的默认装配入口是共享的 `react.InstallDefaults`。
- 生命周日规则：一个 Turn 至多一次模型调用（脚本轮 + 合成轮 = 2 个 Turn）；turn/end 仅收尾时写。
- 会话事件日志是唯一事实来源：每次工具调用照常 `tool/call` + `tool/result` 入库，Seq 连续可重放。
- 命名：驱动名常量 `planner.DriverScripted = "scripted"`；工具调用 ID 前缀 `"step:<stepID>"`；`RunContext` 字段 `ScriptPlanner planner.ScriptPlanner`。
- 测试命令：`go test ./...`（全部回归）；单测：`go test ./agent/driver/scripted/...` 等。
- 提交粒度：每个 Task 一个 commit；只 add 本任务触及的文件，不误收他人未提交改动。

---

### Task 1: `planner/script.go` — 脚本数据模型 + `NewScript` 规划器

**Files:**
- Create: `planner/script.go`
- Test: `planner/script_test.go`

**Interfaces:**
- Consumes: `llm.ChatModel`（`Chat(ctx, *llm.Request) (*llm.Response, error)`）、`llm.Tool{Name, Description, Parameters}`、`llm.Message`
- Produces:
  - `type ScriptStep struct { ID string; Tool string; Args map[string]any; DependsOn []string; Output string }`（json tag：`id`/`tool`/`args,omitempty`/`depends_on,omitempty`/`output,omitempty`）
  - `type ScriptedPlan struct { Goal string; Steps []ScriptStep }`（json tag：`goal`/`steps`）
  - `type ScriptPlanner interface { Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error) }`
  - `func NewScript(model llm.ChatModel) ScriptPlanner`
  - `func parseScriptPlan(text string) (ScriptedPlan, error)`（导出？不导出；Task 5 用不到。测试经 NewScript 间接覆盖）
  - `type ScriptState struct { Plan ScriptedPlan; Outputs map[string]any; Status map[string]string }`（json tag：`plan`/`outputs,omitempty`/`status,omitempty`；Task 3 checkpoint 使用）

- [ ] **Step 1: Write the failing test**

`planner/script_test.go`：

```go
package planner

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/llm"
)

// fakeModel 返回固定响应（供 Script/ReAct 规划器测试）。
type fakeModel struct {
	replies []llm.Response // 按调用顺序弹出；耗尽后复用最后一个
	i       int
}

func (f *fakeModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	r := f.replies[len(f.replies)-1]
	if f.i < len(f.replies) {
		r = f.replies[f.i]
	}
	f.i++
	return &r, nil
}

func (f *fakeModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return f.Chat(context.Background(), nil)
}

func (f *fakeModel) Capabilities() llm.Capability { return 0 }

func scriptReply(json string) llm.Response {
	return llm.Response{Message: llm.Message{Content: json}}
}

func TestNewScriptParsesPlan(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(
		`{"goal":"改名","steps":[{"id":"a","tool":"find_files","args":{"dir":"."},"output":"files"}]}`)}}
	p := NewScript(m)
	plan, err := p.Script(context.Background(), "改名", nil, []llm.Tool{{Name: "find_files", Description: "找文件"}})
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if plan.Goal != "改名" || len(plan.Steps) != 1 || plan.Steps[0].Tool != "find_files" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestNewScriptStripsFence(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(
		"```json\n{\"goal\":\"g\",\"steps\":[{\"id\":\"a\",\"tool\":\"current_time\"}]}\n```")}}
	p := NewScript(m)
	plan, err := p.Script(context.Background(), "g", nil, []llm.Tool{{Name: "current_time"}})
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestNewScriptErrorsOnGarbage(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply("not json at all")}}
	p := NewScript(m)
	if _, err := p.Script(context.Background(), "g", nil, nil); err == nil {
		t.Fatal("expected error on non-JSON reply")
	}
}

func TestScriptPromptIncludesToolNames(t *testing.T) {
	m := &fakeModel{replies: []llm.Response{scriptReply(`{"steps":[{"id":"a","tool":"current_time"}]}`)}}
	p := NewScript(m)
	// 通过注入的 history 抓取本次请求的消息文本
	got := ""
	wrap := &captureModel{inner: m, onReq: func(req *llm.Request) {
		for _, msg := range req.Messages {
			got += msg.Content
		}
	}}
	_ = NewScript(wrap)
	p.Script(context.Background(), "g", nil, []llm.Tool{{Name: "current_time", Description: "时间"}})
	if !strings.Contains(got, "current_time") {
		t.Fatalf("prompt should list tool names, got: %s", got)
	}
}

type captureModel struct {
	inner *fakeModel
	onReq func(*llm.Request)
}

func (c *captureModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.onReq(req)
	return c.inner.Chat(ctx, req)
}
func (c *captureModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return c.inner.ChatStream(ctx, req, emit)
}
func (c *captureModel) Capabilities() llm.Capability { return 0 }
```

注意：`TestScriptPromptIncludesToolNames` 中 `NewScript` 只创建规划器，真正的调用在 `p.Script` 上（上面代码把 `p := NewScript(wrap)` 与 `p.Script(...)` 分开写，确保 wrap 生效）。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./planner/ -run 'TestNewScript' -v`
Expected: FAIL with `undefined: NewScript`（编译错误即失败）。

- [ ] **Step 3: Write minimal implementation**

`planner/script.go`：

```go
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/llm"
)

// ScriptStep 是脚本中的一步（直接工具调用）。
type ScriptStep struct {
	ID        string         `json:"id"`                   // 步骤唯一标识，供 depends_on / output 引用
	Tool      string         `json:"tool"`                 // 工具名（必须在 Registry 中）
	Args      map[string]any `json:"args,omitempty"`       // 参数，可用 ${id} / ${id.field} 引用上游输出
	DependsOn []string       `json:"depends_on,omitempty"` // 依赖的步骤 ID
	Output    string         `json:"output,omitempty"`     // 命名输出，供下游引用（可选）
}

// ScriptedPlan 是脚本阶段的产出：目标 + 带依赖的步骤图。
type ScriptedPlan struct {
	Goal  string       `json:"goal"`
	Steps []ScriptStep `json:"steps"`
}

// ScriptPlanner 生成执行脚本（Scripted 驱动）。
// tools 为当前注册的工具清单：注入名称与描述，防模型编造工具名。
type ScriptPlanner interface {
	Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error)
}

// ScriptState 是脚本执行状态的快照（M2.8 checkpoint 载荷）：
// 脚本 + 已完成步骤的命名输出 + 每步状态。resume 时恢复 outputs/status 续跑，不重跑已完成步骤。
type ScriptState struct {
	Plan    ScriptedPlan      `json:"plan"`
	Outputs map[string]any    `json:"outputs,omitempty"`
	Status  map[string]string `json:"status,omitempty"` // stepID → done/skipped/failed/denied/running
}

// NewScript 创建默认脚本规划器：提示模型输出 JSON 脚本。
func NewScript(model llm.ChatModel) ScriptPlanner {
	return &scriptPlanner{model: model}
}

type scriptPlanner struct {
	model llm.ChatModel
}

// Script 实现 ScriptPlanner。
func (p *scriptPlanner) Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error) {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: scriptPrompt(goal, tools)})

	resp, err := p.model.Chat(ctx, &llm.Request{Messages: msgs})
	if err != nil {
		return ScriptedPlan{}, fmt.Errorf("planner: script: %w", err)
	}
	plan, err := parseScriptPlan(resp.Message.Content)
	if err != nil {
		return ScriptedPlan{}, err
	}
	if plan.Goal == "" {
		plan.Goal = goal
	}
	return plan, nil
}

// scriptPrompt 是脚本阶段的系统提示词：只要求输出 JSON 脚本，工具名限清单内。
func scriptPrompt(goal string, tools []llm.Tool) string {
	var b strings.Builder
	b.WriteString("你是脚本编排器。请将下面的任务目标编排为一个可执行的工具调用脚本。\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 只输出 JSON，不要任何解释或围栏，格式为 {\"steps\":[{\"id\":\"a\",\"tool\":\"工具名\",\"args\":{...},\"depends_on\":[\"a\"],\"output\":\"名字\"}]}\n")
	b.WriteString("2. id 唯一（字母数字下划线），depends_on 只在有真实依赖时声明，output 命名供下游 ${id} 引用\n")
	b.WriteString("3. tool 必须从下面的工具清单中选择，args 用该工具的参数名\n")
	b.WriteString("4. 控制在 3-12 步，每步单意图\n\n")
	b.WriteString("工具清单：\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s：%s\n", t.Name, t.Description)
	}
	b.WriteString("\n目标：\n" + goal)
	return b.String()
}

// parseScriptPlan 从模型输出中解析脚本（失败即报错，不做猜测性解析）。
// 回退：剥 ```json 围栏后再解析；仍失败返回错误（含原始输出）。
func parseScriptPlan(text string) (ScriptedPlan, error) {
	for _, s := range []string{text, stripFence(text)} {
		var plan ScriptedPlan
		if err := json.Unmarshal([]byte(s), &plan); err == nil {
			return plan, nil
		}
	}
	return ScriptedPlan{}, fmt.Errorf("planner: script: 无法解析模型输出为脚本 JSON（原始输出：%q）", truncate(text, 200))
}

// truncate 截断字符串到上限（错误信息用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
```

复用 `planner/plan.go` 的 `stripFence`（同一包，直接可用）。

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./planner/ -run 'TestNewScript' -v`
Expected: PASS（4 个测试全过）。

- [ ] **Step 5: Commit**

```bash
git add planner/script.go planner/script_test.go
git commit -m "feat: 新增 ScriptedPlan/ScriptStep + NewScript 脚本规划器（工具清单注入防编造工具名）"
```

---

### Task 2: `agent/event/events.go` + `agent/observer.go` — script 事件

**Files:**
- Modify: `agent/event/events.go`（追加常量 + 载荷结构体）
- Modify: `agent/observer.go`（追加 `EventScript`）
- Test: `agent/event/events_test.go`（新增）

**Interfaces:**
- Consumes: 无（纯事件定义）
- Produces:
  - `SessionEventScriptStart = "script/start"`、`SessionEventScriptStep = "script/step"`、`SessionEventScriptEnd = "script/end"`
  - `type ScriptStartData struct { Turn int; Goal string; Steps int }`（json：`turn`/`goal`/`steps`）
  - `type ScriptStepData struct { Turn int; StepID string; Tool string; Status string; Detail string }`（json：`turn`/`step_id`/`tool`/`status`/`detail,omitempty`；Status 取值 `pending/running/done/failed/denied/skipped`）
  - `type ScriptEndData struct { Turn int; Status string; Detail string }`（json：`turn`/`status`/`detail,omitempty`；Status：`completed`/`failed`）
  - `agent.EventScript`（observer 观测事件，镜像 `EventPlan`：Content 为脚本摘要文本）

- [ ] **Step 1: Write the failing test**

`agent/event/events_test.go`：

```go
package event

import (
	"encoding/json"
	"testing"
)

func TestScriptEventDataMarshal(t *testing.T) {
	step := ScriptStepData{Turn: 3, StepID: "a", Tool: "find_files", Status: "done"}
	b, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ScriptStepData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.StepID != "a" || back.Status != "done" {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
	if SessionEventScriptStart != "script/start" || SessionEventScriptEnd != "script/end" {
		t.Fatalf("unexpected constants: %q %q", SessionEventScriptStart, SessionEventScriptEnd)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/event/ -run TestScriptEventDataMarshal -v`
Expected: FAIL with `undefined: ScriptStepData`。

- [ ] **Step 3: Add constants + payloads**

在 `agent/event/events.go` 的 plan 事件常量之后追加：

```go
	SessionEventScriptStart = "script/start"
	SessionEventScriptStep  = "script/step"
	SessionEventScriptEnd   = "script/end"
```

在 `PlanEndData` 之后追加（`ScriptStartData` / `ScriptStepData` / `ScriptEndData`）：

```go
// ScriptStartData 脚本阶段开始（Scripted 驱动）：目标 + 步骤数。
type ScriptStartData struct {
	Turn  int    `json:"turn"`
	Goal  string `json:"goal"`
	Steps int    `json:"steps"`
}

// ScriptStepData 脚本中一步的状态迁移（pending → running → done/failed/denied/skipped）。
type ScriptStepData struct {
	Turn    int    `json:"turn"`
	StepID  string `json:"step_id"`
	Tool    string `json:"tool"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// ScriptEndData 脚本阶段结束：completed 或 failed。
type ScriptEndData struct {
	Turn   int    `json:"turn"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
```

在 `agent/observer.go` 的 `EventReflection` 之后追加：

```go
	// EventScript 脚本产出（Scripted 驱动）：Content 为脚本摘要文本。
	EventScript
```

（`EventScript` 插入会令后续 `EventResume`/`EventDisposed` 的数值 +1；它们是内存态枚举，不跨版本序列化，安全。）

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/event/ -run TestScriptEventDataMarshal -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add agent/event/events.go agent/event/events_test.go agent/observer.go
git commit -m "feat: 新增 script/start·script/step·script/end 事件与 EventScript 观测"
```

---

### Task 3: `agent/checkpoint/checkpoint.go` — 增加可选 `ScriptState` 载荷

**Files:**
- Modify: `agent/checkpoint/checkpoint.go`
- Modify: `agent/checkpoint/memstore.go`（如有需要——预计无需改动，Store 接口不变）
- Test: `agent/checkpoint/checkpoint_test.go`（新增）

**Interfaces:**
- Consumes: `planner.ScriptState`（Task 1 产出）
- Produces: `checkpoint.Checkpoint` 新增可选字段 `ScriptState *planner.ScriptState`（json：`script_state,omitempty`），向后兼容（老 checkpoint 无此字段）

- [ ] **Step 1: Write the failing test**

`agent/checkpoint/checkpoint_test.go`：

```go
package checkpoint

import (
	"context"
	"testing"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/planner"
)

func TestCheckpointScriptStateRoundtrip(t *testing.T) {
	plan := planner.ScriptedPlan{
		Goal: "g",
		Steps: []planner.ScriptStep{
			{ID: "a", Tool: "find_files", Args: map[string]any{"dir": "."}, Output: "files"},
		},
	}
	ckpt := &Checkpoint{
		SessionID: "s1", Turn: 2, Step: 1, HookKind: hook.HookToolPreExecute,
		Calls:          []llm.ToolCall{{ID: "step:a", Name: "find_files", Args: "{}"}},
		Request:        approval.ApprovalRequest{ToolName: "find_files"},
		SuspendedCallID: "step:a",
		Decisions:      map[string]approval.ApprovalDecision{},
		Snapshot:       session.TaskState{Goal: "g"},
		ScriptState: &planner.ScriptState{
			Plan:    plan,
			Outputs: map[string]any{"a": "file1.txt"},
			Status:  map[string]string{"a": "running"},
		},
	}
	// 经 Store 往返（内存实现）验证 ScriptState 持久化。
	st := NewMemStore()
	if err := st.Save(context.Background(), ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ScriptState == nil {
		t.Fatal("ScriptState not persisted")
	}
	if got.ScriptState.Status["a"] != "running" || got.ScriptState.Outputs["a"] != "file1.txt" {
		t.Fatalf("ScriptState mismatch: %+v", got.ScriptState)
	}
	if len(got.ScriptState.Plan.Steps) != 1 {
		t.Fatalf("plan steps mismatch: %+v", got.ScriptState.Plan)
	}
}
```

（若 `memstore.go` 里的内存 Store 名称不同，以实际名为准；`llm` 需 import。）

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/checkpoint/ -run TestCheckpointScriptStateRoundtrip -v`
Expected: FAIL（`unknown field ScriptState` 编译错误）。

- [ ] **Step 3: Add the field**

`agent/checkpoint/checkpoint.go`：import `hegq.net/mos/planner`，在 `Snapshot` 字段后追加：

```go
	// ScriptState 脚本执行状态快照（Scripted 驱动专用，可选）：
	// 脚本 + 已完成步骤输出 + 每步状态。resume 恢复 outputs/status 续跑。
	// 老 checkpoint（react/planexecute 等）无此字段，照常可读、恢复语义不变。
	ScriptState *planner.ScriptState `json:"script_state,omitempty"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./agent/checkpoint/ -run TestCheckpointScriptStateRoundtrip -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add agent/checkpoint/checkpoint.go agent/checkpoint/checkpoint_test.go
git commit -m "feat: checkpoint 增加可选 ScriptState 载荷（脚本断点恢复，向后兼容）"
```

---

### Task 4: `agent/execpath` — 共享执行通道提取

**Files:**
- Create: `agent/execpath/execpath.go`
- Create: `agent/execpath/execpath_test.go`
- Modify: `agent/driver/react/react.go`（`execBatchTools`/`executeTool`/`toolOutcome`/`ToolDeniedError`/`ErrToolDenied` 迁出为薄封装；`NewReAct` 拆出 `InstallDefaults`）
- Modify: `agent/driver/react/react_test.go`（如引用已迁移符号，改用 `execpath.*` 或 react 别名——见下）

**Interfaces:**
- Consumes: `driver.RunContext`、`session.Session`、`executor`、`approval`、`hook`、`telemetry`、`tool`、`llm`、`agent`（Auditor/ResultTruncator 仅经接口方法调用）
- Produces:
  - `type ToolOutcome struct { Result any; Text string; Err error }`
  - `var ErrToolDenied = errors.New("agent: tool denied")`
  - `type ToolDeniedError struct{ Reason string }`（`Error() string` 返回 `"工具调用被安全策略拒绝: " + Reason`）
  - `func ExecuteBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall) ([]executor.Outcome[*llm.ToolCall, ToolOutcome], *approval.SuspendError)`（即原 `execBatchTools` 的导出版）
  - `func ApplyOutcomes(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, calls []llm.ToolCall, outs []executor.Outcome[*llm.ToolCall, ToolOutcome]) error`（原 `finalizeToolBatch` 的日志回填部分；denied→`AppendToolResult` 带 deniedErr；正常→`AppendToolResult`；非守卫 preflight 错误返回 error）
  - `func ExecuteHook(rt *driver.RunContext) hook.Hook`、`func TruncateHook(rt *driver.RunContext) hook.Hook`、`func AuditHook(rt *driver.RunContext) hook.Hook`（原 react 的三个默认工具钩子，导出迁移）
  - react 保留：`compressHook`/`ragInjectHook`/`policyGuard`/DoomLoop 守卫；新增导出 `func InstallDefaults(rt *driver.RunContext)`（压缩+RAG+execpath 三钩子+policyGuard）；`NewReAct` 改为 `InstallDefaults` + DoomLoop 守卫
  - react 保留别名：`var ErrToolDenied = execpath.ErrToolDenied`、`type ToolDeniedError = execpath.ToolDeniedError`（兼容既有引用）

- [ ] **Step 1: Write the failing test**

`agent/execpath/execpath_test.go`（直接用驱动级假工具验证通道语义；若 `react` 包已有等价批量测试可复用为基准，本测试独立覆盖通道）：

```go
package execpath

import (
	"context"
	"errors"
	"testing"

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
func (t stubTool) Execute(_ context.Context, _ []byte) (any, error) {
	return "ok:" + t.name, nil
}

func TestExecuteBatchBasic(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(stubTool{name: "t1"})
	sess := session.NewSession("s")
	rt := &driver.RunContext{Session: sess, Tools: reg}
	rt.Hooks = hook.NewHookBus()
	rt.Guards = hook.NewGuardBus()
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/execpath/ -v`
Expected: FAIL with `undefined: ExecuteBatch`（包不存在 → 编译错误）。

- [ ] **Step 3: Create `agent/execpath/execpath.go`**

从 `agent/driver/react/react.go` **原样迁移**以下代码（保留 import）：`toolOutcome`（改名 `ToolOutcome`）、`ErrToolDenied`、`ToolDeniedError`、`execBatchTools`（导出为 `ExecuteBatch`）、`executeTool`、`executeHook`（导出 `ExecuteHook`）、`truncateHook`（导出 `TruncateHook`）、`auditHook`（导出 `AuditHook`）。并新增 `ApplyOutcomes`（从 `finalizeToolBatch` 提取日志回填段）：

```go
// Package execpath 提供单波/单批工具调用的统一安全执行通道（架构 v2 §3.9）。
// 语义中立于驱动：react 批量与 scripted 各波共用，保证 GuardBus 决策 /
// 审批挂起 / tool/call 日志 / 钩子链 / 截断 / 审计语义全库唯一。
package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/executor"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
)

// ErrToolDenied 表示工具调用被单调守卫拒绝（非致命：结果反馈给模型继续）。
var ErrToolDenied = errors.New("agent: tool denied")

// ToolDeniedError 是守卫拒绝的携带原因错误。
type ToolDeniedError struct{ Reason string }

func (e *ToolDeniedError) Error() string { return "工具调用被安全策略拒绝: " + e.Reason }

// ToolOutcome 是单条工具调用的执行结果（result 为 execute 链原始返回值，
// text 为 post-execute 截断后的模型可见文本，err 为运行期错误）。
type ToolOutcome struct {
	Result any
	Text   string
	Err    error
}

// ExecuteBatch 批量执行工具调用（原 react.execBatchTools 导出版）：
//   - preflight（串行）：单调守卫决策（先于 tool/call 日志——挂起时不落日志），
//     拒绝 = 拦截集合；挂起（*approval.SuspendError）= 整批中止（返回第二返回值）；
//   - execute（有界并发）：pre-execute → execute → post-execute 钩子链；任一工具
//     声明 SequentialTool 时整批回退串行；
//   - 返回按源序的 Outcome；调用方负责 ApplyOutcomes 落日志（finalize）。
func ExecuteBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall) ([]executor.Outcome[*llm.ToolCall, ToolOutcome], *approval.SuspendError) {
	// —— 原 execBatchTools 函数体原样迁移 ——
	// （含 items 构造、batchSuspend 闭包、executor.Run 三阶段调用）
}

// ApplyOutcomes 将一批 Outcome 落日志（tool/result + 观测），denied 用 ToolDeniedError 显式失败回填。
// 返回 error：preflight 阶段的非守卫错误（如 tool/call 落日志失败）→ 终止任务。
// 挂起（*SuspendError）与致命（*hook.DoomLoopError）由调用方在调用本函数前检测处理。
func ApplyOutcomes(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, calls []llm.ToolCall, outs []executor.Outcome[*llm.ToolCall, ToolOutcome]) error {
	for _, out := range outs {
		call := out.Input
		if out.Denied {
			var deniedErr *ToolDeniedError
			if errors.As(out.Err, &deniedErr) {
				_ = sess.AppendToolResult(turn, call, nil, "", deniedErr)
				rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call})
				continue
			}
			return fmt.Errorf("agent: tool preflight: %w", out.Err)
		}
		_ = sess.AppendToolResult(turn, call, out.Output.Result, out.Output.Text, out.Output.Err)
		rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call, ToolResult: out.Output.Result})
	}
	return nil
}

// ExecuteHook / TruncateHook / AuditHook：原 react 三个默认工具钩子导出迁移
// （executeHook / truncateHook / auditHook 函数体原样迁移）。
```

**关键约束**：`ExecuteBatch` 与三个钩子为**行为保持搬移**（不要改逻辑）。迁移后 `react.go` 删除相应定义，改引用 `execpath.*`。

- [ ] **Step 4: Refactor `agent/driver/react/react.go`**

1. 删除 `toolOutcome`/`ErrToolDenied`/`ToolDeniedError`/`execBatchTools`/`executeTool`/`executeHook`/`truncateHook`/`auditHook` 的定义；import 改用 `execpath`。
2. 保留兼容别名（文件底部或顶部）：

```go
// 兼容别名：ToolDeniedError / ErrToolDenied 迁至 execpath（既有引用保持可用）。
type ToolDeniedError = execpath.ToolDeniedError
var ErrToolDenied = execpath.ErrToolDenied
```

3. `execBatchTools` 调用点改为直接调用 `execpath.ExecuteBatch`（搜索 `execBatchTools(ctx, rt, sess, turn, step, calls)`，全部替换为 `execpath.ExecuteBatch(ctx, rt, sess, turn, step, calls)`；删除 `execBatchTools` 包装函数本身）。
4. `finalizeToolBatch` 的日志回填循环替换为对 `execpath.ApplyOutcomes` 的调用，**但保留其前置逻辑**（Doom Loop 致命检查）与挂起检测（`out.Output.Err` 为 `*approval.SuspendError` → saveCheckpoint 冒泡）。调整后 `finalizeToolBatch` 结构：

```go
func (d *ReActDriver) finalizeToolBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall, action *planner.Action, outs []executor.Outcome[*llm.ToolCall, execpath.ToolOutcome]) (string, bool, error) {
	// Doom Loop 致命守卫错误：任务中断（状态 blocked），不回填工具结果
	for _, out := range outs {
		var doomErr *hook.DoomLoopError
		if errors.As(out.Err, &doomErr) {
			sess.State.Status = session.StatusBlocked
			_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
			return "", false, doomErr
		}
	}
	// 工具执行路径抛出的挂起（request_human_input 等工具内发起审批）：写 checkpoint 并冒泡
	for _, out := range outs {
		if !out.Denied && out.Output.Err != nil {
			var suspend *approval.SuspendError
			if errors.As(out.Output.Err, &suspend) {
				if err := d.saveCheckpoint(ctx, rt, sess, turn, step, calls, suspend); err != nil {
					return "", false, fmt.Errorf("agent: save checkpoint: %w", err)
				}
				return "", false, suspend
			}
		}
	}
	// finalize：按模型调用顺序回填（tool/result 日志 + 观测）
	if err := execpath.ApplyOutcomes(ctx, rt, sess, turn, calls, outs); err != nil {
		return "", false, err
	}
	_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
	// ...ShouldStopAfterTurn 与收尾逻辑保持原样
}
```

> 注意：原 `finalizeToolBatch` 对 `Denied` 的守卫拒绝是 `AppendToolResult(nil, "", deniedErr)`，`ApplyOutcomes` 已实现同款语义，故去掉原循环。
>
> **顺序调整说明**：原代码先按序处理每条 out（含 deny/suspend/正常），再统一 step/end。新版把 suspend 检测提到 `ApplyOutcomes` 之前，行为等价（suspend 时本就不落结果日志），Doom Loop 检查保持在最前。

5. `NewReAct` 拆出 `InstallDefaults`：

```go
// InstallDefaults 注册默认钩子/守卫（压缩 + RAG + execpath 工具三钩子 + Policy 守卫）。
// 供 react.NewReAct 与 scripted.New 等驱动复用（脚本驱动不注册 Doom Loop，见 scripted）。
func InstallDefaults(rt *driver.RunContext) {
	if rt.Hooks == nil {
		rt.Hooks = hook.NewHookBus()
	}
	if rt.Guards == nil {
		rt.Guards = hook.NewGuardBus()
	}
	if rt.Auditor == nil {
		rt.Auditor = agent.NewDefaultAuditor()
	}
	if rt.Truncator == nil {
		rt.Truncator = agent.NewDefaultTruncator(agent.TruncatorConfig{})
	}
	if rt.Compactor == nil {
		rt.Compactor = agent.NewDefaultCompactor(agent.CompactorConfig{})
	}
	rt.Hooks.On(hook.HookPreStep, compressHook(rt))
	rt.Hooks.On(hook.HookPreStep, ragInjectHook(rt))
	rt.Hooks.On(hook.HookToolExecute, execpath.ExecuteHook(rt))
	rt.Hooks.On(hook.HookToolPostExecute, execpath.TruncateHook(rt))
	rt.Hooks.On(hook.HookToolPostExecute, execpath.AuditHook(rt))
	if rt.Policy != nil {
		rt.Guards.Add(policyGuard(rt))
	}
}

// NewReAct 返回默认装配的 ReAct 驱动（InstallDefaults + Doom Loop 守卫）。
func NewReAct(rt *driver.RunContext) driver.LoopDriver {
	InstallDefaults(rt)
	if rt.DoomLoopMaxRepeat > 0 {
		rt.Guards.Add(hook.NewDoomLoopGuard(rt.DoomLoopMaxRepeat).Guard)
	}
	return &ReActDriver{}
}
```

- [ ] **Step 5: Fix react tests referencing moved symbols**

Run: `go test ./agent/driver/react/...`
Expected: 若有引用 `toolOutcome`/`execBatchTools`/`ErrToolDenied` 的测试编译失败 → 改引 `execpath.*` 或保持 react 别名（别名已保留，通常无需改动）。确保全绿。

- [ ] **Step 6: Run full suite**

Run: `go test ./...`
Expected: PASS（execpath 行为保持 + react 批量回归）。

- [ ] **Step 7: Commit**

```bash
git add agent/execpath/ agent/driver/react/react.go
git commit -m "refactor: 提取 agent/execpath 共享执行通道（ExecuteBatch/ApplyOutcomes/默认工具钩子），react 批量与 scripted 共用"
```

---

### Task 5: `agent/driver/scripted` — 脚本校验 + 分波调度 + 驱动

**Files:**
- Create: `agent/driver/scripted/validate.go`（校验 + 环检测）
- Create: `agent/driver/scripted/dag.go`（插值 + 分波调度 + 状态）
- Create: `agent/driver/scripted/scripted.go`（ScriptedDriver / New / Options / Run）
- Create: `agent/driver/scripted/validate_test.go`
- Create: `agent/driver/scripted/dag_test.go`
- Create: `agent/driver/scripted/scripted_test.go`
- Modify: `agent/driver/driver.go`（RunContext 加 `ScriptPlanner` 字段）

**Interfaces:**
- Consumes: `planner.ScriptPlanner`/`planner.ScriptedPlan`/`planner.ScriptState`、`execpath.ExecuteBatch`/`ApplyOutcomes`、`executor`、`approval`/`checkpoint`、`react.InstallDefaults`、`event`/`hook`/`session`/`telemetry`/`tool`/`llm`
- Produces:
  - `type ScriptedDriver struct { planner planner.ScriptPlanner; maxSteps int }`
  - `func New(rt *driver.RunContext, opts ...Option) *driver.Agent`
  - `type Option func(*ScriptedDriver)`；`WithPlanner(planner.ScriptPlanner) Option`、`WithMaxSteps(int) Option`
  - `func (d *ScriptedDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error)`（LoopDriver）
  - 包内：`validateScript(plan *planner.ScriptedPlan, reg *tool.Registry, maxSteps int) error`、`interpolateArgs(...)`、`executeWaves(...)`（测试可达）

- [ ] **Step 1: 先加 `RunContext.ScriptPlanner` 字段**

`agent/driver/driver.go` 在 `Reflector` 字段后追加：

```go
	// ScriptPlanner 是 Scripted 驱动的脚本生成器（架构 v2 §3.3，PTC 模式）。
	// ReAct/PlanExecute/Reflection 驱动不使用，nil 时 scripted 驱动拒绝运行（对齐 reflection）。
	ScriptPlanner planner.ScriptPlanner
```

- [ ] **Step 2: Write the failing validation tests**

`agent/driver/scripted/validate_test.go`：

```go
package scripted

import (
	"context"
	"testing"

	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

type stubTool struct{ name, desc string }

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return t.desc }
func (t stubTool) Parameters() any {
	if t.name == "noop" {
		return struct{}{}
	}
	return struct {
		Path string `json:"path"`
	}{}
}
func (t stubTool) Execute(_ context.Context, _ []byte) (any, error) { return "ok", nil }

func reg(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	for _, n := range names {
		if err := r.Register(stubTool{name: n}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	return r
}

func step(id, toolName string) planner.ScriptStep {
	return planner.ScriptStep{ID: id, Tool: toolName}
}

func TestValidateRejectsUnknownTool(t *testing.T) {
	r := reg(t, "noop")
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{step("a", "nope")}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	r := reg(t, "noop")
	a, b := step("a", "noop"), step("b", "noop")
	a.DependsOn = []string{"b"}
	b.DependsOn = []string{"a"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a, b}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for cycle")
	}
}

func TestValidateRejectsUnknownDependency(t *testing.T) {
	r := reg(t, "noop")
	a := step("a", "noop")
	a.DependsOn = []string{"ghost"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

func TestValidateRejectsDupIDAndSelfDep(t *testing.T) {
	r := reg(t, "noop")
	a := step("a", "noop")
	a.DependsOn = []string{"a"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for self-dependency")
	}
	plan = &planner.ScriptedPlan{Steps: []planner.ScriptStep{step("a", "noop"), step("a", "noop")}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}

func TestValidateRejectsMaxSteps(t *testing.T) {
	r := reg(t, "noop")
	steps := make([]planner.ScriptStep, 3)
	for i := range steps {
		steps[i] = step(string(rune('a'+i)), "noop")
	}
	plan := &planner.ScriptedPlan{Steps: steps}
	if err := validateScript(plan, r, 2); err == nil {
		t.Fatal("expected error exceeding maxSteps")
	}
}

func TestValidateRejectsUnknownArgName(t *testing.T) {
	r := reg(t, "noop")
	s := step("a", "noop")
	s.Args = map[string]any{"nope_field": 1}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{s}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown arg name")
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	r := reg(t, "noop")
	a, b := step("a", "noop"), step("b", "noop")
	b.DependsOn = []string{"a"}
	b.Args = map[string]any{}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a, b}}
	if err := validateScript(plan, r, 16); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}
```

> 注：`noop` 工具 `Parameters()` 返回 `struct{}{}`，schema 无 `properties`，校验时跳过参数名校验（宽松）；`nope_field` 用例需一个带 `properties` 的工具——把 `noop` 改为 `Parameters()` 返回带字段的结构体更直接。**调整为**：`stubTool.Parameters()` 统一返回 `struct { Path string `json:"path"` }{}`，`TestValidateRejectsUnknownArgName` 用 `Args: map[string]any{"nope_field": 1}`，`TestValidateAcceptsValid` 的 `b.Args` 用 `map[string]any{"path": "/x"}` 或空（空对象键集 ⊆ properties 通过）。

- [ ] **Step 3: Run validation tests to verify they fail**

Run: `go test ./agent/driver/scripted/ -run 'TestValidate' -v`
Expected: FAIL（`undefined: validateScript`）。

- [ ] **Step 4: Implement `agent/driver/scripted/validate.go`**

```go
package scripted

import (
	"errors"
	"fmt"

	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// validateScript 校验脚本（确定性安全闸，fail-closed）：
// 任一校验不过 → 整份脚本拒绝执行。检查：步数上限 / ID 唯一非空 /
// 工具存在 / Args 参数名合法 / DependsOn 已知且无自依赖 / 无环 / Output 命名唯一。
func validateScript(plan *planner.ScriptedPlan, reg *tool.Registry, maxSteps int) error {
	if plan == nil {
		return errors.New("script: nil plan")
	}
	if len(plan.Steps) == 0 {
		return errors.New("script: no steps")
	}
	if maxSteps > 0 && len(plan.Steps) > maxSteps {
		return fmt.Errorf("script: %d steps exceeds max %d", len(plan.Steps), maxSteps)
	}

	// 工具 → schema（参数名校验用）
	schemas := make(map[string]map[string]any, len(plan.Steps))
	for _, t := range reg.List() {
		if m, ok := t.Parameters.(map[string]any); ok {
			schemas[t.Name] = m
		}
	}

	ids := make(map[string]bool, len(plan.Steps))
	outputs := make(map[string]bool)
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.ID == "" {
			return fmt.Errorf("script: step %d: empty id", i)
		}
		if ids[st.ID] {
			return fmt.Errorf("script: duplicate step id %q", st.ID)
		}
		ids[st.ID] = true
		if _, ok := reg.Get(st.Tool); !ok {
			return fmt.Errorf("script: step %q: unknown tool %q", st.ID, st.Tool)
		}
		if st.Output != "" {
			if outputs[st.Output] {
				return fmt.Errorf("script: duplicate output name %q", st.Output)
			}
			outputs[st.Output] = true
		}
		if err := validateArgNames(st, schemas[st.Tool]); err != nil {
			return err
		}
	}

	// 第二遍：DependsOn 已知 + 无自依赖
	for i := range plan.Steps {
		st := &plan.Steps[i]
		seen := make(map[string]bool)
		for _, dep := range st.DependsOn {
			if dep == st.ID {
				return fmt.Errorf("script: step %q self-dependency", st.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("script: step %q depends on unknown step %q", st.ID, dep)
			}
			if seen[dep] {
				return fmt.Errorf("script: step %q duplicate dependency %q", st.ID, dep)
			}
			seen[dep] = true
		}
	}

	// 环检测（Kahn 拓扑排序）
	if hasCycle(plan) {
		return errors.New("script: dependency cycle detected")
	}
	return nil
}

// validateArgNames 校验 Args 键均为工具 schema 声明的属性名。
// schema 无 properties（宽松工具）时跳过；插值引用出现在值中，不在此校验范围。
func validateArgNames(st *planner.ScriptStep, schema map[string]any) error {
	if len(st.Args) == 0 {
		return nil
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil // 宽松 schema，执行期校验
	}
	for k := range st.Args {
		if _, ok := props[k]; !ok {
			return fmt.Errorf("script: step %q: unknown arg %q for tool %q", st.ID, k, st.Tool)
		}
	}
	return nil
}

// hasCycle 用 Kahn 算法检测依赖环（dep 边：step 依赖 dep，即 dep → step）。
func hasCycle(plan *planner.ScriptedPlan) bool {
	indeg := make(map[string]int, len(plan.Steps))
	adj := make(map[string][]string, len(plan.Steps)) // dep → 依赖它的 steps
	for i := range plan.Steps {
		st := &plan.Steps[i]
		indeg[st.ID] = len(st.DependsOn)
		for _, dep := range st.DependsOn {
			adj[dep] = append(adj[dep], st.ID)
		}
	}
	queue := make([]string, 0)
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	count := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		count++
		for _, n := range adj[id] {
			indeg[n]--
			if indeg[n] == 0 {
				queue = append(queue, n)
			}
		}
	}
	return count != len(plan.Steps)
}
```

- [ ] **Step 5: Run validation tests to verify they pass**

Run: `go test ./agent/driver/scripted/ -run 'TestValidate' -v`
Expected: PASS。

- [ ] **Step 6: Write the failing DAG/scheduler tests**

`agent/driver/scripted/dag_test.go`：

```go
package scripted

import (
	"context"
	"reflect"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// order 记录假工具执行顺序（并发时经互斥保护）。
type orderTool struct{ name string }

func (o orderTool) Name() string        { return o.name }
func (o orderTool) Description() string { return o.name }
func (o orderTool) Parameters() any     { return struct{}{} }
func (o orderTool) Execute(_ context.Context, _ []byte) (any, error) {
	return "result-" + o.name, nil
}

func newTestRT(t *testing.T, tools ...tool.Tool) *driver.RunContext {
	t.Helper()
	reg := tool.NewRegistry()
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	rt := &driver.RunContext{Session: session.NewSession("s"), Tools: reg}
	rt.Hooks = hook.NewHookBus()
	rt.Guards = hook.NewGuardBus()
	return rt
}

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

func TestInterpolateUnresolved(t *testing.T) {
	if _, err := interpolateArgs(map[string]any{"v": "${ghost}"}, map[string]any{}); err == nil {
		t.Fatal("expected error for unresolved ref")
	}
}

func TestExecuteWavesOrderAndSkip(t *testing.T) {
	// a、b 无依赖（同波），c 依赖 a（下一波），d 依赖 b（下一波）。
	// 让 a 失败 → c 被跳过，但 b、d 照常。
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

type failTool struct{ name string }

func (f *failTool) Name() string        { return f.name }
func (f *failTool) Description() string { return f.name }
func (f *failTool) Parameters() any     { return struct{}{} }
func (f *failTool) Execute(_ context.Context, _ []byte) (any, error) {
	return nil, context.Canceled
}
```

> `failTool` 的 `Execute` 返回非 nil error，`execpath` 执行链会把它放进 `ToolOutcome.Err`，调度器据此标记 failed 并传播 skip。`context.Canceled` 只是任意非 nil 错误；如 execpath 对 ctx.Err() 有特殊处理（未执行直接回填 ctx.Err 且 `Denied=false`），用普通 `errors.New("boom")` 更稳妥——**改用** `errors.New("boom")`。

- [ ] **Step 7: Run DAG tests to verify they fail**

Run: `go test ./agent/driver/scripted/ -run 'TestInterpolate|TestExecuteWaves' -v`
Expected: FAIL（`undefined: interpolateArgs` / `executeWaves` / `newState`）。

- [ ] **Step 8: Implement `agent/driver/scripted/dag.go`**

```go
package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/execpath"
	"hegq.net/mos/agent/executor"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
)

// 步骤状态。
const (
	statusPending = "pending"
	statusRunning = "running"
	statusDone    = "done"
	statusSkipped = "skipped"
	statusFailed  = "failed"
	statusDenied  = "denied"
)

// scriptState 是脚本执行期状态（含可恢复快照所需的字段）。
type scriptState struct {
	status      map[string]string // stepID → status
	outputs     map[string]any    // stepID → 命名结果
	resumeCalls []llm.ToolCall    // 挂起波的工具调用（resume 重放）
}

func newState(plan *planner.ScriptedPlan) *scriptState {
	st := &scriptState{
		status:  make(map[string]string, len(plan.Steps)),
		outputs: make(map[string]any),
	}
	for i := range plan.Steps {
		st.status[plan.Steps[i].ID] = statusPending
	}
	return st
}

// scriptReport 是脚本执行报告（script/end 载荷 + 合成轮可见摘要）。
type scriptReport struct {
	Status  map[string]string
	Outputs map[string]any
}

var stepRefRe = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+)(?:\.([A-Za-z0-9_]+))?\}`)

// interpolateArgs 递归插值 args：${id} → 上游完整结果；${id.field} → 单层字段。
// 整串为单个引用的值返回结构化结果；含文本片段时做字符串替换。
func interpolateArgs(args map[string]any, outputs map[string]any) (map[string]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		rv, err := interpolateValue(v, outputs)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

func interpolateValue(v any, outputs map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		return interpolateString(t, outputs)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			rv, err := interpolateValue(vv, outputs)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			rv, err := interpolateValue(vv, outputs)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}

func interpolateString(s string, outputs map[string]any) (any, error) {
	if !stepRefRe.MatchString(s) {
		return s, nil
	}
	// 整串恰好是一个引用 → 返回结构化值
	if m := stepRefRe.FindStringSubmatch(s); m != nil && m[0] == s {
		id, field := m[1], m[2]
		val, ok := outputs[id]
		if !ok {
			return nil, fmt.Errorf("script: unresolved step %q", id)
		}
		if field != "" {
			return lookupField(val, field)
		}
		return val, nil
	}
	// 含文本片段 → 字符串替换
	var b strings.Builder
	last := 0
	for _, idx := range stepRefRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:idx[0]])
		id := s[idx[2]:idx[3]]
		field := ""
		if idx[4] >= 0 {
			field = s[idx[4]:idx[5]]
		}
		val, ok := outputs[id]
		if !ok {
			return nil, fmt.Errorf("script: unresolved step %q", id)
		}
		if field != "" {
			fv, err := lookupField(val, field)
			if err != nil {
				return nil, err
			}
			b.WriteString(renderRef(fv))
		} else {
			b.WriteString(renderRef(val))
		}
		last = idx[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

func lookupField(val any, field string) (any, error) {
	m, ok := val.(map[string]any)
	if !ok {
		b, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("script: ${...}.%s: result not an object", field)
		}
		var m2 map[string]any
		if err := json.Unmarshal(b, &m2); err != nil {
			return nil, fmt.Errorf("script: ${...}.%s: result not an object", field)
		}
		m = m2
	}
	v, ok := m[field]
	if !ok {
		return nil, fmt.Errorf("script: ${...}.%s: field not found", field)
	}
	return v, nil
}

func renderRef(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// resolveCall 将一步实例化为 llm.ToolCall（插值 args，ID 前缀 "step:"）。
func resolveCall(step *planner.ScriptStep, outputs map[string]any) (llm.ToolCall, error) {
	args, err := interpolateArgs(step.Args, outputs)
	if err != nil {
		return llm.ToolCall{}, err
	}
	b, err := json.Marshal(args)
	if err != nil {
		return llm.ToolCall{}, fmt.Errorf("script: step %q marshal args: %w", step.ID, err)
	}
	return llm.ToolCall{ID: "step:" + step.ID, Name: step.Tool, Args: string(b)}, nil
}

// executeWaves 分波调度脚本（拓扑层 → 有界并发，复用 execpath 通道）：
//   - resumeCalls 非空时先重放挂起波（M2.8 resume），再继续调度；
//   - 每波 = 依赖已满足的 pending 步骤；插值失败 → 该步 failed + 下游跳过；
//   - 步骤 failed/denied → 传递依赖全部 skipped（旁支照常）；
//   - 波内触发审批挂起（SuspendError）→ 记录 resumeCalls 后返回挂起，由调用方存档。
func executeWaves(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, plan *planner.ScriptedPlan, st *scriptState, resumeCalls []llm.ToolCall) (*scriptReport, *approval.SuspendError, error) {
	if len(resumeCalls) > 0 {
		outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, 1, resumeCalls)
		if suspend != nil {
			st.resumeCalls = resumeCalls
			return nil, suspend, nil
		}
		applyOutcomes(ctx, rt, plan, st, resumeCalls, outs, turn, sess)
		st.resumeCalls = nil
	}

	for wave := 0; wave <= len(plan.Steps); wave++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ready := readySteps(plan, st)
		if len(ready) == 0 {
			break
		}
		calls := make([]llm.ToolCall, 0, len(ready))
		callToStep := make(map[string]string, len(ready))
		for i := range ready {
			call, err := resolveCall(&ready[i], st.outputs)
			if err != nil {
				st.status[ready[i].ID] = statusFailed
				markSkipped(plan, st, ready[i].ID)
				continue
			}
			callToStep[call.ID] = ready[i].ID
			calls = append(calls, call)
		}
		if len(calls) == 0 {
			continue // 该波全部插值失败，进入下一层
		}
		for _, c := range calls {
			id := callToStep[c.ID]
			st.status[id] = statusRunning
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{Turn: turn, StepID: id, Tool: c.Name, Status: statusRunning})
		}
		outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, 1, calls)
		if suspend != nil {
			st.resumeCalls = calls
			return nil, suspend, nil
		}
		applyOutcomes(ctx, rt, plan, st, calls, outs, turn, sess)
	}

	// 剩余 pending（传递依赖失败，不可达）→ skipped
	for i := range plan.Steps {
		if st.status[plan.Steps[i].ID] == statusPending {
			st.status[plan.Steps[i].ID] = statusSkipped
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{Turn: turn, StepID: plan.Steps[i].ID, Tool: plan.Steps[i].Tool, Status: statusSkipped})
		}
	}
	return &scriptReport{Status: st.status, Outputs: st.outputs}, nil, nil
}

// readySteps 返回依赖已全部 done 的 pending 步骤。
func readySteps(plan *planner.ScriptedPlan, st *scriptState) []planner.ScriptStep {
	var out []planner.ScriptStep
	for _, s := range plan.Steps {
		if st.status[s.ID] != statusPending {
			continue
		}
		ok := true
		for _, dep := range s.DependsOn {
			if st.status[dep] != statusDone {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// applyOutcomes 回填每步状态与命名输出，并落 tool/result 日志（经 execpath.ApplyOutcomes）。
// 挂起（SuspendError）不在本函数处理（由 executeWaves 先返回）；DoomLoop 致命错误
// 视为 failed（脚本驱动不注册 Doom Loop，仅防御性处理）。
func applyOutcomes(ctx context.Context, rt *driver.RunContext, plan *planner.ScriptedPlan, st *scriptState, calls []llm.ToolCall, outs []executor.Outcome[*llm.ToolCall, execpath.ToolOutcome], turn int, sess *session.Session) {
	for i := range outs {
		out := outs[i]
		id := strings.TrimPrefix(out.Input.ID, "step:")
		switch {
		case out.Denied:
			st.status[id] = statusDenied
			markSkipped(plan, st, id)
		case out.Output.Err != nil:
			st.status[id] = statusFailed
			markSkipped(plan, st, id)
		default:
			st.status[id] = statusDone
			st.outputs[id] = out.Output.Result
		}
		_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{
			Turn: turn, StepID: id, Tool: out.Input.Name, Status: st.status[id],
			Detail: detailOf(out),
		})
	}
	_ = execpath.ApplyOutcomes(ctx, rt, sess, turn, calls, outs)
}

func detailOf(out executor.Outcome[*llm.ToolCall, execpath.ToolOutcome]) string {
	if out.Denied {
		return "denied"
	}
	if out.Output.Err != nil {
		return out.Output.Err.Error()
	}
	return ""
}

// markSkipped 将 stepID 及其传递依赖中仍 pending 的步骤置 skipped。
func markSkipped(plan *planner.ScriptedPlan, st *scriptState, stepID string) {
	if st.status[stepID] != statusPending {
		return
	}
	st.status[stepID] = statusSkipped
	for i := range plan.Steps {
		dep := &plan.Steps[i]
		for _, d := range dep.DependsOn {
			if d == stepID {
				markSkipped(plan, st, dep.ID)
			}
		}
	}
}
```

- [ ] **Step 9: Run DAG tests to verify they pass**

Run: `go test ./agent/driver/scripted/ -run 'TestInterpolate|TestExecuteWaves' -v`
Expected: PASS。

- [ ] **Step 10: Write the failing driver + integration tests**

`agent/driver/scripted/scripted_test.go`：

```go
package scripted

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// fakePlanner 是固定脚本的 ScriptPlanner（不依赖模型）。
type fakePlanner struct{ plan planner.ScriptedPlan }

func (f *fakePlanner) Script(context.Context, string, []llm.Message, []llm.Tool) (planner.ScriptedPlan, error) {
	return f.plan, nil
}

// synthModel 是合成轮用的假模型：固定返回一段文本。
type synthModel struct{ reply string }

func (m *synthModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Content: m.reply}}, nil
}
func (m *synthModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *synthModel) Capabilities() llm.Capability { return 0 }

func TestScriptedDriverEndToEnd(t *testing.T) {
	reg := tool.NewRegistry()
	for _, tl := range []tool.Tool{
		orderTool{name: "t1"}, orderTool{name: "t2"},
	} {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:    reg,
		Planner:  planner.NewReAct(&synthModel{reply: "完成，共处理了 2 步。"}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal: "g",
			Steps: []planner.ScriptStep{
				{ID: "a", Tool: "t1"},
				{ID: "b", Tool: "t2", DependsOn: []string{"a"}},
			},
		}},
	}
	ag := New(rt)
	reply, err := ag.Run(context.Background(), sess, "g")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "完成") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	// 日志完整性：两个工具调用都落了 tool/call + tool/result，且 script/end 收尾
	logs := eventTypes(sess)
	for _, want := range []string{"script/start", "script/end", "tool/call", "tool/result", "turn/end"} {
		if !containsStr(logs, want) {
			t.Fatalf("log missing %q: %v", want, logs)
		}
	}
}

func TestScriptedDriverRejectsNoPlanner(t *testing.T) {
	rt := &driver.RunContext{Tools: tool.NewRegistry()}
	ag := New(rt)
	_, err := ag.Run(context.Background(), newSession("s"), "g")
	if err == nil {
		t.Fatal("expected error when ScriptPlanner missing")
	}
}

func TestScriptedDriverSynthesisToolCallsFailClosed(t *testing.T) {
	// 合成轮模型返回工具调用 → fail-closed 错误（不执行）
	reg := tool.NewRegistry()
	_ = reg.Register(orderTool{name: "t1"})
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:   reg,
		Planner: planner.NewReAct(&toolCallModel{}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal:  "g",
			Steps: []planner.ScriptStep{{ID: "a", Tool: "t1"}},
		}},
	}
	ag := New(rt)
	if _, err := ag.Run(context.Background(), sess, "g"); err == nil {
		t.Fatal("expected error when synthesis returns tool calls")
	}
}

// 辅助：记录日志事件类型 / 判断包含。
func eventTypes(sess *session.Session) []string {
	out := make([]string, 0, len(sess.Events))
	for _, ev := range sess.Events {
		out = append(out, ev.Type)
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func newSession(id string) *session.Session { return session.NewSession(id) }

// toolCallModel 是合成轮返回工具调用的假模型（触发 fail-closed）。
type toolCallModel struct{}

func (m *toolCallModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{
		Content:   "",
		ToolCalls: []llm.ToolCall{{ID: "t1", Name: "t1", Args: "{}"}},
	}}, nil
}
func (m *toolCallModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *toolCallModel) Capabilities() llm.Capability { return 0 }
```

> `newSession` 用固定 ID（"s"）；`toolCallModel` 的 `Chat` 固定返回带 ToolCalls 的响应（合成轮模型经 `planner.NewReAct(&toolCallModel{})` 包装，`toAction` 会把它转成 `Action.ToolCalls`）。

- [ ] **Step 11: Run driver tests to verify they fail**

Run: `go test ./agent/driver/scripted/ -run 'TestScriptedDriver' -v`
Expected: FAIL（`undefined: New`）。

- [ ] **Step 12: Implement `agent/driver/scripted/scripted.go`**

```go
// Package scripted 提供脚本化编排驱动（架构 v2 §3.3 PTC 模式）：
// 写脚本（依赖 DAG）→ 分波有界并发执行（纯工具调用，0 次额外模型调用）→ 合成最终回复。
// 全程 2 次模型调用；工具执行经 agent/execpath 共享通道（与 react 批量语义一致）。
package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// DefaultMaxSteps 是脚本步数上限（防脚本爆炸 + 防提示词膨胀）。
const DefaultMaxSteps = 16

// ScriptedDriver 是脚本化编排循环驱动。
type ScriptedDriver struct {
	planner  planner.ScriptPlanner
	maxSteps int
}

// Option 是 ScriptedDriver 的装配选项。
type Option func(*ScriptedDriver)

// WithPlanner 注入脚本规划器（默认取 rt.ScriptPlanner；均未设置时拒绝运行）。
func WithPlanner(p planner.ScriptPlanner) Option {
	return func(d *ScriptedDriver) { d.planner = p }
}

// WithMaxSteps 覆盖脚本步数上限（默认 DefaultMaxSteps）。
func WithMaxSteps(n int) Option {
	return func(d *ScriptedDriver) { d.maxSteps = n }
}

// New 返回默认装配的脚本化编排智能体：
// 默认钩子/守卫与 react.New 一致（复用 react.InstallDefaults），但不注册 Doom Loop 守卫
// （脚本中故意重复同一工具调用是合法语义）；规划器取 WithPlanner 或 rt.ScriptPlanner。
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
	d := &ScriptedDriver{maxSteps: DefaultMaxSteps}
	for _, o := range opts {
		o(d)
	}
	react.InstallDefaults(rt)
	return driver.New(rt, d)
}

// Run 实现 LoopDriver：脚本轮（写脚本 + 分波执行）→ 合成轮（模型产出最终回复）。
func (d *ScriptedDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
	if rt == nil || rt.Session == nil {
		return "", errors.New("agent: run requires session")
	}
	if rt.Tools == nil {
		return "", errors.New("agent: no tool registry configured")
	}
	if rt.Planner == nil {
		return "", errors.New("agent: scripted driver requires a Planner")
	}
	if d.planner == nil && rt.ScriptPlanner == nil {
		return "", errors.New("agent: scripted driver requires a ScriptPlanner")
	}
	// 裸实例惰性默认装配（Router 等场景直接构造 ScriptedDriver 时补齐）。
	if rt.Hooks == nil || rt.Guards == nil {
		react.InstallDefaults(rt)
	}
	sess := rt.Session
	ctx = tool.WithSandbox(ctx, rt.Sandbox)

	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != session.StatusRunning {
		sess.State.Status = session.StatusRunning
	}

	firstTurn := sess.NextTurn()
	var plan planner.ScriptedPlan
	var st *scriptState
	var resumeCalls []llm.ToolCall

	if ckpt := rt.ResumeCheckpoint; ckpt != nil {
		// resume：脚本 + 状态从 checkpoint 恢复，跳过计划与事件重放。
		firstTurn = ckpt.Turn
		_ = sess.Append(event.SessionEventCheckpointRestore, event.CheckpointData{
			Turn: ckpt.Turn, Step: 1, HookKind: string(ckpt.HookKind), DecisionOK: ckpt.ResolvedAt != nil,
		})
		rt.Notify(agent.Event{Type: agent.EventResume, SessionID: sess.ID, Turn: ckpt.Turn, Content: "resume scripted from checkpoint"})
		if ckpt.ScriptState == nil {
			return "", errors.New("agent: scripted resume requires ScriptState in checkpoint")
		}
		plan = ckpt.ScriptState.Plan
		st = restoreState(&plan, ckpt.ScriptState)
		resumeCalls = ckpt.Calls
	} else {
		if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: firstTurn, Reason: "user"}); err != nil {
			return "", fmt.Errorf("agent: append turn/start: %w", err)
		}
		if err := sess.AppendUserMessage(input, "user"); err != nil {
			return "", fmt.Errorf("agent: append user/message: %w", err)
		}

		// 脚本阶段
		var err error
		plan, err = d.makeScript(ctx, rt, sess, input)
		if err != nil {
			return "", err
		}
		if err := validateScript(&plan, rt.Tools, d.maxSteps); err != nil {
			return "", fmt.Errorf("agent: script rejected: %w", err)
		}
		st = newState(&plan)
		_ = sess.Append(event.SessionEventScriptStart, event.ScriptStartData{Turn: firstTurn, Goal: plan.Goal, Steps: len(plan.Steps)})
		rt.Notify(agent.Event{Type: agent.EventScript, SessionID: sess.ID, Turn: firstTurn, Content: scriptText(&plan)})
		_ = sess.AppendAssistantMessage(firstTurn, scriptText(&plan), "", nil, llm.Usage{})
		for i := range plan.Steps {
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{
				Turn: firstTurn, StepID: plan.Steps[i].ID, Tool: plan.Steps[i].Tool, Status: statusPending,
			})
		}
	}

	// 执行阶段（分波）
	var report *scriptReport
	var suspend *approval.SuspendError
	var execErr error
	_ = telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanDriverTurn,
		map[string]string{"turn": strconv.Itoa(firstTurn), "driver": "scripted"},
		func(ctx context.Context) error {
			report, suspend, execErr = executeWaves(ctx, rt, sess, firstTurn, &plan, st, resumeCalls)
			return nil
		})
	if suspend != nil {
		if err := d.saveCheckpoint(ctx, rt, sess, firstTurn, &plan, st, suspend); err != nil {
			return "", fmt.Errorf("agent: save checkpoint: %w", err)
		}
		return "", suspend
	}
	if execErr != nil {
		return "", execErr
	}

	_ = sess.Append(event.SessionEventScriptEnd, event.ScriptEndData{Turn: firstTurn, Status: "completed"})
	_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: firstTurn, Step: 1})
	_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: firstTurn, Reason: "done"})

	// 合成轮：模型看到全部工具结果，产出最终回复。
	return d.synthesize(ctx, rt, sess)
}

// makeScript 生成脚本：WithPlanner → rt.ScriptPlanner → 拒绝运行。
func (d *ScriptedDriver) makeScript(ctx context.Context, rt *driver.RunContext, sess *session.Session, input string) (planner.ScriptedPlan, error) {
	p := d.planner
	if p == nil {
		p = rt.ScriptPlanner
	}
	plan, err := p.Script(ctx, input, session.ConvertToLlm(sess.Messages()), rt.Tools.List())
	if err != nil {
		return planner.ScriptedPlan{}, fmt.Errorf("agent: script: %w", err)
	}
	if plan.Goal == "" {
		plan.Goal = input
	}
	return plan, nil
}

// synthesize 合成轮：一次 NextAction（不注入工具，防模型继续调用）；
// 模型仍返回工具调用时 fail-closed（显式失败回填 + 报错），不执行、不进多脚本循环。
func (d *ScriptedDriver) synthesize(ctx context.Context, rt *driver.RunContext, sess *session.Session) (string, error) {
	synTurn := sess.NextTurn()
	_ = sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: synTurn, Reason: "continuation"})

	msgs := make([]session.InternalMessage, 0, 2)
	if sysText, err := rt.AssembleSystem(ctx); err != nil {
		return "", fmt.Errorf("agent: assemble system: %w", err)
	} else if sysText != "" {
		msgs = append(msgs, session.SystemMsg(sysText))
	}
	msgs = append(msgs, sess.Messages()...)

	hev := &hook.HookEvent{Kind: hook.HookPreStep, Messages: msgs, Turn: synTurn, Step: 1}
	hev, err := rt.Hooks.Waterfall(ctx, hev)
	if err != nil {
		return "", fmt.Errorf("agent: pre-step hooks: %w", err)
	}

	action, err := d.nextAction(ctx, rt, sess, synTurn, hev.Messages)
	if err != nil {
		return "", fmt.Errorf("agent: synthesize turn %d: %w", synTurn, err)
	}
	calls := action.ToolCalls
	if err := sess.AppendAssistantMessage(synTurn, action.Content, action.ReasoningContent, calls, action.Usage); err != nil {
		return "", fmt.Errorf("agent: append assistant/message: %w", err)
	}
	rt.MonitorUsage(synTurn, action.Usage)
	if action.Content != "" {
		rt.Notify(agent.Event{Type: agent.EventStep, SessionID: sess.ID, Turn: synTurn, Content: action.Content})
	}

	if len(calls) > 0 {
		// fail-closed：合成轮不允许工具调用（v1 语义，见设计 §6）
		err := errors.New("agent: scripted synthesis must answer directly (no tool calls)")
		for i := range calls {
			_ = sess.AppendToolResult(synTurn, &calls[i], nil, "", err)
		}
		sess.State.Status = session.StatusBlocked
		_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "synthesis_tool_calls"})
		return "", err
	}
	if action.Content == "" {
		sess.State.Status = session.StatusBlocked
		_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "empty"})
		return "", errors.New("agent: scripted synthesis produced no reply")
	}

	sess.State.Status = session.StatusCompleted
	_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "done"})
	rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: synTurn, Content: action.Content})
	return action.Content, nil
}

// nextAction 合成轮规划（非流式；流式路径下转发增量，复用 react 语义）。
func (d *ScriptedDriver) nextAction(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, msgs []session.InternalMessage) (planner.Action, error) {
	llmMsgs := session.ConvertToLlm(msgs)
	if !rt.Stream {
		return rt.Planner.NextAction(ctx, llmMsgs, nil)
	}
	sp, ok := rt.Planner.(planner.StreamingPlanner)
	if !ok {
		return rt.Planner.NextAction(ctx, llmMsgs, nil)
	}
	return sp.NextActionStream(ctx, llmMsgs, nil, func(chunk llm.StreamChunk) error {
		if chunk.Delta == "" && chunk.Reasoning == "" {
			return nil
		}
		_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		rt.Notify(agent.Event{Type: agent.EventStream, SessionID: sess.ID, Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		return nil
	})
}

// restoreState 从 checkpoint 恢复脚本执行状态（resume）。
func restoreState(plan *planner.ScriptedPlan, ss *planner.ScriptState) *scriptState {
	st := newState(plan)
	for id, s := range ss.Status {
		st.status[id] = s
	}
	for id, v := range ss.Outputs {
		st.outputs[id] = v
	}
	return st
}

// saveCheckpoint 落库挂起点（M2.8）：脚本状态 + 挂起波工具调用 + 审批请求 + TaskState 快照。
func (d *ScriptedDriver) saveCheckpoint(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, plan *planner.ScriptedPlan, st *scriptState, suspend *approval.SuspendError) error {
	_ = sess.Append(event.SessionEventApprovalRequest, event.ApprovalRequestData{
		ID: string(suspend.ID), ToolName: suspend.Request.ToolName,
		Command: suspend.Request.Command, Reason: suspend.Request.Reason, Args: suspend.Request.Args,
		HookKind: string(suspend.HookKind),
	})
	if rt.Checkpoints != nil {
		ckpt := &checkpoint.Checkpoint{
			SessionID: sess.ID, Turn: turn, Step: 1,
			HookKind:        suspend.HookKind,
			Calls:           st.resumeCalls,
			Request:         suspend.Request,
			ApprovalID:      suspend.ID,
			SuspendedCallID: suspend.CallID,
			Decisions:       make(map[string]approval.ApprovalDecision),
			Snapshot:        sess.State,
			ScriptState: &planner.ScriptState{
				Plan:    *plan,
				Outputs: st.outputs,
				Status:  st.status,
			},
			CreatedAt: time.Now(),
		}
		if err := rt.Checkpoints.Save(ctx, ckpt); err != nil {
			return fmt.Errorf("agent: save checkpoint: %w", err)
		}
		_ = sess.Append(event.SessionEventCheckpointSave, event.CheckpointData{
			Turn: turn, Step: 1, HookKind: string(suspend.HookKind),
		})
	}
	if p := sess.Persistence; p != nil {
		if err := p.SaveState(sess.ID, sess.State); err != nil {
			return fmt.Errorf("agent: save state: %w", err)
		}
	}
	return nil
}

// scriptText 将脚本渲染为观测文本（EventScript.Content / assistant message）。
func scriptText(plan *planner.ScriptedPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "脚本（%d 步）：\n", len(plan.Steps))
	for i := range plan.Steps {
		st := &plan.Steps[i]
		deps := strings.Join(st.DependsOn, ",")
		line := fmt.Sprintf("%s. [%s] %s", st.ID, st.Tool, formatArgs(st.Args))
		if deps != "" {
			line += "（依赖: " + deps + "）"
		}
		if st.Output != "" {
			line += " → " + st.Output
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(args)
	return string(b)
}
```

> 需要 import `encoding/json`。`applyOutcomes` 已在 dag.go 中引用 `rt`，执行时以 `execpath.ApplyOutcomes` 内部统一处理日志；若 `executeWaves` 调用签名有出入，以编译为准微调。

- [ ] **Step 13: Run driver + full package tests to verify they pass**

Run: `go test ./agent/driver/scripted/... -v`
Expected: PASS。

- [ ] **Step 14: Run full suite**

Run: `go test ./...`
Expected: PASS（execpath 重构回归 + scripted 全部测试）。

- [ ] **Step 15: Commit**

```bash
git add agent/driver/driver.go agent/driver/scripted/
git commit -m "feat: 新增 agent/driver/scripted 脚本化编排驱动（脚本校验 + 分波调度 + 合成回复 + M2.8 断点）"
```

---

### Task 6: 装配 — router / assistant / config / classify

**Files:**
- Modify: `planner/classify.go`（`DriverScripted` 常量）
- Modify: `agent/router/router.go`（注册 scripted 驱动工厂）
- Modify: `assistant/assistant.go`（`Config.ScriptPlanner` + `buildRunContext` + `assembleDriver` 分支）
- Modify: `agent/config/config.go`（`Config.ScriptPlanner` + `resolveScriptPlanner`）
- Modify: `agent/config/config_test.go`（或新增装配测试）
- Test: `agent/router/router_test.go`（新增规则用例）

**Interfaces:**
- Consumes: `scripted.ScriptedDriver`（`scripted.New`）、`planner.ScriptPlanner`
- Produces: `planner.DriverScripted = "scripted"`；router 注册 `planner.DriverScripted` 工厂（**不预置默认规则**——保持默认路由行为不变）；`assistant.Config.ScriptPlanner planner.ScriptPlanner`；`config` YAML 键 `script_planner`

- [ ] **Step 1: Write the failing tests**

`agent/router/router_test.go` 追加：

```go
func TestHybridRouterScriptedRule(t *testing.T) {
	// scripted 工厂已在 NewHybrid 内置注册；显式加规则后，关键词命中返回 ScriptedDriver。
	r := NewHybrid(
		WithRule("批量", planner.DriverScripted),
	)
	d, err := r.Select(context.Background(), "批量重命名文件", session.NewSession("s"))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, ok := d.(*scripted.ScriptedDriver); !ok {
		t.Fatalf("expected *scripted.ScriptedDriver, got %T", d)
	}
}
```

（`context` / `session` 需 import。）

- [ ] **Step 2: Run router test to verify it fails**

Run: `go test ./agent/router/ -run TestHybridRouterScriptedRule -v`
Expected: FAIL（`undefined: planner.DriverScripted`）。

- [ ] **Step 3: Add `DriverScripted` constant**

`planner/classify.go`：

```go
	DriverScripted = "scripted"
```

（仅加常量；**不加**到 classifier 的合法驱动名集合——脚本驱动是 opt-in，避免模糊输入被 LLM 分类误路由。）

- [ ] **Step 4: Register scripted factory in router**

`agent/router/router.go`：import `hegq.net/mos/agent/driver/scripted`；在 `NewHybrid` 的 drivers map 中追加：

```go
			planner.DriverScripted: func() driver.LoopDriver { return &scripted.ScriptedDriver{} },
```

（不追加默认 rule，保持默认路由行为不变。）

- [ ] **Step 5: Wire scripted into assistant**

`assistant/assistant.go`：
1. import `hegq.net/mos/agent/driver/scripted`
2. `Config` 增加字段（`assistant/config.go`）：

```go
	// ScriptPlanner 脚本编排规划器（Driver=scripted 时必需；nil 拒绝运行）。
	ScriptPlanner planner.ScriptPlanner
```

3. `buildRunContext` 追加：`ScriptPlanner: cfg.ScriptPlanner,`
4. `assembleDriver` switch 增加：

```go
	case "scripted":
		return scripted.New(rt), nil
```

- [ ] **Step 6: Wire scripted into config**

`agent/config/config.go`：
1. `Config` 增加字段：

```go
	// ScriptPlanner 脚本编排规划器 provider 名（Driver=scripted 时必需）。
	ScriptPlanner string `yaml:"script_planner"`
```

2. `AssembleConfig` 解析并装配：

```go
	scriptPlanner, err := resolveScriptPlanner(reg, cfg.ScriptPlanner)
	if err != nil {
		return nil, err
	}
	// ac 增加 ScriptPlanner: scriptPlanner
```

3. 新增 `resolveScriptPlanner`（复用 `KindPlanner`，type-assert 到 `planner.ScriptPlanner`），模式对齐 `resolvePlanPlanner`：

```go
func resolveScriptPlanner(reg *seam.SeamRegistry, name string) (planner.ScriptPlanner, error) {
	if name == "" {
		return nil, nil
	}
	v, err := reg.Resolve(seam.KindPlanner, name)
	if err != nil {
		return nil, fmt.Errorf("config: script_planner: %w", err)
	}
	p, ok := v.(planner.ScriptPlanner)
	if !ok {
		return nil, fmt.Errorf("config: script_planner provider %q is not planner.ScriptPlanner (%T)", name, v)
	}
	return p, nil
}
```

- [ ] **Step 7: Run all tests**

Run: `go test ./planner/... ./agent/router/... ./assistant/... ./agent/config/...`
Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add planner/classify.go agent/router/router.go assistant/assistant.go assistant/config.go agent/config/config.go
git commit -m "feat: 脚本驱动接入装配——DriverScripted 常量 + router 工厂 + assistant/config 装配（opt-in，默认不启用）"
```

---

### Task 7: 文档 + 全量回归

**Files:**
- Modify: `README.md`（脚本化编排小节 + 路线图 M3 条目补注）
- Modify: `FEATURES.md`（已实现清单补一行）

**Goal:** 文档同步 + 全量回归确认无破坏。

- [ ] **Step 1: README 新增小节**

在 `README.md` 的 `### DriverRouter` 之后新增 `### 脚本化编排（Scripted Orchestration）`，内容对齐设计 §1/§6/§9，含最小示例：

```go
rt := &driver.RunContext{
	Tools:         reg,
	Planner:       planner.NewReAct(model),   // 合成轮决策器
	ScriptPlanner: planner.NewScript(model),  // 脚本规划器（PTC）
}
a := scripted.New(rt) // 模型写脚本 → 分波并发执行 → 合成回复（共 2 次模型调用）
```

并说明：依赖 DAG / 纯工具调用 / 全程走 Policy+Sandbox+Auditor 共享通道；opt-in（router 规则"批量/重命名"可路由）；步数上限；M2.8 审批断点。

- [ ] **Step 2: 路线图补注**

`README.md` 路线图 M3 的 "LLMCompiler 并行 DAG（可选模式..." 前追加：

> LLMCompiler 的单智能体先行版已落地为 `agent/driver/scripted`（脚本化编排，2026-08）；M3 的 LLMCompiler 在此基础上做多智能体化与更完整的 DAG 控制流。

- [ ] **Step 3: FEATURES.md 同步**

`FEATURES.md` §3.3 追加一行：

```
- **脚本化编排（PTC）**：`agent/driver/scripted`——模型产依赖 DAG 脚本 → 分波有界并发执行（纯工具调用）→ 合成最终回复（共 2 次模型调用）；脚本校验 fail-closed；M2.8 审批断点（checkpoint 携带 ScriptState）；复用 execpath 共享执行通道
```

- [ ] **Step 4: 全量回归 + 提交**

Run: `go test ./...`
Expected: PASS。

```bash
git add README.md FEATURES.md
git commit -m "docs: 脚本化编排使用说明 + 路线图 M3 补注 + FEATURES 同步"
```

---

## Self-Review 结论（已执行）

- **Spec 覆盖**：设计 §2 脚本模型→Task1；§3 校验→Task5(validate)；§4 execpath→Task4；§5 分波→Task5(dag)；§6 驱动/合成→Task5(scripted)；§7 M2.8 断点→Task3+Task5；§8 装配→Task6；§9 影响面→Task4（InstallDefaults 保持默认装配）+Task6（router 无默认规则）；§10 测试→各任务测试；§11 范围外→不在计划内。
- **占位符扫描**：无 TBD/TODO；所有代码步给出完整实现或"原样迁移"明确指引。
- **类型一致性**：`ScriptedPlan/ScriptStep/ScriptPlanner/ScriptState`（planner）、`execpath.ExecuteBatch/ApplyOutcomes/ToolOutcome`、`checkpoint.Checkpoint.ScriptState`、`planner.DriverScripted` 全计划同名同签。
- **一个已修正的草稿缺陷**：dag.go 的 `applyOutcomes` 初稿引用了未传参的 `rt`，已在 Step 8 修正为把 `ctx, rt` 加入参数。
