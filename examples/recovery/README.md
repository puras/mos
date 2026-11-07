# recovery —— 进程被 kill -9 之后，宿主怎么把那一轮接回去

这个示例演示 FR-G3（挂起-恢复）的**完整链路**，用**真进程硬杀**（不是模拟事件
序列）：

```
子进程跑一轮 → 模型要调 note 工具 → 宿主规则判「需人工确认」→
审批应答方永远不答（模拟「人还没来得及答」）→ 父进程 SIGKILL 子进程
→ 父进程重开会话 → 从日志派生现场（未决审批 + 悬空调用 + 被中断的轮次）
→ 宿主显式指示续跑（这次有人批了）→ 工具真跑一遍 → 真实结果回填
→ 本轮正常收尾（沿用原轮次号）
```

## 跑法

```bash
cd examples/recovery
go run .
```

**不需要 API Key**：示例用一个假模型（第一次请求要调工具，拿到结果后作答）——
恢复机制与真模型无关。想接真模型就把 `scriptedModel` 换成 `mos.NewModel(...)`。

## 这个示例要说明的五件事

1. **「日志即 checkpoint」**：不另设 checkpoint 文件、不建 `agent/checkpoint`
   包。挂起的现场全在会话日志里，重开会话即得。
2. **派生是纯读**：`PendingApprovals()` / `HangingCalls()` / `InterruptedTurn()`
   只读日志，不写任何东西——宿主随时可以重算，不推进序号。
3. **框架不自动续跑**：默认只报告。**要不要续由宿主决定**（`Agent.Resume`），
   框架不会自己把挂起的轮次拾起来跑。
4. **续跑不替人回答**：原先那条未决审批仍留在未决列表里（日志的事实：问过了，
   没人答）。续跑只**重新问一次**，答不答由宿主决定。
5. **续跑会把那次调用再执行一遍**：外部副作用（shell 之类）可能重复——框架不猜
   「是否已经执行过」，宿主自己的工具要按幂等设计。

## 续跑的两条幂等口径

- **审批决定可复用**：同一轮、同一调用（匹配键 `(Turn, CallID)`）若**已经有人**
  的决定（`approved` / `rejected`），续跑**不再问人**——但仍写一对新的
  `approval/asked` / `approval/decided`（**新 id** + `reusedFrom` 指回原决定），
  审计链完整。`cancelled` / `unavailable` 不是人的决定，不复用。
- **无可续内容就不收尾**：报错 `react.ErrNothingToResume`，那一轮**保持挂起**
  ——「收尾」会抹掉尚欠调用的日志事实。

## 参考

- 派生接口：`session`（`PendingApprovals` / `ApprovalOutcome` /
  `ApprovalDecision` / `HangingCalls` / `InterruptedTurn`）
- 续跑入口：`mos.Agent.Resume(ctx, sess, mos.ResumeOptions{Turn, Calls})`
- 宿主侧契约：[docs/platform-contract.md](../../docs/platform-contract.md) §3.3
- 设计与口径：[docs/ARCHITECTURE.md](../../docs/ARCHITECTURE.md) §3.17
