# ADR-0104: 崩溃恢复与 State-in-DB：回放、断点续跑、Outbox 幂等、Handoff 续跑快照、运行记录对账

> 状态：Accepted ｜ 首次：2026-07-22（原 0076）/ 2026-08-01（原 0086）/ 2026-09-29（本号） ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0076、ADR-0086 ｜ 模块：M04 / M05 / M08 / M09 / M10(HITL) / M12 / `cmd/polaris`

**原则：执行体可以在内存，状态必须在表；派生状态从账本重建，不另存副本。** 决策一~九 State-in-DB 补齐（HE-6）；十~十三崩溃恢复（原 0076）；十四~十五 Handoff 续跑（原 0086）。

## 缺口（2026-09-29 `cd155fb` 盘点，G1~G11）
三类结构性问题：（1）`running`/`pending` 行的执行体只在内存，重启后无人对账，行悬挂并阻断调度（G1/G4/G5）；（2）会话级状态做成进程单例（todo 文件、任务画布、预算计数器），跨会话串味（G2/G3/G9）；（3）同一事实散落 KV 与内存，或表已设计零写入（G6/G7/G8/G10/G11）。

## 决策一：启动期孤儿对账（原 ADR-0104 决策一；G1/G4/G5/G6）
- `cmd/polaris/boot_orphan_reconciler.go`，单实例锁之后、调度器启动之前执行一次：`automation_runs`/`workflow_runs`/`subagent_runs` 的 `running`→`interrupted`；`automations`/`workflows` 的 `last_run_status='running'`→`error`（不计 failure_count，不触发熔断）；`planner_sessions.running`→`failed`；`hitl_requests.pending`→`orphaned`。
- 单实例锁保证启动时所有 running 行必为孤儿，无需 owner/epoch 列。已删除零调用的 `TimeoutRuns`。

## 决策二：工作流中断可续跑，不自动重跑（原 ADR-0104 决策二）
- `POST /v1/workflows/runs/{id}/resume`：仅 `interrupted|error` 可续跑，同一 runID 重入 StateGraphExecutor，`done` 节点复用（决策十一机制）。
- 启动期不自动续跑：工作流步骤是完整 Agent 运行，`executing` 节点重跑可能重复不可逆副作用，与决策十"S_EXECUTE 保守跳过"同一边界。

## 决策三：`session_todos` 取代 todo 文件（原 ADR-0104 决策三；G2）
- `(session_id, seq)` 主键，`status ∈ pending|in_progress|completed`；会话 ID 取 `protocol.CtxTaskIDKey`，缺失即报错（fail-closed，不回落全局）。`todo_write` 整表替换。

## 决策四：预算以 `llm_calls` 为唯一账本（原 ADR-0104 决策四；G3）
- 删内存计数与 3$/M 估算：会话用量 = `SUM(input_tokens+output_tokens) WHERE session_id=?`；月度花费 = `SUM(cost_usd) WHERE created_at >= 当月 UTC 起点`；月度上限每次读 `BudgetRepository`（短 TTL 缓存）。
- `buildAgent` 为每个 Agent（含池化会话 Agent）注入绑定 sessionID 的预算控制器。账本可滞后一次调用：预算是软熔断不是计费。

## 决策五：`hitl_requests` 取代 HITL KV（原 ADR-0104 决策五；G4）
- 一行一审批，`status ∈ pending|approved|denied|timeout|orphaned`，`decided_by ∈ human|auto_approve|auto_deny|trust_downgrade|timeout_kill`；`Respond` 条件更新 `WHERE status='pending'` 保幂等。
- `TrustScorer` 去内存 map，`ShouldDowngrade` 查表：同 `(checkpoint_type, agent_id)` 在 Window 内、最近一次人工拒绝之后的人工批准数 ≥ MinApprovals。**仅 `decided_by='human'` 参与**（GD-14-004 正反馈防线不变）。

## 决策六：`subagent_runs` 与 `planner_sessions` 写入（原 ADR-0104 决策六；G5/G6）
- `SubagentRunner` 为唯一写入点（三条入口共用），经调用方定义的 recorder 接口注入，nil 安全；子 Agent 用 `child_session_id`（`sub-{agent_id}`）关联轨迹。`PlannerPool` 经 recorder 写 `planner_sessions`（running→done/failed，含 winning_score/winning_engine）。

## 决策七：`session_trajectory` 取代 `events:session:*` KV（原 ADR-0104 决策七；G7/G8/G9）
- 列：`session_id, seq（会话内单调，MAX+1）, event_type, tool_name, tool_ok, latency_ms, payload(JSON), created_at`。唯一轨迹账本，派生：`PolicyEvolver` warm-start；全进程单例 `SurpriseCalculator`（修泄漏），Markov 矩阵由轨迹工具序列 warm-start，`CurrentSurprise` 按 taskID 分桶；`RenderTaskCanvas(ctx, sessionID)` 从会话 tool 行渲染，全局画布单例已删，`GET /v1/agent/mmd-canvas` 必填 `session_id`。
- `events` 表（MutationBus 哈希链）语义不变、不合并：读写模式与完整性要求不同。

## 决策八：PII 快照独立表（原 ADR-0104 决策八；G10）
- `task_pii_vault(task_id, field, enc_value, expired_at)`；删 `preferences` 挪用与 `tasks.pii_vault_blob`；恢复路径由任务消费方按 task_id 读取。

## 决策九：统一游标表（原 ADR-0104 决策九；G11）
- `inbox_cursors`→`consumer_cursors`（`consumer_id` 自由命名），`learning_cursors` 并入，`MemoryAgent` 以 `memory_agent.whisper` 持久化高水位。

## 决策十：Perceive/Plan/Reflect 崩溃恢复回放（原 ADR-0076 决策一，含原 ADR-0057）
- `Agent.Run()` 写 `inflight:session:{id}` KV 标记，干净退出 `defer` 清除，崩溃则残留。`recoverCrashedSessions`（`cmd/polaris/boot_crash_recovery.go`，bootAgent 之后、bootServer 之前）扫候选会话，用 `TrajectoryRecorderImpl` 从 `session_trajectory`（决策七）重建 `TrajectoryTrace`。
- **仅当最后状态为纯 LLM 状态（S_PERCEIVE/S_PLAN/S_REFLECT）才自动恢复**；S_VALIDATE/S_EXECUTE/S_REPLAN/S_ROLLBACK 一律保守跳过（2PC 预写日志未经专门审计，不作自动恢复安全网）。
- 经 `AgentPool.Acquire`（同生产路径）+ `InjectReplayData` 注入 LLM 调用录像 + `SetReplayMode(true)` 驱动 FSM；最后一条录像消费时翻转 false。只回放 LLM 调用，工具在窗口内短路为 stub。

## 决策十一：Task Checkpoint 断点续跑（原 ADR-0076 决策二）
- `task_checkpoints` 表（`(task_id, node_id, attempt)` 主键，`pending/executing/done/failed` + `output_json`）。`StateGraphExecutor.Execute` 每节点前查表，命中 `done` 直接复用输出；Reaper 恢复路径注入已 `done` 节点集合，仅续跑失败/未执行节点。
- 不可逆副作用前须经决策十二幂等键走 Outbox 校验。补齐决策十的执行阶段局限，不推翻其边界。

## 决策十二：Outbox 幂等键唯一性（原 ADR-0076 决策三，含原 ADR-0059）
- 问题：`outbox.Write()` 裸 INSERT，UNIQUE 冲突被 `_ =` 静默丢弃；旧键 `{sessionID}:{阶段}:{agentID}` 全会话不变 → 多轮对话仅第一轮的记忆投影/语义抽取/蒸馏能写入。
- 修复：不改 `NextEventID`（其确定性是决策十重放的必要不变量）；FSM 派生事件键 = 规范前缀（`types.BuildIdempotencyKey`）+ `outboxUniqueSuffix()`（纳秒时间戳 + 进程内原子计数器，单独时间戳不足防同 goroutine 背靠背冲突），见 `Agent.outboxIdemKey`。这批事件必须每次都是新事件、不可被 outbox 去重。

## 决策十三：S_AWAIT_AGENT 被动恢复 Reconciler（原 ADR-0076 决策四）
- `AwaitingHandoffReconciler`（`recoverAwaitingHandoffs`）查 `task_checkpoints` 中 `status="await_agent"` 且 `reason="handoff_wait"` 的记录，重挂子任务监听或（子任务已完成）直接推进 FSM 到 `S_EXECUTE`；退出 `S_AWAIT_AGENT` 时清理该 checkpoint（`status="done"`），扫描幂等。
- 不扩展决策十白名单：属异步监听的被动恢复，非 LLM 现场重放。

## 决策十四：Handoff 唤醒事件化（原 ADR-0086 决策·唤醒机制；GD-13-007）
- 旧 1s ticker 轮询对单写者 `SQLiteBlackboard` 产生 N QPS 读压。现 `watchHandoffCompletion` 订阅 `bb.Subscribe`，收到 `task_completed`/`task_failed` 即投递 `TriggerAgentHandoffDone`。
- `handoffWatchSafetyInterval`（2 分钟）巡检仅兜底"订阅通道静默失活"；`handoffWatchPollInterval`（1s）轮询仅作 `Subscribe` 不可用时降级。订阅者用派生 `watchCtx`，随每次 `transfer_to_agent` 注销，防累积。

## 决策十五：崩溃后无损续跑快照（原 ADR-0086 决策·崩溃恢复；GD-13-009）
- 旧 `ResumeAwaitingHandoff` 只回填 `HandoffTaskID`+`ForceState`，重启后 `sCtx.DAGModel` 丢失，nil-DAGModel 快速路径直接推进 `ExecuteDone`，委派下游 DAG 被静默截断。
- `task_checkpoints.resume_ctx_json`（`types.TaskCheckpointRow.ResumeCtxJSON`）：挂起时 `buildHandoffResumeSnapshot` 序列化 `HandoffResumeContext{SchemaVersion, DAGModel, ExecuteResult, CompletedNodeIDs, GlobalTaintLevel, HandoffNodeID, NamespaceID, SnapshotAt}`。只收 `runExecuteDAG` 重建必需字段，可重算或含敏感内容者不入（PII 最小化）。
- 体积 > `handoffSnapshotMaxBytes`（256KB，`state.yaml` `resume_snapshot_max_bytes`）不落盘，记 Warn + `RecordAgentHandoffSnapshotOversized`，退化为"仅消除死锁"，不让挂起失败。
- `HandoffResumeContextVersion`（当前 1）随结构变更递增，反序列化版本不匹配一律拒绝，不强行解析。
- 恢复回填前**强制重跑 S_VALIDATE 四层校验**（`a.dagValidator.Validate`）：`resume_ctx_json` 来自 DB，属跨信任边界的反序列化输入，须假定可被直接改库篡改（HE-2）；失败或 `dagValidator==nil`（fail-closed）丢弃快照，降级为"仅消除死锁"。`GlobalTaintLevel` only-up 回填（防降级攻击，ADR-0007）。
- `reconcileOne` 返回 `restored` 区分无损续跑与降级，计 `RecordAgentHandoffResume`，禁止把降级误判为完整完成。

## 反例守护 / 已驳方案
- 为运行记录加 owner/epoch 列做精确孤儿判定 — 单实例锁已保证（决策一）
- 启动期自动续跑工作流 — 重复不可逆副作用（决策二）
- 内存另维护预算/信任/画布计数"加速" — 派生状态从账本读（决策四/五/七）；会话级状态挂 `MemImpl` 等进程单例 — 跨会话串味
- 另建 `tool_calls` / `budget_ledger` 表 — 与轨迹表 / `llm_calls` 双源；轨迹并入 `events` — 破坏哈希链单写者语义
- KillSwitch 状态入库 — `.fullstop` 文件刻意设计，DB 损坏仍须 fail-closed；能力令牌撤销表入库 — 密钥每进程生成，重启令牌全失效
- 恢复路径信任 `resume_ctx_json` 跳过重校验 — 违反 HE-2；快照失败令挂起整体失败 — 增强不应拖垮核心；去掉兜底巡检或把巡检当主路径 — 订阅失活无信号

**维持内存态（审查确认）**：KillSwitch 计数、`TokenManager`、`ExemptionVault`、HITL waiter channel、MCP elicitation broker、LLM 熔断/限流/凭证冷却、Supervisor 协程树、DAG/StateGraph 执行 map（`task_checkpoints` 覆盖）、Agent `sCtx`（inflight + 轨迹覆盖）、缓存。

## 重新评估触发条件
① 多实例部署 → 决策一改 owner_instance + 心跳租约；② 轨迹写 p99 > 20ms 持续一周 → 批量异步写；③ 计费级预算 → 调用前预留；④ 决策十扩大到 S_VALIDATE/S_EXECUTE/S_REPLAN/S_ROLLBACK，须先过 2PC 预写日志专项审计；⑤ 放宽决策十五重校验，须先重论证反序列化输入信任模型；⑥ Oversized 计数频繁触顶 → 评估调大 `resume_snapshot_max_bytes`。

## 沿革
| 日期 | 变更 | 来源 |
|---|---|---|
| 2026-07-22 | 崩溃回放 + Checkpoint + Outbox 幂等合集（07-28 并入原 0057/0059） | 原 0076 |
| 2026-08-01 | Handoff 唤醒事件化 + 续跑快照 | 原 0086 |
| 2026-09-29 | State-in-DB 补齐初稿；轨迹迁 `session_trajectory`（订正原 0076 决策一"轨迹存 KV"）；G9 订正：`TrackToolCall` 实有调用但落在全进程单例上，已连同单例删除 | 本号原文 |
| 2026-10-12 | 0076/0086 并入本号为决策十~十五 | 文档精简 |
| 2026-10-12 | 原 0076 决策三"`BuildIdempotencyKey` 保持 DEFER、不迁移"已被取代：全仓幂等键现统一走 `types.BuildIdempotencyKey`，FSM 派生事件为"规范前缀 + 唯一后缀"（决策十二） | 代码事实 |

## 引用代码
- `cmd/polaris/`：`boot_orphan_reconciler.go`、`boot_crash_recovery.go`、`boot_handoff_reconciler.go`、`boot_events.go`、`boot_agent.go`、`boot_server.go`
- `internal/agent/`：`agent_execute_effect_helpers.go`、`agent_handoff.go`、`agent_handoff_resume.go`、`reconciler_handoff.go`、`budget.go`
- `internal/protocol/replay.go`、`internal/execute/orchestrator/pattern_state_graph.go`、`internal/execute/orchestrator/subagent_runner.go`、`internal/swarm/planner/pool.go`、`internal/automation/hitl/`、`internal/learning/surprise/`、`internal/memory/graph/mmd_canvas.go`
- `internal/protocol/schema/`：`002_outbox.sql`、`035_task_checkpoints.sql`、`042_subagent_runs.sql`、`043_session_todos.sql`、`044_hitl_requests.sql`、`045_session_trajectory.sql`、`046_task_pii_vault.sql`
