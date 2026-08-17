package assistant

import "context"

// ConfirmRequest 是一次危险操作的人工确认请求。
type ConfirmRequest struct {
	ToolName string // 工具名（shell / write_file 等）
	Command  string // 命令原文或目标路径（人类可读）
	Reason   string // 确认原因说明
}

// Confirmer 是危险操作的确认器。
//
// Deprecated: M2.8 起人工介入改为异步审批（agent/approval.Approval）。旧同步确认器
// 经 agent/approval.NewConfirmerAdapter 桥接为 Approval（保留 CLI y/N 体验）；
// 新代码应直接实现 agent/approval.Approval（Request/Resolve 异步通道）。
type Confirmer interface {
	Confirm(ctx context.Context, req ConfirmRequest) (bool, error)
}
