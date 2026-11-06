package skill

import (
	"fmt"
	"sort"
	"strings"
)

// ToolProbe 探测「模型当前可见的工具名集合」。
//
// **框架不绑工具注册表**（治理规则 5）：agent/skill 不知道 tool.Registry 的
// 存在——装配层把探针接进来（通常一行 `a.tools.Visible`）。没接 = 不做诊断
// （零开销降级，allowed-tools 字段退回装配期的那条「不生效」警告）。
type ToolProbe func() []string

// allowedToolsCheck 是技能激活时 `allowed-tools` × 当前可见工具集的求交诊断
// 结果（S8.4-D）。
//
// **只诊断、不收窄**（拍板）：工具面不因这个字段改变——缺的工具照样报通用
// 拒绝，只是人（宿主/开发者）在事件与快照里看得到「技能要的工具缺了」。
type allowedToolsCheck struct {
	// Missing 是声明了但当前模型看不见的工具（按声明顺序）。
	Missing []string
	// Notice 是给人看的文案（Missing 为空时为空串）。
	Notice string
}

// checkAllowedTools 把技能声明的 allowed-tools 与可见工具集求交。
//
// 语义注意：可见集**不含 deferred 未激活**的工具——技能说「用 mcp__gh__x」
// 而 x 是 deferred 未激活时，也会列进 Missing，并提示可用 tool_search 激活
// （这正是 deferred 的用法，不算错误，只算提示）。
func checkAllowedTools(declared []string, probe ToolProbe) allowedToolsCheck {
	if len(declared) == 0 || probe == nil {
		return allowedToolsCheck{}
	}
	visible := map[string]bool{}
	for _, n := range probe() {
		visible[n] = true
	}
	var missing []string
	for _, d := range declared {
		if !visible[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return allowedToolsCheck{}
	}
	missing = append([]string(nil), missing...)
	sort.Strings(missing)
	return allowedToolsCheck{
		Missing: missing,
		Notice: fmt.Sprintf("技能声明的工具当前不可用: %s（allowed-tools 不收窄工具面，仅提示；若为 deferred 工具可用 tool_search 激活）",
			strings.Join(missing, ", ")),
	}
}
