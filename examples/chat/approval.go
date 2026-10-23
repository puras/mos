package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/puras/mos/agent/approval"
)

// terminalAnswerer 在**终端里问人**：等一行 y/N。
//
// 这是宿主侧"问人"的最小实现——框架只提供决策点（agent/approval），
// 怎么问由宿主决定：弹窗、终端、另一个进程都行。
//
// 与主循环共用同一个 Scanner：审批发生在驱动阻塞期间（同一 goroutine），
// 所以直接读下一行即可，不会与"你 > "的输入抢。
type terminalAnswerer struct {
	in  *bufio.Scanner
	out io.Writer
}

// Decide 等一个人工决定。
//
// 默认**拒绝**（空回车 = 不批准）：审批的默认答案必须是安全的那个。
func (t terminalAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	fmt.Fprintf(t.out, "\n[审批] 是否允许执行 %s？\n", req.ToolName)
	if req.Reason != "" {
		fmt.Fprintf(t.out, "       理由：%s\n", req.Reason)
	}
	fmt.Fprintf(t.out, "       允许？[y/N]: ")

	if !t.in.Scan() {
		// 输入没了（EOF）：没人应答 = 不可用，fail-closed。
		fmt.Fprintln(t.out)
		return approval.Unavailable, fmt.Errorf("终端已关闭，没人应答")
	}
	switch strings.ToLower(strings.TrimSpace(t.in.Text())) {
	case "y", "yes":
		return approval.Approved, nil
	case "n", "no":
		return approval.Rejected, nil
	default:
		// 空回车或其他输入 → 不批准（默认拒绝）。
		return approval.Rejected, nil
	}
}
