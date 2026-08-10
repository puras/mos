package assistant

import "context"

// ConfirmRequest 是一次危险操作的人工确认请求。
type ConfirmRequest struct {
	ToolName string // 工具名（shell / write_file 等）
	Command  string // 命令原文或目标路径（人类可读）
	Reason   string // 确认原因说明
}

// Confirmer 是危险操作的确认器（人工介入点，M2.8 人工介入的轻量雏形）。
// 实现方负责向操作者展示 ConfirmRequest 并收集决定：
// 返回 true 表示允许执行，false 表示拒绝。
// DefaultPolicy 中该字段为 nil 时视为无法确认，受限危险操作一律拒绝（fail-closed）。
type Confirmer interface {
	Confirm(ctx context.Context, req ConfirmRequest) (bool, error)
}
