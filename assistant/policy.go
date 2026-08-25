package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool/fs"
)

// Checker 是单个工具的策略检查器：返回 error 表示拒绝该次调用。
// 工具名 → Checker 的注册表替代硬编码 switch：新增工具策略 = 注册一行，
// 平台自定义工具可通过 DefaultPolicy.Register 注入策略（扩展点，契合 v2 守卫可组合）。
type Checker func(ctx context.Context, args string) error

// DefaultPolicy 是默认的工具调用安全策略（Harness Engineering）。
// 设计要点：
//   - 只读/自证工具（current_time、read_file、find_files、grep、web_fetch、web_search）显式放行；
//     文件类工具限制路径不得越出 WorkDir。
//   - shell 命令分级防御：
//     ① 绝对危险命令（shutdown/mkfs/dd 等）：无条件拒绝，不提供确认放行；
//     ② 受限危险命令（rm/sudo/chmod/mv/kill/管道 shell）：路径受限在 WorkDir 内，
//     且须经 Approval 人工确认后才可执行；
//     ③ 普通命令：可配置命令前缀白名单（ShellAllowed）。
//   - 未知工具默认拒绝（fail-closed）：未注册检查器的工具一律拒绝。
//   - 人工确认改为异步 Approval（M2.8）：检查器对需确认的操作返回 *approval.RequiredError，
//     由驱动层 policyGuard 发起 Approval.Request（挂起 / 立即决策）；未配置 Approval 时
//     fail-closed（shell 受限拒绝，write_file 路径受限放行——保持旧版行为）。
type DefaultPolicy struct {
	ShellAllowed []string // 命令前缀白名单；空表示仅基础防御
	WorkDir      string   // 文件/受限危险命令的操作根目录；空表示受限危险命令一律拒绝
	// Approval 异步审批通道（M2.8）：检查器返回 *approval.RequiredError 后由驱动处理；
	// nil 表示无法确认，受限危险命令 fail-closed 拒绝。旧 Confirmer 经
	// approval.NewConfirmerAdapter 桥接为 Approval。
	Approval approval.Approval
	// FileAccess 文件工具访问策略（M3.x — file-tool-jail plan）：
	// 由 Mode() 推导 fs.Mode（local-jailed / saas-jailed / danger-full），
	// 供 fs seam 装配层（Task 14）选 Sandboxed/Local 实现。
	FileAccess FileAccessConfig

	mu       sync.RWMutex
	checkers map[string]Checker // 工具名 → 检查器；未注册默认拒绝（fail-closed）
}

// Mode 推导自 FileAccessConfig（与 tool/fs.Sandboxed.Mode 同形映射）。
// 对位 DSH sandboxPolicy：读写都 jail → SaaS 级 read-only；只 jail 一侧 → workspace-write。
// 零值 FileAccessConfig{}（JailReads=false, JailWrites=false）→ ModeDangerFull（显式全开放）。
func (p *DefaultPolicy) Mode() fs.Mode {
	switch {
	case p.FileAccess.JailReads && p.FileAccess.JailWrites:
		return fs.ModeSaaSJailed
	case p.FileAccess.JailReads || p.FileAccess.JailWrites:
		return fs.ModeLocalJailed
	default:
		return fs.ModeDangerFull
	}
}

// NewDefaultPolicy 创建默认安全策略（注册内置工具检查器）。
// workDir 同时作为 FileAccess.WorkspaceRoot 镜像（policy_file / checkFile 共用同一根）。
func NewDefaultPolicy(shellAllowed []string, workDir string) *DefaultPolicy {
	p := &DefaultPolicy{
		ShellAllowed: shellAllowed,
		WorkDir:      workDir,
		FileAccess:   FileAccessConfig{WorkspaceRoot: workDir, JailReads: workDir != "", JailWrites: workDir != ""},
	}
	p.checkers = map[string]Checker{
		"shell":               p.checkShell,
		"read_file":           p.checkFileField("path"),
		"write_file":          p.checkWrite,
		"find_files":          p.checkFileField("root"),
		"grep":                p.checkFileField("root"),
		"web_fetch":           p.checkWebFetch,
		"web_search":          p.checkWebSearch,
		"current_time":        allow,
		"request_human_input": allow, // 自证/只读：无副作用，人工介入工具放行
	}
	return p
}

// Allow 实现 driver.Policy：按工具名查表分发；未注册工具默认拒绝（fail-closed）。
func (p *DefaultPolicy) Allow(ctx context.Context, call *llm.ToolCall) error {
	p.mu.RLock()
	chk, ok := p.checkers[call.Name]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("未知工具 %q 不在安全策略放行名单，已拒绝", call.Name)
	}
	return chk(ctx, call.Args)
}

// Register 为自定义工具注册策略检查器（平台扩展点）。
// 未注册的工具保持 fail-closed；已注册名称返回错误。须在装配期（首次 Run 前）调用。
func (p *DefaultPolicy) Register(name string, chk Checker) error {
	if name == "" || chk == nil {
		return fmt.Errorf("policy: 注册检查器需要非空工具名与检查器")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.checkers[name]; exists {
		return fmt.Errorf("policy: 工具 %q 已注册策略检查器", name)
	}
	p.checkers[name] = chk
	return nil
}

// allow 是只读/自证工具的放行检查器（纯计算、无副作用）。
func allow(context.Context, string) error { return nil }

// checkWrite 是 write_file 的组合检查器：路径校验（限定 WorkDir 内）+ 写入前人工确认。
func (p *DefaultPolicy) checkWrite(ctx context.Context, argsJSON string) error {
	if err := p.checkFile(argsJSON, "path"); err != nil {
		return err
	}
	// 路径已限制在 WorkDir 内，破坏面可控；配置了 Approval 时写入前再人工确认一次。
	// 未配置 Approval 时路径受限放行（保持旧版无确认器行为）。
	if p.Approval == nil {
		return nil
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || args.Path == "" {
		return nil
	}
	return &approval.RequiredError{Request: approval.ApprovalRequest{
		ToolName: "write_file",
		Command:  args.Path,
		Reason:   "写入文件（已限制在工作目录内，执行前确认）",
		Args:     argsJSON,
	}}
}

// checkFileField 返回按指定参数字段做路径校验的检查器（read_file: path / find_files|grep: root）。
func (p *DefaultPolicy) checkFileField(field string) Checker {
	return func(_ context.Context, argsJSON string) error {
		return p.checkFile(argsJSON, field)
	}
}

// checkWebFetch 校验抓取协议：仅 http/https。
func (p *DefaultPolicy) checkWebFetch(_ context.Context, argsJSON string) error {
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
func (p *DefaultPolicy) checkWebSearch(_ context.Context, argsJSON string) error {
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
