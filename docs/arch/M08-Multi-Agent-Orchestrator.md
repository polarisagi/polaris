# 模块 8: Multi-Agent Orchestrator

> 物理实现在 `internal/execute/orchestrator`（ADR-0046、`internal/execute/CLAUDE.md`）；`internal/swarm` 只含 supervisor / planner / agents / startup，是消费方。

> 单机黑板 + CAS（Compare-And-Swap）原子认领 + Supervisor Tree | Go goroutine + SQLite | [HE-Rule-5] [HE-Rule-6]
<!-- §跳读: 0-bis:7 职责 / 0-ter:21 不变量速查 / 1:34 黑板+CAS(核心) / 2:91 Supervisor / 2-bis:107 常驻角色Agent / 3:121 编排模式 / 3-bis:145 (已删除,见ADR-0062) / 3-ter:151 PipelineOrchestrator / 3-quater:170 PatternDAGExecutor / 3-quinquies:181 StateGraphExecutor / 4:214 AgentCard / 5:223 Task分解 / 8:240 拓扑自演化(已删除,见ADR-0062) / 10:248 (SOFT)降级 / 11:264 353 跨模块契约 / 11.2:289 379 已知实现缺口 / 12:304 393 Custom Agent / 13:320 409 CSV Fan-out / §3-sexies:337 442 PatternDebate -->
## 0-bis. 职责边界

| M8 **是** | M8 **不是** |
|-----------|-------------|
| 多 Agent 协调黑板（PostTask + CAS Claim + Lease） | 单 Agent 内部状态机（M4） |
| Supervisor Tree 故障恢复（OneForOne + 指数退避） | 工具沙箱执行（M7） |
| 11 种编排模式执行（§3） | 编排模式的选择决策 |
| Agent Card 注册（AgentRegistry.Register/Get，供 SpawnDepth 校验） | Agent 自身能力实现 |
| Task DAG 分解（跨 Agent 边界子任务） | 子任务内部工具调用 DAG（M4 Micro-DAG） |
| 单进程多 Agent 任务流转（CAS 认领 + Handoff） | Provider 路由（M1） |
| — | **跨机调度**：M8 不做。跨节点委派在 M13 网关层（ADR-0111）：入站 `internal/gateway/mesh` 把对端任务投成黑板 `peer_delegation` 任务，由 `DefaultTaskWorker` 以只读 peer 角色执行；出站为 `MCPA2AWorker`（`a2a_delegate`，带 `spawn_depth`/`chain`）。HTTP 门面在 `internal/gateway/server/meshapi/`（`/mcp`、`/a2a/v1`、Agent Card）。`ROADMAP.md §3.4` 的"跨节点 Agent 不在计划"指共享黑板式跨机调度，不含此委派 |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 | 实现状态 |
|------|--------|---------|---------|
| inv_M8_01 | 禁止自由 NL 多 Agent 对话——所有协调经 schema 原语（Intent/Request/Claim/Result/Fail） | BlackboardEvent schema 强制 | ✅ 已实现 |
| inv_M8_02 | Blackboard 写入与 EventLog 同事务双写——EventLog 是真相源 | M2 CompositeMutationIntent | ✅ 已实现 |
| inv_M8_03 | Task 状态单调推进 Pending→Claimed→Executing→Done/Failed，禁止回退 | CAS Version++ 乐观锁 | ✅ 已实现 |
| inv_M8_04 | Agent Lease TTL 60s + 心跳 15s ±5s jitter + Reaper 1s 扫描——Lease 过期任务自动回收 | M8 §1.7 Reaper | ✅ 已实现（Reaper toxicity 计数为代码扩展） |
| inv_M8_05 | Taint 经 Blackboard 传播——input_data 携带原始 TaintLevel，协调期间不降级 | M8 §4 blackboard_entries taint_level CHECK | ✅ 已实现 |
| inv_M8_06 | 委托链深度 ≤3——跨 Agent 委托禁止超过 3 层 | M11 §8 Layer 4 多 Agent 宪法 | ✅ 已实现（`orchestrator.MaxSpawnDepth=3`，`SQLiteBlackboard.PostTask`/`PostBatch` 校验；跨节点委派共用同值） |

---

## 1. 黑板 + CAS 原子认领

### 1.1 核心结构

`SQLiteBlackboard` 是唯一生产 Blackboard 实现（`internal/execute/orchestrator/sqlite_blackboard*.go`：CAS + TOCTOU 幂等锁 + PostTask）。任务消费者是"自订阅 Blackboard + CAS 认领"模式：`default_worker.go`、`workflow_step_worker.go`（`internal/gateway/server/sysadmin/workflowadmin/`）、`debate_worker.go`、`mcp_a2a_worker.go`。其余：`pipeline*.go`、`pattern_*.go`、`reaper.go`、`csv_fanout.go`、`subagent_runner.go`。类型契约见 `internal/protocol/interfaces_swarm.go`。

已删除（ADR-0062，2026-07-14）：中心化 `Orchestrator`（RegisterWorker/dispatchPendingTasks/ListenLoop）、`Worker`、内存版 Blackboard、`SupervisorEpoch`、`FindBestAgent`——生产从未被驱动，且内存黑板违反 HE-6。

### 1.2 初始化与背压

状态全部在 SQLite `tasks` 表（HE-6），重启后不再"回放重建内存黑板"。`PostTask` 做强类型校验与 SpawnDepth 校验；Heartbeat/RenewLease 直接更新 `ExpiresAt`，不走事件订阅（控制/数据平面分离）。

2026-10 核：旧文所述 chan 占用 80%/50% 背压、`BatchRebuild`、`schemaValidator`、`SupervisorEpoch` 在代码中均无对应实现，已从本节移除；`state.yaml §m8_multiagent.max_blackboard_pending` 目前无代码读取（待决：接线或删键）。

全部常量权威源：`spec/state.yaml §m8_multiagent`。

### 1.3 Claim CAS

- **Claim**：校验 `Pending` 且未被认领 → 原子置 `Claimed`，写 `ClaimedAt`/`ExpiresAt`（lease_ttl_seconds），`Version++` 防 ABA。
- **BeginExecution**：校验认领者身份且仍为 `Claimed` → `Executing`，`Version++`。M4 首次 ExecuteTool 前必经检查点。
- 状态：Pending→Claimed→Executing→Done|Failed，禁止回退。

### 1.4 RenewLease (控制平面带外)

执行期间按心跳周期原地更新 `ExpiresAt` 并递增续约计数，不产生黑板订阅事件。

### 1.5 HITL 挂起/恢复

- **SuspendForHITL**：`Executing`→`Suspended`，以长 HITL 超时覆盖短租约。
- **ResumeFromHITL**：审批通过→`Executing` 并恢复常规 TTL；否则→`Failed`。
- **BeginCompensation / EndCompensation**：M4 回滚或 Saga 补偿期间状态为 `Compensating`（预算上限见 `compensation_timeout_seconds`），结束后置 `Failed` 待 GC。
- 跨节点任务的 HITL 只在执行节点本地完成（ADR-0111 决策五）。

### 1.6 SideEffectPreCheck (每 M7 ExecuteTool 前强制执行)

持 RLock 检查四项：Status==Executing、ClaimedBy==self、ExpiresAt>now、Version==claimedVersion；任一失败返回 `ErrStaleLease`，Agent 进入 S_ROLLBACK。通过后快照到栈并释放锁，执行 tool call，写回时重新 Lock + CAS 校验 Version。

不可逆操作（write_network/privileged）附加 TOCTOU 防护：L1 `SHA-256(taskID+operation_seq+toolName)` 幂等锁（SurrealDB-Core KV，TTL=15s 心跳续期）；L2 外部 API 原生幂等键注入（辅助）。

### 1.7 Reaper

**阶段 1（reaper_scan_interval_seconds）**：跳过 Suspended/Compensating；对 Claimed/Executing 且 `ExpiresAt` 过期的任务：取消 Agent context → 5s 宽限期（供工具 `ctx.Done()` 感知，防孤儿副作用）→ `Version++` 重置 Pending。宽限期内工具若已完成进入 PostCheck，PostCheck 发现 Version 不匹配 → 写 decision_log（M7 §4.6）。崩溃恢复时并发 cancel 全部过期任务（errgroup），统一 5s 宽限；常态为单 goroutine 串行扫描。含 `retry_count` 与 `provider_suspended_count` 逻辑。

**阶段 2（`reaperPhase2`，与阶段 1 同步触发）**：`running` 超 30 分钟的僵尸任务先 cancel goroutine 再标 failed；`pending` 超 60 分钟的饥饿任务标 failed（`reaper_phase2_pending_timeout`）。两个超时现为 `internal/execute/orchestrator/sqlite_blackboard_reaper.go` 的 SQL 字面量，未入 `state.yaml`——需运行期可调时先补 `m8_multiagent` 阈值键。

### 1.8 任务优先级

`TaskEntry.Priority` 与 M13 ResourceGovernor 统一：0 用户交互（始终放行）/ 1 前台辅助（工具链子任务）/ 2 后台优化（Consolidation/Reflection/PromptOptimizer）/ 3 最低（Auto-Curriculum、索引重建）。

认领按 `ORDER BY priority DESC, created_at ASC`（`sqlite_blackboard_ops.go`）。**动态提权防饥饿（`eff_priority`、`escalateP2AfterMinutes`）与 `ListenLoop` 随 ADR-0062 删除，代码无对应实现**（2026-10 核）；防饥饿现仅靠阶段 2 的 60 分钟 pending 超时兜底。

### 1.9 Phased Startup

已删除（ADR-0112 决策八 #1，2026-10-13）：原 swarm 包的 startup.go 的 P0→P4 分阶段启动契约与 bootstrap 包 均生产零接线；生产启动走 `cmd/polaris/boot_*.go` 手工装配。重评触发：装配链 > 6000 行（ADR-0088 决策七），可从 git 历史取回。

---

## 2. Supervisor Tree

**独立子包** `internal/swarm/supervisor/`（`tree.go`），与 Blackboard/Worker 解耦，由 `cmd/polaris/boot_agent.go` 以 `supervisor.NewSupervisor(ctx, 5, 5*time.Minute)` 启动。Root(OneForOne) 下各常驻 Worker 为平级子节点（default-task-worker 等，无中心化 Orchestrator 中间层）。重启窗口权威源 `spec/state.yaml §m8_multiagent.agent_restart_max_in_window` / `agent_restart_window_seconds`；退避自 `supervisor_backoff_initial_ms` 倍增至 `supervisor_backoff_max_seconds`。

| 策略 | 行为 | 适用 |
|------|------|------|
| OneForOne | 只重启崩溃 Agent | 默认 |
| OneForAll | 崩溃→全部重启 | 紧密耦合组 |
| RestForOne | 崩溃→重启它及依赖方 | 依赖链 |
| Stop | 不重启 | 一次性任务 |
| Escalate | 耗尽→上报父级 | 关键 Agent |

⚠️ 冲突待裁：`boot_agent.go` 的 `5 次 / 5 分钟` 与 state.yaml 的 `3 次 / 60 秒` 不一致（代码以字面量传参，未读 state.yaml）。

---

## 2-bis. 常驻角色 Agent（Resident Role Agents）

`internal/swarm/agents/`，与 §2、§3 正交：常驻 goroutine，经 channel 与主脑通信，**不经过 tasks 表**；Tier-0 下静止内存 < 5MB。

- **MemoryAgent**：L1→L2 蒸馏 + 耳语推送 + Extension Librarian 调度 + 个人知识库连接器调度。
- **GovernanceAgent**：PolicyGate 包装 + 幂等网关 + 内存压力监控。
- 辅助：`security_audit_agent.go`（SecurityAuditAgent）、`code_validator.go`（CodeValidator）。

决策（2026-07-03）：产品侧"Librarian"（对接 Obsidian/Notion 等用户知识库）**不新建独立 swarm 角色**，收编进 MemoryAgent 调度——与它已有的 Extension Librarian 调度（`internal/knowledge/connector/extension_librarian_handler.go`，扩展安装后把能力索引进知识图谱）同属"周期性摄入外部信息"，无独立扩缩容/故障隔离必要。若同步复杂度超出 MemoryAgent 承载再拆。

接线状态：✅ 已接入。`cmd/polaris/boot_agent.go` 构造 Obsidian / Notion / MCP 知识源连接器，各包 `connector.NewSyncScheduler` 后经 `memoryAgent.RegisterSyncScheduler` 注册，`MemoryAgent.Run()` 并发拉起。

---

## 3. 编排模式

| # | 模式 | 场景 | 实现 |
|---|------|------|------|
| 1 | Supervisor(默认) | Planner→Worker→汇总 | [Blackboard] |
| 2 | Hierarchy | 递归分解 | ROMA |
| 3 | Sequential | A输出→B输入 | task.DependsOn（`pattern_sequential.go`） |
| 4 | Parallel | 独立子任务并发 | errgroup+BFS（`pattern_parallel.go`） |
| 5 | MapReduce | 分片归并 | MapReduceExecutor |
| 6 | Reflection | 执行→审查→改进 | +M4 S_REFLECT |
| 7 | Swarm | 去中心化handoff | SwarmCoordinator |
| 8 | Pipeline | 专家流水线(含验证) | PipelineOrchestrator（§3-ter） |
| 9 | PatternDAG | 强类型 DAG(防环/补偿) | PatternDAGExecutor（§3-quater） |
| 10 | StateGraph | 条件路由+有界循环(GD-8-001) | StateGraphExecutor（§3-quinquies） |
| 11 | PatternDebate | 对抗性辩论(ADR-0046 决策四) | DebateExecutor（§3-sexies） |

**MapReduceExecutor**：Planner 拆 N 个同构子任务（不同 scope）PostTask → errgroup CAS 认领，任一失败不影响其余 → Reduce 去重 artifact hash、冲突标记人工裁决 → 写回父任务 Done。异构任务走 Supervisor/Hierarchy。

**SwarmCoordinator**：初始 CAS 认领 → 持有者不适配则 `Handoff`（调 `FailTask`，payload 带 `[HANDOFF_NOTE]:` 前缀，状态回落 `Pending`，无独立 handoff 事件类型）→ 其余 Agent 按 `handoff_note` 重匹配 ActivationRule 重认领 → `max_handoff_depth`（3）后升级 Supervisor。

模式 3/4/5/7/9 经 `sysadmin` REST（`POST /v1/admin/tasks/pattern-{dag,mapreduce,parallel,sequential,swarm}` 与 `csv-fanout`，`server_routes.go`）直接驱动。模式 10 的生产消费方是工作流（§3-quinquies-c）；Debate 无 REST 入口，由 `boot_agent.go` 装配与恢复循环驱动。

---

## 3-bis.（已删除）SwarmRouter + CapabilityRegistry

`internal/swarm/topology/` 于 2026-07-14 整包删除（ADR-0062）：`NewSwarmRouter`/`NewTopologyEvolverService`/`NewCapabilityRegistry` 零生产调用。生产编排均为结构显式声明（DAG 边 / StateGraph 转移表 / 固定常驻角色），无"运行时按 Agent 数量切换拓扑"需求。若出现同构 Agent 池场景，在"自订阅 + CAS 认领"模式上扩展按能力过滤，不复活独立路由层。详见 ADR-0062《被驳回的方案》。

---

## 3-ter. PipelineOrchestrator（编排模式 8）

> 实现：`internal/execute/orchestrator/pipeline.go`、`pipeline_compensation.go`；类型：`internal/protocol/types.go`（`PipelineDescriptor` / `PipelineStageSpec` / `VerificationPolicy` / `VerificationResult`）。`cmd/polaris/boot_agent.go` 装配，并注入 Parallel/MapReduce 执行器。

why-do：专家流水线（Researcher→Planner→Executor→Verifier）需要结构化上下文传递（防 Token 膨胀与跨阶段污染）、独立对抗性验证（"目标未达成"假设，区别于 M4 S_REFLECT 自省）、流水线级重试（不重复计费已成功阶段）。

- `Stage[N].Result` 作为 `Stage[N+1].ContextPayload`（JSON）由 `runStage` 在 `PostTask` 时填充；`TaskEntry.PipelineID`/`PipelineStage` 供审计与 Eval 消费（DDL `internal/protocol/schema/007_tasks.sql`，`idx_tasks_pipeline`）。Agent Kernel 自行决定如何拼入 Prompt（Thin Orchestrator）。
- `Run` 逐阶段 PostTask 并轮询完成；配置 VerificationPolicy 时最后执行验证阶段（`Adversarial=true` 注入 `[ADVERSARIAL_STANCE]`，返回 Verdict）。
- 验证 Agent 持独立 PromptBuilder，不继承执行阶段上下文（[Sub-agent-Isolation]）。
- 阶段数上限与 `MaxRetries` 由 `PipelineDescriptor` 约束；全阶段失败则流水线终止，不触发全局 Saga（各 Kernel 内部已持有）。

| Verdict | 值 | 语义 |
|---------|---|------|
| `VerdictPass` | 0 | 目标已达成 |
| `VerdictWarning` | 1 | 有问题但不阻塞，需记录 |
| `VerdictBlocker` | 2 | 关键缺失，`BlockOnFail=true` 时触发 ESCALATE |

---

## 3-quater. PatternDAGExecutor（编排模式 9）

> 实现：`internal/execute/orchestrator/pattern_dag.go`；决策：ADR-0046 决策二。

跨 Agent 边界（Macro-DAG）复杂调度（A→B/C→D 归并）：
1. 执行前 `pkg/graph.ValidateTopology` 做成环检测与深度熔断。
2. Kahn 变形，`BlackboardEvent` 驱动无级联阻塞并发。
3. Fail-Fast：任一节点不可恢复错误 → 标记 DAG 失败（不阻断已就绪节点投递），对已完成且持有 `CompensationAction` 的节点按**逆拓扑序**并发投补偿任务，复用 `PipelineOrchestrator.monitorCompensationTask` 最佳努力机制。

---

## 3-quinquies. StateGraphExecutor（编排模式10，GD-8-001）

> 实现：`internal/execute/orchestrator/pattern_state_graph.go` + `state_graph_saga.go` + `pkg/graph/state_graph.go`（`ValidateStateGraphTopology`）；决策：ADR-0046 决策三（含原 ADR-0041），防退化条款：不得被 Chain/Pipeline 替代。

在 PatternDAG 之上支持"条件路由 + 有界循环"，与 `WorkflowGraphSpec` 共用协议类型（PatternDAG 忽略新增字段）：

- **条件边**：`WorkflowEdgeSpec.Condition`（`Field/Op/Value`，可选 `And`/`Or` 递归复合，复合优先于顶层字段，`And` 优先于 `Or`）对上游输出 JSON 做声明式比较（HE-Rule-2：禁脚本/表达式引擎，CEL 提案已驳回，见 ADR-0046 反例守护）。算子 `eq/ne/gt/lt/ge/le/contains/exists`：数值算子双侧 `float64` 解析，任一侧非数字 fail-closed；`exists` 忽略 `Value`。
- **有界循环**：`WorkflowNodeSpec.MaxVisits` 限制节点重复触发次数；`IsEntry` 显式标记入口（循环节点入度恒 >0）。
- **拓扑校验**：允许环，要求引用完整 + 至少一个合法入口 + 各节点 `effectiveMaxVisits` 之和 ≤ `StateGraphMaxTotalVisitBudget`（200）。
- **终止性**：运行时硬计数器（节点级 + 全局）物理保证，超预算分支直接丢弃（自然终止，非错误）。
- **不替代 Blackboard**：复用 `PostTask`/`Subscribe`，CAS/Lease/Reaper/`inv_M8_02` 不变。
- **限制**：`MaxVisits>1` 与非 nil `Compensation` 互斥（校验期拒绝）；条件仅支持顶层字段（无嵌套路径/数组遍历）。

### 3-quinquies-a. 扇入 AND-Join

仅对无条件（`Condition==nil`）且非自环（`From!=To`）边计入硬依赖（`requiredPreds`/`arrivedPreds`），`arriveJoin` 集齐全部前驱才触发一次；条件边（路由分支）与自环边（失败重试）保持"任一满足即触发"。三者语义不同：路由选择 / 同节点重试 / 依赖。测试 `TestStateGraphExecutor_ANDJoinWaitsForAllParents`。

### 3-quinquies-b. Blackboard Intent 持久化

`tasks` 表含 `intent BLOB`；`PostTask`/`PostBatch` 写入；`TaskSnapshot` 携带 `Intent`/`Type`，`PeekTask` 读回——使自订阅 Worker 能在 `ClaimTask` 前按 `Type` 判断是否属于自己的能力类型，避免误认领。

### 3-quinquies-c. Workflow DAG 并行 + 失败重试

WebUI「工作流」（`internal/gateway/server/sysadmin/workflowadmin`）是 StateGraphExecutor 的第一个生产消费方：

- **图构造**（`workflow_graph.go buildGraphSpec`）：`type='chain'`（默认）忽略 `depends_on`，按 `seq` 合成顺序链；`type='dag'` 按 `depends_on`（**0-based seq 索引字符串**，非步骤 DB id——保存时 `workflow_steps` 全表删后插，id 不稳定）构造无条件依赖边，多依赖由 AND-Join 保证。`max_retries>0` 附加自环条件边（`status=="error"` 重试），`MaxVisits=1+max_retries`；与 `compensation_tool` 互斥，HTTP 层 `validateStepRetryCompensation` 提前拒绝。
- **执行**（`workflow_step_worker.go RunStepWorkerLoop`）：订阅 `task_posted`，按 `PeekTask.Type` 仅认领 `workflow_step`，复用 `runWorkflowStep`（AgentPool headless 推理）。业务失败一律 `CompleteTask` 写 `{"status":"error"}`，由自环条件边判重试；仅基础设施故障（intent 解析失败、步骤配置缺失）走 `FailTask` 触发 Fail-Fast。
- **历史**：`workflow_runs.step_outputs`/`current_step` 以原子 SQL（`json_insert` / `current_step+1`）增量写，避免 DAG 并行下读-改-写丢更新。
- **中断续跑**：启动期孤儿对账把遗留 `running` 置 `interrupted`；`POST /v1/workflows/runs/{id}/resume` 仅允许 `interrupted|error`，同 runID 重入，`task_checkpoints` 中 `done` 节点复用，不自动续跑（ADR-0104 决策一/二）。
- **生命周期**：`RunStepWorkerLoop` 在 `server.Start()` 以 `concurrent.SafeGo` 启动，`Shutdown()` 时随专属 cancel 停止。

---

## 4. Agent Card 与能力发现

AgentCard 声明能力集（Skills/Tools/Models）、激活条件（TaskTypes/MaxLoad/RequiresTools）、信任级别与沙箱层级；AgentRegistry 以 RWMutex 保护 agentID→Handle 映射。`AgentRegistry` 仅保留 Register/Get/Deregister/MarkUnreachable，供 `SQLiteBlackboard.SetRegistry` 做 SpawnDepth 校验（MarkUnreachable 当前无生产调用方）。任务分配是"自订阅 + CAS 认领"（先到先得，无择优评分；`FindBestAgent` 已随 ADR-0062 删除）。跨节点对端不进 AgentRegistry，走 `[[mesh.peers]]` + `[[mcp.servers]]`（ADR-0111 决策六）。

- `SQLiteBlackboard.StartExecution`：`ClaimTask` 后将状态推进到 running，广播 `task_running`，幂等。
- Sequential/MapReduce 构造器接受 `perTaskTimeout` / `totalTimeout`，0 取默认（5min / 10min）。

---

## 5. Task 分解与依赖管理

Macro-DAG（本模块）：节点=跨 Agent 子任务，经 [Blackboard] 发布，边=data|approval|sequential。Micro-DAG（M4）：子任务内部工具调用。

### 5.1 分解实现组件（`internal/swarm/planner/`）

| 组件 | 文件 | 职责 |
|---|---|---|
| `TaskDecomposer` | `decomposer.go` | LLM 结构化输出拆 Macro-DAG 子任务（`id/name/description/tool_name/args/depends_on`）；`tool_name` 须过 `ToolLookup` 白名单（消费方接口，R1.4）；哨兵 `agent:run` 表示递归交子 Agent，直接放行 |
| `PlannerPool` | `pool.go` | 管理多条并发思考流，结果经 `whisperChan`（`protocol.MemoryWhisper`，M09 §异步耳语）异步回传主脑 |

命名注意：`AgentPool` 在 `internal/agent/pool.go`（M04 per-session 实例池，ADR-0025 §E），与 `PlannerPool` 无关。

ExecuteDAG 按拓扑分层并发（errgroup），任一层失败即终止并触发 Saga Rollback；Planner 超时（5min）→ DAG Rollback，崩溃后由 Supervisor 经 [EventLog] 恢复。`rollbackSaga`：`StateContext.SagaLog` 记已执行节点，失败时逆序调各节点 `UndoFn`（无 UndoFn 者跳过并 WARN，最大努力）。

---

## 8.（已删除）编排拓扑自演化

`TopologyEvolverService`（Shadow→A/B→Gradual→Commit）与 `internal/swarm/topology/` 随 §3-bis 于 2026-07-14 删除（ADR-0062）：零生产调用，旧文"已接入"与代码不符。需拓扑级 A/B 前，先确认存在真实的多套可比拓扑候选场景。

编排模式分层存放：执行模式 `internal/execute/orchestrator/pattern_*.go`；容错 `internal/swarm/supervisor/`；认知循环（Reflection）耦合 M4 S_REFLECT/M9。

---

## 10. 降级与失败模式（5 问全覆盖）

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| Agent 心跳超时 | Reaper 回收任务（重置 Pending） | Agent 重认领 |
| Supervisor Tree Agent 崩溃 | OneForOne 重启（退避参数见 §2），超限 Escalate → Root | 同左 |
| Planner DAG 生成超时 (>5min) | DAG Rollback + ErrPlanningTimeout | — |
| 进程崩溃 | `tasks` 表即真相（HE-6），Reaper 回收过期租约；工作流/Handoff 续跑见 ADR-0104 | 启动期孤儿对账 |
| 跨节点对端不可达 | `a2a_delegate` 超时（`mcp_a2a_handoff_timeout_seconds`）→ 任务失败回传委派方 | 人工/重试 |

与 OSMemoryGuard 协同：L2 紧急 → Agent 并发 ≤2；L3 临界 → StopAll（Executing→Suspended）。local_only 下 M13 ResourceGovernor 检测到 LLM 卸载死锁（M13 §2.0）时，M8 接收强制 Rollback：Priority >= 2 的非核心任务直接 Rollback，Priority=1 的前台辅助任务 Suspended + 写 Cold Archive，释放内存供 LLM 重载。

## 默认参数

完整阈值与重评触发条件：`spec/state.yaml §thresholds.m8_multiagent`。

## 11. 跨模块契约

> 接口签名权威源 `internal/protocol/interfaces_swarm.go` + `types.go`。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M8→M4 | Blackboard.CAS Claim / LeaseHeartbeat | 触发 S_EXECUTE；续期；S_ROLLBACK 入口。M4 §4, §7 |
| M8→M2 | EventLog 双写 + MutationBus Reaper 归档 | 终态驱逐。M2 §2.1, §2.3 |
| M8→M7 | SideEffectPreCheck + ExecuteTool | 每 ExecuteTool 前强制执行。M7 §4 |
| M8→M11 | KillSwitch / ESCALATE / Cedar-Gate / CredentialVault | FullStop→StopAll；HITL 审批；deny-by-default；JIT Token Minting。M11 §4 |
| M8→M3 | SurpriseIndex 消费 | 编排决策反馈。M3 §4 |
| M9→M8 | Auto-Curriculum PostTask | priority=3 课程任务。M9 §2.2（原"拓扑自演化候选"已随 §8 删除） |
| M13→M8 | HITL Suspend/Resume | SuspendForHITL / ResumeFromHITL。M13 §2.4 |
| M13→M8 | 跨节点委派入站（`internal/gateway/mesh`） | `peer_delegation` PostTask，SpawnDepth/chain 校验，TaintHigh。ADR-0111 |
| Schema | Blackboard / TaskEntry / AgentCard / AgentHandle | `internal/protocol/interfaces_swarm.go`, `types.go` |
| 全局字典 | Blackboard、HE-Rule-5 | 00-Global-Dictionary §8, §1-bis |

### 11.1 多 Agent 共享记忆说明

**不需要 SharedMemoryBus。** inv_M8_02 确立 EventLog 为真相源（单机单 SQLite），同进程子 Agent 共享同一数据库，`episodic_events`/`semantic_memory`/`reflection_memory` 天然跨 Agent 可见；知识传递走 Blackboard `Result` payload。独立总线会与 MutationBus 写路径重叠，违反单写者原则（HE-Rule-6）。

**命名空间（GD-14-001）**：不是新总线，而是 EpisodicMem 分区键的"可选联合"。`TaskEntry.Namespace`（`007_tasks.sql`）由发布方指定（CSV Fan-out 同批统一赋 `jobID`；委派子任务以发起方根会话 ID 为命名空间，ADR-0097 决策三补）；`PeekTask` 带出该字段，Worker 认领后调 `AgentKernel.SetMemoryNamespace(ns)`，子 Agent 写 Episodic 时以 NamespaceID 替代 SessionID（`Agent.memoryPartitionKey()`），同批协同子任务的 4 类核心写入互相可见。2PC 幂等检查与 FastPath 缓存仍用 SessionID，保持崩溃恢复语义。跨节点对端任务命名空间一律加 `peer:<节点>/` 前缀，不得写入本地用户命名空间（ADR-0111 决策一）。详见 M5 §3.1。

---

## 11.2 已知实现缺口

| 缺口 | 状态 |
|------|------|
| inv_M8_06 委托链深度 ≤3 | ✅ `MaxSpawnDepth=3`，PostTask/PostBatch 前置 `SpawnDepth > 3 → CodeForbidden` |
| inv_M8_02 Blackboard→EventLog 双写 | ✅ 各状态转换方法显式事务同步写 events |
| 个人知识库连接器调度 | ✅ 见 §2-bis |
| 背压与 `max_blackboard_pending` | ❌ 无代码（§1.2） |
| 动态优先级提权 | ❌ 已随 ADR-0062 删除（§1.8） |
| `swarm/startup.go` PhasedStartup | ❌ 已删除（§1.9，ADR-0112） |
| Supervisor 重启参数与 state.yaml 不一致 | ⚠️ 见 §2 |
| Reaper 阶段 2 超时字面量 | ⚠️ 见 §1.7 |

---

## 12. 子 Agent 定义（ADR-0103 决策三补充）

Polaris 私有 `.polaris/agents/*.yaml` + `AgentProfile` 已删除；改为 Claude / Codex 两家标准格式并按角色真实执行。

**来源**（`lifecycle.AgentDefinitionProvider`，`internal/extension/lifecycle/agent_definitions.go`；解析唯一实现 `pluginspec`）：项目 `<project_root>/.polaris/agents/` > 用户 `<data>/agents/`（Claude `*.md` / Codex `*.toml`）；已启用插件 `agents/*.md` → `<plugin>:<agent>`。

**执行链**：`transfer_to_agent`（内置工具，内核特判异步挂起）→ Blackboard `agent_handoff:<name>` → `DefaultTaskWorker` → `SubagentRunner.Run`（`internal/execute/orchestrator/subagent_runner.go`；角色解析 → SubagentStart hook → `AcquireHeadless(WithAgentProfile)` → SubagentStop hook，续跑 ≤3）→ `Agent.SetAgentProfile`。每次运行写 `subagent_runs`（042；`entry`=delegation/fork_skill/hook，子会话 ID `sub-{agent_id}`，写失败仅告警；ADR-0104 决策六）。同一 Runner 服务 `context: fork` 技能（`SlashCommandRouter`）与 `agent` 类型 hook；模型调用的 fork 技能由内核 `tryForkSkill` 转为 `transfer_to_agent`。内置类型 `Explore`/`Plan`/`default`/`worker`/`explorer`（`internal/extension/lifecycle/agent_builtin.go`）。`list_agents`（`internal/tool/builtin/list_agents.go`）列出目标；`general-purpose`=无角色；未知名称任务失败并回传原因。`mcp:` 目标由 `MCPA2AWorker` 认领（ADR-0111 决策一）。

**项目继承**：子 Agent 会话 ID 为一次性 `headless-…`，项目经命名空间（=根会话 ID）解析，继承项目指令、工作目录访问根与记忆项目（ADR-0097 决策三补）；`curriculum_*`、CSV job ID 等非会话命名空间按默认项目处理。

**角色边界**（执行入口硬拦截 `Agent.checkProfileTool`）：工具白/黑名单（Claude 工具名映射）、只读（Codex `read-only` / Claude `plan`）、`maxTurns`→`MaxStepsLimit`、默认禁再委派；指令进 `ZoneMutableSkill`。字段映射与"解析但不生效"清单见 ADR-0103。

**防递归**：`TaskEntry.SpawnDepth` + `sqlite_blackboard.go resolveMaxDepth`，超深度 `CodeForbidden`。

---

## 13. CSV Batch Fan-out（工具型扇出，非编号编排模式）

CSV 输入 → 每行一个 SubAgent Task → 并发 Blackboard 认领 → 结果聚合 CSV（大规模并行审计 / 批量处理）。入口 `RunCSVFanout`（`internal/execute/orchestrator/csv_fanout.go`），REST `POST /v1/admin/tasks/csv-fanout`。按行展开为 TaskEntry 批量 PostTask 并 PeekTask 轮询，全部 Done/Failed 后写结果 CSV。

**状态持久化**（HE-Rule-6）：行状态经 `TaskEntry.Status` 写 Blackboard → EventLog 双写（inv_M8_02），不引入独立 SQLite；`event_type=csv_job_row_*` 供 Eval Harness 消费（HE-Rule-4）。同批任务 `Namespace=jobID`（§11.1）。

| `CSVFanoutJob` 字段 | 类型 | 说明 |
|------|------|------|
| `CSVPath` | string | 输入 CSV（首行 header） |
| `IDColumn` | string | 行标识列（空则用行号） |
| `Instruction` | string | Worker 指令模板，支持 `{column_name}` |
| `OutputCSVPath` | string | 结果路径（空则不写） |
| `MaxConcurrency` | int | 并发上限（0=6） |
| `MaxRuntimeSec` | int | 每行超时秒数（0=1800） |

---

## §3-sexies PatternDebate 编排模式（编排模式 11）

实现：`internal/execute/orchestrator/pattern_debate.go` + `debate_worker.go`；决策：ADR-0046 决策四（GD-6）。流程：Judge 初始议题 → Proponent/Opponent 轮番辩论 → Judge 结案陈词。不用阻塞轮询（`bb.Subscribe`）：`DebateExecutor` 将状态落 Checkpoint 并返回 `apperr.ErrSuspend` 异步挂起，复用 Handoff/Watcher 机制跨 Agent 唤醒；`boot_agent.go` 装配恢复循环做断点续跑。
