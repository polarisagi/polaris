# ADR-0104: State-in-DB 补齐——运行记录对账、会话计划、预算账本、审批表、执行轨迹表

- **状态**: Accepted
- **日期**: 2026-09-29
- **决策者**: 架构评审（MrLaoLiAI 授权）
- **相关模块**: M04 / M05 / M08 / M09 / M10(HITL) / M12(automation) / `cmd/polaris`

## 上下文

2026-09-29 全仓盘点 HE-6（`00-Constitution.md §R3-HE6`）落实情况。结论：真正的缺口不是"少几张表"，
而是三类结构性问题——（1）DB 里有 `running`/`pending` 行，但持有它的执行体只在进程内存，重启后无人对账，
行永久悬挂并**阻断后续调度**；（2）会话/任务级状态被做成进程级单例（全局 todo 文件、全局任务画布、
全局预算计数器），既不落盘也跨会话串味；（3）同一份事实散落在 KV 前缀与内存 map 两处，表结构已设计
却零写入（`planner_sessions`、`inbox_cursors`、`tasks.pii_vault_blob`）。

实证（代码事实，2026-09-29 `cd155fb`）：

| # | 缺口 | 代码事实 | 后果 |
|---|------|---------|------|
| G1 | 自动化/工作流运行孤儿 | `repo_automation.go` 到期查询 `last_run_status != 'running'`；`workflow_cron.go:29` 同；`TimeoutRuns` 零调用点 | 执行中崩溃 → 该自动化/工作流**永久不再调度** |
| G2 | 会话计划（todo）全局单文件 | `guard.GetTodoPath` = `allowedPaths[0]/.polaris_todo.json` | 所有会话/子 Agent 共写一份计划，互相覆盖，不在 DB |
| G3 | 预算仅内存且只注入 agent-0 | `boot_server.go` 仅 `ab.Agent.SetBudget`；`buildAgent` 未注入；`EstimatedSpendUSD` 按 3$/M 估算 | 池化会话 Agent **月度预算从不生效**；重启清零；`llm_calls.cost_usd` 真实账本未被使用 |
| G4 | HITL 审批 KV + 内存信任分 | `hitl:pending:`/`hitl:archive:` KV；kill_pause 超时不删 pending；`TrustScorer.entries` 内存 map | 重启/超时留下幽灵待审；信任累积重启归零且与审计记录不同源 |
| G5 | 子 Agent 运行无记录 | `SubagentRunner.Run` 不落任何行（fork 技能/agent hook 两条入口完全无痕） | 父子血缘、失败原因、续跑次数不可追溯 |
| G6 | `planner_sessions` 零写入 | 全仓无 INSERT | MCTS 规划审计与经验积累从未发生 |
| G7 | 执行轨迹存 KV JSON | `boot_events.go` 写 `events:session:{sid}:{ts}_{seq}`；工具事件无成败/耗时字段 | 不可按类型/工具检索；`RunReplay` 读不存在的 `Offset` 字段恒判一致；学习状态无法从轨迹重建 |
| G8 | 学习状态每会话内存重建 | `buildAgent` 每会话 `NewSurpriseCalculator`（4 goroutine，无 Close）；`PolicyEvolver` 纯内存 | goroutine 泄漏；Markov/工具策略每次重启从零学起；`WithMarkovMatrix` warm-start 零调用 |
| G9 | 任务画布全局且死 | `MemImpl.taskCanvas` 单例；`TrackToolCall` 零调用点 | `RenderTaskCanvas` 恒空，且若接线即跨会话串味 |
| G10 | PII 快照写进 `preferences` | `pii_vault.go` `INSERT INTO preferences key='pii_vault:*'`；`ListPreferences` 无过滤 | 密文与 task_id 经 `/preferences` API 外露；恢复写入全局 Scratch 无人读取；`tasks.pii_vault_blob` 死列 |
| G11 | 游标表两套一死一缺 | `inbox_cursors` 零引用；`learning_cursors` CHECK 限死 4 流；`MemoryAgent.lastSeenID` 内存 | 重启后耳语通道从 0 重推全部高显著事件 |

## 决策

**原则：执行体可以在内存，状态必须在表；派生状态从账本重建，不另存副本。**

1. **决策一 启动期孤儿对账（G1/G5/G6/G4 的收尾面）**。新增 `cmd/polaris/boot_orphan_reconciler.go`，在单实例锁之后、任何调度器启动之前执行一次：`automation_runs`/`workflow_runs`/`subagent_runs` 的 `running` → `interrupted`；`automations`/`workflows` 的 `last_run_status='running'` → `error`（`last_run_error='interrupted by restart'`，**不计入** failure_count，不触发熔断）；`planner_sessions.running` → `failed`；`hitl_requests.pending` → `orphaned`。单实例锁保证"启动时所有 running 行必为孤儿"，无需 owner/epoch 列。删除零调用的 `TimeoutRuns`。
2. **决策二 工作流中断可续跑，不自动重跑**。新增 `POST /v1/workflows/runs/{id}/resume`：仅 `interrupted|error` 可续跑，以**同一 runID** 重入 StateGraphExecutor，`task_checkpoints` 中 `done` 节点直接复用（ADR-0076 决策二已有机制，此前无触发入口）。启动期不自动续跑：工作流步骤是完整 Agent 运行，`executing` 节点重跑可能重复不可逆副作用，与 ADR-0076 决策一"S_EXECUTE 保守跳过"同一边界。
3. **决策三 `session_todos` 表取代 todo 文件（G2）**。`(session_id, seq)` 主键，`status ∈ pending|in_progress|completed`；会话 ID 取工具执行 ctx 的 `protocol.CtxTaskIDKey`（与 `usage_recorder` 同一来源），缺失即报错（fail-closed，不回落全局）。`todo_write` 整表替换语义（单事务先删后插），入参兼容字符串数组与 `{content,status}` 对象数组。
4. **决策四 预算以 `llm_calls` 为唯一账本（G3）**。删除 `BudgetManager.usedTokens` 内存计数与 3$/M 估算：会话用量 = `SUM(input_tokens+output_tokens) WHERE session_id=?`，月度花费 = `SUM(cost_usd) WHERE created_at >= 当月 UTC 起点`，月度上限每次读 `BudgetRepository`（短 TTL 缓存），`HandleSetBudget` 不再需要向 Agent 推送。`buildAgent` 为每个 Agent（含池化会话 Agent）注入绑定其 sessionID 的预算控制器。账本可能滞后一次调用，接受：预算是软熔断不是计费。
5. **决策五 `hitl_requests` 表取代 HITL KV（G4）**。一行一审批，`status ∈ pending|approved|denied|timeout|orphaned`，`decided_by ∈ human|auto_approve|auto_deny|trust_downgrade|timeout_kill`；`Respond` 用 `UPDATE … WHERE status='pending'` 条件更新保证幂等；kill_pause 超时写 `timeout`。`TrustScorer` 去掉内存 map，`ShouldDowngrade` 直接查表：同 `(checkpoint_type, agent_id)` 在 Window 内、最近一次人工拒绝之后的人工批准数 ≥ MinApprovals。**只有 `decided_by='human'` 的行参与**（GD-14-004 正反馈防线不变）。
6. **决策六 `subagent_runs` 与 `planner_sessions` 写入（G5/G6）**。`SubagentRunner` 为唯一写入点（三条入口共用），经调用方定义的 recorder 接口注入，nil 安全；子 Agent 使用确定的 `child_session_id`（`sub-{agent_id}`）以便与轨迹表关联。`PlannerPool` 经 recorder 写 `planner_sessions`（running → done/failed，含 winning_score/winning_engine）。
7. **决策七 `session_trajectory` 表取代 `events:session:*` KV（G7/G8/G9）**。列：`session_id, seq（会话内单调，写入时 MAX+1）, event_type, tool_name, tool_ok, latency_ms, payload(JSON), created_at`。以其为唯一轨迹账本派生三类状态：`PolicyEvolver` 启动 warm-start（每工具最近 window 条）；全进程**单例** `SurpriseCalculator`（修 goroutine 泄漏），Markov 矩阵启动时由轨迹工具序列 warm-start，`CurrentSurprise` 按 taskID 分桶；任务画布删除全局单例与死接口，`RenderTaskCanvas(ctx, sessionID)` 从该会话 tool 行纯函数渲染。`events` 表（MutationBus 哈希链 EventLog）语义不变、不合并——两者读写模式与完整性要求不同。
8. **决策八 PII 快照独立表（G10）**。`task_pii_vault(task_id, field, enc_value, expired_at)`，删除 `preferences` 挪用与 `tasks.pii_vault_blob` 死列；恢复路径由恢复后的任务消费方按 task_id 读取，删除写入全局 Scratch 的死路径。
9. **决策九 统一游标表（G11）**。`inbox_cursors` 更名 `consumer_cursors`（`consumer_id` 自由命名），`learning_cursors` 并入（`learning.task` 等），`MemoryAgent` 以 `memory_agent.whisper` 持久化高水位。

## 后果

- **正向**: 崩溃不再永久阻断调度；会话计划/预算/审批/子 Agent/轨迹均可查询审计；三类学习状态跨重启累积；消除一处 goroutine 泄漏与三处跨会话串味。
- **负向**: 新增 5 张表（042–046）；轨迹写入从 KV Put 改为带 MAX+1 的 INSERT，单写连接下串行；上线前阶段，旧 KV 前缀数据不迁移（开发库重建）。
- **反例守护**: 以下提议引用本 ADR 拒绝——① 为运行记录加 owner/epoch 列做"精确孤儿判定"（单实例锁已保证，见决策一）；② 启动期自动续跑工作流（决策二）；③ 在内存另维护一份预算/信任/画布计数"加速"（派生状态从账本读，决策四/五/七）；④ 把会话级状态挂在 `MemImpl` 等进程单例上。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 另建 `tool_calls` 账本表 | 与轨迹表双写同一事实；轨迹表加 `tool_name/tool_ok/latency_ms` 列即可覆盖 |
| 轨迹并入 `events` 表 | `events` 是 MutationBus 哈希链单写者日志，轨迹是高频会话流水，混写破坏哈希链与主题索引语义 |
| 新建 `budget_ledger` 表 | `llm_calls` 已按调用记 token 与 cost_usd，再建账本即双源 |
| KillSwitch 状态入库 | `.fullstop` 文件是刻意设计：DB 损坏时仍须 fail-closed，维持现状 |
| 能力令牌撤销表入库 | Ed25519 密钥每进程生成，重启后旧令牌全部失效，撤销列表无需跨重启 |

## 维持内存态（审查后确认正确）

KillSwitch 瞬时计数、`TokenManager`（进程级密钥）、`ExemptionVault`（短时豁免，丢失即重新审批，fail-safe）、HITL waiter channel、MCP elicitation broker、LLM 熔断/限流/凭证冷却、Supervisor 协程树、DAG 单次执行 map、StateGraph 运行 map（由 `task_checkpoints` 覆盖）、Agent `sCtx`（由 inflight 标记 + 轨迹回放覆盖）、各类缓存。

## 引用代码

- `cmd/polaris/boot_orphan_reconciler.go`、`cmd/polaris/boot_events.go`、`cmd/polaris/boot_agent.go`、`cmd/polaris/boot_server.go`
- `internal/protocol/schema/042_subagent_runs.sql`、`043_session_todos.sql`、`044_hitl_requests.sql`、`045_session_trajectory.sql`、`046_task_pii_vault.sql`、`002_outbox.sql`、`010_self_improve.sql`
- `internal/agent/budget.go`、`internal/automation/hitl/`、`internal/execute/orchestrator/subagent_runner.go`、`internal/swarm/planner/pool.go`
- `internal/eval/harness/eval.go`、`internal/learning/surprise/`、`internal/action/tool_usage_policy.go`、`internal/memory/graph/mmd_canvas.go`

## 重新评估触发条件

① 引入多实例/集群部署（单实例锁不再成立）→ 决策一须改为 owner_instance + 心跳租约；② 轨迹写入成为 SQLite 单写者瓶颈（p99 写延迟 > 20ms 持续一周）→ 评估批量异步写入；③ 需要计费级精确预算 → 预算检查改为调用前预留而非事后账本。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-29 | 初稿 |
| 2026-09-29 | 决策七实施复核：G9 事实订正——`TrackToolCall/TrackToolResult` 并非零调用，`agent_execute_dag.go` 的 `toolExecFn` 经 `a.memory` 调用，但落在全进程共享的 `MemImpl.taskCanvas` 单例上（跨会话串味）；实施时该两处调用与单例一并删除，画布改由工具轨迹行渲染。`GET /v1/agent/mmd-canvas` 新增必填 query `session_id`（缺失返回 400）。 |
