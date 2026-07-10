# ADR-0041: StateGraphExecutor（显式状态图编排，GD-8-001）

## 1. Context (上下文)

`local_playground/upgrade/04-设计增强与Backlog.md` Backlog-3 记录了 GD-8-001："Multi-Agent 显式状态图编排替代 Blackboard"（LangGraph 式对标建议）。原复核意见判断此项"暂不推荐大改"：现有 `internal/swarm` 以 Blackboard（CAS 认领 + Lease + Reaper）做多 Agent 任务分发，且 `PatternDAGExecutor`（ADR-0037，编排模式9）已将执行控制流从"去中心化认领"收敛为"显式拓扑驱动"，未发现 Blackboard 导致的实际协作缺陷或死锁案例。

用户在后续开发指示中明确要求"仍要完整评估/实现"本项。本 ADR 记录重新评估后的结论与具体实现方案。

### 重新评估：真正的缺口是什么

复核 `PatternDAGExecutor` 发现：它已经是"显式拓扑驱动"而非"Blackboard 去中心化认领"，但存在两个具体限制，与 LangGraph 式状态图的核心能力差着一步之遥：

1. **无条件边**：`WorkflowEdgeSpec` 仅有 `From`/`To`，边的触发完全由静态依赖关系决定，无法表达"验证节点失败 → 路由回执行节点重试"这类基于运行时输出的条件路由。
2. **无循环**：`pkg/graph.ValidateTopology` 通过 DFS 三色法**无条件拒绝任何环**，`WorkflowNodeSpec` 也没有节点级重复执行次数的概念。

即，GD-8-001 真正缺失的能力是"条件路由 + 有界循环"，而不是"用另一套存储/队列机制取代 Blackboard"。

### 是否应当字面意义"替代 Blackboard"：否

评估结论：**不采用字面意义的完全替换**，理由：

1. Blackboard 承载的 CAS 原子认领、Lease TTL/Reaper 崩溃恢复、`SideEffectPreCheck` TOCTOU 版本校验、`inv_M8_02` EventLog 事务双写等机制，是当前 M8 一系列不变量（含 HE-Rule-6 State-in-DB、HE-Rule-2 可验证执行）的基础设施依赖。完全替换意味着这些不变量的实现基础也要推倒重来，属于大改，代价远超"条件路由 + 有界循环"这一具体能力缺口本身的收益。
2. `PatternDAGExecutor` 珠玉在前的做法已经证明：把"显式拓扑驱动"作为一层叠加在 Blackboard（持久化任务队列/事件总线）之上是可行的、代价可控的路径——本 ADR 沿用同一模式，而非另起炉灶。

因此本 ADR 的决策是：**新增编排模式10 `StateGraphExecutor`，在 `PatternDAGExecutor` 之上泛化支持条件边与有界循环，Blackboard 角色不变（仍是持久化任务队列/事件总线）**。这既满足 GD-8-001 的实际能力诉求（条件路由 + 循环反馈），也不违反"暂不推荐大改"的原始审慎判断——两者并不矛盾：大改的是"新增一个模式"，未大改的是"不动 Blackboard 底层机制"。

### 与 ADR-0040 的关系

`ADR-0040-cyclic-graph-executor.md` 是一份此前未落地的设计草案（`CyclicGraphExecutor`），提出了几乎同一目标但路径/机制不同的方案（详见该文档 §4 Status）。本 ADR **取代**该草案：解决同一问题（有界循环 + 条件路由），但落点在 `internal/swarm/orchestrator`（跨 Agent 编排层，与 `PatternDAGExecutor` 同级）而非该草案设想的 `internal/agent/pattern`（该路径实际不存在），机制上以节点级 `MaxVisits` + 边级 `Condition` 取代该草案的全局 `state.yaml` 阈值 + 显式 `BackEdge` 类型。

## 2. Decision (决策)

1. **新增编排模式10 `StateGraphExecutor`**（`internal/swarm/orchestrator/pattern_state_graph.go`），复用 `PatternDAGExecutor` 的 Blackboard 事件驱动模型（`bb.PostTask`/`bb.Subscribe`），但执行算法从"Kahn 拓扑排序式一次性推进"改为"事件驱动的条件触发 + 有界重访"。

2. **协议层扩展 `WorkflowGraphSpec`**（`internal/protocol/dag_node.go`，`PatternDAGExecutor` 与 `StateGraphExecutor` 共用同一结构体）：
   - `WorkflowEdgeSpec.Condition *EdgeCondition`：声明式字段比较（`Field`/`Op`/`Value`），对上游节点输出 JSON 求值决定该边是否触发。HE-Rule-2（可验证执行）要求：**不引入脚本/表达式引擎**作为条件求值器，仅支持声明式字段相等/不等比较，避免"可验证执行"退化为"运行任意代码决定控制流"。
   - `WorkflowNodeSpec.MaxVisits int`：节点允许被（重复）触发的最大次数，0/1 与既有 DAG 语义完全等价（向后兼容，`PatternDAGExecutor` 忽略此字段）。
   - `WorkflowNodeSpec.IsEntry bool`：显式声明入口节点。原因：参与循环反馈的节点（如被验证节点循环边指回的"执行节点"）入度恒 > 0，纯入度分析无法识别其为合法起点，需要显式标记。

3. **拓扑校验**（`pkg/graph/state_graph.go`，`ValidateStateGraphTopology`）：**允许环**（与 `ValidateTopology` 的无条件拒绝环相反），但要求：
   - 引用完整性（边的 From/To 必须是已声明节点）；
   - 至少存在一个合法入口（入度为 0 或显式 `IsEntry`）；
   - 所有节点 `effectiveMaxVisits`（未声明按 1 处理）之和不超过 `StateGraphMaxTotalVisitBudget`（=200，硬编码熔断常量，对齐 `ValidateTopology` 现有的 `maxDepth=10`/节点数 50 上限同类做法，不接入 `state.yaml`）。

4. **终止性保证**：不采用"拓扑分析猜测是否可能死循环"（概率性判断，违反 HE-Rule-2），而是用**运行时硬计数器**（每节点 `visits[nodeID]` 与全局 `totalPosted` 双重上限）物理保证终止——无论图结构如何，超出预算后续触发直接被丢弃（非错误，语义上等价于"该分支的循环/路由自然终止"）。

5. **Compensation 冲突拒绝**：`MaxVisits > 1` 且节点声明 `Compensation`（Saga 逆序补偿）视为非法配置，`StateGraphExecutor` 校验阶段 fail-closed 拒绝——多次执行节点的补偿逆序语义未定义，不做"看起来能跑"的隐式行为。

6. **前置 Bug 修复（`CompleteTask` 缺失 Payload）**：开发过程中发现 `SQLiteBlackboard.CompleteTask`（`sqlite_blackboard.go`）广播的 `task_completed` 事件从未携带 `result` 参数（`Payload` 字段为空），与同级 `FailTask` 的 `Payload: errBytes` 模式不一致——这是一个已存在的遗漏（并非本次引入），导致 `PatternDAGExecutor` 的 `upstream_outputs` 传递此前实际上恒为空字符串，`StateGraphExecutor` 的条件边求值更是完全无法工作。已在 `CompleteTask` 广播中补上 `Payload: result`，对齐 `FailTask` 既有模式（不改动 DB 持久化范围——`tasks` 表本身也无独立 result 列，broadcast payload 与 DB 持久化字段是两回事，与 `FailTask` 处理方式完全一致）。

## 3. Consequences (影响)

- **正向影响**：补齐 GD-8-001 的实际能力缺口（条件路由 + 有界循环），且不触碰 Blackboard 底层机制，风险面局限于新增文件 + `WorkflowGraphSpec` 的向后兼容扩展（新增字段全部 `omitempty`，`PatternDAGExecutor` 忽略新字段，现有调用方零改动）。副带修复了一个影响 `PatternDAGExecutor` 上游产出传递的既有 Bug。
- **负向影响**：`WorkflowGraphSpec` 现在同时承载"严格 DAG"（编排模式9）与"有界状态图"（编排模式10）两种语义，调用方需明确按哪种模式构造图（`Condition`/`MaxVisits`/`IsEntry` 对 `PatternDAGExecutor` 无意义，混用可能造成误解）。`StateGraphExecutor` 暂不支持 Saga 补偿（见决策5），需要补偿的重型副作用节点仍应使用 `PatternDAGExecutor`。
- **已知限制**（非本轮范围）：条件求值仅支持顶层字段字符串比较，不支持嵌套字段/数值比较/逻辑组合（AND/OR）；`StateGraphMaxTotalVisitBudget` 为硬编码常量，未提供运营者可调配置项。

## 4. Status (状态)

**Accepted**（2026-07-11）。实现见 `internal/swarm/orchestrator/pattern_state_graph.go` + `pkg/graph/state_graph.go` + `internal/protocol/dag_node.go`；测试见 `internal/swarm/orchestrator/pattern_state_graph_test.go` + `pkg/graph/state_graph_test.go`；`make lint`/`make test` 全绿。
