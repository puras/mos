# Skills 机制调研笔记（MoS 强化方案参照）

调研时间：2026-08-17 | 范围：Claude Skills / pi-coding-agent / OpenAI Codex / Cursor / Aider / Gemini CLI / LangChain Hub / LlamaIndex

## 1. 框架对比

| 框架 | frontmatter 字段 | 激活方式 | references/按需 | allowed-tools | 懒加载策略 | 事件/观测 |
| --- | --- | --- | --- | --- | --- | --- |
| **Claude Skills (Anthropic)** | 必需 `name`、`description`；可选 `when_to_use`、`paths`(glob)、`user-invocable`、`allowed-tools`、`model`、`agent` | 模型自选（启动时只把 name+description 注入 system prompt）+ 用户 `/<name>` slash command；通过内置 `view` 工具读取正文 | `references/` 子目录文档由模型按需 Read | 子集限制继承自父级工具集 | **渐进式披露**：元数据首屏加载，正文+资源按需 Read；description+when_to_use ≤ 1536 字符 | skill 加载/调用可发事件；已与 telemetry seam 集成 |
| **pi-coding-agent (badlogic/pi-mono)** | `name`、`description`（必需），扩展字段自由 | 显式启用 → 整篇 SKILL.md 注入 system prompt | 通过子目录引用，工具实现自行按需 | 自定义工具定义 | 全量加载，无显式懒加载 | — |
| **OpenAI Codex (AGENTS.md)** | 纯 markdown，无强制 frontmatter | 全量加载进 system prompt；无显式斜杠命令 | 通过工具调用或链接 | 无工具限制 | 全量加载 | — |
| **Cursor `.cursorrules`** | 自由文本 | 全量加载 | — | — | 全量加载 | — |
| **Aider `CONVENTIONS.md`** | 自由文本 | 全量加载 | — | — | 全量加载 | — |
| **Gemini CLI (`GEMINI.md`)** | 自由文本 | 全量加载 | — | — | 全量加载 | — |
| **LangChain Hub** | prompt 模板 + 输入变量（无 SKILL.md） | `hub.push/pull` + 代码 import 引用 | — | — | — | — |
| **LlamaIndex** | `prompts/` 模块化目录 | 代码 import | — | — | — | — |

## 2. 值得借鉴的特性

1. **渐进式披露（progressive disclosure）**：Claude Skills 的设计精髓——元数据进 system prompt、正文/资源按需 Read，匹配 MoS 现状（FEATURES.md §3.3 已提 Skills 懒加载）。
2. **`allowed-tools` 子集限制**：可在技能粒度收窄工具权限，复用 `tool.Sandbox` 思路。
3. **`paths` glob 触发过滤**：让技能仅在特定目录/文件模式下激活，比"全场景激活"更安全。
4. **`references/` 子目录按需读取**：支持技能自带大型说明/API 手册，避免把全部内容塞进 SKILL.md。
5. **可观测事件**：skill.loaded / skill.invoked 挂钩 v2 `telemetry seam`（agent/telemetry 已在最近 commit 落地）。

## 3. 不适合 MoS 现状（避免 YAGNI）

- **`model` / `agent` 字段切换子模型或子代理**：MoS 已有 `llm` 装饰器 + `SeamRegistry` 按名选模型 / `SubAgent` 原语，重复抽象。
- **marketplace / 安装 / 认证 / 索引**：架构文档 §9.3 已划归平台层，SDK 不应承担。
- **Hub / 全局注册中心 / 跨进程发现**：与 MoS 嵌入式、scope 化的设计目标相悖。
- **`user-invocable` 字段**：MoS 现阶段不暴露用户斜杠命令面，延后。
- **hooks / lifecycle 配置**：v2 已有 `HookRegistry`（Guards 单调 / Hooks 可改写），无需在 Skills 层再造一层钩子。

## 4. 设计建议（v1 落地优先级）

**必须有**（最小可用）：
- `name`：唯一标识，映射 slash command 与目录名。
- `description`：触发条件的一句话描述，受字符上限约束（参考 Claude 的 ~500 中文字）。
- `schema_version`：便于未来字段演进。

**v1 推荐**（成本低、价值高）：
- `when_to_use`：自然语言细化触发场景（避免 description 超限）。
- `allowed_tools`：复用 `tool.Sandbox` 注册表，限制技能内可用工具子集。
- `references`：字符串数组，指向技能目录下子文档路径，模型按需 Read。
- `paths`：glob 数组，限定激活路径，避免误触发。
- `source`：可选，记录来源 URL / 本地路径，便于审计。

**延后**（v2+）：
- `model`、`agent`、`user_invocable`、`hooks`、`version`、`dependencies` 等。

## 5. 与 MoS 现有机制的对应

| Skills 字段 | MoS 已有/拟复用 |
| --- | --- |
| 触发加载 | `SystemAssembler` 槽位化（FEATURES.md §3.3） |
| 路径就近覆盖 | `ProjectDoc` 向上递归/就近覆盖（architecture-v2 §9.1） |
| 工具限制 | `tool.Sandbox`（§9.1 M4 沙箱） |
| 模型/子代理选择 | `SeamRegistry` + `SubAgent` 原语（§3.5 / §3.8） |
| 事件/观测 | agent/telemetry seam（5a31e28 已落地） |
| 持久化 | `Session.Events`（a122274 / 4182705 已落地） |

## 6. 参考链接

- Anthropic Skills 概念与字段规范：<https://docs.claude.com/en/docs/claude-code/skills>
- 官方仓库 anthropics/skills：<https://github.com/anthropics/skills>
- pi-coding-agent 概述：<https://github.com/badlogic/pi-mono> 系列文章（pi-coding-agent 完整详解）
- AGENTS.md vs CLAUDE.md 对比：<https://blog.csdn.net/m0_68865259/article/details/161056084>
- Skills 与 MCP/AGENTS.md 关系：<https://blog.csdn.net/weixin_32393347/article/details/159865670>