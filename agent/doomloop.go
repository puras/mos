package agent

import (
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
