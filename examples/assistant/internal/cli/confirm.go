package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"hegq.net/mos/pkg/assistant"
)

// cliConfirm 是纯文本 REPL 下的交互确认器：
// 受限危险操作（rm/sudo/管道 shell/写文件等）执行前向用户展示命令并请求 y/N 确认。
// 全屏 TUI 模式不注入（stdin 被界面接管，受限危险操作按 fail-closed 拒绝）。
type cliConfirm struct{}

func (cliConfirm) Confirm(_ context.Context, req assistant.ConfirmRequest) (bool, error) {
	fmt.Printf("\n  %s %s\n  %s %s\n  %s ",
		yellow("⚠ 需确认"),
		req.Reason,
		dim("命令"),
		req.Command,
		cyan("允许执行？[y/N]"))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
