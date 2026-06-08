# 模块 8: Multi-Agent Orchestrator

> 单机黑板 + CAS 原子认领 + Supervisor Tree | Go goroutine + channel + CAS | [HE-Rule-5] [HE-Rule-6]
> **§跳读**: 0-bis:5 职责 / 0-ter:18 不变量速查 / 1:31 黑板+CAS(核心) / 2:119 Supervisor / 3:136 编排模式 / 3-bis:154 SwarmRouter / 4:213 AgentCard / 5:227 Task分解 / 8:236 拓扑自演化 / 10:251 (SOFT)降级 / 11:270 跨模块契约 / 12:288 Custom Agent / 13:328 CSV Fan-out
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

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M8_01 | 禁止自由 NL 多 Agent 对话——所有协调经 schema 原语（Intent/Request/Claim/Result/Fail） | BlackboardEvent schema 强制 |
| inv_M8_02 | Blackboard 写入与 EventLog 同事务双写——EventLog 是真相源 | M2 CompositeMutationIntent |
| inv_M8_03 | Task 状态单调推进 Pending→Claimed→Executing→Done/Failed，禁止回退 | CAS Version++ 乐观锁 |
| inv_M8_04 | Agent Lease TTL 60s + 心跳 15s ±5s jitter + Reaper 1s 扫描——Lease 过期任务自动回收 | M8 §1.7 Reaper |
| inv_M8_05 | Taint 经 Blackboard 传播——input_data 携带原始 TaintLevel，协调期间不降级 | M8 §4 blackboard_entries taint_level CHECK |
| inv_M8_06 | 委托链深度 ≤3——跨 Agent 委托禁止超过 3 层 | M11 §8 Layer 4 多 Agent 宪法 |

---

## 1. 黑板 + CAS 原子认领

### 1.1 核心结构

Blackboard/TaskEntry/TaskStatus/BlackboardEvent 类型及 CAS Claim/RenewLease/SideEffectPreCheck 实现见 `pkg/swarm/blackboard.go`。

### 1.2 初始化

NewBlackboard:
1. [EventLog] 全量回放 session_events → 重建 tasks + agents
2. 回放时按事件 type 过滤: 只重建 Pending/Claimed/Executing/Suspended/Compensating 状态的 task；Done/Failed + UpdatedAt+5min<now 跳过重建（已被 Reaper 驱逐的终态任务不回灌内存）
3. 维护"重建窗口"（默认 24h），超出窗口的事件按归档语义处理
4. 启动 Reaper(1s) + monitorBackpressure(500ms)
5. backpressure: chan >80%→拒绝 PostTask, <50% 解除; Agent 非阻塞 `select{case ch<-event: default: localQueue(max128)}`
6. **Heartbeat/RenewLease 禁止走 events chan** — Lock 内直接更新 ExpiresAt, 控制/数据平面严格分离
7. Supervisor 重启: BatchRebuild — [EventLog] 全量合并 BatchBlackboardSync, chan 排空后切回常规流
8. schemaValidator: PostTask 强类型校验, 失败→ErrSchemaViolation
9. identityVerifier: Ed25519 签名校验, 私钥仅存 wazero 线性内存 ([Sandbox-L2])

常量（全部权威源 `spec/state.yaml §m8_multiagent`）: `lease_ttl_seconds` | `heartbeat_interval_seconds`（±5s jitter）| `reaper_scan_interval_seconds`
SupervisorEpoch: 启动时 [Storage-SQLite] sys_config 原子递增 `orchestrator_epoch`, 立即开放 CAS。Worker 拉取式: SideEffectPreCheck 时读 epoch(O(1),<0.1ms), 不一致→GracefulTermination+重注册。连续 3 次心跳未响应(45s)→后备轮询。

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

持有 RLock: 检查 Status==Executing, ClaimedBy==self, ExpiresAt>now, Version==self.claimedVersion → 任一失败→释放RLock, ErrStaleLease, Agent→S_ROLLBACK → 全通过→快照到栈, 释放RLock
释放 RLock 后: 执行 tool call(可安全阻塞 [MutationBus]) → 写回时重 Lock+CAS(Version==self.claimedVersion)

不可逆操作 TOCTOU (write_network/privileged):
- L1: `SHA-256(taskID+operation_seq+toolName)` SurrealDB-Core KV Insert-if-not-exists(TTL=15s, 心跳续期). Insert success→执行; completed→返回缓存; inflight→轮询等待(以 Agent A 租约存活/TTL到期为决策, 禁止硬编码5s覆盖)
- L2: 外部 API 原生幂等键注入(辅助)

### 1.7 Reaper

阶段1(1s): Lock 扫描, 跳过 Suspended/Compensating → Claimed/Executing+ExpiresAt过期 → `cancel(agent.ContextCancel)` + 等待 5s 宽限期（供工具 ctx.Done() 感知取消并中止，闭合 M7 §4.6 TOCTOU PostCheck 的孤儿副作用窗口）→ Version++ + 重置(ClaimedBy=nil,Status=Pending) → 发射 EventTaskReaped。Version++ 确保 [MutationBus] 残留 Intent 在乐观锁 `WHERE version=oldVersion` 失败。5s 宽限期内若工具已完成并进入 PostCheck，PostCheck 发现 Version 不匹配 → 审计 side_effect_orphaned + 写入 decision_log（M7 §4.6）。
**崩溃恢复重建时**，扫描到的所有已过期 Executing/Claimed 任务**并发 cancel**（errgroup），等待 max(5s) 统一宽限期，而非串行等待。时间复杂度 O(max(5s))，而非 O(N×5s)。

阶段2(30s, [Tier-0-Limit] 内存守卫): Lock 内 Status∈{Done,Failed}+UpdatedAt+5min<now+DependsOn反向索引满足 → `delete(tasks,taskID)`, len>50000 追加驱逐 → 释放 Lock → 逐条 MutationIntent→[MutationBus].Submit 归档; 失败→WARN+`~/.polarisagi/polaris/workspace/reaper_fallback/{taskID}.pb`

### 1.8 Agent 看板监听

TaskEntry 包含 Priority 字段，与 M13 ResourceGovernor 统一优先级体系:
- Priority=0: 用户交互（CLI/WebUI 直接请求）—— 始终放行
- Priority=1: 前台辅助（Agent 工具调用链中的子任务）
- Priority=2: 后台优化（Consolidation/Reflection/PromptOptimizer）
- Priority=3: 最低（Auto-Curriculum 课程任务、索引重建等）

- **ListenLoop**: 后台长轮询监听新发布任务。对队列中满足优先级的任务尝试 CAS 竞争（由于无中央调度）。
- **动态提权防饥饿**: 优先级较低的后台任务如果在队列中持续待机时间超过阈值，会被自动提权。例如 `Priority=2` 待机超过 5min 升至 1；`Priority=3` 等待超过 15min 也将被强制提权，防止永久被前台任务挤占。
无中央调度, 优先级排序 + CAS 隐式竞争。**优先 inversion 防护**: Priority>=2 的任务 Pending > 5min → 自动升至 Priority=1，防后台任务永久饥饿。Auto-Curriculum (Priority=3) 升到 1 后若再饥饿 10min → 保持 Priority=1 不再降级 + WARN。

### 1.9 Phased Startup

```
1. DependencyGraph: 解析I/O声明, DFS三色环路检测, 有环→ErrCircularDependency
2. TopologicalSort: Kahn入度分层
3. PhasedStart:
   **P0**: [M11] Policy（CredentialVault.Init() 完成 + StorageFabric.Open()）+ Cedar-Gate fail-closed 激活 + Blackboard + [Storage-SQLite]
   → **P1**: [M5] + [M10] 索引就绪
   → **P2**: [M6] + [M7] 注册完成
   → **P3**: Orchestrator + Planner
   → **P4**: Worker / Reviewer

   M11 Cedar-Gate 在 P0 阶段即完成初始化并启用 deny-by-default 语义，确保 P0→P4 全程不存在策略真空窗口。P0 完成前任何工具注册和任务执行均被阻断（HealthCheckGate 30s 超时强制约束）。
4. HealthCheckGate: 每层30s超时→ErrPhaseStartupTimeout; 动态加入仅验证直接依赖
```

---

## 2. Supervisor Tree

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

实现见 `pkg/swarm/topology/swarm.go`。与 §3 各编排模式（执行层）正交：SwarmRouter 是**路由决策层**，决定任务经由 Blackboard CAS 还是 Stigmergy 能力匹配分发；SwarmCoordinator 是**执行协调层**，负责 handoff 流程。

### 三层 Agent 数量限制

基于实证（arxiv 2605.03310）：Sequential Pipeline 3-4 Agent Brier 0.153 最优，Consensus Alignment 0.181 最差；超 10 Agent 协调噪音压过收益。

```
AgentLimits（DefaultAgentLimits）:
  Registry:  10   // 全局注册上限（超额 → ErrRegistryFull）
  Hierarchy: 3    // 单任务 Hierarchy 参与数（Tier 0 goroutine 内存约束）
  Pipeline:  5    // 流水线阶段数
  Mesh:      10   // 单任务 Mesh 参与数 + 拓扑自动升级阈值
```

`NewSwarmRouter` 自动将 `Registry=10` 注入 CapabilityRegistry，无需手动配置。

### CapabilityRegistry — 两道注册门控

```
Register(caps AgentCapabilities) error:
  1. 容量门控: len(byAgent) >= maxCapacity → ErrRegistryFull
  2. 角色唯一性: 已有其他 Agent 持有完全相同能力集 → ErrDuplicateCapabilities
     （相同 = 集合等价；子集 = 合法专项 Agent，允许注册）
  重复注册同一 agentID = 更新（不触发限制）
```

```
AcquireLease(agentID)  // routeMesh 路由成功后自动调用，递增 Load
ReleaseLease(agentID)  // Blackboard Reaper / Agent 完成回调，递减 Load（floor 0）
AgentCount() int       // O(1)，用于自动拓扑切换判断
```

### SwarmRouter — 自动拓扑切换

`RouteTask` 每次调用前根据 `registry.AgentCount()` 动态计算有效拓扑：

```
count >= Limits.Mesh(10) → TopologyMesh（升级）
count < Limits.Mesh 且 CurrentMode==Mesh → TopologyHierarchy（降回）
```

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

## 4. Agent Card 与能力发现

AgentCard 声明 Agent 能力集（Skills/Tools/Models）、激活条件（TaskTypes/MaxLoad/RequiresTools）、信任级别与沙箱层级；AgentRegistry 以 RWMutex 保护 agentID→Handle 映射，支持本地 chan 与远程 A2A gRPC 两种 Handle 类型，心跳 60s 超时标记 unreachable 并从匹配池移除。

FindBestAgent: Phase1 硬过滤（DeclaredCapabilities ⊇ RequiredCapabilities；空→全体）→ Phase2 加权降序评分：`score = 0.6 × LaplaceSuccRate + 0.4 × LoadFactor`。Laplace 成功率使用先验平滑，LoadFactor = `1/max(CurrentLoad, 1)`。

**实时负载数据**：`Orchestrator.dispatchPendingTasks` 在每次调度前通过 `queryAgentLoads` 查询数据库，获取各 Agent 当前 claimed+running 任务数，作为 `currentLoads` 参数传入 FindBestAgent，确保负载均衡基于真实状态而非空映射。实现见 `pkg/swarm/orchestrator.go`。

**SQLiteBlackboard.StartExecution**：`ClaimTask`（claimed）之后，Agent 开始实际执行前调用此方法将状态推进到 running，广播 task_running 事件，提供更细粒度的任务状态追踪。与内存版 Blackboard 保持接口对称；幂等，重复调用 already-running 不报错。

**编排模式可配置超时**：`SequentialExecutor` 和 `MapReduceExecutor` 构造器接受超时参数（`perTaskTimeout` / `totalTimeout`），0 值使用默认值（5min / 10min），调用方可按任务复杂度定制，不再依赖固定兜底时间。实现见 `pkg/swarm/patterns/`。

---

## 5. Task 分解与依赖管理

Macro-DAG(本模块): 节点=子任务跨Agent边界, [Blackboard] 发布, 边=data|approval|sequential
Micro-DAG(M4): 子任务内部工具调用, M4 Agent Kernel 管理

Macro-DAG 节点为跨 Agent 子任务，边类型为 data/approval/sequential；ExecuteDAG 按拓扑分层并发（errgroup），任一层失败即终止并触发 Saga Rollback；Planner 5min 超时 → DAG Rollback，崩溃后由 Supervisor 通过 [EventLog] 恢复。

---

## 8. 编排拓扑自演化

TopologyFitness 字段：Topology/TaskType/SuccessRate/AvgLatencyMs/AvgTokenCost/AgentUtilization(0-1)/SampleSize。TopologyEvolver.Evaluate() 实现双维 Pareto：成功率领先 ≥5pp 且 token 成本不劣化超 10%，SampleSize<10 的候选不参与评估。A/B 50/50 分流由 M13 TrafficSplitter 执行（TopologyEvolver 仅做决策，不直接切流）。

| 阶段 | 流量 | 观察 | 回滚条件 |
|------|------|------|---------|
| Shadow | 0% | 50任务 | — |
| A/B | 50% | 50任务 | 成功率↓>5pp |
| Gradual | 100% | 7d | 成功率/token效率退化>3pp |
| Commit | 100% | 永久 | — |

启用: ≥50历史执行/类型; 全 Tier 已支持 7 种模式（patterns/ 目录: Supervisor/Sequential/Parallel/Hierarchy/MapReduce/Reflection/Swarm）。

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

用户定义同名 Profile → 覆盖内置。

**代码位置**: `pkg/swarm/agent_profile.go`

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
```
ReadCSV → expandTemplate({column_name}) → PostBatch(TaskEntry[]) → 并发 PeekTask 轮询
→ 所有 Task Done/Failed → writeResultCSV
```

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

**代码位置**: `pkg/swarm/csv_fanout.go`, `pkg/swarm/blackboard.go:PeekTask`
