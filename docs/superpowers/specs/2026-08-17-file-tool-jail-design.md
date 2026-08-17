# 文件工具 Jail 设计（File-Tool Capability Seam）

- 日期：2026-08-17
- 状态：设计稿（brainstorming 产出，待实施计划）
- 关联：架构 v2 §3.5 SeamRegistry；M4 远程强隔离（M1.6 `tool.Sandbox` 升级到 Docker / containerd 后端）的先行；DSH `ctx.fs` + `ctx.sandboxPolicy` + `ctx.shell.sandboxMode` Capability Seam 借鉴

## 1. 背景与动机

MoS 现状：`tool/builtin/file.go`（`ReadFile` / `WriteFile`）与 `find.go`（`FindFiles` / `Grep`）的 `Execute` 方法**完全不做路径校验**，直接 `os.Open(p.Path)` 与 `filepath.WalkDir(root, ...)`。模型只要拼出 `../../etc/passwd`、`/etc/shadow` 或绝对路径外逃逸，就能读写整个文件系统。Shell 工具已通过 `tool.Sandbox` + `DefaultPolicy.checkFile` 双层校验获得 WorkDir jail，文件工具是唯一未受 sandbox 约束的 IO 类工具，是当前最大的安全缺口。

DSH / PI 对照（来自源码调研）：

| 项目 | cwd 用途 | 路径 jail | policy 层 |
| --- | --- | --- | --- |
| **DSH** | `exec.agent.session.header.cwd`（per session） | ✅ 三层 seam：`ctx.fs` + `ctx.sandboxPolicy` + `ctx.shell.sandboxMode`；containment 走 lexical + `realpath(dev/ino)`；`writableRoots(policy)` 与 bash Seatbelt 共根 | `sandboxMode` ∈ {`read-only`, `workspace-write`, `danger-full-access`}；失败带 `FS_SANDBOX_DENIED` + 同轮 `sandbox_permissions` 升级提示 |
| **PI** | `AgentSessionConfig.cwd` | ❌ cwd 只是 `path.resolve` base；零 containment | ❌ 仅工具名 allowlist |

结论修正：准确的对照基准是 **DSH 的 Capability Seam 三件套**，PI 不做路径 jail。MoS 此能力**对齐 DSH**，并超出 PI。

## 2. 核心决策

| 维度 | 决策 | 理由 |
| --- | --- | --- |
| 动机 | 对齐 DSH Capability Seam 一等公民 | MoS 当前缺此能力；与现有 `tool.Sandbox` 平级 |
| 范围 | 新建 `tool/fs` 包 + 新增 SeamRegistry `Kind` | 与 DSH 三 seam 同形；M4 远程沙箱换实现无 API 改动 |
| WorkDir 来源 | `ctx.fs` 优先 → `DefaultPolicy.FileAccess.WorkspaceRoot` fallback → **未显式配置 FileAccess（即零值 `JailReads=false, JailWrites=false`）= 保持开放**（向后兼容，老用户无感） | 与现有 `SandboxFrom` ctx 注入模式一致；policy fallback 与 shell policy 共根；零值=开放避免破坏现有使用方 |
| 路径语义 | **严格模式**：Clean+Abs+realpath 后必须以 workspaceRoot 为前缀 | DSH lexical + 实体双层校验，避免 symlink TOCTOU |
| 读 / 写 | **DSH 不等价拆分**：read unconfined，write fenced；通过 `FileAccess{JailReads, JailWrites}` 双 bool 暴露 | 单机本地默认 `JailWrites=true, JailReads=false`（DSH 一致）；SaaS 默认 `JailReads=true, JailWrites=true`（多租户隔离）；platform 装配 preset |
| 共享根 | `writableRoots(policy) []string` = `[workspaceRoot, "/tmp", os.TempDir()]`；fs sandbox 与 bash Seatbelt 等价层共用 | 对齐 DSH 防两子系统配置漂移 |
| 默认部署 preset | `LocalJailed`（单机）+ `SaaSJailed`（云）；用户/平台可显式覆盖 | 单机用户无感切换（仅写 jail），SaaS 全 jail |
| 错误格式 | 结构化 `*tool/fs.PathDeniedError`（含 target、mode、allowed roots），文案复用 `路径 %q 超出工作目录 %q` 风格 | 与现有 `policy_file` 一致；结构化字段供上层 wrap 与审计日志 |

## 3. 组件设计

### 3.1 新包 `tool/fs`（架构 v2 §3.5 新增 Kind）

```go
// tool/fs/file_system.go —— DSH ctx.fs 的 MoS 映射。
package fs

import (
    "context"
    "os"
)

// Mode 描述 fs 访问模式（DSH sandboxMode 镜像）。
type Mode string

const (
    ModePermissive    Mode = "permissive"     // 不 jail（仅诊断模式使用，平台层一般不开）
    ModeLocalJailed   Mode = "local-jailed"   // 单机：写 jail，读开放
    ModeSaaSJailed    Mode = "saas-jailed"    // SaaS：读写都 jail
    ModeDangerFull    Mode = "danger-full"    // 不推荐；调试/迁移专用，需显式开启
)

// FileSystem 是 fs 能力 seam 的抽象（DSH FileSystem 对位）。
type FileSystem interface {
    // Resolve 把 model 给的 path 解析为最终访问的目标路径，
    // 同时按当前 policy 做 jail 校验；越界返回 *PathDeniedError。
    // mode 决定 read/write 不同严格度。
    Resolve(ctx context.Context, p Request) (*Resolved, error)

    // Mode 返回当前模式（用于审计/UI 展示）。
    Mode() Mode
}

// Request 描述一次 fs 访问请求。
type Request struct {
    Path   string // 模型传入的路径（可能含 .. 或绝对路径）
    Op     Op     // OpRead / OpWrite / OpList / OpGrep
    Session SessionRef
}

// Resolved 是解析后的结果：DisplayPath（用户面）+ TargetKey（实体 inode 路径）。
type Resolved struct {
    DisplayPath string // 模型可见的路径
    TargetKey   string // realpath 后的 inode 路径（用于审计 / 弱别名比对）
    AllowedRoots []string // 该请求允许的可写根（仅 Op=OpWrite 时非空）
}

// PathDeniedError 结构化错误。
type PathDeniedError struct {
    Target    string   // 解析后的绝对路径
    Display   string   // 用户传入的原始路径
    Mode      Mode
    Op        Op
    Workspace string   // workspaceRoot
    AllowedRoots []string
}

func (e *PathDeniedError) Error() string {
    return fmt.Sprintf("路径 %q 在 %s 模式下不允许 %s（workspace=%q, allowed=%v）",
        e.Display, e.Mode, e.Op, e.Workspace, e.AllowedRoots)
}
```

### 3.2 两个内置实现（与 DSH `LocalFileSystem` / `SandboxedFileSystem` 对位）

```go
// tool/fs/local.go —— 透传模式（Op 不做 jail；用于 ModePermissive / ModeDangerFull）。
type Local struct{ Mode Mode /* 默认 ModePermissive */ }

// tool/fs/sandboxed.go —— 沙箱模式（按 Mode 应用 JailReads/JailWrites）；
// 与 tool.Sandbox 并存而非替换（Sandbox 仍管 shell 执行；fs 只管文件工具）。
type Sandboxed struct {
    Policy *FileAccessPolicy // 包含 workspaceRoot + JailReads + JailWrites
}

// Resolve 是关键入口，DSH fsio.ts:146-194 + containment.ts:23-76 的 Go 翻译：
//   1) Lexical 阶段：filepath.Clean + filepath.Abs + filepath.Rel 校验前缀
//   2) Realpath 阶段：os.Stat 同 dev+ino 比对兜底（防 symlink TOCTOU）
//   3) OpRead 走 Lexical 即返回（DSH read 透传，不做实体比对）
//   4) OpWrite 强制 realpath，且只能落在 writableRoots(policy) 内
//   5) 越界返回 *PathDeniedError
```

### 3.3 `FileAccessPolicy` 与 `DefaultPolicy` 集成（assistant 包）

```go
// assistant/config.go —— 新增字段。
type Config struct {
    // ... 既有字段
    FileAccess FileAccessConfig // 文件访问策略（M2.x）
}

type FileAccessConfig struct {
    WorkspaceRoot string // 缺省 = os.Getwd()（保持向后兼容）
    JailReads     bool   // 缺省 false（DSH 单机默认）
    JailWrites    bool   // 缺省 true（DSH 单机默认）
    // WritableExtra []string  // 可选：附加可写根（默认 ["/tmp", os.TempDir()] 已包含）
}

// assistant/policy_file.go —— 升级：基于 FileAccessConfig 推导 Mode 并注册 fs seam。
func (p *DefaultPolicy) Mode() fs.Mode {
    if !p.FileAccess.JailReads && !p.FileAccess.JailWrites {
        return fs.ModePermissive
    }
    if p.FileAccess.JailReads && p.FileAccess.JailWrites {
        return fs.ModeSaaSJailed
    }
    return fs.ModeLocalJailed
}

// assistant.New 自动按 Config 装配 fs seam（注册到 RunContext.FS）：
//   reg.Register(seam.KindFileSystem, "default", func() any { return fs.NewSandboxed(p.Mode(), ...) })
```

### 3.4 共享 `writableRoots(policy)` 与 shell policy 对齐

```go
// tool/fs/roots.go —— DSH writableRoots() 的 Go 翻译。
// 返回当前请求允许的可写根（去重、规范化为绝对路径）。
func WritableRoots(p *FileAccessPolicy) []string {
    roots := []string{p.WorkspaceRoot}
    if !p.JailWrites {
        return roots // ModePermissive：只允许 workspace（DSH read-only 行为）
    }
    roots = append(roots, "/tmp", os.TempDir())
    if len(p.WritableExtra) > 0 {
        roots = append(roots, p.WritableExtra...)
    }
    return dedupAbs(roots)
}
```

`assistant/policy_shell.go` 同步：受限危险命令（`rm / sudo / chmod / mv / kill / find -delete`）的 `withinDir` 检查改用 `WritableRoots(p)` 而非 `p.WorkDir`，与 fs sandbox 共根，杜绝"shell 配置与 fs 配置漂移"。

### 3.5 4 个内置工具切换到 `ctx.fs`

```go
// tool/builtin/file.go（ReadFile / WriteFile）
func (ReadFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
    p, _ := tool.Parse[readFileParams](args)
    fsys := fs.FromContext(ctx) // 未注入时 nil → fallback 到 Local{ModePermissive}（向后兼容）
    if fsys == nil {
        fsys = &fs.Local{Mode: fs.ModePermissive}
    }
    r, err := fsys.Resolve(ctx, fs.Request{Path: p.Path, Op: fs.OpRead, Session: sessionOf(ctx)})
    if err != nil {
        return nil, err // *PathDeniedError 直接冒泡；其他错误 wrap
    }
    f, err := os.Open(r.TargetKey)
    // ...
}
```

`FindFiles` / `Grep` 同步改造：`Root` 字段为空时用 `WorkspaceRoot`（DSH `workdir = cwd ?? process.cwd()` 的 Go 翻译），且 `WalkDir` 起点经 `Resolve(root, OpList)` jail 校验。

### 3.6 SessionRef 与 ctx 注入

```go
// tool/fs/context.go —— ctx 注入，与 WithSandbox 同形。
func WithFileSystem(ctx context.Context, fsys FileSystem) context.Context
func FromContext(ctx context.Context) FileSystem // 未注入返回 nil
```

`assistant.New` 装配时：若 Config 显式 `FileAccess != zero`，则 `RunContext.FS = fs.NewSandboxed(...)`；否则 nil（保持向后兼容：4 个工具 fallback 到 `Local{ModePermissive}`）。

## 4. 数据流

```
模型 ToolCall
   │
   ▼
agent/driver react.go → executor.Run → tool.Execute(ctx, args)
   │                                          │
   │                                  ┌───────┴───────┐
   │                                  │ ReadFile 等   │
   │                                  │ Execute 入口  │
   │                                  └───────┬───────┘
   │                                          │
   │                                  fs.FromContext(ctx)
   │                                          │
   │            ┌─────────────────────────────┼─────────────────────┐
   │            ▼                             ▼                     ▼
   │   ctx 未注入 FS → fallback        ctx 注入 Sandbox         ctx 注入 Local
   │   Local{ModePermissive}            Sandboxed{Policy}       (平台显式)
   │   （向后兼容：当前用户无感）         (JailReads/JailWrites)
   │                                          │
   │                                  Resolve(path, op)
   │                                          │
   │                          ┌───────────────┼───────────────┐
   │                          ▼               ▼               ▼
   │                    Lexical 校验     Realpath 实体      越界
   │                    (filepath.       (os.Stat 同         → *PathDeniedError
   │                     Abs+Rel)        dev+ino 比对)
   │                          │
   │                          ▼
   │                    Op=Write 校验是否落在 writableRoots(p) 内
   │                          │
   │                          ▼
   │                    返回 *Resolved{DisplayPath, TargetKey, AllowedRoots}
   │                          │
   │                          ▼
   │                    os.Open(r.TargetKey) / WalkDir
   │                          │
   │                          ▼
   │                    policyGuard / DoomLoopGuard / ToolExecute hook
   │                          │
   │                          ▼
   │                    Outcome → 回到 executor
```

## 5. 错误处理

| 错误源 | 错误类型 | 文案 | 上层处理 |
| --- | --- | --- | --- |
| 路径越界 | `*fs.PathDeniedError` | `路径 %q 在 %s 模式下不允许 %s（workspace=%q, allowed=%v）` | 直接返回；模型看到完整 allowed 列表可重试或申请升级 |
| Lexical 通过、realpath 失败 | `*fs.PathDeniedError` (Op=ResolveError) | 同上，target 填入 lex 解析结果 | 同上 |
| `/tmp` / tmpdir 不存在 | 启动时 `os.MkdirAll` 兜底；运行期不可达 | — | — |
| FS seam 未注册 | `seam: filesystem provider not registered` | 仅发生于显式按名解析；默认装配已注册 "default" | assistant.New 自动注入，无需处理 |

**审计事件**：新增 `fs/resolve` 事件（与 dsh `fs/read-intent` / `fs/write-intent` 对齐），载荷 `{ target, op, mode, decision, session_id }`，让 Session 日志可回放每次路径校验决策。

## 6. 影响面

| 既有代码 | 改动 |
| --- | --- |
| `tool/builtin/file.go`（ReadFile/WriteFile） | Execute 入口前 `fs.Resolve(...)`；Offset/Limit 等逻辑不变 |
| `tool/builtin/find.go`（FindFiles/Grep） | Root 字段空 → WorkspaceRoot；WalkDir 前 `fs.Resolve(root, OpList)` |
| `assistant/config.go` | 新增 `FileAccessConfig` 字段 |
| `assistant/policy_file.go` | 新增 `Mode()` / `WritableRoots()` 方法；现有 `withinDir` 不再直接调用，迁移到 fs |
| `assistant/policy_shell.go` | 受限危险命令的 `withinDir` 改用 `WritableRoots(p)` |
| `agent/seam/seam.go` | 新增 `KindFileSystem = "filesystem"` 常量 |
| `assistant.New` | 装配时注册 fs seam + 注入 RunContext.FS |

**纯增量**：新包 `tool/fs` 不影响任何既有调用路径（fallback `Local{ModePermissive}` 保持原行为）。向后兼容保证：未配置 `FileAccess` 时，`FileAccessConfig{}` 零值 → `JailReads=false, JailWrites=false` → `ModePermissive`；当前 4 个工具无 ctx 注入时 fallback `Local{ModePermissive}` 仍开放，老用户无感。

**回归保护**：
- 现有 `assistant/assistant_test.go` 全量通过（fallback 路径不变）
- 新增 `tool/fs` 包内 contract test（与 `agent/telemetry` 的 `Conformance` 同形）
- shell policy 单元测试改用 `WritableRoots` 后应等价（同一断言集合）

## 7. 测试策略

| 层 | 内容 |
| --- | --- |
| `tool/fs/resolve_test.go` | Lexical 校验全分支（`..`、绝对路径、相对路径、空）；realpath 实体比对（mock fs.MakeSyscallChanges）；Op=Read 不需 realpath；Op=Write 必 realpath |
| `tool/fs/sandboxed_test.go` | ModeLocalJailed：读开放、写 jail；ModeSaaSJailed：读写都 jail；ModePermissive：全开放 |
| `tool/fs/roots_test.go` | `WritableRoots`：去重、规范化、缺省值、追加 WritableExtra |
| `tool/builtin/file_jail_test.go` | 4 工具接到 ctx 注入的 Sandboxed 后越界返回 `*PathDeniedError`；未注入时维持原行为 |
| `assistant/policy_shell_test.go` | `rm` 等受限命令校验用 `WritableRoots`；与原 `withinDir` 等价（同一断言集合） |
| `assistant/assistant_test.go` | 回归：`FileAccess{}` 零值 → ModeLocalJailed + 无 ctx 注入 → Local 透传；既有测试无修改通过 |
| 集成 | 真模型跑"读 /etc/passwd"、"写 /tmp/x"两条路径，确认 fail-closed 与升级提示出现在同一 Turn |

## 8. 范围外（v1 明确不做）

- **远程沙箱后端**（Docker / containerd）：属 M4；`SandboxedFS` 接口已为此预留（`fs.Sandboxed` 可换实现）
- **`fs-observation-policy`**（DSH `WeakMap<session, target, observation>` 强制 read-before-edit）：与 Auditor 重叠多，本期不做；下一轮审视是否独立
- **`/proc` / `/sys` 等敏感路径的读黑名单**：DSH 也只到 `writableRoots` 级别；read 不 jailed 时模型能读 `/proc/self/cmdline` 等，本期不增加 read 黑名单
- **per-call approval**（write 超 jail 后走 `approval.RequiredError` 申请升级）：M2.8 Confirmer 已支持，可由平台层在 fs seam 上挂 `approval` 回调；本期先在结构化错误里返回 `allowedRoots` 供模型重试，升级流程留待 M4 远程沙箱一并做
- **Win32 / 8.3 文件名兜底**：DSH 提供了 dev/ino 比对，Go `os.SameFile` 已跨平台覆盖到 ino；Windows 强别名场景（`PROGRA~1`）本期不做专门测试

## 9. 升级路径（M4）

M4 远程沙箱落地时，`SandboxedFS` 可换为 `DockerFS`（挂载 workspaceRoot 为可写，其他写路径通过 Docker 卷暴露 `/tmp` / tmpdir），`writableRoots` 与 shell sandbox 保持单一来源。4 个工具与 policy 调用点零改动。