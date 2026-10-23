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
}

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
}

// NewService 创建审批服务。answerer 为 nil 时服务不可用（调用方 fail-closed）；
// audit 为 nil 时不记审计（不记审计不是安全问题的成因，但会丢审计线索）。
func NewService(answerer Answerer, audit Audit) *Service {
	return &Service{answerer: answerer, audit: audit}
}

// reqSeq 是请求序号（配合时间戳保证进程内唯一）。
var reqSeq atomic.Uint64

// Decide 实现 Approver。
//
// 流程：签发 id → 记"问了" → 问人 → 归一化结果 → 记"答了"。
// 审计写失败会中止（不执行）——审计链断了就不知道谁批的。
func (s *Service) Decide(ctx context.Context, req Request) (Outcome, error) {
	if s == nil || s.answerer == nil {
		return Unavailable, fmt.Errorf("approval: 未装配应答方（fail-closed）")
	}
	req.ID = newRequestID()

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

// newRequestID 签发一个请求标识（进程内唯一 + 带时间戳，跨进程也不易撞）。
func newRequestID() string {
	return "ap-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatUint(reqSeq.Add(1), 36)
}
