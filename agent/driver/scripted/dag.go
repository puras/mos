package scripted

import (
	"context"
	"encoding/json"
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
// 字段导出：dag_test 的 TestRestoreStateDeepCopyOutputs 在恢复后 mutate
// Outputs/Status 断言与 checkpoint 快照的内存隔离（resume 续跑不污染存档）。
type scriptState struct {
	Status      map[string]string // stepID → status
	Outputs     map[string]any    // stepID → 命名结果
	ResumeCalls []llm.ToolCall    // 挂起波的工具调用（resume 重放）
}

func newState(plan *planner.ScriptedPlan) *scriptState {
	st := &scriptState{
		Status:  make(map[string]string, len(plan.Steps)),
		Outputs: make(map[string]any),
	}
	for i := range plan.Steps {
		st.Status[plan.Steps[i].ID] = statusPending
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

// lookupField 提取引用的单层字段。统一经 JSON 归一化取字段：
// 工具输出在模型面/参数面是 JSON 形态，数字即 float64；直接读 Go 原生 map
// 会保留 int 等类型，与后续参数序列化（json.Marshal）表现不一致。
func lookupField(val any, field string) (any, error) {
	b, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("script: ${...}.%s: result not an object", field)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("script: ${...}.%s: result not an object", field)
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
			st.ResumeCalls = resumeCalls
			return nil, suspend, nil
		}
		applyOutcomes(ctx, rt, plan, st, resumeCalls, outs, turn, sess)
		st.ResumeCalls = nil
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
			call, err := resolveCall(&ready[i], st.Outputs)
			if err != nil {
				st.Status[ready[i].ID] = statusFailed
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
			st.Status[id] = statusRunning
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{Turn: turn, StepID: id, Tool: c.Name, Status: statusRunning})
		}
		outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, 1, calls)
		if suspend != nil {
			st.ResumeCalls = calls
			return nil, suspend, nil
		}
		applyOutcomes(ctx, rt, plan, st, calls, outs, turn, sess)
	}

	// 剩余 pending（传递依赖失败，不可达）→ skipped
	for i := range plan.Steps {
		if st.Status[plan.Steps[i].ID] == statusPending {
			st.Status[plan.Steps[i].ID] = statusSkipped
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{Turn: turn, StepID: plan.Steps[i].ID, Tool: plan.Steps[i].Tool, Status: statusSkipped})
		}
	}
	return &scriptReport{Status: st.Status, Outputs: st.Outputs}, nil, nil
}

// readySteps 返回依赖已全部 done 的 pending 步骤。
func readySteps(plan *planner.ScriptedPlan, st *scriptState) []planner.ScriptStep {
	var out []planner.ScriptStep
	for _, s := range plan.Steps {
		if st.Status[s.ID] != statusPending {
			continue
		}
		ok := true
		for _, dep := range s.DependsOn {
			if st.Status[dep] != statusDone {
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
			st.Status[id] = statusDenied
			markSkipped(plan, st, id)
		case out.Output.Err != nil:
			st.Status[id] = statusFailed
			markSkipped(plan, st, id)
		default:
			st.Status[id] = statusDone
			st.Outputs[id] = out.Output.Result
		}
		_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{
			Turn: turn, StepID: id, Tool: out.Input.Name, Status: st.Status[id],
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
	if st.Status[stepID] != statusPending {
		return
	}
	st.Status[stepID] = statusSkipped
	for i := range plan.Steps {
		dep := &plan.Steps[i]
		for _, d := range dep.DependsOn {
			if d == stepID {
				markSkipped(plan, st, dep.ID)
			}
		}
	}
}
