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
