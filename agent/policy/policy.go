// Package policy 提供工具调用的规则层安全策略（Policy）：声明式规则 → 单调守卫。
//
// 定位：它回答「这个工具这次调用允不允许」，不回答「能不能解析参数」「怎么执行」
// ——后者归 filesystem jail（S5d）与 sandbox（S5f）。Policy 是纵深防御的第一层，
// 便宜、可配置、装配期即可看清全貌。
//
// ## 语义（对齐 PI 与 DSH 的共识）
//
//	允许(name) = (Allow 未配置 OR name 匹配 Allow) AND NOT name 匹配 Deny
//
//	- Deny 命中 → 一律拒绝（**优先**，不提供任何放行后门）
//	- Allow **未配置**（nil）→ 不限制
//	- Allow 配置了但**是空集** → 全禁
//	- Allow 非空 → 只有名单内的工具可用
//
// 两份名单都可选，与 PI 的 allowedToolNames / excludedToolNames 和 DSH 的
// ToolRestriction{allow?, deny?} 同形。名字支持 `*` 通配（PI 的语义：`*` 匹配任意
// 字符，含分隔符；整串锚定）。
//
// ## 分层：策略是**收窄层**，不是能力清单
//
// fail-closed 的那道门是**注册表**：没注册的工具，模型看不见也调不动。策略层只
// 负责在已给出的能力上挖洞——"只允许某几个工具"靠**不注册**其余的来表达，不必
// 在策略里再写一遍白名单。
//
// ## 可见性与运行时是同一份名单
//
//	Policy.Visible  → 工具**不出现在模型看到的清单里**（PI/DSH 的可见性层）
//	Policy.Check    → 运行时守卫，拦下模型幻觉出的名字、策略中途变更、非模型路径
//
// 两者读同一份名单，所以不会出现"看不见却能调"的矛盾。
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
	"regexp"
	"strings"

	"github.com/puras/mos/agent/hook"
)

// Config 是策略配置。
//
// 刻意**不带 yaml/json tag**：配置格式与键名归宿主，框架只认这个结构本身。
// 宿主可用自己的 DTO 解析后再转成 Config（推荐，键名与框架解耦）。
type Config struct {
	// Allow 是显式放行的工具名（支持 `*` 通配）。
	//
	// nil（未配置）= 不限制；**空集 = 全禁**（对齐 PI 的 "An empty list exposes no
	// tools" 与 DSH 的 `allow !== undefined && !allow.has(name)`）。
	Allow []string
	// Deny 是拒绝的工具名（支持 `*` 通配），优先于 Allow。
	Deny []string
}

// Policy 是按配置裁决的工具名策略，实现 hook.Guard：
// 装到 GuardBus 上即成为单调守卫链的一环，钩子改写不了它的结论（S5b 双轨制）。
//
// 它同时实现可见性判定（Visible）：被禁的工具不出现在模型看到的清单里。
type Policy struct {
	// allow 为 nil 表示未配置（不限制）；非 nil 但为空集表示全禁。
	allow *matcher
	deny  *matcher
}

// New 按配置创建策略。
//
// 注意 Allow 的 nil 与空集含义不同：nil = 不限制，[] = 全禁。
func New(cfg Config) *Policy {
	p := &Policy{deny: newMatcher(cfg.Deny)}
	if cfg.Allow != nil {
		p.allow = newMatcher(cfg.Allow)
	}
	return p
}

// Check 实现 hook.Guard：裁决一次工具调用。
//
// nil 接收者也返回拒绝而不是 panic——策略对象缺失是收紧，不是放开。
func (p *Policy) Check(_ context.Context, ev *hook.HookEvent) error {
	if p == nil {
		return fmt.Errorf("安全策略未装配，已拒绝")
	}
	if ev == nil || ev.ToolCall.Name == "" {
		return fmt.Errorf("工具调用缺少工具名，无法裁决，已拒绝")
	}
	name := ev.ToolCall.Name
	if p.deny.match(name) {
		return fmt.Errorf("工具 %q 在策略拒绝名单中，已拒绝", name)
	}
	if p.allow != nil && !p.allow.match(name) {
		return fmt.Errorf("工具 %q 不在策略放行名单中，已拒绝", name)
	}
	return nil
}

// Visible 报告该工具是否应当出现在**模型看到的工具清单**里。
//
// 与 Check 读同一份名单，所以"看不见"与"调不动"永远一致。
//
// 必须**无副作用**——它会在每次组装工具清单时被调用（PI/DSH 的可见性层同义）。
// nil 接收者返回 false（收紧）：策略对象缺失是 bug，不能因此把工具全放出去。
func (p *Policy) Visible(name string) bool {
	if p == nil {
		return false
	}
	if p.deny.match(name) {
		return false
	}
	if p.allow != nil && !p.allow.match(name) {
		return false
	}
	return true
}

// matcher 是工具名匹配器：精确名走集合，含 `*` 的走模式。
//
// 模式语义照 PI（mcp-servers.ts 的 toolPatternRegExp）：`*` → `.*`（匹配任意字符，
// 含分隔符），其余字符按字面量转义，整串锚定。于是 `write_*` 能匹配 `write_a_b`。
type matcher struct {
	names    map[string]struct{}
	patterns []*regexp.Regexp
}

// newMatcher 编译一份名单。空名单得到"什么都不匹配"的匹配器。
func newMatcher(entries []string) *matcher {
	m := &matcher{names: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "*") {
			m.names[e] = struct{}{}
			continue
		}
		parts := strings.Split(e, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
		if err != nil {
			// 编译失败（理论上不会发生：QuoteMeta 后无非法字符）：当成精确名，
			// 不静默放过。
			m.names[e] = struct{}{}
			continue
		}
		m.patterns = append(m.patterns, re)
	}
	return m
}

// match 报告名字是否命中名单。
func (m *matcher) match(name string) bool {
	if m == nil {
		return false
	}
	if _, ok := m.names[name]; ok {
		return true
	}
	for _, re := range m.patterns {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}
