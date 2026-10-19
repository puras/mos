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
package policy

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/puras/mos/agent/hook"
)

// Config 是策略配置，与 YAML 一一对应。
type Config struct {
	// Allow 是显式放行的工具名。空表示不放行任何工具（fail-closed）。
	Allow []string `yaml:"allow"`
	// Deny 是无条件拒绝的工具名，优先于 Allow。
	Deny []string `yaml:"deny"`
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

// LoadYAML 解析策略配置。未知字段报错（fail-closed：拼错字段名不得被静默忽略，
// 那会让用户以为规则生效了）；空白与重复条目在 New 时归一。
func LoadYAML(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("policy: 解析策略配置: %w", err)
	}
	return cfg, nil
}

// LoadFile 从文件读取策略配置。
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("policy: 读取策略文件 %q: %w", path, err)
	}
	return LoadYAML(data)
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
