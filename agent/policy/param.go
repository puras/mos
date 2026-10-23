package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/puras/mos/agent/hook"
)

// Verdict 是参数级检查的结论。
type Verdict int

const (
	// VerdictAllow 放行。
	VerdictAllow Verdict = iota
	// VerdictDeny 拒绝：不可被钩子改写绕过（走守卫这道门）。
	VerdictDeny
	// VerdictAsk 需人工确认：走 pre-execute 钩子那道门；没有审批通道时
	// 由执行通道 fail-closed 拒绝（S5g 落地后才变成"挂起等审批"）。
	VerdictAsk
)

// Call 是被检查的调用（**只读快照**）。
//
// 传值而非传指针是有意的：检查器拿到的是副本，**物理上改不动载荷**。
// 载荷冻结（工具名/参数/CallID 不可被钩子改写）是执行通道的不变式，
// 参数级检查器不该有机会破坏它。
type Call struct {
	// Tool 是被调用的工具名。
	Tool string
	// Args 是参数的 JSON 编码。
	Args string
	// CallID 是本次调用的标识。
	CallID string
	// Turn 是所属轮次（跨轮判定用，如重复调用检测）。
	Turn int
}

// Check 是参数级检查函数：判定一次调用，返回结论与理由。
//
// **理由会回填给模型**——它要靠理由自我纠正（"为什么被拒"比"被拒了"有用得多）。
//
// 实现约定：判定只应依据**参数本身**（不查外部状态），这样同一份日志能重放出
// 同样的判定（审计可复现）。需要外部事实（如"这个路径在不在界内"）时，宿主
// 应在**装配期**把结论收进闭包，而不是在判定时去查。
type Check func(ctx context.Context, call Call) (Verdict, string)

// ParamPolicy 是参数级检查的接入件：把检查器接到执行通道的**两个门**。
//
// 为什么是两个门：守卫（Guard）是单调的、只判"放/拒"，且不可被钩子洗白；
// pre-execute 钩子能表达"需人工确认"这种第三档。两个门各有其位，混用会
// 丢掉其中一档的能力。
//
// **两个都要装**：只装 Guard → ask 档的命令直接放行；只装钩子 → deny 档
// 不可被洗白这条保证就没了（deny 走钩子，钩子链后面的钩子仍能改写决策）。
type ParamPolicy struct {
	name   string
	checks []Check
}

// NewParamPolicy 创建一个参数级策略。name 用于拒绝理由的前缀（便于排查）。
func NewParamPolicy(name string, checks ...Check) *ParamPolicy {
	return &ParamPolicy{name: name, checks: checks}
}

// Guard 返回守卫接线：**只认 deny 结论**（ask / allow 放行）。
//
// 守卫跑在钩子之前、看到的是原始载荷——deny 在这里短路，模型看不到后续环节。
func (p *ParamPolicy) Guard() hook.Guard {
	return func(ctx context.Context, ev *hook.HookEvent) error {
		if p == nil {
			return nil
		}
		for _, c := range p.checks {
			if c == nil {
				continue
			}
			verdict, reason := c(ctx, callOf(ev))
			if verdict != VerdictDeny {
				continue
			}
			return errors.New(p.prefix(reason))
		}
		return nil
	}
}

// Approval 返回 pre-execute 钩子接线：**只认 ask 结论**（deny / allow 放行）。
//
// ask 的执行结果是宿主的事：没有审批通道时由执行通道 fail-closed 拒绝，
// 装了审批通道则挂起等待人工决定（S5g）。
func (p *ParamPolicy) Approval() hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if p == nil {
			return next(ctx, ev)
		}
		for _, c := range p.checks {
			if c == nil {
				continue
			}
			verdict, reason := c(ctx, callOf(ev))
			if verdict != VerdictAsk {
				continue
			}
			// 决策置 ask：执行通道按"无审批通道 → fail-closed"处理。
			ev.Decision = hook.Decision{Kind: hook.DecisionAsk, Reason: p.prefix(reason)}
			return next(ctx, ev)
		}
		return next(ctx, ev)
	}
}

// callOf 从事件取只读快照（载荷改不动）。
func callOf(ev *hook.HookEvent) Call {
	if ev == nil {
		return Call{}
	}
	return Call{
		Tool:   ev.ToolCall.Name,
		Args:   ev.ToolCall.Args,
		CallID: ev.ToolCall.ID,
		Turn:   ev.Turn,
	}
}

// prefix 给理由加上策略名前缀（空名字时原样返回）。
func (p *ParamPolicy) prefix(reason string) string {
	if p == nil || p.name == "" {
		return reason
	}
	return p.name + ": " + reason
}

// ArgsInto 把调用的参数解码到 v（JSON 编码）。
//
// 参数解析失败是**拒绝**的理由（fail-closed）：参数都读不懂，谈不上判定。
func ArgsInto(call Call, v any) error {
	if v == nil {
		return errors.New("解码目标为空")
	}
	raw := strings.TrimSpace(call.Args)
	if raw == "" {
		return fmt.Errorf("工具 %q 没有参数", call.Tool)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("解析工具 %q 的参数失败: %w", call.Tool, err)
	}
	return nil
}

// Arg 解出调用的某个**字符串**参数。
func Arg(call Call, field string) (string, error) {
	var m map[string]any
	if err := ArgsInto(call, &m); err != nil {
		return "", err
	}
	v, ok := m[field]
	if !ok {
		return "", fmt.Errorf("工具 %q 没有参数 %q", call.Tool, field)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("工具 %q 的参数 %q 不是字符串", call.Tool, field)
	}
	return s, nil
}
