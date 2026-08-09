# ADR-0046: internal/execute 模块创建 + 编排模式演进（模式9 PatternDAG / 模式10 StateGraph / 模式11 Debate-Critic，含原 ADR-0037/0041/0080）

- **状态**: Implemented | **日期**: 2026-07-13（模式演进 2026-07-11~07-26，合并 2026-07-28）| **模块**: M04/M08 `internal/execute/`

## 决策一：模块创建（原决策）

新建 `internal/execute` 顶层模块，物理迁入 `internal/agent/dag` → `internal/execute/dag`（单 Agent 内工具链 DAG）与 `internal/swarm/orchestrator` → `internal/execute/orchestrator`（跨 Agent Blackboard 编排）。**规划（`swarm/planner`）不迁入**——Planner-Executor 分离是行业验证的最优实践（LangGraph/AutoGen/CrewAI 共识），混合会导致规划失败传播。

`agent/dag` 原被 FSM 核心直接 import，迁出后按消费端接口模式（HE-3）改造：新增 `DAGRunner`/`DAGValidator` 接口，`execute/dag.Runner`/`Validator` 作为无状态适配器由 `boot_agent.go` 构造注入。`execute/dag` 与 `execute/orchestrator` 不合并为单一包——调度对象（工具调用 vs Agent 任务）、生命周期、失败语义均不同。

## 决策二：模式9 PatternDAGExecutor（原 ADR-0037）

支持跨 Agent 边界的有向无环图调度（`ModePatternDAG`，`protocol.WorkflowGraphSpec`），区别于 `internal/execute/dag`（单 Agent 内部工具级 Micro-DAG）。图校验逻辑（环检测/深度限制）下沉至 `pkg/graph/dag.go` 复用。基于 Blackboard `Subscribe()` 事件驱动（Kahn 拓扑排序思想），仅分发上游依赖已完成的节点。Fail-Fast：任一节点失败立即停止投递后续节点，已完成节点按拓扑逆序并发投递补偿。

## 决策三：模式10 StateGraphExecutor（原 ADR-0041，GD-8-001）

在 PatternDAGExecutor 之上叠加条件边与有界循环，**不**替换 Blackboard 底层机制（CAS 认领/Lease/Reaper 是既有不变量的基础设施，替换代价远超能力缺口本身）。

**协议扩展**（`WorkflowGraphSpec`，两个执行器共用）：`WorkflowEdgeSpec.Condition` 声明式字段比较（`Field`/`Op`/`Value`，算子集合 `eq`/`ne`/`gt`/`lt`/`ge`/`le`/`contains`/`exists` + `And`/`Or` 递归复合，HE-2 要求不引入脚本/表达式引擎含 CEL）；`WorkflowNodeSpec.MaxVisits` 节点最大触发次数；`WorkflowNodeSpec.IsEntry` 显式声明入口。

**拓扑校验**（`ValidateStateGraphTopology`）：允许环，但要求引用完整性、至少一个合法入口、`effectiveMaxVisits` 总和 ≤200（硬编码熔断常量）。终止性由运行时硬计数器（非拓扑猜测）物理保证。`MaxVisits>1` 且声明 `Compensation` fail-closed 拒绝——补偿逆序语义未定义。无条件多前驱边 AND-Join 语义；条件边/自环边 OR 语义。

## 决策四：模式11 DebateExecutor（原 ADR-0080，GD-6）

既有 Sequential/Parallel/MapReduce/PatternDAG/StateGraph 均无法原生表达"对抗性辩论/互审"任务流（强用 StateGraph 会致节点逻辑膨胀）。新增 `DebateExecutor`（`PatternDebate`）：Judge/Proponent/Opponent 三方通过 `agent_handoff:<role>` 类型 Blackboard 事件交互，复用既有 Checkpoint+Watcher 异步挂起原语，不引入新阻塞轮询机制。配置 `debate.max_rounds`/`debate.concurrent_sides`；初始阶段强制串行交替（防多 LLM 实例并发 OOM，Tier-0 约束）。

## 反例守护

拒绝把 `swarm/planner` 也迁入 `internal/execute`——与 Planner-Executor 分离共识矛盾。拒绝接入 CEL 等表达式引擎——审查确定性优先于表达力。拒绝新增编排模式时引入独立阻塞轮询机制——统一复用 Checkpoint+Watcher 异步挂起原语。

## 引用代码

`internal/execute/dag/`、`internal/execute/orchestrator/`（`pattern_dag.go`/`pattern_state_graph.go`/`pattern_debate.go`/`debate_worker.go`）、`pkg/graph/{dag,state_graph}.go`、`internal/agent/provider.go`（`DAGRunner`/`DAGValidator`）、`internal/execute/CLAUDE.md`

## 防退化条款：StateGraph 编排不得被 Chain/Pipeline 替代

`PatternStateGraphExecutor`（internal/execute/orchestrator/pattern_state_graph.go）
的确定性状态机流转 + EventLog 回放能力是刻意设计，不是过度工程。
任何"简化为线性 Chain/Pipeline"的提议一律驳回，除非同时给出等价的
确定性回放方案。删除或弱化该执行器需新开 ADR 论证。

（2026-08-01 追加，来源：阶段06 D-plan GD-14-104 复核——审查曾建议简化，经核实为刻意设计后驳回，见 `local_playground/upgrade/98-rejected-findings.md` §5。）

> 2026-08-09 追记：重新评估触发条件——① `swarm/planner` 是否迁入 `internal/execute`
> 只在 Planner-Executor 分离被证明是本项目场景下的错误取舍（而非行业惯例本身）
> 时重议；② StateGraph 防退化条款要求任何简化提议必须给出等价确定性回放方案，
> 缺此方案的简化提议一律驳回，不因"看起来更简单"而放行。
