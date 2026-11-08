# MoS PRD（产品需求文档）

> 版本：v0.2（草案）｜ 日期：2026-11-04 ｜ 维护：puras
> 更新记录：**2026-11-07** —— **FR-E10 重裁为「宿主职责」（已决，零代码）**：FR-E10 原写「M2.8 收尾：Approval TTL 自动超时、checkpoint 加密/签名」，两半都要改。**①「checkpoint 加密/签名」失去对象**：FR-G3 重定后**没有 checkpoint 文件**（旧 checkpoint 的 `Calls` / `Decisions` / `Snapshot` 都已住在会话事件日志里），故**不做**哈希链 / 签名机制——防篡改的「锚」只能由宿主保管（存在日志之外），否则能改整个文件的人会重算整条链；真要强保证，做在**存储层**（文件权限 / 加密卷 / 只追加）比做在事件字段里更有效。日志的**机密性 / 防篡改 / 留存归宿主**，MoS 只保证事件结构与审计链**结构**完整（`approval/asked` 与 `approval/decided` 成对、审计写失败即中止执行）。**②「Approval TTL」框架侧本就没有**：`approval.Service.Decide` 只阻塞等应答方（超时是宿主 `Answerer` 自己的事），框架只做 fail-closed 兜底——ctx 取消 → `cancelled`，通道坏 / 未装应答方 → `unavailable`，两者**一律拒绝执行**。同步：ARCHITECTURE §3.17 拍板 7 的「待复核」改为已决；平台契约 §7 增两条边界外事项；PRD 风险表新增 **R7**（日志可被改写 → 归宿主）。
> 更新记录：**2026-11-07** —— **FR-G3 收口（S9.3）：kill -9 恢复 E2E 演练**：演练用**真进程**——子进程 = 测试二进制自身（`-test.run` 指向子进程用例），卡在**审批等待**里被父进程 `SIGKILL`（工具永远不返回）；父进程按宿主流程走一遍：重开会话（`session.Open` → `Restore` 给孤儿轮合成 `turn/end{reason:"interrupted"}`，**只补事件、不回写日志**）→ 派生出一个**未决审批** + 一个**悬空调用** + 轮次号 1 → **显式指示** `Agent.Resume` → 沿用轮次 1、原样重放载荷、走同一执行通道、**重走审批**（重新问了一次——原先那条没人答）、回填**真实** `tool/result` → 继续问模型 → 本轮正常 `turn/end{completed}` → 落盘日志里悬空调用清零、`InterruptedTurn()` 归 0。顺带修一处口径：`InterruptedTurn()` 现在认「以 `interrupted` 收尾」的轮次（重开会话时合成的那种收尾——那一轮还挂着悬空调用，仍然算被中断）；正常收尾（completed / error / terminated）的轮次不算。**诚实的边界**：①原先那条**未决审批仍留在未决列表里**（日志是事实：问过了，没人答）——续跑**不替宿主回答人**（不写 `decided`），由宿主按「那次调用是否还悬空」决定要不要撤下；②续跑会把那次调用**再执行一遍**（演练里父进程用的是同名但**立即返回**的工具实现），shell 之类的外部副作用可能重复，文件类靠观察门 CAS。**FR-G3 至此收口**；FR-E6（可恢复运行目录）的框架侧前置已就位。另附**可跑示例** `examples/recovery`（`go run .`，**不需要 API Key**——用假模型把整条链路走一遍：子进程卡在审批等待 → 父进程 SIGKILL → 重开会话派生现场 → 显式续跑 → 本轮正常收尾）。
> 更新记录：**2026-11-07** —— **FR-G3 第二片落地（S9.2）：宿主显式续跑那一次调用**：公开入口 = 装配层 `mos.Agent.Resume(ctx, sess, mos.ResumeOptions{Turn, Calls})`（**不新增 `Config` 字段**——恢复是「打开会话后的一个动作」，不是装配能力）。**不自动续跑**（拍板 ⑥）：不调 `Resume` 就没人续（守护测试）。驱动接缝 = `driver.RunContext.Resume`（**续跑模式位**，不是新增 `Driver.Resume` 方法——续跑与普通轮**共用同一条决策循环**，另开方法就要把循环抄一遍）；驱动**不得忽略**它（当普通轮跑 = 宿主要求续跑却开了新轮）。续跑做什么：① **沿用原轮次号**（不开新轮、不记新的 user 消息——那一轮的输入已在日志里）→ 先验复用的 `(Turn, CallID)` 匹配得上，已批过的调用**不再问人**；② 取该轮的悬空调用，载荷**原样重放**（**不追加** `tool/call`——载荷冻结）；③ 走**同一条** `execpath.ExecuteBatch` 通道（守卫裁决 / 审批 / 调度语义一致，续跑不是特权路径）；④ 回填**真实** `tool/result`（不是投影里的合成失败回执）；⑤ 进决策循环继续问模型 → 本轮正常 `turn/end`。选轮次 = `session.InterruptedTurn()`（有 `turn/start` 无 `turn/end` 的最后一轮，`Turn = 0` 时自动选）。**没有可续的 → 明确报错 `react.ErrNothingToResume`**，那一轮**保持挂起**（不收尾——收尾等于把「还欠一次调用」的事实抹掉）。**幂等边界（拍板 ②）**：文件类重复执行由既有观察门 CAS 拒（读后写 / 读后改用版本令牌比对）；**shell 等外部副作用不受保护**（可能重复写文件 / 发请求，**不猜「是否已经执行」**），观察门的跨进程竞争暂不提供强保证。**未做**：S9.3（kill -9 E2E）。分析见 `docs/ARCHITECTURE.md` §3.17。
> 更新记录：**2026-11-07** —— **FR-D3 SubAgent 第三片落地（S8.5-C）：装配层接线**：`Config.SubAgents`（`Dirs` / `Reader` / `MaxCatalogBytes` / `Timeout` / `Runner`）——nil = 不装这个能力（**零行为变化**：没有工具、提示词不多、快照写明未装配）。默认执行体（根包实现，`Runner` 可由宿主替换）：**独立子会话**（新的 `session.Session`，同一持久化后端，id = `<父会话>-sub-<名字>-<序号>`——子代理的中间推理与工具调用留在子会话日志，不进父上下文）+ **白名单子注册表**（再恒定扣掉 `subagent__*` 与 `tool_search`：深度 0 不许再委派、工具面不经 search）+ **模型覆盖**（声明就用目录里那个，没声明跟父的当前模型；单模型模式声明了模型 → 装配期报错）+ **子提示词**（人设 + 项目约定 + 回执格式，**不复用父的**——父的含技能清单 / MCP 摘要，子代理没那些工具，列了骗模型）。子代理工具按名注册（`subagent__<name>`，**不进 deferred**）；清单进**静态**系统提示词新分节 `<available_subagents>`（`agent/prompt` 新增 `OrderSubAgents = 875` + `AddSubAgents`）；`Describe()` 快照写明子代理数、名字、描述、白名单、模型覆盖、拆分判据诊断与装配期告警；**模型覆盖装配期校验**（声明了宿主目录里没有的模型 → 装配失败，假能力 fail-closed）。超时默认 5 分钟（`DefaultSubAgentTimeout`：子代理跑的是一整轮循环，不能套 MCP 的 60s，但也不能不限）。回执解析：子代理被要求回答 JSON，**不是 JSON 就整段当 `summary`**（形状由框架保证，内容是自述，NFR-5 父模型要复核）。**未做**：README / 示例 / 文档收口（S8.5-D）。
> 更新记录：**2026-11-07** —— **FR-D3 SubAgent 收口（S8.5-D）：文档与示例**：README（示例表 + 路线图 M3 行）、ARCHITECTURE §3.18（切片表 + 状态表）、PRD（本记录）与一页说明 `docs/s8.5-subagent.md` 状态同步；新建可跑示例 `examples/subagent`（两个子代理声明 `agents/reviewer` / `agents/log-triage` + 装配 + 运行）。**FR-D3（S8.5）至此收口**；M3b（AgentTeam / 并行 DAG / 合约 + Elo）仍未开始。
> 更新记录：**2026-11-07** —— **FR-D3 SubAgent 第二片落地（S8.5-B）：子代理 = 一个普通工具**：`agent/subagent` 新增 `Tool`（实现 `tool.Tool`，名字 `subagent__<name>`，参数 `task`（必填）+ `context`（可选），描述里写明「summary 是子代理自述、findings 是待复核的线索」）+ `ChildRegistry`（**白名单交集真正落地**：子代理拿到的是**新注册表**，只有「声明白名单 ∩ 父可见工具集」里的工具——父注册表里它拿不到的工具**压根不存在**；**deny 优先**，被 policy 挡掉的工具进不了交集；父的 deferred 未激活工具也不进——子代理工具面不经 `tool_search`）+ 固定结构回执 + **工具级超时与取消透传**（超时 = `status: timeout` 的**正常结果**，不是父轮次失败；父轮次取消 → 子代理跟着取消；执行体不理会 ctx 时超时照样返回）。**错误分层**：框架层错误（参数坏 / 定义读不出来 / 没有 Runner）返回 Go error；子代理层失败（跑挂了 / 回执不合法）返回 `status: error` 的正常回执——父轮次不因此失败。**独占**：不实现 `tool.ConcurrencySafe` → 默认串行（子代理内部要发模型请求）。**切片口径修正**：原 S8.5-B 写的「独立子会话」**改由 S8.5-C 装配层落地**——造子 Agent 要拿到整套 Agent 机器，而 `agent/subagent` import 根包就成环；框架层交付的是 `Runner` 接缝（宿主注入执行体）。**未做**：装配层接线（`Config.SubAgents` + Runner 实现 + 清单进静态提示词 + `Describe()`，S8.5-C）、示例与文档收口（S8.5-D）。
> 更新记录：**2026-11-07** —— **FR-D3 SubAgent 第一片落地（S8.5-A）**：新包 `agent/subagent`——**只定义声明与接缝，不造 Agent**（治理规则 5 + 避免根包与子包成环）：`Definition`（frontmatter：`name` / `description` / `model` / `tools` + 指令正文现读）、`Catalog`（目录发现 `<subagent-dir>/<name>/AGENT.md`，装配期固定、降级而非失败）、`Render`（清单进**静态**系统提示词，预算降级照抄 Skills）、`Runner`（**宿主注入**的执行体接口，契约：同步、失败返回 `status: error` 的回执而非报错、取消透传）、`Result`（固定结构 `{status, summary, findings[]}`，`summary` 标为**子代理自述**）。另落三项判定：工具面求交 `ResolveTools`（声明白名单 ∩ 父可见集，**deny 优先**——父可见集已过宿主策略，声明不能放大策略）、模型覆盖校验 `CheckModel`（声明了却没装 = 假能力，**装配期报错**，不静默降级到父模型）、拆分判据 `SplitDiagnostic`（instructions / tools / model **任一不同即可**；三者全同则给诊断「拆了只是多一层开销」）。frontmatter 解析与 frontmatter 辅助函数**与 `agent/skill` 各写一份**（字段集不同；新建公共包需 PRD 有 FR 指名，治理规则 1）。**未做**：工具注册（S8.5-B）、装配层接线（S8.5-C）、示例与文档收口（S8.5-D）。说明见 `docs/s8.5-subagent.md`。
> 更新记录：**2026-11-07** —— **FR-G3 第一片落地（S9.1）：派生读接口 + 先验决定复用**：`session` 新增**四个纯读派生接口**（不写日志、不改状态、不新增事件类型）：`PendingApprovals()`（有 `approval/asked` 无 `approval/decided`，按 id 配对）、`ApprovalOutcome(id)`、`ApprovalDecision(turn, callID)`（**比原设计多一个**——先验复用的匹配键是 `(Turn, CallID)`，而按 id 匹配等于永远匹配不上）、`HangingCalls()`（有 `tool/call` 无 `tool/result`，按 CallID 配对；与未决审批能靠 CallID 对上）。`agent/approval` 新增**注入式先验查询** `Prior`（`Service.SetPrior`）+ `PriorDecision{Outcome, ReusedFrom}`（原设计的 `(Outcome, bool)` 载不下被复用的那次 id，拍板 ⑤ 要求 `reusedFrom` 指回原决定 → 签名必须带 id）+ `ParseOutcome`；命中即**复用决定、不再问人**，复用**记新 id**、审计记一对 `asked`/`decided`（两条载荷都带 `reusedFrom`，保持「每个决定都有配对的 asked」不变式）。**命中条件两个都要**：①日志里有 `(Turn, CallID)` 对应的已决记录；②那次调用**仍悬空**（没有 `tool/result`）——②是「批准只对本次调用生效」（DSH allowed-once）的护栏，否则模型换个参数重发同一个 callId 就能白嫖一次批准。**只有 `approved`/`rejected` 可复用**（`cancelled`/`unavailable` 不是人的决定）。胶水在装配层（根包 `prior.go`：`session` 派生接口 → `approval.Prior` → `driver.RunContext.PriorApproval`），**框架层不依赖 session**（治理规则 5）。**未做**：S9.2（react 续跑那一次调用 + 幂等边界文档）、S9.3（kill -9 E2E）——**当前没有公开入口能把同一轮重新跑起来**，先验复用的价值要等 S9.2 才显形。分析见 `docs/ARCHITECTURE.md` §3.17。
> 更新记录：**2026-11-06** —— **FR-G3 挂起-恢复重裁（已拍板，未实现）**：**「日志即 checkpoint」**——不另设 checkpoint 文件、**不建 `agent/checkpoint` / `agent/handle` 两个包**、不引入 PI 的 phase 状态机；恢复所需的事实**已在会话日志里**（`approval/asked`/`decided` 带 `Turn` + `CallID`；`tool/call` 无 `tool/result` = 悬空调用），要补的只是**读（派生）与补（续跑）**。**三片**：S9.1 = `session` 三个纯读派生接口（`PendingApprovals` / `ApprovalOutcome` / `HangingCalls`）+ `agent/approval` 注入式先验决定复用（`Prior`，匹配键 `(Turn, CallID)`，复用时**记新 id** + 载荷加 `reusedFrom`）；S9.2 = react 把「合成失败回执」那条路径改成「按宿主指示续跑那一次调用」+ 幂等边界文档；S9.3 = kill -9 E2E。**七条拍板（全部按建议）**：①恢复语义 = **默认只报告 + 宿主显式指示才续跑那一次调用**（整轮重跑是宿主手动选项）；②shell 副作用重复执行接受并写进「诚实的边界」，**不做**「跳过已执行调用」机制；③**不提供** MoS 侧 run 句柄（归平台层 FR-E6）；④排期 = S9.1 插在 S8.5 期间、S9.2/S9.3 在 M3b 之前（偏离原「先于 M3a」：M3a 已开坑）；⑤复用时记新 id + `reusedFrom`（**不新增事件类型**）；⑥**必须宿主显式指示**才续跑；⑦验收措辞改为「kill -9 → 列出未决/悬空 → 宿主指示 → 续完 → 本轮正常收尾」。**关键约束**：决定复用只在**同一轮内**成立（整轮重跑 turn 号变 → 匹配不上 → 审批必然重新问）→ **续跑与复用必须配套**。**宿主核查（mos-web，只读）**：它的 ask/approval 待批状态只在进程内存（`ApprovalHub.pending`；注释自陈「权威记录仍在 `approval/asked` 事件里」），审批等待上限 `MOS_APPROVAL_TIMEOUT` 默认 300s 且超时按拒绝，重开会话只把孤儿 turn 合成 `interrupted`（不续跑）——故 MoS 侧净增量 = **派生原语 + 续跑语义**，**不是**「审批跨进程留存」。分析见 `docs/ARCHITECTURE.md` §3.17。
> 更新记录：**2026-11-06** —— **FR-D3 SubAgent 设计分析已出 + 六条拍板已定（未实现）**：PI/DSH 源码级对账落 `docs/ARCHITECTURE.md` §3.18（一页说明 `docs/s8.5-subagent.md`）——实查结论：**PI 没有子代理委派层**（只有可复用的 agent loop + codemode 替代路径），**DSH 有完整 `packages/subagent/*` 包族**（provider 接缝 + one-shot/continuable + 能力探测 fail-closed + 深度闸 + `toolFilter`/父策略向下委派 + 异步 id 通知）。**拍板（六条全按建议）**：①形态取 **agents-as-tools + 声明式 frontmatter 定义**（对齐 `.claude/agents/`，两家都没有 → MoS 自创），工具名 `subagent__<name>`；②交付取**同步**（不抄 DSH 的异步 id + 通知，MoS 没有运行期消息回注通道）；③白名单与父策略**取交集、deny 优先**（声明不能放大宿主策略）；④允许声明 `model`，但宿主目录里没有 → **装配期报错**（声明了却没装 = 假能力）；⑤子代理**不能再委派**（深度 0）；⑥子代理工具面**不经** `tool_search`，子代理工具本身**不** deferred 化。另两个自创点：回执固定结构且**自述与事实分离**（NFR-5：子代理自述不进 `TaskState`，父模型要自己复核）、**不加** `subagent/*` 事件（治理规则 2 预算制，那一档留给 M3b 的 AgentTeam）。**分期勘误**：§5 分期表把 FR-D3 列在 M3a，§4 状态行原写「M3b（SubAgent / AgentTeam / 并行 DAG）」——以 §5 为准，**FR-D3 属 M3a 收尾**，AgentTeam（FR-D6）/ 并行 DAG（FR-D4/D5）/ 合约 + Elo（FR-D7/D10）属 M3b。
> 更新记录：**2026-11-06** —— **FR-D8 / FR-C 收尾（四项 E2E + 四处修复与口径变更）**：①**激活在同轮内生效**——驱动每轮重取声明清单，`tool_search` 激活后下一次请求即声明（原先要等下一个 turn；对齐 PI 的「下一轮请求声明给模型」）。②**装配期 `Config.Policy` 守卫 fail-open 已修**——装配中途的 `a.rt` 是空壳（`Guards` 为 nil，`GuardBus.Add` 是 no-op），`SetPolicy` 挂守卫等于丢掉：拒绝名单里的工具「模型看不见却调得动」；改为 `a.rt` 装好后再挂（与热替换同一条路）。③**`DeferredTools()` 与 `List()` 同口径**——被策略拒绝的 deferred 工具不再能被 `tool_search` 搜到并激活（「看不见」与「搜不到」一致）。④**401/403 恒归认证类**——认证层无条件装上，不再只在配了凭据时才装。另补三项验收：真 MCP 工具经 `tool_search` 激活 E2E、MCP 工具受 policy/guard 拦截 E2E、streamable-http 真服务端联调（官方 SDK handler + `httptest`）。说明见 `docs/s8.2-mcp.md`。
> 更新记录：**2026-11-04** —— **FR-D8 可用性优化第四片（S8.4-E）**：**MCP 资源工具**——三个共享工具 `mcp_resource_list` / `mcp_resource_templates` / `mcp_resource_read`（显式 `server` 参数，工具面恒为 0 或 3，不随服务器数增长）；只对「非 hidden 且确实有资源」的服务器可达（hidden 资源不泄露）；文本直返、二进制降级 OpaquePart 占位；SDK 类型不外泄（`Client` 增三个资源方法，投影成 MoS 类型）。名字带 `mcp_resource_` 前缀（两家参考源不带前缀、与 `mcp__<server>__<tool>` 不统一）。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D11 可用性优化第三片（S8.4-D）**：**`allowed-tools` 求交诊断**——技能激活时把声明的 `allowed-tools` 与当前可见工具集求交，缺失写进新事件 `skill/allowed-tools-mismatch` 与 `Describe()` 快照；**只诊断、不收窄**（工具面不变，模型返回值不出现诊断文案）。注册表增 `Visible()`（模型可见工具名，与 `List()` 同一过滤）；`skill.Options.ToolProbe` 由装配层注入探针。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D11 可用性优化（S8.4-C）**：**技能正文调用时现读 + 宿主显式重扫**——目录扫描只取 frontmatter 元数据与正文路径（`Skill.Body` → `Skill.BodyPath`），正文在 `skill` 工具 / `/name` 展开时现读（改了 `SKILL.md` 立刻生效）；`Catalog` 改快照持有者（`atomic.Pointer[Snapshot]`，重扫整体换指针）；`Agent.ReloadSkills()` 显式、幂等、全量重扫，**摘要不含正文**（改正文不必 reload）；清单变化走 **user 侧替换目录消息**（静态提示词不重渲染，保 prompt cache）；正文预算改为调用时校验。**破坏性变更**：`ExpandSkill` / `ExpandInvocation` 增返回 `error`。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 可用性优化第二片（S8.4-B）**：**认证可诊断 + `TokenProvider`**——`ErrorKind` 新增 `auth` 类（HTTP 401/403 与取凭据失败）；`ServerConfig.TokenProvider`（每次请求取一次，**不缓存不刷新**，空串 = 不注入头）；凭据注入点 = HTTP RoundTripper（`tool/mcp/auth.go`，把状态码变成结构化分类，而不是解析 SDK 的错误文本）；静态 `Headers` 此前是死配置，本片接线。**不做** OAuth 流程（凭据生命周期归宿主）。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 可用性优化第一片（S8.4-A）**：**MCP 服务器摘要进静态系统提示词**——`agent/prompt` 新增 `mcp_servers` 节（`OrderMCPServers = 850`）、`tool/mcp/summary.go` 渲染器（只列连接成功且有 deferred 工具的服务器；摘要取静态 `ServerConfig.Description` 首行；整节 ≤4KB、单条 ≤250 字节；渲染层自己排序保证确定性）、装配层把提示词渲染点后移到 MCP 装配之后。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.4-usability.md`。
> 更新记录：**2026-11-04** —— **FR-D8 MCP 接入落地（S8.2 + S8.3）**：新包 `tool/mcp`（**官方 Go SDK** `github.com/modelcontextprotocol/go-sdk` v1.8.0，stdio + streamable-http 两个 transport，协议基线 `2026-07-28`）+ 工具适配（`mcp__<server>__<tool>` 命名、schema 直通、结果投影）+ 两阶段原子换代（换代失败保留上一代）+ 装配入口（`mos.Config.MCP`：一开即装、默认 deferred、hidden 档、失败隔离、服务器说明每会话一次性 user 侧注入、`Describe()` 快照、`Agent.Close()`）。**依赖纪律破例**：新增 9 个传递依赖，SDK 只在 `tool/mcp` 内部使用、类型不外泄。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.2-mcp.md`。
> 更新记录：**2026-11-04** —— **FR-D11 Skills 重做落地（S8.1）**：新包 `agent/skill`（frontmatter 解析 + 目录发现 + `<available_skills>` 清单 + `<skill_content>` 正文 + 用户显式 `/name` 展开器 + 本轮激活状态）+ 专用 `skill` 工具（正文按需加载，走普通工具结果/历史）+ `skill/activate` 事件 + 装配入口（`mos.Config.Skills` / `Agent.ExpandSkill`）；**技能目录不再要求登记为文件读根**——目录列表 = 宿主声明的授权边界，读操作走注入的 `Reader`。分析见 `docs/ARCHITECTURE.md` §3.16，说明见 `docs/s8.1-skills.md`。
> 更新记录：**2026-11-03** —— 按重建树实况**回填与收口**：§4 改为「重建树现状 / 旧树未带回」两栏对照（原文整段描述的是 10-15 前的旧树，含重建未带回的 EventBus / seam / scope / telemetry / 4 驱动 / Skills / RAG / Doom Loop 等）；新增 **FR-G（重建期补齐）** 三条——FR-G1 上下文文件、FR-G2 联网工具、FR-G3 挂起-恢复（checkpoint + handle，**重建未做**，是 FR-E6 的前置）；FR-A1/FR-A2 标 ✅（协议层已独立发布、v0.1.0 已发）；失效文档引用（`docs/architecture-v2.md` / `docs/sdk-contracts.md` / `docs/agent/*` / `FEATURES.md` / `examples/{cli,protocli,agentcli}`）换成重建树实际文件。
> 更新记录：**2026-11-02** —— Skills 状态从「已交付（M2.6）」改为 **FR-D11 待建**（重建树无实现；用户拍板：按新架构重做，不移植旧码）；FR-D8 五步交互的「请求制定」改为「**暴露策略（exposure）**」；开放问题 2（MCP transport）定「先 stdio」、5（SubAgent/MCP 先后）定「MCP 先」。分析见 `docs/ARCHITECTURE.md` §3.16。
> 需求权威来源：`docs/ARCHITECTURE.md`（目标架构与不变式，含各子系统契约）+ `docs/platform-contract.md`（平台契约：事件 JSON / 状态序列化 / 并发）+ `docs/assembly.md`（装配层）+ `README.md` 路线图；本文将其整理为可跟踪的产品需求视图。
> 状态约定：**重建树现状见 §4**（§4.1 分层 / §4.2 能力盘点按「重建树现状 vs 旧树未带回」两栏写）；待建需求见 §5（FR-A ~ FR-G）+ §6（NFR）。API 细节以 `docs/ARCHITECTURE.md` 与源码为准。

---

## 1. 产品概述

### 1.1 一句话定位

MoS（取自 Sapient）是基于 Go 的**智能体基础框架（Agent Base SDK）**：提供统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御，帮助快速构建智能体应用与智能体平台。

### 1.2 解决的问题

在模型能力之上，长程任务的可靠性瓶颈在 **harness**（驾驭层）：上下文腐化、工具误用、不可验证的"自我声明式完成"、无法断点恢复、缺乏审计。MoS 把这些工程问题沉淀为可复用的框架能力：

- **状态写文件不写上下文**：`TaskState` 外置、会话事件日志（append-only）是唯一事实来源；
- **只接受环境独立验证过的事实**：`Auditor` 审计门，模型自述不算数；
- **上下文越少越好**：截断（M2.3）+ fresh-context 压缩（M2.5）+ 槽位式系统提示词（M2.6）+ Skills 懒加载（**M3a 已落地，FR-D11**）；
- **能写成 Linter 的约束别写成文档**：`Policy` 执行前拦截 + `GuardBus` 单调守卫 + `Sandbox` 执行中隔离。

### 1.3 三层职责边界（MoS 是 SDK，不越界）

| 层 | 归属 | 职责 |
| --- | --- | --- |
| MoS | 本仓库（SDK 内核） | Agent 任务循环、工具系统、会话/审计/状态、上下文工程、钩子守卫、审批与断点恢复、事件观测 |
| SaaS 平台 | 独立平台（MOSSCloud） | HTTP/WS API、多租户、鉴权、任务调度、计费、Dashboard、对象存储、人工确认通道 |
| 协议层 | `github.com/puras/moproto`（**已独立发布**，v0.1.0） | LLM 协议归一、密钥管理、模型路由；MoS 只消费其纯定义包，仓库只读 |

MoS 明确不做：HTTP 服务、多租户、任务队列、计费、可视化；不操作数据库（持久化只提供 `Persistence` 接口 + 内存/JSONL 默认实现，数据库后端由业务层注入）；不做运行期插件热加载（只做装配时选组件）。

### 1.4 设计理念（需求推导原则）

Harness Engineering 四铁律 + Loop Engineering 九杠杆 + Audit Gate 审计式状态迁移 + Graph Engineering 判据门控（满足分支/并行/持久/可审计才引入显式图）+ 补偿面理论（harness 随模型变强主动拆组件）。详见 README「工程参考」章节。

---

## 2. 目标用户与使用场景

| 用户 | 场景 | 入口 |
| --- | --- | --- |
| 应用开发者 | 在 Go 工程中嵌入一个开箱即用的智能体（工具 + 安全策略 + 审计） | `mos.New` 一行装配（根包） |
| 平台开发者 | 自建智能体平台：声明式配置装配、挂起-恢复、事件流对接观测 | `mos.Config`（结构进，格式归宿主）、`session.Persistence`；挂起-恢复待建（FR-G3） |
| 框架研究者 | 理解/扩展 agent 运行时（驱动、钩子、守卫、安全边界） | `docs/ARCHITECTURE.md`、`docs/tutorial.html`、`examples/chat` |
| 业务集成方 | 换模型/换后端而不改业务代码 | `llm.Registry`（协议注册表）+ `llm` 装饰器（Retry / Logging / Timeout / Fallback / Cache） |

---

## 3. 产品目标与非目标

### 3.1 目标（G）

| 编号 | 目标 | 度量 |
| --- | --- | --- |
| G1 | 循环可替换：ReAct / Plan&Execute / Reflection / Scripted 按 `LoopDriver` 实现，任务可路由切换 | 新增一种循环 = 一个 `LoopDriver.Run` 实现，不污染共享设施 |
| G2 | 环节可改写：模型输入 / 工具执行前后等环节可被钩子改写，安全约束不可被钩子绕过（HookBus / GuardBus 双轨制） | 纵深防御测试覆盖（守卫绕过不可行） |
| G3 | 提供者可替换：Sandbox / Memory / Retriever / Policy / Driver 经 `agent/seam` 按名注册、YAML 装配 | 纯配置可组装可运行 Agent（`config.Assemble`） |
| G4 | 会话可重放可恢复：append-only 事件日志 + `Restore` + Checkpoint 挂起-续跑 | 崩溃恢复测试（孤儿 turn 合成 interrupted）；审批挂起-恢复 E2E |
| G5 | 安全纵深：Policy（规则层）→ Sandbox（隔离层）→ 审批（人工层）→ 审计（验证层）四层防线 | fail-closed 纪律全绿（NFR-1） |
| G6 | 近零依赖：SDK 仅依赖协议层库 + `google/uuid` + `gopkg.in/yaml.v3` | `go.mod` require 审计 |

> **实际覆盖度（2026-11-03 对账重建树）**：G2 / G5 / G6 已闭环；**G1** 只有 ReAct 一个驱动（PlanExecute / Reflection / Scripted 待回填）；**G3** 无 `agent/seam` 与 YAML 装配（`agent/policy` 支持 YAML，框架层未做）；**G4** 的「Checkpoint 挂起-续跑」未做（→ FR-G3）。README 曾写「S1–S7 完成即覆盖 G1–G6 最小闭环」，按重建树实况**不成立**，以本表为准。

### 3.2 非目标（不做）

产品层（Web UI / 网关 / 调度 / 多租户 / 计费）、数据库驱动与迁移、运行期 DI 容器、模型训练与微调、CLI 产品化（`examples/*` 仅为示例与学习材料）。

---

## 4. 现状盘点（重建树 vs 旧树基线）

### 4.1 分层架构（重建树现状）

```text
装配层：mos.New（根包；模型 + 工具 + 安全 + 上下文工程一次装成，nil 用框架默认）
驱动层：LoopDriver（ReAct 已落地；PlanExecute / Reflection / Scripted 待回填）
执行通道：agent/execpath（守卫裁决 → 屏障式有界并发 → 按调用序回填，各驱动共用）
基础设施：session 事件日志 + agent/hook（HookBus / GuardBus）+ agent/prompt + agent/contextfile
能力层：llm（+transport 共用 envelope）/ tool（builtin + web + searchtools）/ filesystem / sandbox / httpx
协议层：moproto（外部已发布仓库，只读）
```

> 旧树（`main_bak1015`）多出的层——EventBus / Scope / Telemetry seam / `agent/seam` / `agent/config` / DriverRouter / `memory` ——**重建未带回**。是否回填按 PRD 逐项决定：目前只有 **FR-G3（挂起-恢复）** 明确要回填，其余待定（§4.2 末列逐项标出）。

### 4.2 能力盘点（按域）

| 域 | 重建树现状（2026-11-03） | 旧树有、重建未带回 |
| --- | --- | --- |
| LLM 接入 | `llm.ChatModel` 抽象；OpenAI / Anthropic / Gemini 三适配器（函数调用 + 流式 + 用量归一 + FinishReason 透传）；`llm/transport` 共享 HTTP envelope；`llm.Registry` 协议注册表；5 个装饰器（Retry / Logging / Timeout / Fallback / Cache）；`StatusError` 结构化状态码 | — |
| 工具系统 | `tool`：结构体 tag → JSON Schema（注册时缓存）+ 并发 opt-in 分类器 + deferred 懒加载（`tool/searchtools` 的 `tool_search`，BM25）；`tool/builtin` 9 个工具（`current_time` / `read` / `write` / `edit` / `grep` / `glob` / `ls` / `grant_access` / `shell`）；`tool/web`（`web_search` / `web_fetch` + SSRF 防御） | `tool/extension`（5 家搜索提供商）、`tool.SequentialTool` 串行回退 |
| 安全 | `agent/policy` 名字级黑白名单（fail-closed）+ 参数级决策点（deny → 守卫 / ask → 审批，规则归宿主）；`agent/hook` HookBus（可改写）+ GuardBus（单调裁决）；`filesystem` 双层校验 jail + 运行期可加根（`RootSet`）+ 写前观察门；`sandbox` 进程隔离（超时 + 进程组击杀 + 输出上限 + 资源限制 + 环境裁剪）+ macOS Seatbelt 路径强制；`agent/approval` 四档人工审批 | `DefaultPolicy` 分级（绝对危险 / 受限危险）、`request_human_input` 工具 |
| 会话与状态 | `session` append-only 事件日志 + 确定性投影 + JSONL 持久化（fail-fast）+ 崩溃恢复（孤儿 turn 合成 interrupted）+ `TaskState` 外置 + `agent` 的 Auditor 审计门 | fork（Header 已预留 `parentId` / `seedSeq` 字段，能力未做） |
| 上下文工程 | `agent` 截断（行/字节双限 + 全文落盘可回读）+ fresh-context 压缩（token 预算触发 + 模型生成摘要落日志 + 投影遮蔽）；`agent/prompt` 分节装配（Add / Set / Complete + 工具自带指引通道）；`agent/contextfile` 上下文文件发现（AGENTS.md / CLAUDE.md 向上递归 + 同目录 override + 字节预算）；**`agent/skill` 技能（FR-D11，2026-11-04 落地）**：目录发现 + 清单进系统提示词 + 专用 `skill` 工具按需加载正文 | 40% Dumb Zone 告警、Doom Loop 守卫、RAG / `memory.Retriever` |
| 循环生态 | `agent/driver` LoopDriver + `agent/driver/react` ReAct；批量工具三阶段执行（preflight → 屏障式有界并发 → 按调用序回填） | PlanExecute / Reflection / Scripted 驱动、HybridRouter、`agent/driver/scripted` |
| 生命周期 | `agent/approval`（问人由宿主注入 + 审计落会话日志）；`filesystem.RootSet` 运行期加根 | `agent/scope`；`agent/checkpoint`、`agent/handle`（→ FR-G3；**2026-11-06 重定后不建这两个包**：恢复改由会话日志派生 + 宿主指示续跑） |
| 可观测 | 事件类型覆盖 turn / assistant / tool / approval / compaction；`sess.Events()` 可遍历；`Describe()` 装配快照（装了什么 + 没装什么 + 降级警告） | `EventBus` 四模式、`Observer` 回调、Telemetry seam、`fs/resolve` 审计事件 |
| 装配 | 根包 `mos.New(mos.Config)` + 运行期旋钮（并发 / 决策轮次 / 换模型）+ `mos.NewModel` / `session.Open` / `builtin.ToolsIn` 样板收进框架；`mos.Version` 版本常量 | `assistant.Config`、`agent/seam` + `agent/config`（YAML → Agent）、声明式驱动选择 |
| 示例与文档 | `examples/minimal`（装配层最小示例）、`examples/chat`（全功能演示）、`examples/agents`（多 Agent 协作示例）；`docs/ARCHITECTURE.md` / `docs/platform-contract.md` / `docs/assembly.md` / `docs/tutorial.html` / `docs/index.html` / `docs/release-v0.1.0.md` | `examples/cli` / `examples/protocli` / `examples/agentcli`、`docs/agent/` 18 篇、`docs/assistant.md`、`docs/llm.md`、`FEATURES.md` |

### 4.3 里程碑交付记录

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 / M1.5 / M1.6 | 任务循环、多协议接入、工具系统、事件观测 / 装配 + Policy / 执行沙箱 | ✅（旧树交付；重建树重写为根包装配层） |
| M2.1 ~ M2.7 | 状态外置、审计门、输出截断、fresh-context 压缩、槽位式系统提示词 | ✅（重建期 S6 重做：截断 + 压缩 + TaskState/Auditor + `agent/prompt`）；**Doom Loop / 40% Dumb Zone / RAG 未带回**（不排期） |
| v2 架构重构（v2.3 ~ v2.17） | EventBus / Session 日志 / Hook+Guard / RunContext+LoopDriver / seam / router / executor / scope / 持久化 / telemetry | ◐ 重建带回：Session 日志 / Hook+Guard / RunContext+LoopDriver / 持久化；**未带回**：EventBus / seam / router / executor / scope / telemetry（归档在 `main_bak1015`，按 §4.2 末列处置） |
| M2.8 异步 Approval | `agent/approval`：四档结果 + 问人由宿主注入 + `approval/asked` / `approval/decided` 审计落会话日志 | ✅（重建期 S5g） |
| M2.8+ 文件工具 Jail | `filesystem`：Lexical + Realpath 双校验 + symlink 逃逸防护；写前观察门（S5h） | ✅（重建期 S5d / S5h） |
| M2.8+ Checkpointer + AgentHandle | 跨进程挂起-续跑（checkpoint 快照 + handle create/resume/dispose） | ⬜ **重建未做** → FR-G3（FR-E6 的前置）。**2026-11-06 重定**：FR-G3 改为「日志即 checkpoint + 派生原语 + 宿主指示续跑」，**不建 `agent/checkpoint` / `agent/handle` 两个包** |
| M2.10 | FR-C 工具规模化：deferred 懒加载 + `tool_search` BM25 发现激活（C1 缓存在重建时已落地；C2/C4 守护测试锁定）；C3 的「超阈值自动启用」改为「deferred 集非空才装配 `tool_search`」（PI/DSH 均无阈值机制，不自创，分析见 ARCHITECTURE §3.15） | ✅（重建周期 S7.3） |
| M2.9 | FR-B 经验记忆 | ❌ 已决定不做（2026-10，用户拍板） |
| 重建补齐 | FR-G1 上下文文件（`agent/contextfile` + `agent/prompt`）/ FR-G2 联网工具（`tool/web`） | ✅（2026-10/11 落地，PRD 于 2026-11-03 回填登记） |
| M3a | FR-D11 Skills 重做：`agent/skill` 新包（frontmatter 解析 + 目录发现 + `<available_skills>` 清单 + `<skill_content>` 正文 + `/name` 展开器 + 本轮激活状态）+ 专用 `skill` 工具（正文走普通工具结果）+ `skill/activate` 事件 + 装配入口（`mos.Config.Skills` / `Agent.ExpandSkill`） | ✅（2026-11-04，S8.1；说明见 `docs/s8.1-skills.md`） |
| M3a | FR-D8 MCP 接入：`tool/mcp` 新包（**官方 Go SDK**：`github.com/modelcontextprotocol/go-sdk` v1.8.0；stdio + streamable-http 两个 transport；协议基线 `2026-07-28`）+ 工具适配（`mcp__<server>__<tool>` 命名、schema 直通、结果投影）+ 两阶段原子换代 + 装配入口（`mos.Config.MCP`：一开即装、默认 deferred、hidden 档、失败隔离、服务器说明每会话一次性注入、`Describe()` 快照） | ✅（2026-11-04，S8.2 + S8.3；**2026-11-06 补齐**四项 E2E + 四处修复/口径变更，说明见 `docs/s8.2-mcp.md`） |
| M3a | FR-D8 可用性优化 **S8.4-A**：MCP 服务器摘要进**静态**系统提示词（`agent/prompt` 新增 `mcp_servers` 节 + `tool/mcp` 渲染器 + 装配期接线；只列连上且有 deferred 工具的服务器） | ✅（2026-11-04，S8.4-A；说明见 `docs/s8.4-usability.md`） |
| M3a | FR-D8/D11 可用性优化 **S8.4-B/C/D/E**：认证可诊断 + `TokenProvider`、技能正文现读 + `Agent.ReloadSkills()`、`allowed-tools` 求交诊断、MCP 资源工具（三个共享工具） | ✅ 全部落地（B/C/D/E 2026-11-04；分析见 `docs/ARCHITECTURE.md` §3.16） |
| M3 ~ M5 | 见 §5 需求 | ◐ M3a（FR-D11 Skills / FR-D8 MCP）已落地；**FR-D3 SubAgent 设计分析已出 + 六条拍板已定（2026-11-06，见 ARCHITECTURE §3.18），S8.5-A `agent/subagent` 包 + S8.5-B 工具形态 + S8.5-C 装配层接线 + S8.5-D 文档与示例已落地（2026-11-07）——FR-D3 收口**；M3b（AgentTeam / 并行 DAG / 合约 + Elo）未开始 |

---

## 5. 功能需求（Roadmap → FR）

优先级定义：**P0** = 发布/下一里程碑阻塞项；**P1** = 本版本目标；**P2** = 规划中、实施前需修订细化。

### 5.1 FR-A：发布前待办（P0，已完成）

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-A1 | 协议层 `moproto` 脱离本地 `replace`：拆分为独立 module 或发布到可达仓库（对齐 AGENTS.md「对外发布前需拆分协议层」待办）；MoS 对协议层只消费 `pkg/` 纯定义包，**协议仓库只读**（缺陷与建议走 PR/issue，不直接改） | 外部工程 `go get github.com/puras/mos` 无需本地路径即可构建；`go.mod` 无本地 replace | ✅（2026-11-03：replace 已移除，moproto v0.1.0 已独立发布，`go get` 可用） |
| FR-A2 | 文档与实现同步：README 等处 import 路径 `hegq.net/mos` → `github.com/puras/mos`；FEATURES.md 更新记录补齐 2026-09 ~ 10 变更（capability 拆分、`llm/transport`、moproto 迁移、agentcli/docs 新增） | 文档 grep 无旧模块路径；新贡献者按 README 可跑通 | ✅（模块路径已统一；`FEATURES.md` 未建——属旧树文档，随旧树归档；重建树的文档集见 §4.2 末行） |

### 5.2 FR-B：M2.9 经验记忆（知识进化第 1 层，P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-B1 | 执行历史沉淀为经验库：成功策略 / 已知陷阱入库，按成功率 / 时间 / 成本加权评分择优 | 经验写入 → 评分 → 检索 → 注入决策链路 E2E 测试 |
| FR-B2 | 决策时检索注入：复用 M2.7 `memory.Retriever` 基础设施与注入钩子 | 未注入经验时行为与现状完全一致（零回归）；注入走 `CustomMsg(Source="experience")`，不进会话历史 |

### 5.3 FR-C：M2.10 工具规模化（P1，已交付，见 §4.3 里程碑表）

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-C1 | schema 生成缓存：`generateSchema` 反射结果按工具 memo 化 | 循环内不再重复反射（基准测试佐证） | ✅ 重建时已落地（`Registry.Register` 时生成） |
| FR-C2 | schema 压缩：删 title / $defs、过滤注入参数 | 注入 schema 体积下降且参数解析不受影响 | ✅ 生成器本就不产出 title/$defs（守护测试锁定） |
| FR-C3 | deferred 懒加载：工具只发名字进 prompt，`tool_search` 按需取 schema；工具描述占上下文超阈值（参考 Claude Code 10%）自动启用，小工具集保持全量注入 | 工具数增长时 prompt token 亚线性；默认行为对现有用户不变 | ✅ deferred 集 + `tool_search`（BM25）已落地；**「10% 阈值」未做**——PI/DSH 均无此机制（PI 配置驱动 / DSH 作者标记），不自创，改为「deferred 集非空才装配」 |
| FR-C4 | 静态系统提示保 prompt cache：动态内容移至 user 消息侧 | 连续两轮请求 system 段逐字节一致（缓存命中） | ✅ 分节装配器装配期渲染一次（守护测试锁定） |

### 5.4 FR-D：M3 多智能体编排（P1，建议分期 M3a / M3b）

| 编号 | 需求 | 关键约束 / 验收标准 |
| --- | --- | --- |
| FR-D1 | Coordinator 路由与编排拓扑：按意图分派（规则硬路由保证确定性可审计，LLM 分类兜底）；拓扑先「监督者/层次化」，后演进「批评者-审查者」 | 基于 `agent/router` 扩展；路由决策落会话日志可回放 |
| FR-D2 | 三权分离 Manager / Executor / Auditor：派发、执行、审计职责分属不同角色 | 审计不变式对每个成员独立生效（NFR-5） |
| FR-D3 | SubAgent 子智能体（首选 agents-as-tools 形态）：声明式定义（`name` / `description` 自动委派匹配 / `model` 覆盖 / `tools` 白名单 / 提示词正文，对齐 `.claude/agents/` 风格）；实现 `tool.Tool` 包装进 Registry，主 Agent 经 function calling 调用只收结构化结果 | 工具白名单即 blast radius 收缩（fail-closed）；独立 Context，中间工具调用留内部；回传自述仍须 Auditor 验证；决策判据入文档——instructions/tools/policy 真正改变才拆（**2026-11-06 设计分析已出、六条拍板已定，见 ARCHITECTURE §3.18；S8.5-A / B / C / D 已落地（2026-11-07）——FR-D3 收口**） |
| FR-D4 | 双阶段架构（Plan&Solve）：Initializer 写 plan.md 退出 → Executor 接力执行，不共享 Context Window；Replanner 随新约束修订计划 | 计划文件落 `runs/<run-id>`（依赖 FR-E6 可先落工作目录）；跨窗口接力 E2E |
| FR-D5 | LLMCompiler 多智能体并行 DAG：基于 `agent/driver/scripted`（单智能体先行版已落地）扩展 Map-Reduce——分支并发、结果经 TaskState 汇聚、合并只读分支摘要、并行度作为计划元数据 | DAG 校验 fail-closed（依赖环 / 节点存在性）；并发分支数有界 |
| FR-D6 | AgentTeam 智能体团队：Team Lead + Teammates + Shared Task List（`pending → in_progress → completed` + `blocks/blockedBy` 依赖自动解锁）+ Mailbox 点对点消息；状态落盘（角色/上下文/决策/任务文件/名册写 `runs/<run-id>`，中断后可重建）；工作树隔离（独立工作目录/分支，测试通过才合并） | 事件 `subagent/*`、`task_claim/task_complete` 落日志；团队重建演练（kill 后一条命令恢复） |
| FR-D7 | 合约化任务 + Elo 择优：可验证交付物 + 协商澄清 + 子合约分解；多方案生成按 Elo 排序择优 | 交付物可机器验证（退出码 / 路径命中 / 事件日志），禁止 LLM 自评闭环 |
| FR-D8 | MCP 接入：宿主以结构声明服务器与 transport（stdio / streamable-http；**先 stdio**；`mcp.json` 解析归宿主，框架不绑配置格式）；五步交互（发现 → **暴露策略**（exposure：`deferred`/`direct`/`hidden` + 逐工具覆盖）→ 调用 → 错误回传 → 结果注入）；不做 OAuth（凭据由宿主注入）。**协议层用官方 Go SDK**（v1.8.0，基线 `2026-07-28`；SDK 类型不外泄） | MCP 工具进 Registry 后与内置工具同受 Policy/Guard 管辖；单个 MCP 失败不阻塞其余加载；MCP 工具默认 deferred（经 `tool_search` 发现激活），工具面增长不吃线性 prompt；**模型知道有哪些 MCP 服务器**——有 deferred 工具的服务器摘要进静态系统提示词（S8.4-A）；**认证可诊断**——HTTP 401/403 归认证类 + 宿主可注入 `TokenProvider`（S8.4-B）；**资源可达**——三个共享资源工具 + 显式 server 参数（S8.4-E） |
| FR-D9 | Agent Loop 升级：Turn 语义完善 + 错误注入反馈（错误信息解释"为何违反规则 + 正确做法"，Linter 报错即上下文工程） | 错误注入后模型自纠成功率可观测（测试集） |
| FR-D10 | 显式图边界（判据门控）：节点 / 边 / 状态 schema + reducers / checkpointer；编译期校验（节点可达性、状态字段一致性、边类型合法性），不合法图拒绝运行 | **仅在满足分支/并行/持久/可审计判据时引入**——图是可选编排层，不是默认架构 |
| FR-D11 | Skill 能力（Skills 重做，M3a）：装配期技能目录（`SKILL.md` + frontmatter：name/description/when-to-use/allowed-tools/disable-model-invocation/metadata）+ 专用 `skill` 工具（正文按需加载，进工具结果）+ 技能清单进系统提示词（静态，保 prompt cache）+ `skill/*` 事件 | 渐进式披露（元数据进上下文、正文按需）；**不做技能级工具收窄**（PI 有字段无实现、DSH 无字段 → 不自创；`allowed-tools` 只解析留痕 + 诊断）；用户显式 `/name` 由宿主调用展开器（框架不自动扫文本）；**技能目录列表 = 宿主声明的授权边界**（读走注入的 `Reader`，不重复登记 Roots）；`disable-model-invocation` 的技能不进清单、模型不可加载（fail-closed）；**正文调用时现读** + 宿主显式 `Agent.ReloadSkills()` 重扫（不接 watcher，S8.4-C）；`allowed-tools` × 当前可见工具集**求交诊断**（只诊断不收窄，S8.4-D） |

### 5.5 FR-E：M4 生产化（P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-E1 | 异常处理与恢复三阶段：检测（工具输出验证 / API 错误码 / 超时）→ 处理（日志/重试/回退/优雅降级/通知）→ 恢复（状态回滚 / 诊断 / 自我纠正 / 升级） | 注入故障演练集通过；升级路径接 Approval |
| FR-E2 | 沙箱强隔离后端（L3）：Docker / containerd 替换进程级实现——**宿主侧实现，经 `sandbox.Runner` 注入**（强隔离属宿主侧：它替换整条执行能力，不是同世界路径强制后端；MoS 出同世界后端，归属见 ARCHITECTURE §3.10.1。**已达成部分（2026-11-06）**：macOS Seatbelt / Linux Landlock / Windows 低完整性 token 的内核级**写范围**强制（§3.10）；**能力上报**（`sandbox.Capabilities()` + `sandbox.Probe(ctx)` + 每次调用自报）设计已定、**未实现**，见 §3.10.2。**暂缓（2026-11）**：外部后端注入——`mos.Config.Sandbox` 仍是 `*sandbox.Local`，当前阶段只用三个平台原生后端，接缝留着） | `sandbox.Runner` 接口不变，后端可切换（前置：`mos.Config.Sandbox` 从 `*sandbox.Local` 改成接口，**未决**）；越狱测试通过 |
| FR-E3 | 模型分级路由：按任务复杂度选档（简单→经济模型、复杂→强模型），附顺序回退链 | 分级决策可配置可审计；回退链演练通过 |
| FR-E4 | 评估与监控（知识进化第 2 层 = 配置进化）：轨迹评估 Golden 测试集 + LLM-as-a-Judge 结构化评分 + 漂移检测 + 配置 A/B 择优（工具描述 / 提示词 / 路由与 Policy 规则版本化晋级） | 评估跑批可重复；版本化档案支持回滚 |
| FR-E5 | 优先级调度与配额：紧急性/重要性/依赖关系，限流配额按优先级区分 | 调度策略接口化（策略由平台注入） |
| FR-E6 | 生命周期控制 API + `runs/<run-id>` 可恢复运行目录：启动/暂停/恢复/终止；状态/中间结果/审计回放走文件系统（Workspace 即真相），断点续传 | kill -9 后可从运行目录恢复续跑 |
| FR-E7 | 人工网关 + Dashboard 可视化（平台侧消费 SDK 事件流实现） | 事件契约（`docs/platform-contract.md` §2 事件 JSON）稳定不再变更 |
| FR-E8 | 限流 / 重试 / 缓存（横切能力下沉至 llm 装饰器与 transport 层） | 指标可观测 |
| FR-E9 | 插件化：对齐 Agent Plugins 1.0 目录规范（`plugin.json` + `skills/` + `mcp.json` + 客户端命名空间目录）；组件独立校验（单个 skill 无效跳过、MCP 失败不阻塞其余）；安装/权限/来源验证走 MoS 的 Policy/Sandbox/Approval | 合规 plugin 目录可被 `mos.New` 加载；可移植字段不污染 |
| FR-E10 | **审批超时的 fail-closed 兜底 + 日志的机密性/防篡改归宿主**（2026-11-07 重裁）：框架**不实现审批 TTL**（`approval.Service.Decide` 只阻塞等应答方，超时是宿主 `Answerer` 自己的事）——框架只保证兜底：ctx 取消 → `cancelled`、通道坏 / 未装应答方 → `unavailable`，两者**一律 fail-closed（默认拒绝）**。**不再有 checkpoint 文件**：FR-G3 重定后恢复的权威是**会话事件日志**（旧 checkpoint 里的 `Calls` / `Decisions` / `Snapshot` 都已住在日志里），故「checkpoint 加密/签名」**失去对象** → **不做**哈希链/签名机制（锚只能由宿主保管，那是宿主存储层的事：权限 / 加密卷 / 只追加） | 审批超时后**不执行**该调用（fail-closed）；日志的加密 / 防篡改 / 留存策略由宿主决定，MoS 只保证事件结构与审计链（`approval/asked` 与 `approval/decided` 成对）完整 |

### 5.6 FR-F：M5 自我进化第 3 层（P2，实验性）

| 编号 | 需求 | 关键约束 |
| --- | --- | --- |
| FR-F1 | 独立"进化 Agent"：不参与主循环、只消费审计事件流，在工作区修改外围策略/工具代码 → 编译 → 跑测试 → 通过才归档；核心引擎人类控制，合并走人工批准；版本档案支持回滚 | 硬约束：没有外部基准就没有进化——闭环必须有机器可回放信号，禁止 LLM 自评进化（共谋确认）；每层可回滚 |

### 5.7 FR-G：重建期补齐的基础能力（P1）

> 这组是**重建周期里长出来、当时没登记**的能力（治理规则 1：新增一个包 = PRD 有一条 FR 指名要它）。回填它们是为了把闸门闭上，**不是新增范围**。FR-G1/G2 已交付；FR-G3 是 FR-E6 的前置。

| 编号 | 需求 | 验收标准 | 状态 |
| --- | --- | --- | --- |
| FR-G1 | 上下文文件（`agent/contextfile` + `agent/prompt` 分节装配）：候选名优先级（`AGENTS.override.md` > `AGENTS.md` > `AGENTS.MD` > `CLAUDE.md` > `CLAUDE.MD`）+ 向上递归 + 同目录 override + 真实路径去重 + 字节预算裁剪；正文作为「项目约定」节进系统提示词 | 装配快照写明装了哪些文件、多少字节、降级警告；读取以 `Reader` 接口注入（框架不碰文件系统）；未装则零开销 | ✅（2026-10-27 落地；2026-11-03 回填登记） |
| FR-G2 | 联网工具包（`tool/web`）：`web_search` / `web_fetch`，含 SSRF 防御（私有网段 / 环回拦截） | 抓取目标越界即拒；工具进注册表后与内置工具同受 Policy/Guard 管辖 | ✅（2026-10-28 落地；2026-11-03 回填登记） |
| FR-G3 | 挂起-恢复（**日志即 checkpoint**）：从会话日志派生未决审批与悬空调用；宿主显式指示后**续跑那一次工具调用**；审批决定在同一轮内可复用（不重复问人） | kill -9 → 重开会话 → 列出未决审批与悬空调用 → 宿主指示 → 续完该次调用 → 本轮正常收尾；文件类重跑被观察门 CAS 拒；**是 FR-E6（`runs/<run-id>` 可恢复运行目录）的前置**。设计见 `docs/ARCHITECTURE.md` §3.17（PI/DSH 对账 + 取舍 + 拍板结果，2026-11-06） | ✅ **S9.1 / S9.2 / S9.3 全部落地**（2026-11-07：`session` 四个派生读接口 + `approval.Prior` 先验复用 + `reusedFrom` 载荷；**S9.2** 装配层 `Agent.Resume` + `RunContext.Resume` 续跑模式——沿用原轮次号、原样重放载荷、走同一执行通道、回填真实结果、无可续则明确报错且不收尾；**S9.3** 真进程 SIGKILL 演练通过）。**不建 `agent/checkpoint` / `agent/handle` 包**（由 `session` 派生接口 + `agent/approval` 先验复用 + 续跑承担）；排期：S9.1 插在 S8.5 期间，S9.2/S9.3 在 M3b 之前 |

---

## 6. 非功能需求（NFR）

| 编号 | 需求 | 现状 / 度量 |
| --- | --- | --- |
| NFR-1 | **fail-closed 纪律**：未知工具拒绝、未声明文件根即无根（越界一律拒）、缺模型/缺沙箱即装配报错、截断工具调用 fail-closed、审批未配置即拒绝、缺能力不降级放行 | 已执行；新增能力一律按此纪律设计并有测试 |
| NFR-2 | **防御性 nil-safe**：`RunContext` 方法对 nil 接收者安全；扩展点 nil 时降级为框架默认或 no-op，行为不变 | 已执行 |
| NFR-3 | **零开销降级**：钩子 / 守卫 / 压缩器 / 截断器 / 审批通道未注入时零额外开销（未注入 = 行为与没用该能力完全一致） | no-op 路径基准测试 |
| NFR-4 | **日志即事实**：会话日志 append-only、`Append` 同步落盘 fail-fast、事件 Seq 连续可重放；日志不允许截断（崩溃恢复只补事件不改写） | 已执行；重放测试覆盖 |
| NFR-5 | **审计不变式**：任何智能体（含 SubAgent / 队友）的自述不入 `TaskState`，环境验证说了算；机器可回放信号优先于 LLM 判断 | 已执行；M3 多智能体延续 |
| NFR-6 | **依赖纪律**：近零依赖（协议层 + uuid + yaml.v3）；**例外**：Linux 路径强制后端引入 `landlock-lsm/go-landlock`（内核 API 无标准库绑定，自研 syscall = 自己实现内核 ABI）；协议仓库只读（AGENTS.md 硬规则）；MoS 只消费协议层 `pkg/` 纯定义包，不依赖网关实现层 | `go.mod` 审计 + import 边界检查 |
| NFR-7 | **可测试性**：适配器用 httptest 协议模拟不依赖真实网络；关键行为用守护测试钉住（如 schema 逐字节不变、提示词两次 Render 一致） | `go test ./...` 全绿（注：需嵌套 Seatbelt 的沙箱 / shell 类测试要在允许 sandbox 的常规环境跑） |
| NFR-8 | **API 演进纪律**：未发布、无兼容负担，可按目标架构直接落地；但破坏性变更（如文件工具改名 `read_file`→`read`、提示词分节改名）必须在文档更新记录显式声明 | 更新记录惯例执行 |

---

## 7. 里程碑规划

| 里程碑 | 内容（对应 FR） | 优先级 | 依赖 |
| --- | --- | --- | --- |
| **R（发布准备）** | FR-A1 协议层独立发布、FR-A2 文档同步 | P0 | ✅ 已完成（v0.1.0 已发布，2026-11） |
| ~~**M2.9**~~ | ~~FR-B1/B2 经验记忆~~ | ~~P1~~ | 已决定不做（2026-10） |
| **M2.10** | ~~FR-C1~C4 工具规模化~~ | ~~P1~~ | ✅ 已交付（S7.3） |
| **FR-G3 挂起-恢复** | 日志即 checkpoint（派生未决/悬空 + 宿主指示续跑；不建 checkpoint/handle 包） | P1 | 无；**FR-E6 的前置**；排期：S9.1 插在 S8.5 期间，S9.2/S9.3 在 M3b 之前 |
| **M3a** | FR-D1/D2/D3/D8/D9/**D11**（路由拓扑、三权分离、SubAgent、MCP、Loop 升级、Skills 重做；MCP 先于 SubAgent） | P1 | R |
| **M3b** | FR-D4~D7/D10（双阶段、并行 DAG、AgentTeam、合约+Elo、显式图） | P1 | M3a |
| **M4** | FR-E1~E10 生产化 | P1 | M3a（部分项可并行） |
| **M5** | FR-F1 自我进化（实验） | P2 | M4 的 E4（评估与机器信号） |

> 排期原则：不设日历日期，按依赖顺序推进；每个里程碑完成后在 `docs/ARCHITECTURE.md` 更新记录与本文 §4.3 同步状态。

---

## 8. 风险与开放问题

### 8.1 风险

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | 规则层黑名单可被解释器入口/混淆命令绕过（README「诚实的边界」），进程级沙箱非强隔离 | 安全边界定位为纵深防御之一；FR-E2 强隔离后端是最终答案；SaaS 部署强制 `saas-jailed` preset |
| R2 | 审计门天花板 = 模型自分解清单的完整性（漏画的交付物审计也发现不了）；"换一个 LLM 盖章"不构成独立性 | 保持环境只读验证 + 机器信号；LLM 评审只作质量补充不作完成判定；有界轮次耗尽诚实停机（`audit_unmet`） |
| R3 | M3 范围过大（10 项 FR），一次性实施易失控 | 分期 M3a/M3b；逐项按决策判据门控（不满足判据不引入：SubAgent「合同论」、显式图四判据） |
| R4 | 文档与实现漂移（**已发生两次**：Skills 在 PRD 里一直标 ✅、重建树里却没有；M2.8 的 ✅ 掩盖了 checkpoint / handle 缺口） | 治理规则 1 的闸门靠 FR 登记维持：新包必登记 FR；每次切片合入同步 §4.2 / §4.3 + 顶部更新记录 |
| R5 | 上下文工程经验参数（窗口默认 128K、预留 16384、保留 20000 token）随模型迭代可能失效 | 补偿面理论：随模型变强复测并主动简化 harness；参数全部可配置（`mos.Config.ContextWindow` / `ReserveTokens` / `KeepRecentTokens`） |
| R6 | 协议层已独立发布（FR-A1 ✅），仓库**已有 CI**（2026-11-07：macOS + Linux 的 `go build` / `go vet` / `go test`，外加 ubuntu 的 `-race`）——外部拉取能否构建由 CI 持续验证，不再只靠人工一次 | CI 跑在**发布出去的依赖**上（`go.mod` 无 `replace`，协议层是独立 module）；沙箱类用例在本机受限时会失败，故 CI 必须跑在正常权限的 runner 上 |
| R7 | 会话日志是**权威状态**：能写它的人就能改它（改 `tool/call` 的载荷、伪造 `approval/decided`）——框架**不做**哈希链 / 签名（FR-E10 已决：防篡改的「锚」只能由宿主保管，否则能改整个文件的人会重算整条链） | 归宿主：存储层做权限 / 加密 / 只追加，必要时宿主在日志外另记锚并自校验；MoS 保证的是审计链的**结构**完整（`approval/asked` 与 `approval/decided` 成对、审计写失败即中止执行）与派生接口的只读性（改了日志，派生出来的现场就变了——但那是同一个信任域内的事） |

### 8.2 开放问题（待用户/维护者决策）

1. ~~`moproto` 拆独立 module 的时间点与版本策略~~ —— **已决（2026-11-03）**：已拆为独立 module 并发布 v0.1.0，`go get github.com/puras/mos@v0.1.0` 直接可用。
2. ~~MCP transport 优先级~~ —— **已决（2026-11-02）**：先 stdio（无网络依赖、易测、与进程管理同构），streamable-http 紧随（`httpx` 已有）。
3. 远程沙箱后端选型：Docker 还是 containerd（或两者都做适配器）？—— **归属已定（2026-11）**：强隔离属**宿主侧**（整体替换执行环境，不是 `sandbox.Runner` 的同世界后端，见 ARCHITECTURE §3.10.1），MoS 只出同世界路径强制后端（Landlock / Seatbelt / Windows 低完整性 token）。**暂缓（2026-11）**：当前阶段只跑三个平台的原生后端，不接外部后端——`mos.Config.Sandbox` 仍是 `*sandbox.Local`，接缝留着，宿主真要接时改这一个字段。剩余待决：宿主侧用 Docker 还是 containerd。另：能力上报（`sandbox.Capabilities()` / `sandbox.Probe(ctx)`）设计已定、未实现，见 §3.10.2。
4. ~~M2.9 经验记忆的默认介质~~ —— **已作废（2026-10）**：M2.9 决定不做。
5. ~~M3a 中 SubAgent 与 MCP 的先后~~ —— **已决（2026-11-02）**：**MCP 先**（边界独立可单测；工具面立稳后，SubAgent 的工具白名单才有稳定母集）。

---

## 9. 术语与引用

- 目标架构与子系统契约：`docs/ARCHITECTURE.md`（分层、不变式、各子系统契约，含 PI/DSH 对位分析）。
- 装配层：`docs/assembly.md`（`mos.New` 的不变式、默认值表、运行期变更）。
- 平台集成契约：`docs/platform-contract.md`（职责边界 / 事件 JSON / 状态序列化 / 并发 / 人工审批）。
- 上手与产品介绍：`docs/tutorial.html`（可复制可编译的教程）、`docs/index.html`（产品介绍）。
- 发布说明：`docs/release-v0.1.0.md`（v0.1.0 = 首个公开版本）与 `docs/release-v0.2.0.md`（v0.2.0 = Skills 重做 + MCP 接入；标签打在 `4a563df`，**不含**其后的 SubAgent 与挂起-恢复）。**发布约定**：标签处的 `mos.Version` 必须与标签同名，发布后再把 `main` 上的常量推到下一版（v0.2.0 发布后 `main` 为 `0.3.0`）。
- 旧实现与旧文档（考古用，`main_bak1015` 分支）：`docs/architecture-v2.md`、`docs/sdk-contracts.md`、`docs/agent/`（18 篇）、`docs/assistant.md`、`docs/llm.md`、`FEATURES.md`、`examples/{cli,protocli,agentcli}`。
