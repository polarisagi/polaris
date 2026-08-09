# ADR-0076: 崩溃恢复回放 + Task Checkpoint + Outbox 幂等修复合集（含原 ADR-0057/0059）

- **状态**: Accepted（已执行）| **日期**: 2026-07-22（合并 2026-07-28）| **模块**: M04 §8 / `internal/execute/orchestrator/` / `internal/agent/` / `internal/store/`

## 决策一：Perceive/Plan/Reflect 崩溃恢复回放（原 ADR-0057）

`Agent.Run()` 写入 `inflight:session:{id}` KV 标记，干净退出经 `defer` 清除，进程崩溃则残留供下次启动识别。`recoverCrashedSessions`（`main.go`，`bootAgent` 之后 `bootServer` 之前调用）扫描候选会话，用 `TrajectoryRecorderImpl` 从 EventLog 重建 `TrajectoryTrace`，**仅当最后状态落在纯 LLM 状态（S_PERCEIVE/S_PLAN/S_REFLECT）时才自动恢复**——S_VALIDATE/S_EXECUTE/S_REPLAN/S_ROLLBACK 一律保守跳过（2PC 预写日志理论可保护但未经专门审计，不作为自动恢复安全网）。经 `AgentPool.Acquire`（与生产请求相同路径）+ `InjectReplayData` 注入历史 LLM 调用录像 + `SetReplayMode(true)` 驱动 FSM 续跑；消费最后一条录像的同一瞬间翻转为 false。仅 LLM 调用被回放，工具执行在 ReplayMode 窗口内统一短路为 stub，不重放工具历史输出。

## 决策二：Task Checkpoint 断点续跑（补齐决策一的保守局限）

新增 `task_checkpoints` 表（`(task_id, node_id, attempt)` 主键，记录 `pending/executing/done/failed` + `output_json`）。`StateGraphExecutor.Execute` 执行每节点前查表，命中 `done` 直接复用输出。非幂等动作双重防御：真正触发不可逆副作用前须经决策三幂等键走 Outbox 校验。Reaper 恢复路径注入已 `done` 节点集合，仅对失败/未执行节点续跑。本决策正式补齐决策一"保守跳过 execute 阶段"的局限，不推翻其安全边界。

## 决策三：Outbox 幂等键唯一性修复（原 ADR-0059，非 BuildIdempotencyKey 统一迁移）

`pkg/types.BuildIdempotencyKey` 的 `version int` 参数不对应任何真实调用点的业务概念，强行迁移等同臆造语义（R1 违规），**保持 DEFER**。真正问题是 7 处调用点的幂等键构造本身不唯一：`outbox.Write()` 是裸 INSERT，`idempotency_key UNIQUE` 冲突时 error 被 `_ = outbox.Write(...)` 静默丢弃。最严重的一处：`agent_execute_effect_helpers.go`（perceive/plan/exec/reflect 投影 + consolidate 触发）用 `"{sessionID}:{阶段}:{agentID}"` 作键，session 全生命周期不变——**多轮对话场景下，记忆投影/语义抽取/记忆蒸馏链路实际只有第一轮能写入，第二轮起全部静默丢弃**。

修复：不改 `NextEventID`（其确定性是决策一崩溃恢复重放的必要不变量），在 outbox 幂等键构造层追加 `outboxUniqueSuffix()`（纳秒时间戳 + 进程内单调原子计数器，单独时间戳不足以防同一 goroutine 背靠背调用冲突）。

## 决策四：S_AWAIT_AGENT 被动恢复 Reconciler

进程崩溃前若停留在 `S_AWAIT_AGENT`（等待 Handoff 子任务）且没有在 LLM 状态写入标记，原有恢复机制会遗漏该会话。新增 `AwaitingHandoffReconciler`（启动期由 `recoverAwaitingHandoffs` 调度），通过查询 `task_checkpoints` 中 `status="await_agent"` 且 `reason="handoff_wait"` 的记录，重新挂载子任务监听或直接推进 FSM 到 `S_EXECUTE`（子任务已完成）。FSM 退出 `S_AWAIT_AGENT` 进入 `S_EXECUTE` 时清理该 checkpoint (`status="done"`)，确保扫描幂等。不扩展原 LLM 回放白名单，因为这本质是异步任务监听的被动恢复，而非 LLM 执行现场的重放。

## 引用代码

`internal/protocol/replay.go`、`cmd/polaris/boot_crash_recovery.go`、`internal/execute/orchestrator/pattern_state_graph.go`、`internal/agent/agent_execute_effect_helpers.go`、`cmd/polaris/boot_handoff_reconciler.go`、`internal/agent/reconciler_handoff.go`

> 2026-08-09 追记：重新评估触发条件——① 决策一"S_VALIDATE/S_EXECUTE/S_REPLAN/
> S_ROLLBACK 保守跳过自动恢复"须先有专门的 2PC 预写日志审计通过，才能扩大自动
> 恢复范围到这些状态；② `pkg/types.BuildIdempotencyKey` 的 `version` 参数保持
> DEFER，重提统一迁移须先找到真实对应业务概念的调用点，而非为了"统一"而臆造
> 语义。
