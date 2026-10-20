// Package policy 提供工具调用的规则层安全策略（Policy）：声明式规则 → 单调守卫。
//
// 定位：它回答「这个工具这次调用允不允许」，不回答「能不能解析参数」「怎么执行」
// ——后者归 filesystem jail（S5d）与 sandbox（S5f）。Policy 是纵深防御的第一层，
// 便宜、可配置、装配期即可看清全貌。
//
// 两条纪律：
//   - fail-closed（不变式 1）：未列入 allow 的工具一律拒绝。平台新增自定义工具
//     必须显式声明放行——「忘了声明」的后果是拒绝而不是放行，这是安全默认值。
//   - deny 优先于 allow：黑白名单同时命中时以拒绝为准，deny 不提供任何放行后门。
//
// 本切片刻意只做**工具名级**规则：参数级校验（shell 命令分级、文件路径越界）
// 依赖 jail 与沙箱，那两块（S5d/S5f）就位后再加进 Checker 体系，不在此处造
// 一个没有消费者的正则引擎。
//
// 分层：框架只认**结构**，不认文件、不认配置格式。
//   - 读文件（os.ReadFile）是宿主的活；
//   - 解析 YAML/JSON 也是宿主的活——格式与键名归平台，框架不绑定任何一种
//     （Config 不带 yaml/json tag，也不提供 ParseYAML：绑一个格式就要为它背
//     依赖，而平台可能用 JSON、配置中心或远端下发）。
//
// 框架替宿主打开路径或替宿主认格式，等于替宿主做了本该可替换的决定。
package policy

import (
	"context"
	"fmt"
	"strings"

	"github.com/puras/mos/agent/hook"
)

// Config 是策略配置。
//
// 刻意**不带 yaml/json tag**：配置格式与键名归宿主，框架只认这个结构本身。
// 宿主可用自己的 DTO 解析后再转成 Config（推荐，键名与框架解耦），也可直接
// unmarshal 到 Config（此时键名默认是字段名小写，allow/deny 正好对上）。
type Config struct {
	// Allow 是显式放行的工具名。空表示不放行任何工具（fail-closed）。
	Allow []string
	// Deny 是无条件拒绝的工具名，优先于 Allow。
	Deny []string
}

// Policy 是按配置裁决的工具名策略，实现 hook.Guard：
// 装到 GuardBus 上即成为单调守卫链的一环，钩子改写不了它的结论（S5b 双轨制）。
type Policy struct {
	allow map[string]struct{}
	deny  map[string]struct{}
}

// New 按配置创建策略。Config 零值 = 拒绝一切。
func New(cfg Config) *Policy {
	p := &Policy{allow: index(cfg.Allow), deny: index(cfg.Deny)}
	if p.allow == nil {
		p.allow = map[string]struct{}{}
	}
	if p.deny == nil {
		p.deny = map[string]struct{}{}
	}
	return p
}

// Check 实现 hook.Guard：裁决一次工具调用。
//
// nil 接收者也返回拒绝而不是 panic——fail-closed 是行为，不只是默认值。
func (p *Policy) Check(_ context.Context, ev *hook.HookEvent) error {
	if p == nil {
		return fmt.Errorf("安全策略未装配，已拒绝")
	}
	if ev == nil || ev.ToolCall.Name == "" {
		return fmt.Errorf("工具调用缺少工具名，无法裁决，已拒绝")
	}
	name := ev.ToolCall.Name
	if _, denied := p.deny[name]; denied {
		return fmt.Errorf("工具 %q 在策略拒绝名单中，已拒绝", name)
	}
	if _, ok := p.allow[name]; !ok {
		return fmt.Errorf("工具 %q 未列入策略放行名单（fail-closed），已拒绝", name)
	}
	return nil
}

// index 把名单归一为集合：去空白、去空项、去重。
func index(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		out[n] = struct{}{}
	}
	return out
}
