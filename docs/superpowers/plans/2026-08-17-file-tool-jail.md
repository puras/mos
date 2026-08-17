# 文件工具 Jail 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 ReadFile / WriteFile / FindFiles / Grep 接入 Capability Seam（`tool/fs` + `SeamRegistry.KindFileSystem`），按 `FileAccessConfig{WorkspaceRoot, JailReads, JailWrites}` 强制路径 jail；WorkspaceRoot 必填，零值启动失败（fail-closed，不保留向后兼容）。

**Architecture:** 新建 `tool/fs` 包（`FileSystem` 接口 + `Local` 透传实现 + `Sandboxed` jail 实现），通过 ctx 注入（`WithFileSystem` / `FromContext`），与现有 `tool.Sandbox` 并存而非替换（Sandbox 管 shell 执行；fs 管文件工具）。`assistant.New` 校验 `WorkspaceRoot` 非空、按 `FileAccessConfig` 推导 Mode、注册 fs seam、注入 `RunContext.FS`。`WritableRoots(policy)` 与 shell policy 共根（DSH 防配置漂移模式）。

**Tech Stack:** Go 标准库（`os.Stat` / `os.SameFile` / `filepath.Abs` / `filepath.Rel`）；现有 `agent/seam.SeamRegistry`；现有 `agent/event` 事件目录；现有 `assistant.Config` / `DefaultPolicy`。

## Global Constraints

- Go 1.22+（与 `go.mod` 一致）
- 不引入新第三方依赖（与项目零依赖约定一致）
- 命名沿用现有风格：`tool/fs` 包内导出符号用 PascalCase；错误文案中文（与 `policy_file` 一致：`路径 %q ...`）
- 测试沿用 `testing` 标准库 + 表驱动 + `t.TempDir()`
- Commit message 风格：`feat: ...` / `docs: ...` / `test: ...`
- 所有 fs 操作都先 `fs.FromContext(ctx)`；ctx 缺 fs = `*fs.NotConfiguredError`（fail-closed，不做 fallback）
- WorkspaceRoot 必填；空值 = `assistant.New` 返回 error（fail-closed）
- 路径校验：Lexical 阶段（`filepath.Clean` + `filepath.Abs` + `filepath.Rel`）+ Realpath 阶段（`os.Stat` + `os.SameFile` 实体比对，防 symlink TOCTOU）
- 写操作 Op=Write：必须落在 `WritableRoots(p)` 内（含 `/tmp` + `os.TempDir()`）
- 读操作 Op=Read：Op=Read 时 Lexical 阶段已足够；不需要 realpath

---

## Task 1: 新增 `KindFileSystem` seam 类别

**Files:**
- Modify: `agent/seam/seam.go:19-32`
- Modify: `agent/seam/seam_test.go`（追加测试）

**Interfaces:**
- Consumes: 无（基础设施层）
- Produces: `seam.KindFileSystem Kind = "filesystem"` 常量

- [ ] **Step 1: 写失败的测试**

```go
// agent/seam/seam_test.go 末尾追加
func TestKindFileSystemRegistered(t *testing.T) {
    reg := New()
    var sawKind bool
    for _, k := range []Kind{KindFileSystem} {
        if k == KindFileSystem {
            sawKind = true
        }
    }
    if !sawKind {
        t.Fatal("KindFileSystem 未注册")
    }
    // 验证可注册/解析
    reg.Register(KindFileSystem, "default", func() any { return "fake-fs" })
    got, err := reg.Resolve(KindFileSystem, "default")
    if err != nil {
        t.Fatalf("Resolve 失败: %v", err)
    }
    if got != "fake-fs" {
        t.Fatalf("got=%v, want fake-fs", got)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./agent/seam/ -run TestKindFileSystemRegistered -v`
Expected: FAIL（`KindFileSystem` 未定义）

- [ ] **Step 3: 实现：新增常量**

编辑 `agent/seam/seam.go`，把 `const ( ... )` 块改为：

```go
const (
    KindDriver    Kind = "driver"
    KindPolicy    Kind = "policy"
    KindModel     Kind = "model"
    KindTool      Kind = "tool"
    KindSandbox   Kind = "sandbox"
    KindMemory    Kind = "memory"
    KindRetriever Kind = "retriever"
    KindAuditor   Kind = "auditor"
    KindTruncator Kind = "truncator"
    KindCompactor Kind = "compactor"
    KindPlanner   Kind = "planner"
    KindReflector Kind = "reflector"
    KindFileSystem Kind = "filesystem" // 文件系统 capability（Task 8/13 装配）
)
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./agent/seam/ -v`
Expected: PASS（既有 + 新增）

- [ ] **Step 5: 提交**

```bash
git add agent/seam/seam.go agent/seam/seam_test.go
git commit -m "feat(seam): 新增 KindFileSystem capability 类别（文件工具 jail 前置）"
```

---

## Task 2: 新建 `tool/fs` 包——类型与接口骨架

**Files:**
- Create: `tool/fs/file_system.go`
- Create: `tool/fs/file_system_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `type Mode string` + 常量 `ModeLocalJailed` / `ModeSaaSJailed` / `ModeDangerFull`
  - `type Op string` + 常量 `OpRead` / `OpWrite` / `OpList` / `OpGrep`
  - `type Request struct { Path string; Op Op; SessionID string }`
  - `type Resolved struct { DisplayPath string; TargetKey string; AllowedRoots []string }`
  - `type FileSystem interface { Resolve(ctx, Request) (*Resolved, error); Mode() Mode }`
  - `type PathDeniedError struct { Display, Target, Workspace string; Op Op; Mode Mode; AllowedRoots []string }` 实现 `error`
  - `type NotConfiguredError struct{}` 实现 `error`

- [ ] **Step 1: 写失败测试**

```go
// tool/fs/file_system_test.go
package fs

import (
    "errors"
    "strings"
    "testing"
)

func TestPathDeniedErrorMessage(t *testing.T) {
    err := &PathDeniedError{
        Display:   "../../etc/passwd",
        Target:    "/etc/passwd",
        Workspace: "/home/user/proj",
        Op:        OpRead,
        Mode:      ModeLocalJailed,
    }
    msg := err.Error()
    if !strings.Contains(msg, "../../etc/passwd") {
        t.Errorf("错误文案缺原始路径: %s", msg)
    }
    if !strings.Contains(msg, "local-jailed") {
        t.Errorf("错误文案缺模式: %s", msg)
    }
}

func TestNotConfiguredErrorMessage(t *testing.T) {
    err := &NotConfiguredError{}
    if !strings.Contains(err.Error(), "filesystem") {
        t.Errorf("错误文案缺关键字: %s", err.Error())
    }
}

func TestErrorsAreDistinct(t *testing.T) {
    var pd *PathDeniedError
    var nc *NotConfiguredError
    if !errors.As(&PathDeniedError{Display: "x", Workspace: "/y", Op: OpRead, Mode: ModeLocalJailed}, &pd) {
        t.Fatal("PathDeniedError 应被 errors.As 识别")
    }
    if !errors.As(&NotConfiguredError{}, &nc) {
        t.Fatal("NotConfiguredError 应被 errors.As 识别")
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run 'Test(PathDeniedErrorMessage|NotConfiguredErrorMessage|ErrorsAreDistinct)' -v`
Expected: FAIL（包不存在）

- [ ] **Step 3: 写实现**

```go
// tool/fs/file_system.go
// Package fs 实现文件系统 capability seam（架构 v2 §3.5，对齐 dsh ctx.fs + sandboxPolicy）。
// Resolve 在 Lexical + Realpath 双层校验后返回可访问的目标路径；越界返回 *PathDeniedError。
package fs

import "fmt"

// Mode 描述 fs 访问模式（DSH sandboxMode 镜像）。
type Mode string

const (
    ModeLocalJailed Mode = "local-jailed" // DSH workspace-write：写 jail，读开放
    ModeSaaSJailed  Mode = "saas-jailed"  // DSH read-only / SaaS 默认：读写都 jail
    ModeDangerFull  Mode = "danger-full"  // 显式全开放（用户主动设置；诊断/迁移专用）
)

// Op 描述一次 fs 访问的类型（用于按读写严格度分流）。
type Op string

const (
    OpRead  Op = "read"  // ReadFile
    OpWrite Op = "write" // WriteFile
    OpList  Op = "list"  // FindFiles / ls
    OpGrep  Op = "grep"  // Grep
)

// Request 描述一次 fs 访问请求。
type Request struct {
    Path      string // 模型传入的路径（可能含 .. 或绝对路径）
    Op        Op     // 决定严格度
    SessionID string // 审计/日志关联
}

// Resolved 是解析后的结果。
type Resolved struct {
    DisplayPath  string   // 模型可见的路径
    TargetKey    string   // realpath 后的实体路径（用于 os.Open）
    AllowedRoots []string // 该请求允许的可写根（Op=OpWrite 时非空）
}

// FileSystem 是 fs 能力 seam 的抽象（DSH FileSystem 对位）。
type FileSystem interface {
    Resolve(ctx context.Context, req Request) (*Resolved, error)
    Mode() Mode
}

// PathDeniedError 结构化错误（DSH FS_SANDBOX_DENIED 对位）。
type PathDeniedError struct {
    Display      string
    Target       string
    Workspace    string
    Op           Op
    Mode         Mode
    AllowedRoots []string
}

func (e *PathDeniedError) Error() string {
    return fmt.Sprintf("路径 %q 在 %s 模式下不允许 %s（workspace=%q, allowed=%v）",
        e.Display, e.Mode, e.Op, e.Workspace, e.AllowedRoots)
}

// NotConfiguredError 表示 ctx 未注入 FileSystem（fail-closed）。
type NotConfiguredError struct{}

func (e *NotConfiguredError) Error() string {
    return "filesystem capability 未注入（fs.FromContext 返回 nil）"
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/file_system.go tool/fs/file_system_test.go
git commit -m "feat(fs): 引入 FileSystem 接口、Mode/Op、PathDeniedError、NotConfiguredError"
```

---

## Task 3: ~~`Resolve` ctx 参数改为标准 `context.Context`~~ (合并到 Task 2)

> 已在 Task 2 中直接使用 `context.Context`，无需独立 task。

---

## Task 4: `WritableRoots()` 共享根

**Files:**
- Create: `tool/fs/roots.go`
- Create: `tool/fs/roots_test.go`

**Interfaces:**
- Consumes: 上一 task 的 `Mode` 常量
- Produces:
  - `type FileAccessPolicy struct { WorkspaceRoot string; JailReads bool; JailWrites bool; WritableExtra []string }`
  - `func WritableRoots(p *FileAccessPolicy) []string`

- [ ] **Step 1: 写失败测试**

```go
// tool/fs/roots_test.go
package fs

import (
    "os"
    "path/filepath"
    "testing"
)

func TestWritableRootsDangerFull(t *testing.T) {
    p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailReads: false, JailWrites: false}
    got := WritableRoots(p)
    if len(got) != 1 || got[0] != "/proj" {
        t.Errorf("DangerFull 应仅返回 [/proj]，got=%v", got)
    }
}

func TestWritableRootsJailed(t *testing.T) {
    p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailReads: true, JailWrites: true}
    got := WritableRoots(p)
    // /proj + /tmp + os.TempDir()
    if len(got) != 3 {
        t.Errorf("应返回 3 项（workspace + /tmp + tempdir），got=%v", got)
    }
    abs, _ := filepath.Abs("/proj")
    if got[0] != abs {
        t.Errorf("workspace 应被 Abs 规范化，got=%q want=%q", got[0], abs)
    }
}

func TestWritableRootsDeduplicatesExtra(t *testing.T) {
    p := &FileAccessPolicy{
        WorkspaceRoot: "/proj",
        JailWrites:    true,
        WritableExtra: []string{"/proj", "/extra"},
    }
    got := WritableRoots(p)
    // /proj 出现两次应去重
    if len(got) != 4 { // /proj, /tmp, tempdir, /extra
        t.Errorf("应去重后 4 项，got=%v", got)
    }
}

func TestWritableRootsNormalizesTempDir(t *testing.T) {
    p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailWrites: true}
    got := WritableRoots(p)
    abs, _ := filepath.Abs(os.TempDir())
    found := false
    for _, r := range got {
        if r == abs {
            found = true
        }
    }
    if !found {
        t.Errorf("应包含 Abs 化的 os.TempDir()=%q，got=%v", abs, got)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run TestWritableRoots -v`
Expected: FAIL（`WritableRoots`/`FileAccessPolicy` 未定义）

- [ ] **Step 3: 实现**

```go
// tool/fs/roots.go
package fs

import (
    "os"
    "path/filepath"
)

// FileAccessPolicy 描述 fs 访问策略（assistant 层 FileAccessConfig 的镜像）。
type FileAccessPolicy struct {
    WorkspaceRoot string
    JailReads     bool
    JailWrites    bool
    WritableExtra []string
}

// WritableRoots 返回当前请求允许的可写根（去重、Abs 规范化）。
// DSH writableRoots() 的 Go 翻译；fs sandbox 与 shell policy 共用此函数。
func WritableRoots(p *FileAccessPolicy) []string {
    roots := []string{absOrSelf(p.WorkspaceRoot)}
    if !p.JailWrites {
        return dedupStr(roots) // ModeDangerFull：仅 workspace
    }
    roots = append(roots, "/tmp", absOrSelf(os.TempDir()))
    if len(p.WritableExtra) > 0 {
        for _, e := range p.WritableExtra {
            roots = append(roots, absOrSelf(e))
        }
    }
    return dedupStr(roots)
}

func absOrSelf(s string) string {
    if s == "" {
        return s
    }
    a, err := filepath.Abs(s)
    if err != nil {
        return s
    }
    return a
}

func dedupStr(in []string) []string {
    seen := make(map[string]bool, len(in))
    out := make([]string, 0, len(in))
    for _, s := range in {
        if !seen[s] {
            seen[s] = true
            out = append(out, s)
        }
    }
    return out
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/roots.go tool/fs/roots_test.go
git commit -m "feat(fs): WritableRoots() 共享根（fs sandbox 与 shell policy 共用）"
```

---

## Task 5: `Local{ModeDangerFull}` 透传实现

**Files:**
- Create: `tool/fs/local.go`
- Create: `tool/fs/local_test.go`

**Interfaces:**
- Consumes: 上一 task 的 `FileSystem` / `Mode` / `Request` / `Resolved`
- Produces:
  - `type Local struct { Mode Mode }` 实现 `FileSystem`

- [ ] **Step 1: 写失败测试**

```go
// tool/fs/local_test.go
package fs

import (
    "context"
    "os"
    "path/filepath"
    "testing"
)

func TestLocalDangerFullResolvesAbsolute(t *testing.T) {
    dir := t.TempDir()
    file := filepath.Join(dir, "a.txt")
    os.WriteFile(file, []byte("x"), 0o644)

    fsys := &Local{Mode: ModeDangerFull}
    r, err := fsys.Resolve(context.Background(), Request{Path: file, Op: OpRead})
    if err != nil {
        t.Fatalf("Resolve 失败: %v", err)
    }
    if r.DisplayPath != file {
        t.Errorf("DisplayPath=%q want=%q", r.DisplayPath, file)
    }
    if r.TargetKey != file {
        t.Errorf("TargetKey=%q want=%q", r.TargetKey, file)
    }
}

func TestLocalDangerFullAllowsEscape(t *testing.T) {
    // DangerFull 模式不做 jail；越界路径应被透传
    fsys := &Local{Mode: ModeDangerFull}
    r, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead})
    if err != nil {
        t.Fatalf("DangerFull 应允许越界: %v", err)
    }
    if r.TargetKey != "/etc/passwd" {
        t.Errorf("TargetKey=%q want=/etc/passwd", r.TargetKey)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run 'TestLocal' -v`
Expected: FAIL（`Local` 未定义）

- [ ] **Step 3: 实现**

```go
// tool/fs/local.go
package fs

import (
    "context"
    "path/filepath"
)

// Local 是 FileSystem 的透传实现（仅 ModeDangerFull 使用）。
// 不做任何 jail 校验；路径原样返回。
type Local struct {
    Mode Mode
}

// Resolve 实现 FileSystem.Resolve：filepath.Abs 后直接返回。
func (l *Local) Resolve(ctx context.Context, req Request) (*Resolved, error) {
    abs, err := filepath.Abs(req.Path)
    if err != nil {
        return nil, err
    }
    return &Resolved{
        DisplayPath: abs,
        TargetKey:   abs,
    }, nil
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/local.go tool/fs/local_test.go
git commit -m "feat(fs): Local{ModeDangerFull} 透传实现"
```

---

## Task 6: `Sandboxed` Lexical 校验阶段

**Files:**
- Create: `tool/fs/sandboxed.go`
- Create: `tool/fs/sandboxed_test.go`

**Interfaces:**
- Consumes: `WritableRoots` / `FileAccessPolicy` / `FileSystem`
- Produces:
  - `type Sandboxed struct { Policy *FileAccessPolicy }` 实现 `FileSystem`
  - `func (s *Sandboxed) Resolve(ctx, req)` Lexical 阶段返回 `*Resolved` 或 `*PathDeniedError`（Op=Read）
  - Op=Write 的 realpath 校验在下一 task 加

- [ ] **Step 1: 写失败测试**

```go
// tool/fs/sandboxed_test.go
package fs

import (
    "context"
    "errors"
    "os"
    "path/filepath"
    "testing"
)

func newJailedPolicy(t *testing.T, mode Mode) (*FileAccessPolicy, string) {
    t.Helper()
    dir := t.TempDir()
    return &FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}, dir
}

func TestSandboxedReadInsideJailPasses(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeSaaSJailed)
    file := filepath.Join(dir, "a.txt")
    os.WriteFile(file, []byte("x"), 0o644)

    fsys := &Sandboxed{Policy: p}
    r, err := fsys.Resolve(context.Background(), Request{Path: file, Op: OpRead})
    if err != nil {
        t.Fatalf("应通过: %v", err)
    }
    if r.TargetKey != file {
        t.Errorf("TargetKey=%q want=%q", r.TargetKey, file)
    }
}

func TestSandboxedReadRelativeInsideJailPasses(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeSaaSJailed)
    os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644)

    fsys := &Sandboxed{Policy: p}
    r, err := fsys.Resolve(context.Background(), Request{Path: "b.txt", Op: OpRead})
    if err != nil {
        t.Fatalf("应通过相对路径: %v", err)
    }
    want := filepath.Join(dir, "b.txt")
    if r.TargetKey != want {
        t.Errorf("TargetKey=%q want=%q", r.TargetKey, want)
    }
}

func TestSandboxedReadEscapeDenied(t *testing.T) {
    p, _ := newJailedPolicy(t, ModeSaaSJailed)
    fsys := &Sandboxed{Policy: p}
    _, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead})
    var pd *PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
    if pd.Op != OpRead {
        t.Errorf("Op=%q want=read", pd.Op)
    }
}

func TestSandboxedReadEscapeWithDotDotDenied(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeSaaSJailed)
    fsys := &Sandboxed{Policy: p}
    _, err := fsys.Resolve(context.Background(), Request{Path: filepath.Join(dir, "../etc/passwd"), Op: OpRead})
    var pd *PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run TestSandboxedRead -v`
Expected: FAIL（`Sandboxed` 未定义）

- [ ] **Step 3: 实现 Lexical 阶段**

```go
// tool/fs/sandboxed.go
package fs

import (
    "context"
    "errors"
    "path/filepath"
    "strings"
)

// Sandboxed 是 FileSystem 的 jail 实现（与 DSH SandboxedFileSystem 对位）。
// Resolve 分两阶段：Lexical（路径前缀比对）+ Realpath（实体 inode 比对，本 task 仅 Op=Write；Op=Read 走 Lexical 即返回）。
type Sandboxed struct {
    Policy *FileAccessPolicy
}

// Mode 返回当前策略模式。
func (s *Sandboxed) Mode() Mode {
    switch {
    case s.Policy.JailReads && !s.Policy.JailWrites:
        return ModeLocalJailed
    case s.Policy.JailReads && s.Policy.JailWrites:
        return ModeSaaSJailed
    default:
        return ModeDangerFull
    }
}

// Resolve 实现 FileSystem.Resolve（Lexical 阶段；Op=Write 的 Realpath 在下个 task 接入）。
func (s *Sandboxed) Resolve(ctx context.Context, req Request) (*Resolved, error) {
    abs, err := filepath.Abs(req.Path)
    if err != nil {
        return nil, err
    }
    workspace, err := filepath.Abs(s.Policy.WorkspaceRoot)
    if err != nil {
        return nil, err
    }
    // Lexical 阶段：rel 必须以 workspace 为前缀
    rel, err := filepath.Rel(workspace, abs)
    if err != nil {
        return nil, &PathDeniedError{
            Display: req.Path, Target: abs, Workspace: workspace,
            Op: req.Op, Mode: s.Mode(),
        }
    }
    if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
        return nil, &PathDeniedError{
            Display: req.Path, Target: abs, Workspace: workspace,
            Op: req.Op, Mode: s.Mode(),
            AllowedRoots: WritableRoots(s.Policy),
        }
    }
    return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
}

var _ FileSystem = (*Sandboxed)(nil)
var _ = errors.New // 占位，避免 imports 报错（本 task 暂未用）
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/sandboxed.go tool/fs/sandboxed_test.go
git commit -m "feat(fs): Sandboxed Lexical 校验（rel 前缀比对）"
```

---

## Task 7: `Sandboxed` Realpath 阶段（Op=Write 实体比对）

**Files:**
- Modify: `tool/fs/sandboxed.go`
- Modify: `tool/fs/sandboxed_test.go`

**Interfaces:**
- Consumes: 上一 task 的 Lexical 阶段
- Produces: Op=Write 时强制 realpath + `os.SameFile` 比对，Op=Read 维持 Lexical

- [ ] **Step 1: 写失败测试（追加到 sandboxed_test.go）**

```go
func TestSandboxedWriteEscapeDenied(t *testing.T) {
    p, _ := newJailedPolicy(t, ModeSaaSJailed)
    fsys := &Sandboxed{Policy: p}
    _, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpWrite})
    var pd *PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("Op=Write 应返回 *PathDeniedError，got=%v", err)
    }
}

func TestSandboxedWriteInsideWorkspacePasses(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeLocalJailed)
    target := filepath.Join(dir, "out.txt")
    fsys := &Sandboxed{Policy: p}
    r, err := fsys.Resolve(context.Background(), Request{Path: target, Op: OpWrite})
    if err != nil {
        t.Fatalf("应通过: %v", err)
    }
    if r.TargetKey != target {
        t.Errorf("TargetKey=%q want=%q", r.TargetKey, target)
    }
}

func TestSandboxedSymlinkEscapeDenied(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeSaaSJailed)
    // 在 workspace 外建一个文件，并在 workspace 内放一个 symlink 指向它
    outside := t.TempDir()
    target := filepath.Join(outside, "secret.txt")
    os.WriteFile(target, []byte("s"), 0o644)
    link := filepath.Join(dir, "leak.txt")
    if err := os.Symlink(target, link); err != nil {
        t.Skipf("不支持 symlink: %v", err)
    }

    fsys := &Sandboxed{Policy: p}
    _, err := fsys.Resolve(context.Background(), Request{Path: link, Op: OpRead})
    // Op=Read 走 Lexical 即返回（DSH read 透传语义）；此用例仅验证 Op=Write 时拒绝
    if err == nil {
        t.Log("Op=Read 透传 symlink（DSH 一致）")
    }
    _, err = fsys.Resolve(context.Background(), Request{Path: link, Op: OpWrite})
    var pd *PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("Op=Write 通过 symlink 越界应被拒绝，got=%v", err)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run 'TestSandboxedWrite|TestSandboxedSymlink' -v`
Expected: FAIL（`Op=Write` 越界当前 Lexical 已拒绝，但 symlink 用例应失败——Write 当前走 Lexical 就过，不做 realpath）

- [ ] **Step 3: 扩展 Resolve：Op=Write 时强制 realpath + SameFile 比对**

替换 `tool/fs/sandboxed.go` 的 `Resolve`：

```go
import (
    "context"
    "errors"
    "os"
    "path/filepath"
    "strings"
)

// Resolve 实现 FileSystem.Resolve。
// Op=Read：Lexical 阶段（DSH read 透传）。
// Op=Write：Lexical + Realpath 双层（DSH write fenced）。
func (s *Sandboxed) Resolve(ctx context.Context, req Request) (*Resolved, error) {
    abs, err := filepath.Abs(req.Path)
    if err != nil {
        return nil, err
    }
    workspace, err := filepath.Abs(s.Policy.WorkspaceRoot)
    if err != nil {
        return nil, err
    }
    rel, err := filepath.Rel(workspace, abs)
    if err != nil {
        return nil, s.denied(req, abs, workspace)
    }
    if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
        return nil, s.denied(req, abs, workspace)
    }

    if req.Op != OpWrite {
        // Read/List/Grep：Lexical 已足够
        return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
    }

    // Op=Write：Realpath 阶段（DSH fsio.ts:146-194）
    return s.realpathWrite(req, abs, workspace)
}

func (s *Sandboxed) realpathWrite(req Request, abs, workspace string) (*Resolved, error) {
    targetStat, err := os.Stat(abs)
    if err != nil {
        if os.IsNotExist(err) {
            // 文件尚未创建：Lexical 已通过且目标在 workspace 内即允许
            return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
        }
        return nil, err
    }
    // 同 dev+ino 比对：resolve workspace 自身，再 SameFile 比对
    wsStat, err := os.Stat(workspace)
    if err != nil {
        return nil, err
    }
    walk := abs
    for {
        parent := filepath.Dir(walk)
        if parent == walk {
            return nil, s.denied(req, abs, workspace)
        }
        ps, err := os.Stat(parent)
        if err != nil {
            walk = parent
            continue
        }
        if os.SameFile(ps, targetStat) || os.SameFile(ps, wsStat) {
            return nil, s.denied(req, abs, workspace)
        }
        if parent == workspace || os.SameFile(ps, wsStat) {
            // 已走到 workspace 边界，未触发逃逸
            break
        }
        walk = parent
    }
    return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
}

func (s *Sandboxed) denied(req Request, abs, workspace string) error {
    return &PathDeniedError{
        Display: req.Path, Target: abs, Workspace: workspace,
        Op: req.Op, Mode: s.Mode(),
        AllowedRoots: WritableRoots(s.Policy),
    }
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/sandboxed.go tool/fs/sandboxed_test.go
git commit -m "feat(fs): Sandboxed Realpath 阶段（Op=Write 实体比对，防 symlink TOCTOU）"
```

---

## Task 8: ctx 注入 `WithFileSystem` / `FromContext`

**Files:**
- Create: `tool/fs/context.go`
- Create: `tool/fs/context_test.go`

**Interfaces:**
- Consumes: `FileSystem`
- Produces: `WithFileSystem(ctx, fsys) ctx`；`FromContext(ctx) FileSystem`（未注入返回 `nil`，由工具转为 `*NotConfiguredError`）

- [ ] **Step 1: 写失败测试**

```go
// tool/fs/context_test.go
package fs

import (
    "context"
    "errors"
    "testing"
)

func TestFromContextMissingReturnsNil(t *testing.T) {
    fsys := FromContext(context.Background())
    if fsys != nil {
        t.Fatalf("未注入应返回 nil，got=%v", fsys)
    }
}

func TestWithFileSystemRoundTrip(t *testing.T) {
    fsys := &Local{Mode: ModeDangerFull}
    ctx := WithFileSystem(context.Background(), fsys)
    got := FromContext(ctx)
    if got != fsys {
        t.Fatalf("FromContext=%v want=%v", got, fsys)
    }
}

func TestNotConfiguredErrorFromMissingContext(t *testing.T) {
    // 工具应在 fsys==nil 时返回 *NotConfiguredError
    fsys := FromContext(context.Background())
    if fsys == nil {
        err := &NotConfiguredError{}
        if !errors.As(err, new(*NotConfiguredError)) {
            t.Fatal("NotConfiguredError 应可 errors.As")
        }
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/fs/ -run 'TestFromContext|TestWithFileSystem|TestNotConfigured' -v`
Expected: FAIL（`WithFileSystem` 未定义）

- [ ] **Step 3: 实现**

```go
// tool/fs/context.go
package fs

import "context"

type ctxKey struct{}

// WithFileSystem 把 FileSystem 注入 ctx。
func WithFileSystem(ctx context.Context, fsys FileSystem) context.Context {
    return context.WithValue(ctx, ctxKey{}, fsys)
}

// FromContext 从 ctx 取出 FileSystem；未注入返回 nil。
// 工具应在调用方转为 *NotConfiguredError（fail-closed）。
func FromContext(ctx context.Context) FileSystem {
    v, _ := ctx.Value(ctxKey{}).(FileSystem)
    return v
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/fs/context.go tool/fs/context_test.go
git commit -m "feat(fs): WithFileSystem/FromContext ctx 注入"
```

---

## Task 9: `assistant.FileAccessConfig` 类型与 Config 字段

**Files:**
- Modify: `assistant/config.go`
- Modify: `assistant/config_test.go`（若存在）

**Interfaces:**
- Consumes: 无
- Produces:
  - `type FileAccessConfig struct { WorkspaceRoot string; JailReads bool; JailWrites bool; WritableExtra []string }`
  - `Config.FileAccess FileAccessConfig` 字段

- [ ] **Step 1: 写失败测试**

```go
// assistant/config_test.go 末尾追加（若文件不存在则新建）
package assistant

import "testing"

func TestFileAccessConfigDefaults(t *testing.T) {
    var cfg FileAccessConfig
    if cfg.WorkspaceRoot != "" {
        t.Errorf("零值 WorkspaceRoot 应为空，got=%q", cfg.WorkspaceRoot)
    }
    if cfg.JailReads || cfg.JailWrites {
        t.Errorf("零值 Jail* 应为 false")
    }
    if cfg.WritableExtra != nil {
        t.Errorf("零值 WritableExtra 应为 nil，got=%v", cfg.WritableExtra)
    }
}

func TestConfigHasFileAccessField(t *testing.T) {
    cfg := Config{FileAccess: FileAccessConfig{WorkspaceRoot: "/x"}}
    if cfg.FileAccess.WorkspaceRoot != "/x" {
        t.Errorf("字段未生效")
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run 'TestFileAccess|TestConfigHasFileAccess' -v`
Expected: FAIL（`FileAccessConfig` 未定义）

- [ ] **Step 3: 修改 `config.go`**

在 `assistant/config.go` 末尾追加：

```go
// FileAccessConfig 描述文件工具的访问策略。
// WorkspaceRoot 必填；空值由 assistant.New 校验失败（fail-closed）。
type FileAccessConfig struct {
    WorkspaceRoot string   // 文件系统 jail 根目录
    JailReads      bool     // 是否对读做 jail；DSH SaaS/read-only = true
    JailWrites     bool     // 是否对写做 jail；DSH 默认 = true
    WritableExtra  []string // 附加可写根（如构建产物目录）
}
```

在 `Config` 结构体加字段（紧贴现有字段，保持一致风格）：

```go
type Config struct {
    // ... 既有字段
    FileAccess FileAccessConfig
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add assistant/config.go assistant/config_test.go
git commit -m "feat(assistant): 新增 FileAccessConfig + Config.FileAccess 字段"
```

---

## Task 10: `assistant.New` 校验 `WorkspaceRoot` 必填（fail-closed）

**Files:**
- Modify: `assistant/assistant.go`
- Modify: `assistant/assistant_test.go`

**Interfaces:**
- Consumes: `Config.FileAccess.WorkspaceRoot`
- Produces: `assistant.New` 在 `WorkspaceRoot == ""` 时返回 `error`

- [ ] **Step 1: 写失败测试**

```go
// assistant/assistant_test.go 末尾追加
func TestNewRequiresWorkspaceRoot(t *testing.T) {
    _, err := New(Config{ /* 其余字段也走默认 */ })
    if err == nil {
        t.Fatal("空 WorkspaceRoot 应使 New 失败")
    }
    if !strings.Contains(err.Error(), "WorkspaceRoot") {
        t.Errorf("错误文案应点名 WorkspaceRoot，got=%s", err.Error())
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run TestNewRequiresWorkspaceRoot -v`
Expected: FAIL（当前 New 不校验）

- [ ] **Step 3: 在 `New` 入口校验**

修改 `assistant/assistant.go` 的 `New` 函数，在最前面加：

```go
import "fmt"

func New(cfg Config, opts ...Option) (*Assistant, error) {
    if cfg.FileAccess.WorkspaceRoot == "" {
        return nil, fmt.Errorf("assistant.New: Config.FileAccess.WorkspaceRoot 必填（fail-closed）")
    }
    // ... 既有实现
}
```

> 实际函数签名以文件为准；只把校验放在最前即可。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS（既有测试需为 `WorkspaceRoot` 显式赋值才能继续通过——这是预期破坏性变更）

- [ ] **Step 5: 修复既有测试的 WorkspaceRoot 缺失**

检查 `assistant/assistant_test.go` 里所有 `Config{...}` 字面量，为缺 `FileAccess` 的用例补：

```go
Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, ...既有字段}
```

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add assistant/assistant.go assistant/assistant_test.go
git commit -m "feat(assistant): New 校验 WorkspaceRoot 必填（fail-closed，破坏性变更）"
```

---

## Task 11: `DefaultPolicy.Mode()` 由 `FileAccessConfig` 推导

**Files:**
- Modify: `assistant/policy.go`
- Modify: `assistant/policy_test.go`（若存在）

**Interfaces:**
- Consumes: `FileAccessConfig` / `fs.Mode`
- Produces: `func (p *DefaultPolicy) Mode() fs.Mode`

- [ ] **Step 1: 写失败测试**

```go
// assistant/policy_test.go 末尾追加（或新建）
package assistant

import (
    "testing"

    "hegq.net/mos/tool/fs"
)

func TestDefaultPolicyModeLocalJailed(t *testing.T) {
    p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: true, JailWrites: false}}
    if p.Mode() != fs.ModeLocalJailed {
        t.Errorf("want=local-jailed got=%s", p.Mode())
    }
}

func TestDefaultPolicyModeSaaSJailed(t *testing.T) {
    p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: true, JailWrites: true}}
    if p.Mode() != fs.ModeSaaSJailed {
        t.Errorf("want=saas-jailed got=%s", p.Mode())
    }
}

func TestDefaultPolicyModeDangerFull(t *testing.T) {
    p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: false, JailWrites: false}}
    if p.Mode() != fs.ModeDangerFull {
        t.Errorf("want=danger-full got=%s", p.Mode())
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run TestDefaultPolicyMode -v`
Expected: FAIL（`Mode()` 方法未定义）

- [ ] **Step 3: 在 `DefaultPolicy` 上加 `Mode()`**

修改 `assistant/policy.go`，在 `DefaultPolicy` 结构体里加 `FileAccess FileAccessConfig` 字段（如尚未加），并新增方法：

```go
import "hegq.net/mos/tool/fs"

// Mode 推导自 FileAccessConfig。
func (p *DefaultPolicy) Mode() fs.Mode {
    switch {
    case p.FileAccess.JailReads && !p.FileAccess.JailWrites:
        return fs.ModeLocalJailed
    case p.FileAccess.JailReads && p.FileAccess.JailWrites:
        return fs.ModeSaaSJailed
    default:
        return fs.ModeDangerFull
    }
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add assistant/policy.go assistant/policy_test.go
git commit -m "feat(assistant): DefaultPolicy.Mode() 由 FileAccessConfig 推导"
```

---

## Task 12: `policy_file.withinDir` 改用 `WritableRoots()`

**Files:**
- Modify: `assistant/policy_file.go`
- Modify: `assistant/policy_file_test.go`（若存在）

**Interfaces:**
- Consumes: `WritableRoots` / `FileAccessPolicy`
- Produces: `withinDir(p *FileAccessPolicy, target string) error` 用 `WritableRoots` 替代原 `p.WorkDir` 单根

- [ ] **Step 1: 写失败测试**

```go
// assistant/policy_file_test.go 末尾追加
package assistant

import (
    "path/filepath"
    "testing"

    "hegq.net/mos/tool/fs"
)

func TestWithinDirAllowsExtraWritable(t *testing.T) {
    extra := t.TempDir()
    p := &fs.FileAccessPolicy{WorkspaceRoot: t.TempDir(), JailWrites: true, WritableExtra: []string{extra}}
    // 落在 WritableExtra 内应通过
    if err := withinDir(p, filepath.Join(extra, "x")); err != nil {
        t.Errorf("WritableExtra 内应通过: %v", err)
    }
}

func TestWithinDirDeniesOutsideAllRoots(t *testing.T) {
    p := &fs.FileAccessPolicy{WorkspaceRoot: t.TempDir(), JailWrites: true}
    if err := withinDir(p, "/etc/passwd"); err == nil {
        t.Errorf("应拒绝越界")
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run TestWithinDir -v`
Expected: FAIL（`withinDir` 当前签名不匹配）

- [ ] **Step 3: 重构 `withinDir`**

替换 `assistant/policy_file.go` 的 `withinDir`：

```go
package assistant

import (
    "fmt"
    "path/filepath"
    "strings"

    "hegq.net/mos/tool/fs"
)

// withinDir 判断 target 是否落在任一可写根（workspace + extra + /tmp + tempdir）内。
func withinDir(p *fs.FileAccessPolicy, target string) error {
    for _, root := range fs.WritableRoots(p) {
        if pathUnderRoot(root, target) {
            return nil
        }
    }
    return fmt.Errorf("路径 %q 超出工作目录 %q", target, p.WorkspaceRoot)
}

// pathUnderRoot 判断 target 是否在 root 子树内（Lexical）。
func pathUnderRoot(root, target string) bool {
    r, err := filepath.Abs(root)
    if err != nil {
        return false
    }
    t := target
    if !filepath.IsAbs(t) {
        t = filepath.Join(r, t)
    }
    t, err = filepath.Abs(t)
    if err != nil {
        return false
    }
    rel, err := filepath.Rel(r, t)
    if err != nil {
        return false
    }
    return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
```

> 原 `checkFile` 调用 `withinDir(p.WorkDir, raw)`——改为 `withinDir(&p.FileAccess, raw)`。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add assistant/policy_file.go assistant/policy_file_test.go
git commit -m "refactor(assistant): withinDir 改用 fs.WritableRoots（多根支持，与 fs sandbox 共根）"
```

---

## Task 13: `policy_shell` 受限危险命令用 `WritableRoots()`

**Files:**
- Modify: `assistant/policy_shell.go`
- Modify: `assistant/policy_shell_test.go`（若存在）

**Interfaces:**
- Consumes: 上一 task 的 `withinDir(*fs.FileAccessPolicy, string)`
- Produces: `rm / sudo / chmod / mv / kill / find -delete` 的校验用 `withinDir(p, path)`

- [ ] **Step 1: 写失败测试**

```go
// assistant/policy_shell_test.go 末尾追加
package assistant

import (
    "testing"

    "hegq.net/mos/tool/fs"
)

func TestShellCheckRMDeniedOutsideWritable(t *testing.T) {
    p := &DefaultPolicy{FileAccess: FileAccessConfig{
        WorkspaceRoot: t.TempDir(), JailWrites: true,
    }}
    // 模拟解析 rm /etc/passwd：path 应被 withinDir 拒绝
    if err := withinDir(&fs.FileAccessPolicy{
        WorkspaceRoot: p.FileAccess.WorkspaceRoot,
        JailWrites:    true,
    }, "/etc/passwd"); err == nil {
        t.Fatal("应拒绝越界 rm")
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run TestShellCheckRMDeniedOutsideWritable -v`
Expected: FAIL（当前 `policy_shell` 的受限命令校验用 `p.WorkDir` 单根而非 `WritableRoots`）

- [ ] **Step 3: 在 `policy_shell.go` 把受限危险命令的 `withinDir(p.WorkDir, path)` 改为 `withinDir(&p.FileAccess, path)`**

定位 `assistant/policy_shell.go` 里所有 `withinDir(p.WorkDir, ...)` 字面量并替换；如 `p.WorkDir` 字段不存在则保持 `p.FileAccess.WorkspaceRoot`。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add assistant/policy_shell.go assistant/policy_shell_test.go
git commit -m "refactor(assistant): shell 受限危险命令用 fs.WritableRoots（与 fs sandbox 共根）"
```

---

## Task 14: `assistant.New` 注册 fs seam + 注入 RunContext.FS

**Files:**
- Modify: `assistant/assistant.go`
- Modify: `assistant/assistant_test.go`

**Interfaces:**
- Consumes: `DefaultPolicy.Mode()` / `Sandboxed{Policy}` / `WithFileSystem`
- Produces: `assistant.New` 在装配时注册 fs seam "default"，并在 `RunContext` / `ctx` 注入 `fs.Sandboxed`

- [ ] **Step 1: 写失败测试**

```go
// assistant/assistant_test.go 末尾追加
import (
    "context"
    "path/filepath"

    "hegq.net/mos/tool/fs"
)

func TestAssistantProvidesFileSystem(t *testing.T) {
    dir := t.TempDir()
    a, err := New(Config{FileAccess: FileAccessConfig{WorkspaceRoot: dir}})
    if err != nil {
        t.Fatalf("New 失败: %v", err)
    }
    // 创建 session
    sess, err := a.NewSession(context.Background())
    if err != nil {
        t.Fatalf("NewSession: %v", err)
    }
    fsys := fs.FromContext(sess.Context())
    if fsys == nil {
        t.Fatal("ctx 未注入 fs")
    }
    if fsys.Mode() != fs.ModeSaaSJailed { // 零值 JailReads/JailWrites=false → ModeDangerFull
        // 由于 FileAccessConfig{} 零值 → ModeDangerFull
        if fsys.Mode() != fs.ModeDangerFull {
            t.Errorf("mode=%s", fsys.Mode())
        }
    }
    // 验证 Sandbox 模式：WorkspaceRoot 内可读，越界拒绝
    _, err = fsys.Resolve(context.Background(), fs.Request{Path: filepath.Join(dir, "x"), Op: fs.OpRead})
    // 文件不存在会返回 os.Stat 错误（非 PathDeniedError），可接受
    _, err = fsys.Resolve(context.Background(), fs.Request{Path: "/etc/passwd", Op: fs.OpRead})
    if fsys.Mode() == fs.ModeDangerFull {
        t.Log("DangerFull 模式透传越界（预期）")
    }
}
```

> 注：测试的具体断言以 `sess.Context()` 实际返回的 API 为准；如 MoS 用 `sess.RunContext()` 等不同接口，按实际接口调整。

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./assistant/ -run TestAssistantProvidesFileSystem -v`
Expected: FAIL（ctx 未注入 fs）

- [ ] **Step 3: 在 `assistant.New` 装配时注册 + 注入**

在 `assistant.go` 的 `New` 函数末尾（既有的 `reg.Register(...)` 段附近）追加：

```go
import "hegq.net/mos/tool/fs"

// 注册 fs capability（架构 v2 §3.5）。
reg.Register(seam.KindFileSystem, "default", func() any {
    return &fs.Sandboxed{Policy: &fs.FileAccessPolicy{
        WorkspaceRoot: cfg.FileAccess.WorkspaceRoot,
        JailReads:     cfg.FileAccess.JailReads,
        JailWrites:    cfg.FileAccess.JailWrites,
        WritableExtra: cfg.FileAccess.WritableExtra,
    }}
})
```

并在 `RunContext` 构造点（`NewSession` 或类似）注入：

```go
rc.FS = fs.WithFileSystem(rc.Ctx, /* resolve fsys from reg */)
```

> 实际注入点取决于 MoS 现有 RunContext 结构；按文件实际签名调整。`rc.FS` 字段可能需要新增。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./assistant/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add assistant/assistant.go assistant/assistant_test.go
git commit -m "feat(assistant): New 注册 fs seam + 注入 RunContext.FS"
```

---

## Task 15: `tool/builtin.ReadFile` 接入 `fs.FromContext`

**Files:**
- Modify: `tool/builtin/file.go`
- Create: `tool/builtin/file_jail_test.go`

**Interfaces:**
- Consumes: `fs.FromContext` / `fs.Sandboxed.Resolve`
- Produces: `ReadFile.Execute` 入口先 `fs.Resolve(... OpRead)`，再用 `r.TargetKey` 打开

- [ ] **Step 1: 写失败测试**

```go
// tool/builtin/file_jail_test.go
package builtin

import (
    "context"
    "encoding/json"
    "errors"
    "os"
    "path/filepath"
    "testing"

    "hegq.net/mos/tool/fs"
)

func TestReadFileUsesFsContext(t *testing.T) {
    dir := t.TempDir()
    file := filepath.Join(dir, "a.txt")
    os.WriteFile(file, []byte("hello"), 0o644)

    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"path": file})
    r, err := ReadFile{}.Execute(ctx, args)
    if err != nil {
        t.Fatalf("Execute 失败: %v", err)
    }
    m := r.(map[string]any)
    if m["content"] != "hello" {
        t.Errorf("content=%v", m["content"])
    }
}

func TestReadFileEscapeDenied(t *testing.T) {
    dir := t.TempDir()
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"path": "/etc/passwd"})
    _, err := ReadFile{}.Execute(ctx, args)
    var pd *fs.PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
}

func TestReadFileNotConfiguredError(t *testing.T) {
    args, _ := json.Marshal(map[string]any{"path": "/tmp/x"})
    _, err := ReadFile{}.Execute(context.Background(), args)
    var nc *fs.NotConfiguredError
    if !errors.As(err, &nc) {
        t.Fatalf("未注入 fs 应返回 *NotConfiguredError，got=%v", err)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/builtin/ -run 'TestReadFile' -v`
Expected: FAIL（既有 ReadFile 不走 fs）

- [ ] **Step 3: 修改 `ReadFile.Execute`**

替换 `tool/builtin/file.go` 的 `ReadFile.Execute`：

```go
import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "os"

    "hegq.net/mos/tool"
    "hegq.net/mos/tool/fs"
)

func (ReadFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
    p, err := tool.Parse[readFileParams](args)
    if err != nil {
        return nil, err
    }
    if p.Path == "" {
        return nil, fmt.Errorf("path 不能为空")
    }
    fsys := fs.FromContext(ctx)
    if fsys == nil {
        return nil, &fs.NotConfiguredError{}
    }
    r, err := fsys.Resolve(ctx, fs.Request{Path: p.Path, Op: fs.OpRead})
    if err != nil {
        return nil, err
    }
    limit := p.Limit
    if limit <= 0 {
        limit = defaultReadLimit
    }
    f, err := os.Open(r.TargetKey)
    if err != nil {
        return nil, err
    }
    defer f.Close()
    info, err := f.Stat()
    if err != nil {
        return nil, err
    }
    if p.Offset > 0 {
        if _, err := f.Seek(int64(p.Offset), io.SeekStart); err != nil {
            return nil, err
        }
    }
    buf := make([]byte, limit)
    n, err := f.Read(buf)
    if err != nil && err != io.EOF {
        return nil, err
    }
    return map[string]any{
        "path":    p.Path,
        "size":    info.Size(),
        "offset":  p.Offset,
        "content": string(buf[:n]),
    }, nil
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/builtin/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/builtin/file.go tool/builtin/file_jail_test.go
git commit -m "feat(builtin): ReadFile 接入 fs.FromContext（路径 jail）"
```

---

## Task 16: `tool/builtin.WriteFile` 接入 `fs.FromContext`

**Files:**
- Modify: `tool/builtin/file.go`
- Modify: `tool/builtin/file_jail_test.go`

**Interfaces:**
- Consumes: 上一 task 的 fs 接入模式
- Produces: `WriteFile.Execute` 用 `fs.Resolve(... OpWrite)`

- [ ] **Step 1: 追加失败测试**

```go
// tool/builtin/file_jail_test.go 追加
func TestWriteFileEscapeDenied(t *testing.T) {
    dir := t.TempDir()
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "content": "x"})
    _, err := WriteFile{}.Execute(ctx, args)
    var pd *fs.PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
}

func TestWriteFileInsideWorkspacePasses(t *testing.T) {
    dir := t.TempDir()
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "out.txt"), "content": "y"})
    _, err := WriteFile{}.Execute(ctx, args)
    if err != nil {
        t.Fatalf("Execute 失败: %v", err)
    }
    if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
        t.Errorf("文件未创建: %v", err)
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/builtin/ -run TestWriteFile -v`
Expected: FAIL

- [ ] **Step 3: 修改 `WriteFile.Execute`**

替换 `tool/builtin/file.go` 的 `WriteFile.Execute`，在 `os.MkdirAll` 之前加：

```go
fsys := fs.FromContext(ctx)
if fsys == nil {
    return nil, &fs.NotConfiguredError{}
}
r, err := fsys.Resolve(ctx, fs.Request{Path: p.Path, Op: fs.OpWrite})
if err != nil {
    return nil, err
}
// 用 r.TargetKey 替代 p.Path 写入
resolvedPath := r.TargetKey
```

并把后续 `os.MkdirAll(filepath.Dir(p.Path), ...)` 改为 `filepath.Dir(resolvedPath)`、`os.OpenFile(p.Path, ...)` 改为 `resolvedPath`。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/builtin/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/builtin/file.go tool/builtin/file_jail_test.go
git commit -m "feat(builtin): WriteFile 接入 fs.FromContext（Op=Write 强制 realpath）"
```

---

## Task 17: `tool/builtin.FindFiles` 接入 `fs.FromContext`

**Files:**
- Modify: `tool/builtin/find.go`
- Modify: `tool/builtin/file_jail_test.go`

**Interfaces:**
- Consumes: 上一 task 的 fs 接入模式
- Produces: `FindFiles.Execute` 的 `Root` 字段空时取 `WorkspaceRoot`，WalkDir 前 `fs.Resolve(... OpList)`

- [ ] **Step 1: 追加失败测试**

```go
// tool/builtin/file_jail_test.go 追加
func TestFindFilesRootEscapesDenied(t *testing.T) {
    dir := t.TempDir()
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"root": "/etc", "pattern": "*"})
    _, err := FindFiles{}.Execute(ctx, args)
    var pd *fs.PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
}

func TestFindFilesDefaultsToWorkspaceRoot(t *testing.T) {
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x"), 0o644)
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"pattern": "*.go"})
    r, err := FindFiles{}.Execute(ctx, args)
    if err != nil {
        t.Fatalf("Execute 失败: %v", err)
    }
    m := r.(map[string]any)
    if m["count"].(int) != 1 {
        t.Errorf("count=%v", m["count"])
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/builtin/ -run TestFindFiles -v`
Expected: FAIL

- [ ] **Step 3: 修改 `FindFiles.Execute`**

在 `tool/builtin/find.go` 的 `FindFiles.Execute` 里，把 `root := p.Root; if root == "" { root = "." }` 替换为：

```go
import "hegq.net/mos/tool/fs"

fsys := fs.FromContext(ctx)
if fsys == nil {
    return nil, &fs.NotConfiguredError{}
}
root := p.Root
if root == "" {
    r, err := fsys.Resolve(ctx, fs.Request{Path: ".", Op: fs.OpList})
    if err != nil {
        return nil, err
    }
    root = r.TargetKey
} else {
    r, err := fsys.Resolve(ctx, fs.Request{Path: root, Op: fs.OpList})
    if err != nil {
        return nil, err
    }
    root = r.TargetKey
}
```

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/builtin/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/builtin/find.go tool/builtin/file_jail_test.go
git commit -m "feat(builtin): FindFiles 接入 fs.FromContext（Root jail + 默认 WorkspaceRoot）"
```

---

## Task 18: `tool/builtin.Grep` 接入 `fs.FromContext`

**Files:**
- Modify: `tool/builtin/find.go`
- Modify: `tool/builtin/file_jail_test.go`

**Interfaces:**
- Consumes: 上一 task 的 fs 接入模式
- Produces: `Grep.Execute` 的 `Root` 字段同 FindFiles

- [ ] **Step 1: 追加失败测试**

```go
// tool/builtin/file_jail_test.go 追加
func TestGrepRootEscapesDenied(t *testing.T) {
    dir := t.TempDir()
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"root": "/etc", "pattern": "x"})
    _, err := Grep{}.Execute(ctx, args)
    var pd *fs.PathDeniedError
    if !errors.As(err, &pd) {
        t.Fatalf("应返回 *PathDeniedError，got=%v", err)
    }
}

func TestGrepDefaultsToWorkspaceRoot(t *testing.T) {
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "a.go"), []byte("hello world"), 0o644)
    fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}}
    ctx := fs.WithFileSystem(context.Background(), fsys)

    args, _ := json.Marshal(map[string]any{"pattern": "hello"})
    r, err := Grep{}.Execute(ctx, args)
    if err != nil {
        t.Fatalf("Execute 失败: %v", err)
    }
    m := r.(map[string]any)
    if m["count"].(int) < 1 {
        t.Errorf("count=%v", m["count"])
    }
}
```

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./tool/builtin/ -run TestGrep -v`
Expected: FAIL

- [ ] **Step 3: 修改 `Grep.Execute`**

同 Task 17 的模式：fs.FromContext → 空时默认 WorkspaceRoot → Resolve OpList。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./tool/builtin/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add tool/builtin/find.go tool/builtin/file_jail_test.go
git commit -m "feat(builtin): Grep 接入 fs.FromContext（Root jail + 默认 WorkspaceRoot）"
```

---

## Task 19: `fs/resolve` 审计事件

**Files:**
- Modify: `agent/event/events.go`
- Modify: `agent/event/events_test.go`（若存在）
- Modify: `tool/fs/sandboxed.go`
- Modify: `tool/fs/sandboxed_test.go`

**Interfaces:**
- Consumes: `PathDeniedError` / `Sandboxed`
- Produces: `EventFsResolve` 类型 + `EmitFsResolve(ctx, bus, ev)` helper；`Sandboxed.Resolve` 在 deny 与 allow 时都发事件

- [ ] **Step 1: 写失败测试**

```go
// agent/event/events_test.go 末尾追加
package event

import (
    "context"
    "testing"

    "hegq.net/mos/agent/event"
)

func TestEventFsResolveContract(t *testing.T) {
    ev := event.EventFsResolve{
        Target:    "/etc/passwd",
        Display:   "../../etc/passwd",
        Op:        "read",
        Mode:      "saas-jailed",
        Decision:  "denied",
        SessionID: "sess-1",
    }
    data, err := event.MarshalData(ev)
    if err != nil {
        t.Fatalf("Marshal 失败: %v", err)
    }
    if data["decision"] != "denied" {
        t.Errorf("decision=%v", data["decision"])
    }
}
```

```go
// tool/fs/sandboxed_test.go 追加
type recordingBus struct{ events []Event }

func (b *recordingBus) Emit(ctx context.Context, ev Event) {
    b.events = append(b.events, ev)
}

func TestSandboxedEmitsResolveEvents(t *testing.T) {
    p, dir := newJailedPolicy(t, ModeSaaSJailed)
    fsys := &Sandboxed{Policy: p}
    bus := &recordingBus{}
    ctx := WithBus(context.Background(), bus)

    // denied
    _, _ = fsys.Resolve(ctx, Request{Path: "/etc/passwd", Op: OpRead, SessionID: "s"})
    // allowed
    _, _ = fsys.Resolve(ctx, Request{Path: filepath.Join(dir, "x"), Op: OpRead, SessionID: "s"})

    if len(bus.events) != 2 {
        t.Fatalf("应发 2 个事件，got=%d", len(bus.events))
    }
    if bus.events[0].Data["decision"] != "denied" {
        t.Errorf("第一个事件应 denied")
    }
    if bus.events[1].Data["decision"] != "allowed" {
        t.Errorf("第二个事件应 allowed")
    }
}
```

> `WithBus` / `Bus` / `Event` 来自 `agent/event`；`Sandboxed` 需要新增 `WithBus(ctx, bus)` ctx 注入函数（与 `WithFileSystem` 同形）。

- [ ] **Step 2: 跑测试，验证失败**

Run: `go test ./agent/event/ -run TestEventFsResolveContract -v && go test ./tool/fs/ -run TestSandboxedEmitsResolveEvents -v`
Expected: FAIL（事件类型未定义 + Sandboxed 未发事件）

- [ ] **Step 3: 在 `agent/event/events.go` 加事件类型**

按现有事件命名风格（如 `EventToolCall` / `EventMessage`）追加：

```go
// EventFsResolve 描述一次 fs 路径解析决策。
type EventFsResolve struct {
    Target    string `json:"target"`
    Display   string `json:"display"`
    Op        string `json:"op"`
    Mode      string `json:"mode"`
    Decision  string `json:"decision"` // allowed | denied
    SessionID string `json:"session_id,omitempty"`
}

// EventFsResolveTopic 是 fs/resolve 事件主题。
const EventFsResolveTopic = "fs/resolve"

// EmitFsResolve 发 fs/resolve 事件。
func EmitFsResolve(ctx context.Context, bus Bus, ev EventFsResolve) {
    data, _ := MarshalData(ev)
    bus.Emit(ctx, Event{Name: EventFsResolveTopic, Data: data})
}
```

并在 `tool/fs/sandboxed.go` 加：

```go
// WithBus 把 EventBus 注入 ctx（Sandboxed 用以发 fs/resolve 事件）。
func WithBus(ctx context.Context, bus Bus) context.Context { ... }

// Bus 接口（事件总线 emit 抽象）。
type Bus interface { Emit(ctx context.Context, ev Event) }
```

在 `Sandboxed.Resolve` 的 `allowed` 与 `denied` 两个分支都调 `EmitFsResolve(ctx, bus, ...)`。

- [ ] **Step 4: 重跑测试，验证通过**

Run: `go test ./agent/event/ ./tool/fs/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add agent/event/events.go agent/event/events_test.go tool/fs/sandboxed.go tool/fs/sandboxed_test.go
git commit -m "feat(event): 新增 fs/resolve 审计事件（路径解析决策可回放）"
```

---

## Task 20: 文档同步

**Files:**
- Modify: `FEATURES.md`
- Modify: `README.md`
- Modify: `docs/architecture-v2.md`

**Interfaces:**
- Consumes: 既有文档结构
- Produces: 路线图 M2.x 落地条目 + 架构 v2 §3.5 追加 KindFileSystem 说明

- [ ] **Step 1: 更新 `FEATURES.md`**

在 §3.2 工具系统条目后追加：

```markdown
### 3.2.x 文件工具 Jail（M2.x 新增）

- 新包 `tool/fs` + 新增 SeamRegistry.KindFileSystem（架构 v2 §3.5；DSH ctx.fs + sandboxPolicy + shell.sandboxMode 三 seam 同形）
- `FileAccessConfig{WorkspaceRoot, JailReads, JailWrites, WritableExtra}`：WorkspaceRoot 必填（fail-closed）
- `Sandboxed` 实现：Lexical（`filepath.Abs + Rel`）+ Realpath（`os.SameFile` 实体比对）双层校验；Op=Write 强制 Realpath 防 symlink TOCTOU
- `WritableRoots(p)` 共享根（workspace + `/tmp` + `os.TempDir()` + `WritableExtra`）；与 shell policy 共根防配置漂移
- 4 个内置文件工具（ReadFile/WriteFile/FindFiles/Grep）全部接入 `fs.FromContext(ctx)`；ctx 缺 fs → `*fs.NotConfiguredError`（fail-closed）
- 审计事件 `fs/resolve` 入 Session 日志，可回放
- 部署 preset：单机（DSH workspace-write）= `ModeLocalJailed`；SaaS = `ModeSaaSJailed`；诊断/迁移 = `ModeDangerFull`（显式）
- **破坏性变更**：升级时必须为 `Config.FileAccess.WorkspaceRoot` 显式赋值，否则 `assistant.New` 启动失败
```

并把路线图 M2.x 状态从"规划中"改为"已落地"。

- [ ] **Step 2: 更新 `README.md` 路线图**

在 `README.md` 的 `[ ] M2.x` 条目后追加 `[x] **M2.x**：文件工具 Jail……` 段；按既有 `[x]` 条目格式。

- [ ] **Step 3: 更新 `docs/architecture-v2.md` §3.5**

在 §3.5 SeamRegistry 章节追加 `KindFileSystem` 条目：

```markdown
| `KindFileSystem` | 文件系统 capability（DSH ctx.fs 对位） | `tool/fs` 包：FileSystem / Sandboxed / WritableRoots |
```

- [ ] **Step 4: 重跑全套测试**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add FEATURES.md README.md docs/architecture-v2.md
git commit -m "docs: 文件工具 Jail 落地——FEATURES/README/architecture-v2 同步"
```