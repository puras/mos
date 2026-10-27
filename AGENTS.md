# AGENTS.md

## 硬性规则：MoGo 只读边界（禁止直接修改）

- MoS 经 `go.mod` 的 `replace` 引用本地仓库 `hegq.net/mogo`（路径 `/Users/puras/workspace/proj/106hz/mogo`）。MoGo 是独立的 AI 能力中转站项目，**禁止直接修改 MoGo 仓库中的任何文件**（代码、配置、文档、`go.mod` 等一律不得改动）。
  - 更新（2026-10-15）：协议层依赖现已迁移为 `github.com/puras/moproto`（本地 `/Users/puras/workspace/proj/hegq/moproto`）与 `github.com/puras/mog`（本地 `/Users/puras/workspace/proj/hegq/mog`）；本规则对两者同等生效，下文"MoGo"均按此理解。
- 当 MoS 依赖的 MoGo API 发生变更（签名变化 / 行为调整 / 删除导出符号等）导致编译失败或行为不符时：
  - 在 **MoS 侧**做最小适配（如补齐调用参数、调整调用方式），不改变 MoGo 的实现；
  - 需要 MoGo 侧配合的改动，只能以 **PR 建议 / 问题描述**的形式提出，由 MoGo 维护者（puras）决策实施，绝不越界直接修改。
- 发现的 MoGo 缺陷、协议演进建议、架构改进意见，一律记录为待确认建议交付给用户，不得擅自落代码。

## 依赖边界：MoS 对 MoGo 的引用范围

- MoS 只允许引用 MoGo 的协议 / 类型层包：`pkg/protocol/*`、`pkg/usage` 等 `pkg/` 下的纯定义包。
- 不得引用 MoGo 的 `internal/` 或网关实现层，防止"协议共享"退化为"框架依赖网关实现"。
- 对外发布前需将协议层拆分为独立 module 或确认 MoGo 发布到可达仓库（当前开发阶段维持 `replace`，此事项为发布前待办）。

## Git

本仓库可能有多个会话同时改同一棵树（用户在多个终端里并行干活，各改各的文件）。**凡是会碰到本次会话之外的文件**（未暂存 / 已暂存 / 未跟踪）的 git 操作——`git add -A`、`git clean`、`git reset --hard`、`git checkout .`——都会踩掉别人的工作。这不是理论风险：S5g 演示的 lab 修复曾被一次 `git add -A` 卷进提交，**同一个错犯了两次**（见提交 `38f1e8e`）。

### 提交

- 除非用户明确要求，不主动提交。
- 只提交**本次会话自己改动**的文件。
- 显式路径暂存：`git add <path1> <path2>`；禁止 `git add -A` / `git add .` / `git add -u`。
- 提交前跑 `git status`，逐行确认暂存区里只有本次改动的文件——别人的改动、生成物、临时文件都不该在里面。
- 一次提交只做一件事：互相独立的改动分开提交（DSH：split independent changes）。
- 提交信息格式：`<type>(<scope>): <中文摘要>`——type 取 `feat` / `fix` / `docs` / `refactor` / `test` / `chore` / `perf`；scope 是包名或目录（`agent/prompt`、`tool/builtin`、`examples/chat`）；摘要一句话说清改了什么。破坏性变更在正文写明迁移方式。
- 提交前自检：`go build ./... && go test ./...` 通过；文件以恰好一个换行结尾（`git diff --cached --check` 无输出——DSH 用它做 pre-commit 门）。

### 禁止执行（毁掉别人的工作或绕过检查）

- `git reset --hard`、`git checkout .`、`git checkout -- .`
- `git clean -fd` / `git clean -fdx`
- `git stash`（含 `git stash pop`）——它会把别人的改动一起藏起来
- `git add -A`、`git add .`、`git add -u`
- `git commit --no-verify`
- `git push --force`——改写远端历史只能用 `git push --force-with-lease`；远端已经前移就中止并说明（DSH：never raw `--force`，abort on remote movement）

### 冲突与中断

- 只解**自己改过**的文件里的冲突。
- 冲突落在没碰过的文件上 → 中止（`git rebase --abort` / `git merge --abort`）并向用户说明，不替对方取舍。
- 删除类命令（含 `git rm`）在本仓库是**需人工确认**的操作（示例宿主的规则键在「删除命令」上，见 `examples/chat/guard.go`）；**被拒后不要拆解命令绕过**（把 `git rm -r` 改成逐个 `git rm` 达成同一目的），唯一合法路径是征得同意后**原样**重新发起。

### 分支与远端

- 未经用户明确要求，不切分支、不 `gh pr checkout`、不把工作树挪到别的分支。
- 看别人的分支 / PR 内容用只读手段：`git show <ref>:<path>`、`git diff <ref>`、`gh pr view`、`gh pr diff`——不切分支也能读。
- 不主动 `git push`；要推分支或开 PR，先问用户。
- MoGo / moproto / mog 是**只读**的外部仓库（见上文硬性规则）：不要在那些仓库里 stage / commit / push。

### 与用户指令冲突时

用户的指令与本文件任何规则冲突时，先说明冲突点、征得明确同意后再执行——不要默默绕过（PI 的 User Override 规则）。
