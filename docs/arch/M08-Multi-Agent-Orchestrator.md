# 模块 8: Multi-Agent Orchestrator

> 单机黑板 + CAS 原子认领 + Supervisor Tree | Go goroutine + channel + CAS | [HE-Rule-5] [HE-Rule-6]
> **§跳读**: 0-bis:5 职责 / 0-ter:18 不变量速查 / 1:31 黑板+CAS(核心) / 2:106 Supervisor / 3:125 编排模式 / 3-bis:143 SwarmRouter / 4:222 AgentCard / 5:236 Task分解 / 8:245 拓扑自演化 / 10:260 (SOFT)降级 / 11:279 跨模块契约 / 12:316 Custom Agent / 13:354 CSV Fan-out
## 0-bis. 职责边界

| M8 **是** | M8 **不是** |
|-----------|-------------|
| 多 Agent 协调黑板（PostTask + CAS Claim + Lease） | 单 Agent 内部状态机（那是 M4） |
| Supervisor Tree 故障恢复（OneForOne + 指数退避） | 工具沙箱执行（那是 M7） |
| 7 种编排模式执行（Supervisor/Hierarchy/Sequential/Parallel/MapReduce/Reflection/Swarm） | 编排模式的选择决策（由任务复杂度自适应） |
| Agent Card 注册与能力发现（FindBestAgent） | Agent 自身的能力实现（各 Agent 自行声明） |
| Task DAG 分解（跨 Agent 边界的子任务） | 子任务内部的工具调用 DAG（那是 M4 Micro-DAG） |
| A2A 跨机互操作（gRPC/HTTP） | Provider 路由（那是 M1） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 | 实现状态 |
|------|--------|---------|---------|
| inv_M8_01 | 禁止自由 NL 多 Agent 对话——所有协调经 schema 原语（Intent/Request/Claim/Result/Fail） | BlackboardEvent schema 强制 | ✅ 已实现 |
| inv_M8_02 | Blackboard 写入与 EventLog 同事务双写——EventLog 是真相源 | M2 CompositeMutationIntent | ✅ 已实现 |
| inv_M8_03 | Task 状态单调推进 Pending→Claimed→Executing→Done/Failed，禁止回退 | CAS Version++ 乐观锁 | ✅ 已实现 |
| inv_M8_04 | Agent Lease TTL 60s + 心跳 15s ±5s jitter + Reaper 1s 扫描——Lease 过期任务自动回收 | M8 §1.7 Reaper | ✅ 已实现（注：Reaper toxicity 计数机制为代码扩展，文档未含） |
| inv_M8_05 | Taint 经 Blackboard 传播——input_data 携带原始 TaintLevel，协调期间不降级 | M8 §4 blackboard_entries taint_level CHECK | ✅ 已实现 |
| inv_M8_06 | 委托链深度 ≤3——跨 Agent 委托禁止超过 3 层 | M11 §8 Layer 4 多 Agent 宪法 | ✅ 已实现（`sqlite_blackboard.go` 和内存版 `blackboard.go` 均已校验） |

---

## 1. 黑板 + CAS 原子认领

### 1.1 核心结构

Blackboard/TaskEntry/TaskStatus/BlackboardEvent 类型及 CAS Claim/RenewLease/SideEffectPreCheck 实现见 `internal/swarm/orchestrator/`。核心文件：`orchestrator.go`（主控+PostTask）、`sqlite_blackboard.go`（SQLiteBlackboard CAS+TOCTOU 幂等锁）、`worker.go`（认领+执行循环）、`pipeline.go`（PipelineOrchestrator）、`pattern_*.go`（各编排模式）、`reaper.go`（Reaper 泄漏回收）、`csv_fanout.go`（CSV 扇出）、`tree.go`（Supervisor Tree，因依赖在同包内但逻辑独立）。

### 1.2 初始化

初始化时从 EventLog 全量回放重建内存黑板（仅回放活跃状态任务，Done/Failed + 5min 超时的终态跳过，重建窗口 24h）。启动后台 Reaper（1s 扫描）和 monitorBackpressure（500ms）。

背压控制：events chan 占用 >80% 时拒绝 PostTask，<50% 恢复。Heartbeat/RenewLease 直接在 Lock 内更新 ExpiresAt，禁止走 events chan（控制/数据平面严格分离）。崩溃重建时执行 BatchRebuild，chan 排空后切回常规流。PostTask 经 schemaValidator 强类型校验，身份经 Ed25519 签名校验。

SupervisorEpoch 在启动时写入 SQLite sys_config 原子递增，Worker 在 SideEffectPreCheck 时读取校验（不一致触发 GracefulTermination + 重注册）。

> ✅ `SupervisorEpoch.Increment()` 已改为 `atomic.AddInt64`，并发安全。

全部常量权威源：`spec/state.yaml §m8_multiagent`。

### 1.3 Claim CAS

- **Claim (认领)**: 验证状态为 `Pending` 且未被其他 Agent 认领，原子化地将状态置为 `Claimed`，并设置 `ClaimedAt` 和过期时间 `ExpiresAt`（默认为 60s），同时递增版本号 `Version++` 防 ABA。
- **BeginExecution (执行)**: 验证当前认领者身份并确保状态仍为 `Claimed`，将其流转为 `Executing` 并递增版本号。这是 M4 Agent Kernel 首次 ExecuteTool 前必须流经的检查点。



状态: Pending→Claimed→Executing→Done|Failed, 禁止回退。

### 1.4 RenewLease (控制平面带外)

- **RenewLease (续约)**: 在任务执行过程中，控制平面周期性（心跳时间）原地更新过期时间 `ExpiresAt`，避免被 Reaper 错误回收，续约计数器递增。该操作直接使用锁更新内存或数据库，不产生黑板订阅事件。

### 1.5 HITL 挂起/恢复

- **SuspendForHITL**: 将处于 `Executing` 的任务原子性地转为 `Suspended`，使用长期 HITL 超时时间覆盖短期租约。
- **ResumeFromHITL**: 将任务从 `Suspended` 状态中恢复。若人类审批通过则切回 `Executing` 并恢复常规租约 TTL；否则直接将其置为 `Failed` 状态。
- **BeginCompensation**: 当 M4 发起回滚或 Saga 补偿流程时，状态变更为 `Compensating`，并自动提供最高 5min 的长生命周期预算。
- **EndCompensation**: 补偿工作完成后，将任务状态标为 `Failed`，等待常规垃圾回收。

### 1.6 SideEffectPreCheck (每 M7 ExecuteTool 前强制执行)

每次 ExecuteTool 前，持有 RLock 检查四项：Status==Executing、ClaimedBy==self、ExpiresAt>now、Version==self.claimedVersion，任一失败返回 ErrStaleLease，Agent 进入 S_ROLLBACK。全通过后快照到栈，释放 RLock，执行 tool call，写回时重新 Lock + CAS 校验 Version。

> ✅ `SideEffectPreCheck` Version 校验已在内存黑板中补齐，ABA 防护有效。

不可逆操作（write_network/privileged）附加 TOCTOU 防护：L1 基于 `SHA-256(taskID+operation_seq+toolName)` 幂等锁（SurrealDB-Core KV，TTL=15s 心跳续期）；L2 外部 API 原生幂等键注入（辅助）。

### 1.7 Reaper

**阶段 1（1s 扫描）**：Lock 扫描，跳过 Suspended/Compensating；对 Claimed/Executing + ExpiresAt 过期的任务：取消 Agent context → 等待 5s 宽限期（供工具 ctx.Done() 感知，防止孤儿副作用）→ Version++ + 重置为 Pending → 发射 EventTaskReaped。5s 宽限期内若工具已完成进入 PostCheck，PostCheck 发现 Version 不匹配 → 写 decision_log（M7 §4.6）。

**崩溃恢复时**并发 cancel 所有过期任务（errgroup），统一 max(5s) 宽限期，O(max(5s)) 而非 O(N×5s)。正常运行期间始终为单一 goroutine 串行扫描。

> ✅ Reaper 已回退至规范的 `retry_count` 及 `provider_suspended_count` 逻辑；CompleteTask 竞态已修复。

**阶段 2（Phase 2 GC，✅ 已实现）**：`reaperPhase2` 每秒随 Phase 1 一同触发；`running` 状态超 30s（`ZombieTaskTTL`）的僵尸任务先 cancel goroutine 再标 failed，`pending` 状态超 30min（`StarvationTaskTTL`）的饥饿任务直接标 failed，防止 SQLite 黑板体积失控。

### 1.8 Agent 看板监听

TaskEntry 包含 Priority 字段，与 M13 ResourceGovernor 统一优先级体系:
- Priority=0: 用户交互（CLI/WebUI 直接请求）—— 始终放行
- Priority=1: 前台辅助（Agent 工具调用链中的子任务）
- Priority=2: 后台优化（Consolidation/Reflection/PromptOptimizer）
- Priority=3: 最低（Auto-Curriculum 课程任务、索引重建等）

- **ListenLoop**: 后台长轮询监听新发布任务。对队列中满足优先级的任务尝试 CAS 竞争（由于无中央调度）。
- **动态提权防饥饿**（✅ **已实现**）：`dispatchPendingTasks` SQL 计算 `eff_priority = priority - age_bonus`：Priority=2 等待 >5min 升至 1，Priority=3 等待 >15min 强制升至 1（阈值常量 `escalateP2AfterMinutes` / `escalateP3AfterMinutes`，来源 M08 §1.8）；提权时记录 `slog.Info("orchestrator: task priority escalated")`。

无中央调度，优先级排序 + CAS 隐式竞争。

### 1.9 Phased Startup

✅ **已实现**：`internal/swarm/startup.go` 提供分阶段启动（PhasedStartup），按 P0→P4 顺序启动，每阶段 30s 健康检查门控（并行 Ping）；P0 失败 panic（策略真空不可接受），P1-P4 失败返回 error；SQLiteBlackboard 接入 P0 健康检查。

---

## 2. Supervisor Tree

**独立子包**：`internal/swarm/supervisor/`（`tree.go`），OTP 风格独立模块，**与 Orchestrator 解耦**，直接从 `cmd/polaris/main.go` 启动（`supervisor.NewSupervisor(5, 5*time.Minute)`），不集成在 Orchestrator 内部。

Root(suture, OneForOne) → Orchestrator → Agent-*(Supervisor, OneForOne)。重启窗口策略权威源 `spec/state.yaml §m8_multiagent.agent_restart_max_in_window` / `agent_restart_window_seconds`。
退避指数从 `spec/state.yaml §m8_multiagent.supervisor_backoff_initial_ms` 倍增至 `supervisor_backoff_max_seconds` 封顶。

| 策略 | 行为 | 适用 |
|------|------|------|
| OneForOne | 只重启崩溃 Agent | 默认 |
| OneForAll | 崩溃→全部重启 | 紧密耦合组 |
| RestForOne | 崩溃→重启它及依赖方 | 依赖链 |
| Stop | 不重启 | 一次性任务 |
| Escalate | 耗尽→上报父级 | 关键 Agent |

实现选型：主选 thejerf/suture v4（成熟开源 Erlang 风格 supervisor）；备选自建（仅当 suture 引入额外依赖冲突时启用）。

---

## 3. 编排模式

| # | 模式 | 场景 | 实现 |
|---|------|------|------|
| 1 | Supervisor(默认) | Planner→Worker→汇总 | [Blackboard] |
| 2 | Hierarchy | 递归分解 | ROMA |
| 3 | Sequential | A输出→B输入 | task.DependsOn |
| 4 | Parallel | 独立子任务并发 | errgroup+BFS |
| 5 | MapReduce | 分片归并 | MapReduceExecutor |
| 6 | Reflection | 执行→审查→改进 | +M4 S_REFLECT |
| 7 | Swarm | 去中心化handoff | SwarmCoordinator |

MapReduceExecutor: Map(Planner拆N个同构子任务,不同scope,PostTask) → 并发(errgroup CAS认领,任一失败不影响其余) → Reduce(收集Result,去重artifact hash,冲突标记人工裁决) → 聚合写回父任务Done。子任务完全同构, 异构走 Supervisor/Hierarchy。

SwarmCoordinator: 初始CAS认领→持有者不自适→handoff(Status→Pending+handoff_note+EventTaskHandoff)→其余Agent按handoff_note重匹配ActivationRule→重认领→max_handoff_depth(3)后升级Supervisor。

---

## 3-bis. SwarmRouter + CapabilityRegistry（拓扑路由层）

实现见 `internal/swarm/topology/swarm.go`。与 §3 各编排模式（执行层）正交：SwarmRouter 是**路由决策层**，决定任务经由 Blackboard CAS 还是 Stigmergy 能力匹配分发；SwarmCoordinator 是**执行协调层**，负责 handoff 流程。

### 三层 Agent 数量限制

基于实证（arxiv 2605.03310）：Sequential Pipeline 3-4 Agent Brier 0.153 最优，Consensus Alignment 0.181 最差；超 10 Agent 协调噪音压过收益。

默认限制（`DefaultAgentLimits`）：Registry=10（全局注册上限）、Hierarchy=3（Tier 0 内存约束）、Pipeline=5（流水线阶段上限）、Mesh=10（单任务参与数 + 自动升级阈值）。`NewSwarmRouter` 自动注入，无需手动配置。

`NewSwarmRouter` 自动将 `Registry=10` 注入 CapabilityRegistry，无需手动配置。

### CapabilityRegistry — 两道注册门控

`Register` 执行容量门控（超过 maxCapacity → ErrRegistryFull）和角色唯一性检查（完全相同能力集的 Agent 不重复注册，子集合法）。`AcquireLease`/`ReleaseLease` 维护负载计数，`AgentCount()` O(1) 供拓扑切换判断。

### SwarmRouter — 自动拓扑切换

`RouteTask` 每次调用前根据 `registry.AgentCount()` 动态计算有效拓扑：

`RouteTask` 每次调用前根据 `registry.AgentCount()` 动态判断：Agent 数 ≥10 升级为 Mesh，<10 且当前为 Mesh 则降回 Hierarchy。

- **Hierarchy 路径**: `publisher.Publish(intent)` → Blackboard CAS → 返回 taskID
- **Mesh 路径**: `FindAgents(capabilities)` → 负载排序 → Top-3 随机选主 → `AcquireLease` → 截断到 `Limits.Mesh` → 返回 AgentIDs
- Mesh 无匹配 Agent → 自动降级 Hierarchy（零任务丢失）

`SetMode` 保留手动覆盖语义，但下次 `RouteTask` 会根据实时 Agent 数重新判断。

### 拓扑选择原则（对齐 2026 前沿研究）

| Agent 数 | 推荐拓扑 | 理由 |
|---------|---------|------|
| 1-3 | Hierarchy（默认） | Tier 0 内存预算；Sequential Pipeline 最优 |
| 4-9 | Hierarchy / Pipeline | 线性依赖关系明确时 Pipeline；独立子任务 Parallel |
| 10+ | Mesh | Stigmergy 隐式协调；超过此数仍以 Limits.Mesh 截断单任务参与数 |

---

## 3-ter. PipelineOrchestrator（编排模式 8）

> 实现: `internal/swarm/orchestrator/`（PipelineOrchestrator）
> 类型定义: `internal/protocol/types.go`（`PipelineDescriptor` / `PipelineStageSpec` / `VerificationPolicy` / `VerificationResult`）

### 设计动机

§3 的 Sequential 模式（编排模式 3）通过 `task.DependsOn` 在 Blackboard 层实现串行依赖。但对于**专家流水线**（Researcher→Planner→Executor→Verifier 四阶段）场景，需要：
1. **结构化上下文传递**：前序阶段的结构化产出（JSON）精确注入下游阶段，而非共享全量上下文（防止 Token 膨胀与跨阶段污染）。
2. **对抗性验证**：末尾专设独立验证 Agent，从"目标未达成"假设出发（不是 M4 内部的 S_REFLECT 自省）。
3. **流水线级重试**：单阶段失败时在流水线层而非 Agent 层重试，避免重复计费已成功的前序阶段。

### ContextPayload 传递协议

`Stage[N].Result` 作为 `Stage[N+1].ContextPayload` 注入下一阶段，`TaskEntry.PipelineID`/`PipelineStage` 标识归属。

- `TaskEntry.ContextPayload`（`[]byte`，JSON）由 `PipelineOrchestrator.runStage` 在 `PostTask` 时填充。
- `TaskEntry.PipelineID` / `TaskEntry.PipelineStage` 标识流水线归属，供审计与 Eval Harness 消费。
- DDL 见 `internal/protocol/schema/007_tasks.sql`（含 `idx_tasks_pipeline` 索引）。
- Agent Kernel 从 `ContextPayload` 读取上游产出，自行决定如何拼装至 Prompt——PipelineOrchestrator 不参与 Prompt 组装（Thin Orchestrator 原则）。

### 执行流程

`PipelineOrchestrator.Run` 逐阶段顺序执行，每阶段通过 Blackboard.PostTask 分发并轮询完成，Result 注入下一阶段 ContextPayload。若配置 VerificationPolicy，最终执行对抗性验证阶段（Adversarial=true 时注入 [ADVERSARIAL_STANCE] 指令，返回 VerdictPass/Warning/Blocker）。

### VerificationVerdict 语义

| Verdict | 值 | 语义 |
|---------|---|------|
| `VerdictPass` | 0 | 目标已达成，可继续 |
| `VerdictWarning` | 1 | 存在问题但不阻塞，需记录 |
| `VerdictBlocker` | 2 | 关键缺失，`BlockOnFail=true` 时触发 ESCALATE |

### 约束

- 流水线阶段数上限 `AgentLimits.Pipeline = 5`（§3-bis），对齐前沿研究最优区间。
- `Adversarial=true` 验证 Agent 拥有完全独立的 PromptBuilder 实例（[Sub-agent-Isolation]），不继承任何执行阶段上下文。
- 单阶段重试不超过 `PipelineDescriptor.MaxRetries`；全阶段失败则流水线终止，不触发全局 Saga 补偿（各 Agent Kernel 内部已持有 Saga 逻辑）。

---

## 4. Agent Card 与能力发现

AgentCard 声明 Agent 能力集（Skills/Tools/Models）、激活条件（TaskTypes/MaxLoad/RequiresTools）、信任级别与沙箱层级；AgentRegistry 以 RWMutex 保护 agentID→Handle 映射，支持本地 chan 与远程 A2A gRPC 两种 Handle 类型，心跳 60s 超时标记 unreachable 并从匹配池移除。

FindBestAgent: Phase1 硬过滤（DeclaredCapabilities ⊇ RequiredCapabilities；空→全体）→ Phase2 加权降序评分：`score = 0.6 × LaplaceSuccRate + 0.4 × LoadFactor`。Laplace 成功率使用先验平滑，LoadFactor = `1/max(CurrentLoad, 1)`。

**实时负载数据**：Orchestrator 在每次调度前查询数据库获取各 Agent 当前 claimed+running 任务数，确保负载均衡基于真实状态而非空映射。实现见 `internal/swarm/orchestrator/`。

**SQLiteBlackboard.StartExecution**：`ClaimTask`（claimed）之后，Agent 开始实际执行前调用此方法将状态推进到 running，广播 task_running 事件，提供更细粒度的任务状态追踪。与内存版 Blackboard 保持接口对称；幂等，重复调用 already-running 不报错。

**编排模式可配置超时**：`SequentialExecutor` 和 `MapReduceExecutor` 构造器接受超时参数（`perTaskTimeout` / `totalTimeout`），0 值使用默认值（5min / 10min），调用方可按任务复杂度定制，不再依赖固定兜底时间。实现见 `internal/swarm/orchestrator/`。

---

## 5. Task 分解与依赖管理

Macro-DAG(本模块): 节点=子任务跨Agent边界, [Blackboard] 发布, 边=data|approval|sequential
Micro-DAG(M4): 子任务内部工具调用, M4 Agent Kernel 管理

Macro-DAG 节点为跨 Agent 子任务，边类型为 data/approval/sequential；ExecuteDAG 按拓扑分层并发（errgroup），任一层失败即终止并触发 Saga Rollback；Planner 5min 超时 → DAG Rollback，崩溃后由 Supervisor 通过 [EventLog] 恢复。**✅ 已实现（Saga rollbackSaga）**：`rollbackSaga` 从存根升级为真实补偿：`StateContext.SagaLog` 记录每步已执行节点，失败时逆序调用各节点注册的 `UndoFn`（无 UndoFn 的工具跳过并 WARN，最大努力补偿）。

---

## 8. 编排拓扑自演化

TopologyFitness 评估维度：成功率、平均延迟、平均 token 成本、Agent 利用率。TopologyEvolver 采用双维 Pareto：成功率领先 ≥5pp 且 token 成本不劣化超 10%，样本 <10 不参与评估。A/B 50/50 分流由 M13 TrafficSplitter 执行（TopologyEvolver 仅做决策，不直接切流）。✅ 已接入：TopologyEvolverService（`internal/swarm/topology/`）封装状态机与 TrafficSplitter，Orchestrator 任务完成后驱动演化与自动回滚。

| 阶段 | 流量 | 观察 | 回滚条件 |
|------|------|------|---------|
| Shadow | 0% | 50任务 | — |
| A/B | 50% | 50任务 | 成功率↓>5pp |
| Gradual | 100% | 7d | 成功率/token效率退化>3pp |
| Commit | 100% | 永久 | — |

启用: ≥50历史执行/类型; 全 Tier 已支持 7 种模式，按职责分层存放：执行模式（`patterns/`：Sequential/Parallel/MapReduce/Swarm）、容错基础设施（`supervisor/`：Supervisor OneForOne 重启树）、路由拓扑（`topology/`：Hierarchy，与执行层正交见 §3-bis）、认知循环（`reflexion.go`：Reflection，深度耦合 M4 S_REFLECT/M9）。

---

## 10. 降级与失败模式（5 问全覆盖）

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| 黑板 chan 满（>80%） | 拒绝 PostTask + backpressure 信号 | <50% 解除 |
| Agent 心跳超时 (>45s 无响应) | Reaper 回收任务 (重置 Pending) | Agent 重注册后参与认领 |
| Supervisor Tree Agent 崩溃 | OneForOne 自动重启 (100ms→30s 退避，5 次上限) | 超过上限 Escalate → Root Supervisor |
| Planner DAG 生成超时 (>5min) | DAG Rollback + ErrPlanningTimeout | — |
| 黑板 entries 丢失 (崩溃前未写 EventLog) | 从 EventLog 回放重建 | — |
| A2A 远程 Agent 不可达 | mark_unreachable → 不参与匹配 | 心跳恢复自动重新注册 |
| 拓扑自演化 A/B 退化 | 自动回滚到 baseline 拓扑 | 7 天稳定后重试 |

与 OSMemoryGuard 协同: L2 紧急 → 限制 Agent 并发 ≤2 / L3 临界 → StopAll（所有 Executing→Suspended），恢复后从 [EventLog] 回放重建黑板。local_only 模式下若 M13 ResourceGovernor 检测到 LLM 卸载死锁（M13 §2.0），M8 接收强制 Rollback 指令：Priority >= 2 的非核心任务直接 Rollback，Priority=1 的前台辅助任务 Suspended + 写入 Cold Archive，释放内存供 LLM 重载。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m8_multiagent`。

## 11. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M8→M4 | Blackboard.CAS Claim / LeaseHeartbeat | 触发 S_EXECUTE；续期；S_ROLLBACK 入口。M4 §4, §7 |
| M8→M2 | EventLog 初始化回放 + MutationBus Reaper 归档 | 全量重建 tasks + agents；终态驱逐。M2 §2.1, §2.3 |
| M8→M7 | SideEffectPreCheck + ExecuteTool | 每 ExecuteTool 前强制执行。M7 §4 |
| M8→M11 | KillSwitch / ESCALATE / Cedar-Gate / CredentialVault | FullStop→StopAll；HITL 审批；deny-by-default；JIT Token Minting。M11 §4 |
| M8→M3 | SurpriseIndex 消费 | 编排决策反馈。M3 §4 |
| M9→M8 | Auto-Curriculum PostTask | priority=3 → 拓扑自演化候选。M9 §2.2 |
| M13→M8 | HITL Suspend/Resume | SuspendForHITL / ResumeFromHITL。M13 §2.4 |
| Schema | Blackboard / TaskEntry / AgentCard / AgentHandle | `internal/protocol/interfaces.go`, `types.go` |
| 全局字典 | Blackboard 定义、HE-Rule-5 状态机持有控制流 | 00-Global-Dictionary §8, §1-bis |

### 11.1 多 Agent 共享记忆说明

**结论：不需要 SharedMemoryBus。**

inv_M8_02 确立 EventLog 为真相源（单机单 SQLite）。同进程内所有子 Agent 共享同一 SQLite 数据库，`episodic_events`/`semantic_memory`/`reflection_memory` 等表天然跨 Agent 可见。Agent 间知识传递通过 Blackboard `Result` payload + 各 Agent 读取 EventLog 实现。引入独立的 SharedMemoryBus 会引入额外同步开销并与 MutationBus 产生写路径重叠，违反单写者原则（HE-Rule-6）。

---

## 11.2 已知实现缺口

| 缺口 | 严重度 | 说明 |
|------|--------|------|
| 委托链深度 ≤3 校验（inv_M8_06） | ✅ 已实现 | `MaxSpawnDepth=3`，PostTask/PostBatch 前置 `SpawnDepth > 3 → CodeForbidden` |
| inv_M8_02：Blackboard → EventLog 双写 | ✅ 已修复 | 各状态转换方法改为显式事务，同步写 events 表 |
| 内存版 SideEffectPreCheck Version 校验 | ✅ 已修复 | SQLiteBlackboard 版本已在内存黑板中正确实现 |
| SupervisorEpoch 原子性 | ✅ 已修复 | 改为 `atomic.AddInt64` |
| 崩溃恢复并发 cancel（§1.7） | ✅ 已修复 | 改为 `errgroup.Group` 并发取消 |
| CompleteTask 阻塞写 channel | ✅ 已修复 | 通道写增加 `default` 分支防死锁 |

---

## 12. Custom Agent Profile（ADR-0015 §2.4）

> End-User 通过 YAML 文件定义专用子 Agent，无需修改源码。
> 映射到现有 AgentCard 注册到 Blackboard，不引入新执行路径。

**配置位置**:
- `~/.polarisagi/polaris/agents/*.yaml` — 用户级
- `.polaris/agents/*.yaml` — 项目级

**Profile 格式**:
```yaml
name: pr_explorer
description: "只读探索 Agent，用于 PR 代码路径映射"
instructions: "探索代码，追踪调用链，禁止修改文件"
model: deepseek-v4         # 可选，空则继承全局配置
sandbox_tier: 1            # 1=read-only, 2=workspace-write, 3=privileged
max_depth: 1               # 防递归嵌套（默认 1）
max_threads: 0             # 0=继承全局 agents.max_threads
skills: []
mcp_servers: []
```

**max_depth 防递归**:
- `TaskEntry` 注入 `SpawnDepth int`，子 Agent PostTask 时检查 `SpawnDepth ≥ Profile.MaxDepth`
- 默认 `max_depth=1`（直接子 Agent 可生成，禁止孙 Agent），全局阈值见 `state.yaml §agents.max_depth`
- 超深度 → 返回 `ErrMaxDepthExceeded`，冒泡至父 Saga 决策

**内置 Agent 类型**（参考 Codex）:
| 名称 | 用途 | Sandbox |
|------|------|---------|
| `default` | 通用 fallback | Sbx-L2 |
| `worker` | 实现/修复 focused | Sbx-L2 |
| `explorer` | 只读代码探索 | Sbx-L1 |

用户定义同名 Profile → 覆盖内置。AgentProfile 实现位于 `internal/swarm/orchestrator/`。

---

## 13. CSV Batch Fan-out（ADR-0015 §2.5）

> 编排模式 8：CSV 输入 → 每行一个 SubAgent Task → 并发 Blackboard 认领执行 → 结果聚合 CSV。
> 适合大规模并行审计（PR 逐文件 review / 批量数据处理 / 多目标扫描）。

**触发方式**（用户 prompt）:
```
请读取 /tmp/components.csv（列：path,owner），为每行并发启动子 Agent 做安全审计，
汇总结果写入 /tmp/audit-results.csv，最多 6 个并发。
```

**执行流程**:
`ReadCSV` 解析后按行展开为 TaskEntry 批量 PostTask，并发 PeekTask 轮询；所有行 Done/Failed 后写入结果 CSV。每行状态经 EventLog 双写（inv_M8_02，已实现）。

**状态持久化**（HE-Rule-6 State-in-DB）:
- 每行 Task 的状态变更经 `TaskEntry.Status` 写入 Blackboard → EventLog 双写（inv_M8_02）
- 不引入独立 SQLite（禁止，[ADR-0015] §2.5）
- `event_type=csv_job_row_*` 事件可供 Eval Harness 消费（HE-Rule-4）

**配置参数**（CSVFanoutJob）:
| 字段 | 类型 | 说明 |
|------|------|------|
| `CSVPath` | string | 输入 CSV（第一行 header） |
| `IDColumn` | string | 行标识列（空则用行号） |
| `Instruction` | string | Worker 指令模板，支持 `{column_name}` |
| `OutputCSVPath` | string | 结果输出路径（空则不写） |
| `MaxConcurrency` | int | 并发上限（0=6） |
| `MaxRuntimeSec` | int | 每行超时秒数（0=1800） |

实现见 `internal/swarm/orchestrator/`（csv_fanout + Blackboard.PeekTask）。
