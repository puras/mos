// Package approval 提供人工审批的词汇与编排。
//
// 分工（对齐 PI / DSH）：
//   - **框架**负责"什么时候问、问什么、结果怎么归一、怎么记审计"；
//   - **真正问人的那一方**（弹窗 / 终端 / 另一个进程）由宿主注入（Answerer）。
//
// 框架不弹窗——弹窗是宿主的事：PI 的扩展用 ctx.ui.confirm，DSH 用
// approval/request 事件上的 answerer 链，框架本身都不画界面。
//
// 三条语义：
//   - **未装配即拒绝**（fail-closed，NFR-1）：没有 Answerer 时，ask 档直接
//     拒绝，不"先执行再问"；
//   - **批准只对本次调用生效**（DSH 的 allowed-once）：不缓存批准——上一次
//     批过的命令不代表这一次也行；
//   - **无人应答 = Unavailable**：调用方按 fail-closed 处理（拒绝）。
package approval

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// Outcome 是一次审批的结果。
type Outcome int

const (
	// Approved 批准（仅本次调用生效）。
	Approved Outcome = iota
	// Rejected 拒绝：人明确说不许。
	Rejected
	// Cancelled 撤回：请求被取消（人撤回或上层取消）。
	Cancelled
	// Unavailable 无人应答：审批通道不可用或没有回答者。
	// **调用方必须按 fail-closed 处理**（拒绝执行）。
	Unavailable
)

// String 返回结果的名字（写进审计事件与拒绝理由）。
func (o Outcome) String() string {
	switch o {
	case Approved:
		return "approved"
	case Rejected:
		return "rejected"
	case Cancelled:
		return "cancelled"
	case Unavailable:
		return "unavailable"
	default:
		return "unknown(" + strconv.Itoa(int(o)) + ")"
	}
}

// Request 是一次审批请求。
type Request struct {
	// ID 是服务签发的请求标识（每次请求一个新值）——用来把"问了"与
	// "答了"两条审计记录配对。
	ID string
	// ToolName 是被审批的工具名。
	ToolName string
	// CallID 是被审批的那次调用（模型给的标识）。
	CallID string
	// Reason 是问人的理由（要写成人话：为什么要批这一条）。
	Reason string
	// Turn 是所属轮次。
	Turn int
	// ReusedFrom 是「本次结果复用了哪一次审批决定」（S9.1 先验复用时填）。
	//
	// 复用时**签发新 id**（事件流里每次审批身份独立、可审计），这个字段
	// 指回被复用的那次决定的 id——审计看得出「这次没重新问人」。
	// 空 = 不是复用（正常新问的一次）。
	ReusedFrom string
}

// PriorDecision 是一次「先验决定」：此前已有人对同一轮同一次调用批过。
type PriorDecision struct {
	// Outcome 是被复用的结果（只有 approved / rejected 可复用）。
	Outcome Outcome
	// ReusedFrom 是被复用的那次审批的 id（写进审计载荷，可审计）。
	ReusedFrom string
}

// Prior 是「先验决定查询」：命中就**复用已有决定、不再问人**，没命中照常问。
//
// **匹配键由实现方定**（框架给出去的是 `(Turn, CallID)`）——`Service` 每次
// 请求都签发新 id，拿 id 匹配等于永远匹配不上。
//
// 实现方由**装配层**注入：框架层不依赖 session（治理规则 5：框架不绑
// 宿主形态），由装配层用 session 的派生读接口包出来。
type Prior func(ctx context.Context, req Request) (PriorDecision, bool)

// Approver 是"能问人"的能力（Service 是它的标准实现）。
type Approver interface {
	// Decide 等待并返回人工决定。
	Decide(ctx context.Context, req Request) (Outcome, error)
}

// Answerer 是"问人"的那一方（宿主实现）：等一个人工决定。
//
// 怎么等由宿主决定：弹窗、读终端、轮询别的进程、查数据库……框架不关心，
// 只要求最终给出四档结果之一。
type Answerer interface {
	// Decide 等待人工决定。返回 error 表示通道本身坏了（不是"拒绝"），
	// 调用方按 fail-closed 处理。
	Decide(ctx context.Context, req Request) (Outcome, error)
}

// Audit 是审批审计的落地口（会话日志等）。
//
// 写失败**必须**返回 error：审计写不进去就不该继续执行（"日志即事实"）。
type Audit interface {
	// Asked 记录"问了"。
	Asked(req Request) error
	// Decided 记录"答了"。
	Decided(req Request, out Outcome) error
}

// Service 是审批服务：签发请求 id、问人、归一化结果、记审计。
type Service struct {
	answerer Answerer
	audit    Audit
	prior    Prior
}

// NewService 创建审批服务。answerer 为 nil 时服务不可用（调用方 fail-closed）；
// audit 为 nil 时不记审计（不记审计不是安全问题的成因，但会丢审计线索）。
func NewService(answerer Answerer, audit Audit) *Service {
	return &Service{answerer: answerer, audit: audit}
}

// SetPrior 装上先验决定查询（可选；传 nil = 不查）。
//
// 命中时**不再问人**，直接复用已有决定（记新 id + 载荷带 reusedFrom）。
// 这是「同一轮内不重复问人」的实现（S9.1）。
func (s *Service) SetPrior(p Prior) {
	if s == nil {
		return
	}
	s.prior = p
}

// reqSeq 是请求序号（配合时间戳保证进程内唯一）。
var reqSeq atomic.Uint64

// Decide 实现 Approver。
//
// 流程：签发 id → **先验复用**（命中就不问人）→ 记"问了" → 问人 → 归一化结果
// → 记"答了"。审计写失败会中止（不执行）——审计链断了就不知道谁批的。
func (s *Service) Decide(ctx context.Context, req Request) (Outcome, error) {
	if s == nil || s.answerer == nil {
		return Unavailable, fmt.Errorf("approval: 未装配应答方（fail-closed）")
	}
	req.ID = newRequestID()

	// 先验复用（S9.1）：同一轮、同一次调用上已经有人批过 → 复用，不再问人。
	//
	// 复用记**新 id** + 载荷带 reusedFrom（事件流里每次审批身份独立、可审计）。
	// 审计照样记一对 asked + decided（新 id）：`asked` 在这里表示「这次调用
	// 进了审批通道」，是不是真问过人由 reusedFrom 说——保持「每个决定都有
	// 配对的 asked」这条不变式，宿主按事件流对账不会看到孤儿。
	//
	// 只有**真决定**可复用：approved / rejected。cancelled（撤回）与
	// unavailable（无人应答）不是人的决定，复用它们等于把「没人答」当成
	// 「批过了」——不。
	if s.prior != nil {
		if d, ok := s.prior(ctx, req); ok && reusable(d.Outcome) {
			req.ReusedFrom = d.ReusedFrom
			if s.audit != nil {
				if err := s.audit.Asked(req); err != nil {
					return Unavailable, fmt.Errorf("approval: 记录审批请求失败: %w", err)
				}
				if err := s.audit.Decided(req, d.Outcome); err != nil {
					return Unavailable, fmt.Errorf("approval: 记录审批结果失败: %w", err)
				}
			}
			return d.Outcome, nil
		}
	}

	if s.audit != nil {
		if err := s.audit.Asked(req); err != nil {
			return Unavailable, fmt.Errorf("approval: 记录审批请求失败: %w", err)
		}
	}

	out, err := s.answerer.Decide(ctx, req)
	if err != nil {
		// 通道坏了 ≠ 人拒绝：归一到 Unavailable（fail-closed），但把原因
		// 带给调用方（它要写进拒绝理由，模型才知道是人没答还是通道崩了）。
		out = Unavailable
	}
	if out != Approved && out != Rejected && out != Cancelled {
		out = Unavailable
	}

	if s.audit != nil {
		if aerr := s.audit.Decided(req, out); aerr != nil {
			return Unavailable, fmt.Errorf("approval: 记录审批结果失败: %w", aerr)
		}
	}
	return out, err
}

// reusable 判断一个先验结果能不能复用：只有「真决定」（批准 / 拒绝）可复用。
func reusable(o Outcome) bool {
	return o == Approved || o == Rejected
}

// ParseOutcome 解析结果名（"approved" / "rejected" / "cancelled" /
// "unavailable"）。第二个返回值表示「认不认识这个名字」。
//
// 与 Outcome.String 互逆：会话日志里存的是结果名（session 包不依赖
// approval 包），读回来时用它兑成 Outcome。
func ParseOutcome(name string) (Outcome, bool) {
	switch name {
	case "approved":
		return Approved, true
	case "rejected":
		return Rejected, true
	case "cancelled":
		return Cancelled, true
	case "unavailable":
		return Unavailable, true
	}
	return Unavailable, false
}

// newRequestID 签发一个请求标识（进程内唯一 + 带时间戳，跨进程也不易撞）。
func newRequestID() string {
	return "ap-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatUint(reqSeq.Add(1), 36)
}
