# ADR-0100: DeepSeek Harness 设计评审——上下文溢出是请求侧故障、执行结果 spill 统一到可取回通道

- **状态**: Accepted
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M1（internal/llm）/ M4（internal/agent）/ M5（internal/memory/compact）

## 上下文

评审对象：DeepSeek Harness（`dsh`）0.1.7-rc.2（commit `477b4f4205`），TypeScript，Cordis 插件微内核，事件溯源会话日志。逐项对照其 `.agents/notes/implemented/architecture/` 决策记录与 Polaris 代码，筛出"Polaris 存在可复现缺陷、且 dsh 的做法可在不改变 Polaris 架构（Go FSM 主控、手工装配）的前提下落地"的三项：

1. **上下文溢出被当作 Provider 故障**。`error_classifier.go` 为超限错误打 `ShouldCompress=true`，全仓无消费方；路由按 `Retryable=true` 把同一超长请求依次发给池内每个 Provider、每个降级池，逐个 `recordOutcome(false)` 打开健康 Provider 的熔断器，最终报 `ErrAllProvidersFailed`；Agent 据此累加 `ProviderSuspendCount`（满 5 次触发"provider 耗尽" HITL），从未尝试缩减请求。热路径压缩（ADR-0033）按固定 90000 token 容量估算做事前预防，小窗口本地模型或估算偏差时兜不住。dsh 对应决策：`2026-07-10-after-call-compaction-pressure-and-overflow-recovery`（适配器把超限规范化为 `CONTEXT_WINDOW_EXCEEDED`，非暂时性；恢复有界、须证明表层确已缩小才重试）。
2. **超限执行结果取不回**。`truncateExecResult` 把 >8KB 结果写到 `logs/exec_results/<session>-<纳秒>.txt`，回给模型 512 字节头部预览 + `log_ref` id；没有任何工具能按该 id 读回（`read_tool_ref` 只读工作区 `tool_refs/`），文件名可预测且非独占写入。而 `ToolRefOffloader` + `read_tool_ref` 已是完整的可取回通道（热路径 Stage 1、网关压缩在用），Agent 主路径绕过了它。dsh 对应决策：`2026-07-08-tool-output-spill-files`（单一 spill seam，预览 + 定位符 + 检索提示，存储失败不给出取不回的引用）。
3. **截断只留头部**。执行结果预览与观察（`RecordObservation`，4KB）都只保留开头；编译错误、堆栈、测试汇总、退出码多在末尾。dsh `compaction-tool-result-pruner` / `spill-policy` 一律首尾保留。

## 决策

**上下文溢出是请求侧故障：路由只做容量感知 failover，不计入 Provider 健康度，最终以 `protocol.ErrContextOverflow` 交还调用方；Agent 确定性修剪一次后重试。执行结果超限统一经 ToolRefOffloader 卸载，预览首尾保留且首行给出 read_tool_ref 取回提示。**

- **决策一（路由）**：`ShouldCompress` 类错误（上下文超限 / payload 过大）不说明 Provider 不健康——`recordAttempt` 与 `trackedProvider` 均不计入熔断器、成功率、模型注册表。failover 只转向 `MaxContextTokens` 严格大于失败方的 Provider（先同池，后不限 role 全局；窗口未知的不参选；窗口严格递增保证终止）。没有更大窗口，或更大窗口也失败，返回包装 `protocol.ErrContextOverflow` 的错误。实现：`internal/llm/router_request_fault.go`。
- **决策二（Agent）**：`streamInferWithOverflowRecovery` 收到 `ErrContextOverflow` 后，对固定前缀（开头连续 system 消息，同 `compact.SplitPinnedHead`）之外的非 system 消息按"注水"上限做首尾保留截断，可修剪字节约降到 50%，最大的消息先被截，短消息（本轮意图）原样保留；在 PII 令牌化之前修剪、之后重新令牌化；只重试一次，没有可修剪内容则如实上抛。**不走 LLM 摘要**：Stage 2 摘要会把 TaintHigh 围栏内数据改写为 assistant 角色消息（污点提权，HE-2/HE-7），首尾截断保留原角色与围栏起止标记。指标 `polaris.agent.context_overflow_recovery_total`。
- **决策三（spill）**：`spillExecResult` 对超过观察上限（4KB）的结果调用 `ToolRefOffloader.Offload` 一次；`ExecuteResult`（≤8KB）与观察（≤4KB）各自投影，共享同一卸载副本，首行 `[output truncated: N bytes total; full output: read_tool_ref(task_id=..., id=...)]`。未注入卸载器或卸载失败时首行声明 `full output not retained`，不给出取不回的引用。`logs/exec_results/` 写入删除。
- **决策四（截断原语）**：`pkg/util.ElideMiddle`——头 3/4、尾 1/4、UTF-8 边界对齐、标记长度按最坏情况预留。执行结果预览、观察截断、溢出修剪共用。

## 后果

- **正向**: 超长请求不再打开健康 Provider 的熔断器、不再被误报为"所有 Provider 耗尽"、不再触发错误的 HITL；小窗口本地模型超限时自动升到大窗口 Provider；估算失准时有兜底恢复。模型对超限工具输出可按需取回全文，且看得到输出末尾。
- **负向**: 修剪是有损的——被省略的中段对本次请求不可见（执行结果全文仍可经 read_tool_ref 取回，其他数据如召回记忆、历史不可取回）。4–8KB 的执行结果现在也会写一次工作区文件并登记 `workspace_vfs`。
- **遗留（未在本 ADR 修复）**: 热路径 Stage 2（`agent_context_compaction.go` hotPathCompact 与网关 SessionCompressor）把含 TaintHigh 数据的 body 摘要为 assistant 角色消息，存在污点提权，需单独评估。
- **反例守护**: 提议"超限错误也按普通失败 failover 到同等容量 Provider""超限计入熔断""溢出恢复直接调用 LLM 摘要""截断只保留头部""给模型一个没有工具能读回的引用 id"——引用本 ADR 拒绝。

## 被驳回的方案（dsh 设计中评审后不引入的部分）

| 方案 | 驳回理由 |
|------|---------|
| Cordis "一切皆插件"微内核 + YAML profile/bundle 组合 | Polaris 维持 `cmd/polaris/boot_*.go` 手工装配（ADR-0081 / ADR-0088 记录的现状与迁移代价）；插件化替换 agent loop 与"Go FSM 持有控制流"（HE-5）冲突——dsh 允许替换循环本身，Polaris 的安全与预算门控挂在 FSM 转移上，循环不可替换是刻意的。 |
| 事件溯源会话日志作为模型请求的唯一真源（"模型可见 ⟺ 已记录"、逐字节重建每个请求、invariant 伴随插件） | 方向正确，但 Polaris 每阶段 prompt 由记忆检索（非确定性召回）现场组装，达到可重建需把每次召回结果落为会话事件并改造 5 个 Build*Context；现有 `WriteLLMCallEvent` 已落完整请求消息，覆盖审计与崩溃回放（M04 §8）。代价与收益不匹配，暂缓。 |
| 系统提示词作为 surface 第 0 号节点、`request/header` 快照 | 针对 dsh 单循环多步对话的前缀缓存设计；Polaris 的 ImmutableCore 已按 stable → volatile 分层且排序确定（`immutable_core_prompt.go`），多阶段 FSM 各阶段指令不同，前缀在阶段指令处分叉是阶段化设计的固有代价，不是可局部修复的缺陷。 |
| 重复工具调用提醒（repeat-tool-reminder：同工具同参数第 3/5/8 次注入提醒） | 属新增模型可见 Prompt，HE-4 要求 Eval 门控；Polaris 的重规划有 ReplanGuard 与观察回灌（ADR-0098 决策六/八）兜底，未见复现证据。 |
| `dsh_session_log`：默认把会话日志后缀随每个模型请求上传给 Provider | 自托管产品默认向第三方上传会话全文与隐私承诺冲突；Polaris 不引入。 |
| Agent 级持久化 `llm/retry` 事件 + 适配器单次尝试 | Polaris 重试在路由层（`DefaultBackoff` + 凭证轮换 + failover），会话可见性由 SessionOrchestrator 的错误事件承担；改为 Agent 级重试需重分重试所有权，无当前缺陷驱动。 |

## 引用代码

- `internal/llm/router_request_fault.go`（决策一：`isRequestFault` / `recordAttempt` / `overflowFailover` / `streamOverflowFailover`）
- `internal/llm/router.go`、`router_failover.go`、`router_stream.go`、`provider_registry.go`（六处调用点与 trackedProvider）
- `internal/protocol/errors.go`（`ErrContextOverflow`）
- `internal/agent/agent_overflow_recovery.go`（决策二）
- `internal/agent/agent_execute_result.go`（决策三：`spillExecResult` / `execResultView.render`）
- `internal/agent/fsm/observations.go`（`ObservationMaxBytes`，首尾保留）
- `pkg/util/elide.go`（决策四）
- dsh：`.agents/notes/implemented/architecture/2026-07-10-after-call-compaction-pressure-and-overflow-recovery.md`、`2026-07-08-tool-output-spill-files.md`、`2026-06-21-bounded-llm-request-recovery.md`

## 重新评估触发条件

- `polaris.agent.context_overflow_recovery_total` 持续增长且重试后仍以 `ErrContextOverflow` 失败占比 >20%：修剪比例不足或超限源在固定前缀（ImmutableCore + AmbientContext），需按 Provider 实际窗口动态设定 ContextWindowManager 容量，而非加大修剪力度。
- 引入需要逐字节回放模型请求的能力（如离线 Prompt 回归 Eval 以生产请求为样本）：重提"事件溯源会话日志作为请求真源"。
- 出现同工具同参数重复调用导致回合耗尽 max_steps 的复现样本：配套 Eval 用例后重提重复调用提醒。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-25 | 初稿 |
