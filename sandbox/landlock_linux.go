//go:build linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

// Linux 的路径强制后端：Landlock LSM（内核 5.13 起）。
//
// ## 语义对齐 macOS Seatbelt：**只收窄写**
//
// 读不强制。理由与 Seatbelt 后端一致：deny 读会碎掉动态链接器、locale、
// 系统证书与包缓存（`/etc`、`/usr`、Go module 缓存、.gitconfig……），
// 成本远超收益；而这个执行器的事故威胁模型全在**写**侧（模型改坏文件、
// 删数据、外泄代码到不该去的地方）。写只在声明的写根里允许，由**内核**
// 强制（不是我们自己在 Go 里判断路径）。
//
// Landlock 原生支持这种"只处理写类权限"的用法：`Config.HandledAccessFS`
// 是导出字段，可以精确指定被强制的权限集合；没被强制的权限（读、执行）
// 内核一律放行。
//
// ## 与 Seatbelt 的三处差异（诚实边界，不是 bug）
//
//  1. **根必须存在**。Landlock 的规则绑在**目录 inode** 上（path-beneath），
//     Seatbelt 绑在**路径前缀**上。声明时尚不存在的根，Seatbelt 会放行
//     （前缀匹配），Landlock 没有 inode 可绑 → 跳过该根（fail-closed：
//     写它会被拒）。
//  2. **refer**（跨目录重命名/链接）。内核 ≥5.19 才能处理；V2+ 我们把
//     refer 授予写根，工作区内部的重命名（`mv a ../b`、`git mv`）照常。
//     更老的内核（V1）上 refer 无法处理，跨目录重命名会被内核拒。
//  3. **truncate**（`> file` 截断写）。内核 ≥6.2 才能处理；V3+ 处理它，
//     更老的内核上截断写不在写范围里也做得成——已知缺口，写在这里，
//     不假装没有。
//
// ## 为什么用「重新执行自己」的启动器
//
// Landlock 的 landlock_restrict_self(2) **只对调用它的那个线程生效**，
// 而 Go 没有 fork 与 exec 之间的钩子（golang/go#68595 在讨论给
// syscall.SysProcAttr 加字段）。要给**子进程**单独套约束，只能让子进程
// 在 exec 之前自己套。
//
// 于是：父进程把命令与写根通过管道交给「重新执行出来的自己」，子进程在
// 初始化阶段套上 Landlock，再 exec 真正的命令（/bin/sh -c ...）。
// 约束由内核在 exec 时继承，子进程及其**所有后代**都在约束里。
//
// 已知窗口：子进程里**在 sandbox 包初始化之前**执行的宿主初始化代码不受
// 约束——那是宿主自己的可信代码，模型的命令还没开始跑。

const (
	// landlockLaunchEnv 是启动器标记：子进程靠它认出「我是被沙箱拉起来的那个」。
	landlockLaunchEnv = "MOS_SANDBOX_LANDLOCK_LAUNCH"
	// landlockLaunchFDEnv 是载荷管道在子进程里的 fd 号。
	landlockLaunchFDEnv = "MOS_SANDBOX_LANDLOCK_FD"
	// landlockPayloadMax 是载荷上限（管道缓冲区 64KB，留足余量）。
	landlockPayloadMax = 32 << 10
)

// landlockPayload 是父进程交给启动器的载荷。
type landlockPayload struct {
	// Argv 是要 exec 的命令行（/bin/sh -c ...）。
	Argv []string `json:"argv"`
	// WriteRoots 是允许写的根。
	WriteRoots []string `json:"write_roots"`
}

// wrapPathScope 用 Landlock 后端包装命令。
func wrapPathScope(eff *PathScope, base []string) (invocation, error) {
	if writeOpen(eff) {
		// 宿主显式声明写根含 "/"：不要路径强制（危险写在脸上）。
		return invocation{argv: base}, nil
	}
	if landlockABIVersion() < 1 {
		return invocation{}, fmt.Errorf("%w: 本机内核没有 Landlock（需要 Linux ≥ 5.13 且启用 CONFIG_SECURITY_LANDLOCK）：shell 工具未装配（fail-closed，不假装隔离）；确需放开请在宿主显式声明写根 \"/\"", ErrPathScopeUnsupported)
	}
	self, err := os.Executable()
	if err != nil {
		return invocation{}, fmt.Errorf("sandbox: 取宿主可执行文件路径失败（Landlock 启动器要重新执行自己）: %w", err)
	}
	payload, err := json.Marshal(landlockPayload{Argv: base, WriteRoots: eff.WriteRoots})
	if err != nil {
		return invocation{}, fmt.Errorf("sandbox: 编码 Landlock 载荷失败: %w", err)
	}
	if len(payload) > landlockPayloadMax {
		return invocation{}, fmt.Errorf("sandbox: Landlock 载荷过大（%d 字节，上限 %d）：写根太多", len(payload), landlockPayloadMax)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return invocation{}, fmt.Errorf("sandbox: 建 Landlock 载荷管道失败: %w", err)
	}
	if err := writeLandlockFrame(w, payload); err != nil {
		_ = r.Close()
		_ = w.Close()
		return invocation{}, fmt.Errorf("sandbox: 写 Landlock 载荷失败: %w", err)
	}
	// 载荷已进管道缓冲区：关掉写端，子进程按长度读，不必等 EOF。
	_ = w.Close()
	return invocation{
		argv:  []string{self},
		env:   []string{landlockLaunchEnv + "=1", landlockLaunchFDEnv + "=3"},
		files: []*os.File{r},
	}, nil
}

// landlockABIVersion 探测内核的 Landlock ABI 版本（0 = 不支持）。
//
// landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION) 返回
// ABI 版本号；ENOSYS/EOPNOTSUPP/EINVAL = 内核没有这个特性（没启用 LSM，
// 或内核太老）。
func landlockABIVersion() int {
	r, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(r)
}

// landlockWriteRights 是「写」这一族的访问权限：改内容、删、建（文件/
// 目录/符号链接/各类节点）。
//
// 不含读类权限（read_file/read_dir/execute）——**只收窄写**（见包注释）。
var landlockWriteRights = uint64(
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
)

// landlockSinkFiles 是命令必需的「文件型写目标」。
//
// /dev/null 是 shell 的通用丢弃口（`2>/dev/null`、`> /dev/null`）——
// 不放行它，一大批正常命令会直接失败（连 SDK 自己的 ulimit 前缀都跑不了）。
// 与 Seatbelt 后端的 `(allow file-write* (literal "/dev/null"))` 对齐。
//
// 注意：字面量文件只能用**文件类**权限（write_file/truncate）——MAKE_*/
// REMOVE_*/REFER 是目录权限，用在普通文件上内核会报 "inconsistent access
// rights"。所以它不能混进目录规则，必须单开一条规则。
var landlockSinkFiles = []string{"/dev/null"}

// landlockPlan 是一套「权限集 + 匹配的内核 ABI」组合。
type landlockPlan struct {
	cfg landlock.Config
	// dirRights 是授予写根（目录）的权限。
	dirRights uint64
	// fileRights 是授予字面量文件（如 /dev/null）的权限。
	fileRights uint64
}

// landlockPlans 按内核 ABI 从高到低给出候选方案。
//
// 高 ABI 多带两样东西：
//
//   - truncate（V3+，内核 6.2）：处理 `> file` 截断写；
//   - refer（V2+，内核 5.19）：处理跨目录重命名/链接，授予写根。
//
// 内核老到不支持某一档时，那一档的权限**不在被强制集合里**——这是
// Landlock 的语义（没被处理 = 不限制），也是上面「已知缺口」的来源。
func landlockPlans(abi int) []landlockPlan {
	const fileBase = unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	plans := make([]landlockPlan, 0, 3)
	if abi >= 3 {
		plans = append(plans, landlockPlan{
			cfg:        landlock.V3,
			dirRights:  landlockWriteRights | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_REFER,
			fileRights: fileBase | unix.LANDLOCK_ACCESS_FS_TRUNCATE,
		})
	}
	if abi >= 2 {
		plans = append(plans, landlockPlan{
			cfg:        landlock.V2,
			dirRights:  landlockWriteRights | unix.LANDLOCK_ACCESS_FS_REFER,
			fileRights: fileBase,
		})
	}
	plans = append(plans, landlockPlan{
		cfg:        landlock.V1,
		dirRights:  landlockWriteRights,
		fileRights: fileBase,
	})
	return plans
}

// applyLandlock 给**当前进程**（启动器子进程）套上 Landlock 约束。
//
// 之后的 exec 会把约束带给被执行的命令及其所有后代。
func applyLandlock(roots []string) error {
	abi := landlockABIVersion()
	if abi < 1 {
		return fmt.Errorf("本机内核没有 Landlock（需要 Linux ≥ 5.13 且启用 CONFIG_SECURITY_LANDLOCK）")
	}
	var lastErr error
	for _, plan := range landlockPlans(abi) {
		cfg := plan.cfg
		// 精确指定被强制的权限集：只有「写」这一族 + 当前 ABI 支持的
		// refer/truncate。读类权限不处理 → 内核放行（语义对齐 Seatbelt）。
		cfg.HandledAccessFS = landlock.AccessFSSet(plan.dirRights)
		// 所有规则必须进**同一个** ruleset：多个 ruleset 之间是「取交集」，
		// 分两次 RestrictPaths 会把上一轮的授权也收掉（实测：第二轮只授予
		// /dev/null，写根里的文件立刻变成不可写）。
		rules := []landlock.Rule{
			// 写根：目录类权限（MAKE_*/REMOVE_*/REFER/TRUNCATE）。
			// 不存在的根跳过（fail-closed：写它会被拒）。
			landlock.PathAccess(cfg.HandledAccessFS, roots...).IgnoreIfMissing(),
			// 字面量文件（/dev/null）：文件类权限，单开一条规则。
			landlock.PathAccess(landlock.AccessFSSet(plan.fileRights), landlockSinkFiles...).IgnoreIfMissing(),
		}
		if err := cfg.RestrictPaths(rules...); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("套 Landlock 失败: %w", lastErr)
}
