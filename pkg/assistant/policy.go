package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"hegq.net/mos/pkg/llm"
)

// DefaultPolicy 是默认的工具调用安全策略（Harness Engineering）。
// 设计要点：
//   - 只读/自证工具（calculator、current_time、read_file、find_files、grep、web_fetch、web_search）显式放行；
//     文件类工具限制路径不得越出 WorkDir。
//   - shell 命令分级防御：
//     ① 绝对危险命令（shutdown/mkfs/dd 等）：无条件拒绝，不提供确认放行；
//     ② 受限危险命令（rm/sudo/chmod/mv/kill/管道 shell）：路径受限在 WorkDir 内，
//        且须经 Confirmer 人工确认后才可执行；
//     ③ 普通命令：可配置命令前缀白名单（ShellAllowed）。
//   - 未知工具默认拒绝（fail-closed）：新增工具须在 Allow 中显式声明放行。
type DefaultPolicy struct {
	ShellAllowed []string  // 命令前缀白名单；空表示仅基础防御
	WorkDir      string    // 文件/受限危险命令的操作根目录；空表示受限危险命令一律拒绝
	Confirm      Confirmer // 危险操作确认器；nil 表示无法确认，受限危险命令拒绝
}

// NewDefaultPolicy 创建默认安全策略。
func NewDefaultPolicy(shellAllowed []string, workDir string) *DefaultPolicy {
	return &DefaultPolicy{ShellAllowed: shellAllowed, WorkDir: workDir}
}

// Allow 实现 agent.Policy。
func (p *DefaultPolicy) Allow(ctx context.Context, call *llm.ToolCall) error {
	switch call.Name {
	case "shell":
		return p.checkShell(ctx, call.Args)
	case "read_file":
		return p.checkFile(call.Args, "path")
	case "write_file":
		if err := p.checkFile(call.Args, "path"); err != nil {
			return err
		}
		// 路径已限制在 WorkDir 内，破坏面可控；配置了确认器时写入前再人工确认一次。
		return p.confirmWrite(ctx, call.Args)
	case "find_files", "grep":
		return p.checkFile(call.Args, "root")
	case "web_fetch":
		return p.checkWebFetch(call.Args)
	case "web_search":
		return p.checkWebSearch(call.Args)
	case "calculator", "current_time":
		return nil // 纯计算/只读自证，无副作用
	default:
		// fail-closed：不在放行名单的未知工具默认拒绝
		return fmt.Errorf("未知工具 %q 不在安全策略放行名单，已拒绝", call.Name)
	}
}

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

	// ③ 受限危险命令：路径受限在 WorkDir 内 + 人工确认
	if bad := restrictedDangerous(cmd); bad != "" {
		if p.WorkDir == "" {
			return fmt.Errorf("命令包含受限危险操作 %q，但未配置工作目录限定范围，已拒绝", bad)
		}
		if err := p.checkPathsInWorkDir(cmd); err != nil {
			return fmt.Errorf("命令 %q 的路径超出工作目录: %w", cmd, err)
		}
		if p.Confirm == nil {
			return fmt.Errorf("命令包含受限危险操作 %q，需人工确认但未配置确认器，已拒绝", bad)
		}
		ok, err := p.Confirm.Confirm(ctx, ConfirmRequest{
			ToolName: "shell",
			Command:  cmd,
			Reason:   "受限危险操作: " + bad,
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("用户拒绝执行命令 %q", cmd)
		}
	}
	return nil
}

// confirmWrite 写入前的人工确认（配置了 Confirmer 时触发；否则范围受限放行）。
func (p *DefaultPolicy) confirmWrite(ctx context.Context, argsJSON string) error {
	if p.Confirm == nil {
		return nil
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || args.Path == "" {
		return nil
	}
	ok, err := p.Confirm.Confirm(ctx, ConfirmRequest{
		ToolName: "write_file",
		Command:  args.Path,
		Reason:   "写入文件（已限制在工作目录内，执行前确认）",
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("用户拒绝写入文件 %q", args.Path)
	}
	return nil
}

// checkWebFetch 校验抓取协议：仅 http/https。
func (p *DefaultPolicy) checkWebFetch(argsJSON string) error {
	var args struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) == nil && args.URL != "" {
		if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
			return fmt.Errorf("仅允许抓取 http/https 地址")
		}
	}
	return nil
}

// checkWebSearch 校验搜索参数（fail-closed 下显式声明放行 + 参数约束）。
func (p *DefaultPolicy) checkWebSearch(argsJSON string) error {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析搜索参数: %w", err)
	}
	q := strings.TrimSpace(args.Query)
	if q == "" {
		return fmt.Errorf("搜索关键词不能为空")
	}
	if len([]rune(q)) > 500 {
		return fmt.Errorf("搜索关键词过长")
	}
	return nil
}

// checkFile 校验文件路径不超出工作目录。
func (p *DefaultPolicy) checkFile(argsJSON, field string) error {
	if p.WorkDir == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析参数: %w", err)
	}
	raw, ok := args[field].(string)
	if !ok || raw == "" {
		return nil
	}
	return withinDir(p.WorkDir, raw)
}

// checkPathsInWorkDir 校验命令中的路径参数不越出工作目录。
// 启发式：取所有非 flag（不以 - 开头）且像路径的 token 逐一校验；
// 裸文件名视为工作目录内的相对路径（shell 在执行目录下运行）。
func (p *DefaultPolicy) checkPathsInWorkDir(cmd string) error {
	if p.WorkDir == "" {
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
		if err := withinDir(p.WorkDir, target); err != nil {
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

// withinDir 判断 target 路径是否位于 root 目录内。
func withinDir(root, target string) error {
	r, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	t := target
	if !filepath.IsAbs(t) {
		t = filepath.Join(r, t)
	}
	t, err = filepath.Abs(t)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, t)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 超出工作目录 %q", target, r)
	}
	return nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
