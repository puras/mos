package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/agent/approval"
)

// checkShell 校验 shell 命令：绝对危险拒绝 → 白名单 → 受限危险（路径受限 + 确认）。
func (p *DefaultPolicy) checkShell(ctx context.Context, argsJSON string) error {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析命令参数: %w", err)
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return fmt.Errorf("命令不能为空")
	}

	// ① 绝对危险命令：无条件拒绝（无合理业务场景，不提供确认放行）
	if bad := absolutelyDangerous(cmd); bad != "" {
		return fmt.Errorf("命令包含危险操作 %q，已拒绝", bad)
	}

	// ② 命令前缀白名单（非空时）：不匹配一律拒绝
	if len(p.ShellAllowed) > 0 && !matchPrefixes(cmd, p.ShellAllowed) {
		return fmt.Errorf("命令不在允许列表，允许前缀: %s", strings.Join(p.ShellAllowed, ", "))
	}

	// ③ 受限危险命令：路径受限在 FileAccess.WorkspaceRoot（WritableRoots 多根）内 + 人工确认
	if bad := restrictedDangerous(cmd); bad != "" {
		if p.FileAccess.WorkspaceRoot == "" {
			return fmt.Errorf("命令包含受限危险操作 %q，但未配置工作目录限定范围，已拒绝", bad)
		}
		if err := p.checkPathsInWorkDir(cmd); err != nil {
			return fmt.Errorf("命令 %q 的路径超出工作目录: %w", cmd, err)
		}
		if p.Approval == nil {
			return fmt.Errorf("命令包含受限危险操作 %q，需人工确认但未配置 Approval，已拒绝", bad)
		}
		// 异步审批（M2.8）：由驱动层 policyGuard 发起 Approval.Request（挂起 / 立即决策）。
		return &approval.RequiredError{Request: approval.ApprovalRequest{
			ToolName: "shell",
			Command:  cmd,
			Reason:   "受限危险操作: " + bad,
			Args:     argsJSON,
		}}
	}
	return nil
}

// checkPathsInWorkDir 校验命令中的路径参数不越出工作目录。
// 启发式：取所有非 flag（不以 - 开头）且像路径的 token 逐一校验；
// 裸文件名视为工作目录内的相对路径（shell 在执行目录下运行）。
func (p *DefaultPolicy) checkPathsInWorkDir(cmd string) error {
	if p.FileAccess.WorkspaceRoot == "" {
		return nil
	}
	for _, tok := range strings.Fields(cmd) {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		if !isPathToken(tok) {
			continue
		}
		target := strings.Trim(tok, `"'`)
		if target == "" || strings.HasPrefix(target, "$") {
			continue
		}
		if err := withinDir(p.fileAccessPolicy(), target); err != nil {
			return err
		}
	}
	return nil
}

// isPathToken 粗略判断 token 是否像路径：以 ./、../、/ 开头，或含 / 且非纯数字（排除 chmod 模式位等）。
func isPathToken(tok string) bool {
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") || strings.HasPrefix(tok, "/") {
		return true
	}
	if strings.Contains(tok, "/") && !isNumeric(tok) {
		return true
	}
	return false
}

// absolutelyDangerous 无条件拒绝的绝对危险命令（无合理业务场景）。
func absolutelyDangerous(cmd string) string {
	// 子串型模式：写块设备、fork 炸弹
	for _, p := range []string{"> /dev/sd", ":(){"} {
		if strings.Contains(cmd, p) {
			return p
		}
	}
	// 首 token 匹配型
	first := firstWord(cmd)
	switch first {
	case "shutdown", "reboot", "halt", "poweroff", "dd":
		return first
	}
	if strings.HasPrefix(first, "mkfs") { // mkfs.ext4 等
		return first
	}
	return ""
}

// restrictedDangerous 受限危险命令：路径受限在 WorkDir 内 + 人工确认后可执行。
// 除 rm/sudo 等直接命令外，还覆盖"任意代码执行入口"（解释器 -c/-e 内嵌命令、find 删除语义），
// 防止模型绕过黑名单改用 python3 -c / bash -c / find -delete 达成删除等破坏性意图。
func restrictedDangerous(cmd string) string {
	if strings.Contains(cmd, "| sh") || strings.Contains(cmd, "| bash") {
		return "| sh / | bash"
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	// 首 token 型直接命令
	switch fields[0] {
	case "rm", "sudo", "chmod", "chown", "mv", "kill":
		return fields[0]
	}
	// 解释器执行入口：<bin> -c/-e <代码> 等价于 shell 内嵌任意命令
	for _, ie := range interpreterExecList {
		if fields[0] == ie.bin && len(fields) > 1 && fields[1] == ie.flag {
			return ie.bin + " " + ie.flag
		}
	}
	// find 的删除/执行语义：-delete、-exec
	if fields[0] == "find" && (strings.Contains(cmd, "-delete") || strings.Contains(cmd, "-exec")) {
		return "find -delete / -exec"
	}
	return ""
}

// interpreterExecList 任意代码执行入口清单（首词 + 代码参数 flag）。
var interpreterExecList = []struct{ bin, flag string }{
	{"bash", "-c"},
	{"sh", "-c"},
	{"python3", "-c"},
	{"python", "-c"},
	{"perl", "-e"},
	{"node", "-e"},
	{"ruby", "-e"},
	{"php", "-r"},
}

// firstWord 取命令首个空白分隔的词（命令名）。
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// matchPrefixes 判断命令是否以白名单中任意前缀开头。
func matchPrefixes(cmd string, prefixes []string) bool {
	for _, p := range prefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") || strings.HasPrefix(cmd, p+"\t") {
			return true
		}
	}
	return false
}
