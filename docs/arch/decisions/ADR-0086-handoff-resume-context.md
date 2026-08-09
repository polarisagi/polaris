# ADR-0086: Handoff 唤醒事件化 + 崩溃后无损续跑快照（GD-13-007/GD-13-009）

- **状态**: Accepted（已执行）| **日期**: 2026-08-01 | **模块**: M4 `internal/agent/agent_handoff.go`、`internal/agent/agent_handoff_resume.go`、`internal/agent/reconciler_handoff.go`

## 上下文

`transfer_to_agent` 委派子任务后，父 Agent 进入 `S_AWAIT_AGENT` 挂起等待子任务终态。原实现（GD-13-007 修复前）以 1s ticker 轮询 `handoffPoster.PeekTask` 判断完成，在 N 个 Agent 并发委派时对 `SQLiteBlackboard`（`MaxOpenConns=1` 单写者）产生 N QPS 的无谓读压，与 `MutationBus` 写入争用连接池。同时（GD-13-009 修复前）`ResumeAwaitingHandoff` 只回填 `HandoffTaskID` + `ForceState`，进程崩溃重启后 `a.sCtx.DAGModel` 等执行期上下文彻底丢失——`runExecuteDAG` 命中 nil-DAGModel 快速路径直接推进 `ExecuteDone`，委派节点下游的 DAG 节点全部被截断、永不执行，父任务"看似正常完成"实为静默丢弃剩余工作。

## 决策

**唤醒机制：事件订阅为主，分钟级巡检为兜底**

`watchHandoffCompletion` 改为订阅 `SQLiteBlackboard` 既有的事件广播（`bb.Subscribe`，与 `debate_worker.go`/`default_worker.go`/`pattern_dag.go`/`pattern_state_graph.go` 已确立的 idiom 一致），收到 `task_completed`/`task_failed` 事件即唤醒投递 `TriggerAgentHandoffDone`。`handoffWatchSafetyInterval`（2 分钟）巡检仅覆盖"订阅通道因实现方内部异常静默失活"这一残余风险，不再是主路径。`handoffWatchPollInterval`（1 秒）纯轮询实现保留作为 `Subscribe` 本身不可用时的降级路径。订阅者用本函数派生的 `watchCtx`（非直接透传 `a.ctx`）以便随每次 `transfer_to_agent` 调用逐次收敛注销，防止同一 Agent 生命周期内多次委派导致订阅者无上限累积。

**崩溃恢复：`resume_ctx_json` 快照 + 强制重跑 S_VALIDATE**

`task_checkpoints` 表新增 `resume_ctx_json` 列（`types.TaskCheckpointRow.ResumeCtxJSON`），挂起时 `buildHandoffResumeSnapshot` 序列化 `HandoffResumeContext{SchemaVersion, DAGModel, ExecuteResult, CompletedNodeIDs, GlobalTaintLevel, HandoffNodeID, NamespaceID, SnapshotAt}` 落盘。字段取舍原则：只收纳 `runExecuteDAG` 重建执行所必需的字段；`sCtx` 中其余字段（`RawIntentTS`/`SysEnvSnapshot`/`EpochTracker`/`LastReasoningContent` 等）不入快照——要么可从 DB/配置重算，要么含敏感内容（PII 最小化）。序列化体积超过 `handoffSnapshotMaxBytes`（256KB）不落盘，只记 Warn + counter，退化为旧的"仅消除死锁"行为，而非让整个挂起流程失败——快照是无损续跑的增强，不是挂起本身的前提条件。

`HandoffResumeContextVersion`（当前 1）随结构变更递增；`ResumeAwaitingHandoff` 反序列化遇到版本不匹配一律拒绝恢复，不按当前结构强行解析（字段错位会产出"看似合法"的错误 DAG）。恢复成功回填 `sCtx.DAGModel`/`ExecuteResult`/`CompletedNodeIDs` 前，**强制重跑与正常路径完全相同的 S_VALIDATE 四层校验**（`a.dagValidator.Validate`）——`resume_ctx_json` 来自数据库，属于跨越信任边界的反序列化输入，即便源自本进程此前写入，也必须假定可能被直接改库篡改（HE-2 不得因"数据来自自家表"而放行）；校验失败或 `dagValidator` 为 `nil`（fail-closed）一律丢弃快照 DAG，降级为"仅消除死锁"的旧行为。`GlobalTaintLevel` 回填遵循 only-up 语义（快照值仅在高于当前值时采纳，防降级攻击，ADR-0007）。

`AwaitingHandoffReconciler.reconcileOne` 返回值 `restored` 区分"无损续跑成功"与"仅消除死锁"两种结果，两者都记录明确日志（`metrics.RecordAgentHandoffResume(ctx, "restored"|"degraded")`），禁止把降级结果误判为正常完整完成。

## 后果

- **正向**：Blackboard 读压从 O(N×轮询频率) 降为事件驱动的近零常态开销；崩溃重启后委派节点下游 DAG 节点不再静默丢失（此前是完全不可观测的数据丢失）。
- **负向**：快照体积上限（256KB）意味着超大 DAG 场景会静默降级为"仅消除死锁"，需要监控 `RecordAgentHandoffSnapshotOversized` counter 判断是否触顶。`handoffSnapshotMaxBytes` 当前是硬编码常量，尚未登记进 `state.yaml` 可配置阈值（见本轮 state.yaml 阈值登记，`agent_handoff.resume_snapshot_max_bytes`）。
- **反例守护**：拒绝在恢复路径跳过 S_VALIDATE 重校验——"数据来自自家表"不构成信任依据，任何放宽此约束的提议需重新论证反序列化输入的信任模型。拒绝把 `handoffWatchSafetyInterval` 巡检当作主唤醒路径重新加权——事件订阅是主路径，巡检间隔刻意取分钟级以避免退化回轮询版本的读压问题。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 恢复时信任 `resume_ctx_json`，跳过重校验以降低恢复延迟 | 违反 HE-2：反序列化输入跨越信任边界，"数据来自自家表"不是安全边界 |
| 快照失败时让整个挂起流程失败 | 快照是无损续跑的增强能力，不应把增强能力的失败升级为核心功能（消除死锁）的失败 |
| 完全依赖事件订阅，去掉分钟级兜底巡检 | 订阅通道可能因实现方内部异常静默失活且无信号，兜底巡检是该残余风险的唯一保险 |

## 引用代码

- `internal/agent/agent_handoff.go`（`watchHandoffCompletion`/`pollHandoffCompletion`/`handoffWatchPollInterval`/`handoffWatchSafetyInterval`）
- `internal/agent/agent_handoff_resume.go`（`HandoffResumeContext`/`buildHandoffResumeSnapshot`/`persistHandoffWaitCheckpoint`/`ResumeAwaitingHandoff`）
- `internal/agent/reconciler_handoff.go`（`AwaitingHandoffReconciler`）
- `internal/protocol/schema/035_task_checkpoints.sql`（`resume_ctx_json` 列）
- `local_playground/upgrade/04-arch-refactor.md` §A-01/§A-02（原始设计记录）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-01 | 初稿，阶段06 ADR 归档补录（对应阶段04 A-01/A-02 已落地实现） |
| 2026-08-09 | 追记：重新评估触发条件——恢复路径跳过 S_VALIDATE 重校验的提议须先重新论证 `resume_ctx_json` 反序列化输入的信任模型（"数据来自自家表"不构成理由）；`handoffSnapshotMaxBytes`（256KB，当前硬编码常量）若观测到 `RecordAgentHandoffSnapshotOversized` 触顶频繁，应先登记为 `state.yaml` 可配置阈值再评估是否调大。 |
