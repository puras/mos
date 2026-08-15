package hook

import (
	"context"
	"fmt"
	"strings"

	"hegq.net/mos/llm"
)

// doomLoopDetector 检测 Doom Loop（M2.5 上下文工程）：
// 相同工具连续多次相同参数调用（如模型反复重试同一失败操作）时判定为循环。
// 命中后任务应中断（状态 blocked），避免空转耗尽上下文。
type doomLoopDetector struct {
	// maxRepeat 连续相同调用多少次判定为 Doom Loop。
	maxRepeat int
	// 上一次调用指纹
	toolName string
	argsHash string
	count    int
}

// newDoomLoopDetector 创建 Doom Loop 检测器；maxRepeat<=0 时默认 3。
func newDoomLoopDetector(maxRepeat int) *doomLoopDetector {
	if maxRepeat <= 0 {
		maxRepeat = 3
	}
	return &doomLoopDetector{maxRepeat: maxRepeat}
}

// observe 记录一次工具调用；返回 true 表示已连续 maxRepeat 次相同调用（Doom Loop）。
func (d *doomLoopDetector) observe(call *llm.ToolCall) bool {
	h := strings.TrimSpace(call.Args)
	if d.toolName == call.Name && d.argsHash == h {
		d.count++
	} else {
		d.toolName = call.Name
		d.argsHash = h
		d.count = 1
	}
	return d.count >= d.maxRepeat
}

// DoomLoopError 表示任务因 Doom Loop（相同工具连续相同参数调用）被中断。
// 由 DoomLoopGuard 作为守卫错误抛出；循环层识别后终止任务并置状态 blocked。
type DoomLoopError struct {
	ToolName string
	Repeat   int
}

func (e *DoomLoopError) Error() string {
	return fmt.Sprintf("agent: doom loop detected: tool %q called %d times with identical args", e.ToolName, e.Repeat)
}

// DoomLoopGuard 是工具执行前（tool/pre-execute）的单调守卫：
// 相同工具连续相同参数调用达到阈值时拒绝并终止任务，避免空转耗尽上下文。
// 与 Policy 守卫同级注册在 GuardBus（不可被普通钩子改写绕过），见架构 v2 §3.4。
type DoomLoopGuard struct {
	maxRepeat int
	det       *doomLoopDetector
}

// NewDoomLoopGuard 创建 Doom Loop 守卫；maxRepeat<=0 时默认 3。
func NewDoomLoopGuard(maxRepeat int) *DoomLoopGuard {
	return &DoomLoopGuard{maxRepeat: maxRepeat, det: newDoomLoopDetector(maxRepeat)}
}

// Guard 实现 Guard 接口：命中 Doom Loop 时返回 *DoomLoopError（致命，循环层终止任务）。
func (g *DoomLoopGuard) Guard(_ context.Context, ev *HookEvent) error {
	if g == nil || ev == nil || ev.ToolCall == nil {
		return nil
	}
	if g.det.observe(ev.ToolCall) {
		return &DoomLoopError{ToolName: ev.ToolCall.Name, Repeat: g.maxRepeat}
	}
	return nil
}
