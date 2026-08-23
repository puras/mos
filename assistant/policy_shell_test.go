package assistant

import (
	"context"
	"strings"
	"testing"
)

func TestShellCheckRMDeniedOutsideWritable(t *testing.T) {
	p := &DefaultPolicy{FileAccess: FileAccessConfig{
		WorkspaceRoot: t.TempDir(),
		JailReads:     true,
		JailWrites:    true,
	}}
	// 模拟解析 rm /etc/passwd：受限危险命令 + 路径越界 → 应拒绝。
	// 不配置 Approval → 即使路径越界也会先返回 approval 缺失错误；
	// 改用注入 Approval 让错误回到路径校验这一支。
	p.Approval = nil // 显式空（NewDefaultPolicy 默认即空）
	err := p.checkShell(context.Background(), `{"command":"rm /etc/passwd"}`)
	if err == nil {
		t.Fatal("应拒绝 rm 越界")
	}
	// 路径越界错误应优先于 Approval 缺失（短路顺序在 checkShell 中：restricted → path check → approval）
	if !strings.Contains(err.Error(), "超出工作目录") && !strings.Contains(err.Error(), "工作目录限定") {
		t.Errorf("应含路径越界/工作目录错误，got=%v", err)
	}
}

func TestShellCheckRMAllowsInsideWorkspace(t *testing.T) {
	workDir := t.TempDir()
	p := &DefaultPolicy{FileAccess: FileAccessConfig{
		WorkspaceRoot: workDir,
		JailReads:     true,
		JailWrites:    true,
	}}
	// 在 workspace 内的相对路径：解析失败但不应"路径越界"
	// 仍会因 Approval 未配置返回错误（拒绝放行需确认），但文案不应是"路径越界"
	err := p.checkShell(context.Background(), `{"command":"rm tmp/a.txt"}`)
	if err == nil {
		t.Fatal("未配置 Approval 应拒绝受限危险命令")
	}
	if strings.Contains(err.Error(), "超出工作目录") {
		t.Errorf("workspace 内路径不应触发越界，got=%v", err)
	}
}
