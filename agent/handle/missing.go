package handle

import (
	"encoding/json"
	"fmt"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
)

// Missing 描述恢复时缺失的资源（对齐 PI SuspendedOperation 的 missing 校验）：
// resume 前校验会话事件引用的工具在 RunContext.Tools 中存在，缺失则明确报错，
// 避免"恢复后跑一半发现工具没了"的静默失败。
type Missing struct {
	Tools []string // 会话事件引用但 RT.Tools 中不存在的工具名
}

// Error 实现 error。
func (m Missing) Error() string {
	return fmt.Sprintf("agent: resume missing resources: tools %v not in RunContext", m.Tools)
}

// HasMissing 报告是否缺少资源。
func (m Missing) HasMissing() bool { return len(m.Tools) > 0 }

// ValidateMissing 扫描会话事件中出现的工具名，断言其存在于 rt.Tools。
// 模型引用（rt.Planner）缺失不在此校验（Planner 由装配层保证非 nil）。
func ValidateMissing(rt *driver.RunContext, sess *session.Session) error {
	if rt == nil || sess == nil {
		return nil
	}
	seen := make(map[string]bool)
	var missing []string
	for _, ev := range sess.Events {
		if ev.Type != event.SessionEventToolCall && ev.Type != event.SessionEventToolResult {
			continue
		}
		var name string
		switch ev.Type {
		case event.SessionEventToolCall:
			var d event.ToolCallData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			name = d.Name
		case event.SessionEventToolResult:
			var d event.ToolResultData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			name = d.Name
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := rt.Tools.Get(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Missing{Tools: missing}
	}
	return nil
}
