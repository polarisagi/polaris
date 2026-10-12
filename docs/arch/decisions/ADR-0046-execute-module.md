# ADR-0046: internal/execute 模块 + 编排模式演进（模式9 PatternDAG / 模式10 StateGraph / 模式11 Debate）

> 状态：Implemented ｜ 首次：2026-07-13 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0037/0041/0080 ｜ 模块：M04/M08 `internal/execute/`

## 决策一：模块创建

新建 `internal/execute`，物理迁入 `internal/agent/dag` → `execute/dag`（单 Agent 内工具链 DAG）与 `swarm/orchestrator` → `execute/orchestrator`（跨 Agent Blackboard 编排）。**`swarm/planner` 不迁入**——Planner-Executor 分离是行业共识（LangGraph/AutoGen/CrewAI），混合会导致规划失败传播。
- 迁出后 FSM 核心按消费端接口（HE-3）依赖 `DAGRunner`/`DAGValidator`（`internal/agent/provider.go`），`execute/dag.Runner`/`Validator` 为无状态适配器，由 `cmd/polaris/boot_agent.go` 注入
- `execute/dag` 与 `execute/orchestrator` 不合并：调度对象、生命周期、失败语义均不同

## 决策二：模式9 PatternDAGExecutor（原 ADR-0037）

跨 Agent 边界的 DAG 调度（`ModePatternDAG`，`protocol.WorkflowGraphSpec`），区别于单 Agent 内 Micro-DAG。图校验（环检测/深度限制）在 `pkg/graph/dag.go`。基于 Blackboard `Subscribe()` 事件驱动（Kahn 拓扑），仅分发上游已完成的节点。Fail-Fast：任一节点失败即停止后续投递，已完成节点按拓扑逆序并发补偿。

## 决策三：模式10 StateGraphExecutor（原 ADR-0041，GD-8-001）

在 PatternDAGExecutor 之上叠加条件边与有界循环，**不**替换 Blackboard 底层（CAS 认领/Lease/Reaper 是既有不变量基础设施）。
- 协议扩展（`WorkflowGraphSpec`，两执行器共用）：`WorkflowEdgeSpec.Condition` 声明式比较（`Field`/`Op`/`Value`，算子 `eq/ne/gt/lt/ge/le/contains/exists` + `And`/`Or` 递归；HE-2 禁脚本/表达式引擎含 CEL）；`WorkflowNodeSpec.MaxVisits`；`WorkflowNodeSpec.IsEntry`
- 拓扑校验 `ValidateStateGraphTopology`：允许环，要求引用完整、至少一个合法入口、`effectiveMaxVisits` 总和 ≤200（硬编码熔断）；终止性由运行时硬计数器保证
- `MaxVisits>1` 且声明 `Compensation` → fail-closed 拒绝（补偿逆序语义未定义）
- 无条件多前驱边 AND-Join；条件边/自环边 OR

## 决策四：模式11 DebateExecutor（原 ADR-0080，GD-6）

既有模式无法原生表达对抗性辩论/互审。新增 `DebateExecutor`（`PatternDebate`）：Judge/Proponent/Opponent 经 `agent_handoff:<role>` Blackboard 事件交互，复用 Checkpoint+Watcher 异步挂起原语，不引入新阻塞轮询。配置 `debate.max_rounds`/`debate.concurrent_sides`；初期强制串行交替（防多 LLM 并发 OOM，Tier-0 约束）。

## 防退化条款：StateGraph 不得被 Chain/Pipeline 替代

`PatternStateGraphExecutor` 的确定性状态机流转 + EventLog 回放是刻意设计。"简化为线性 Chain/Pipeline"的提议一律驳回，除非同时给出等价的确定性回放方案；删除或弱化该执行器需新开 ADR（2026-08-01，阶段06 GD-14-104 复核：审查曾建议简化，核实为刻意设计后驳回）。

## 反例守护

- 把 `swarm/planner` 迁入 `internal/execute` — 违反 Planner-Executor 分离
- 接入 CEL 等表达式引擎 — 审查确定性优先于表达力
- 新编排模式引入独立阻塞轮询 — 统一复用 Checkpoint+Watcher

## 重新评估触发条件

`swarm/planner` 迁入仅在 Planner-Executor 分离被证明对本项目是错误取舍时重议；StateGraph 简化提议缺等价确定性回放方案一律驳回。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-07-11~07-26 | 模式9（0037）、模式10（0041）、模式11（0080） |
| 2026-07-13 | 模块创建 |
| 2026-07-28 | 合并 ADR-0037/0041/0080 |
| 2026-08-01 | 追加防退化条款 |

## 引用代码

`internal/execute/dag/`、`internal/execute/orchestrator/`（`pattern_dag.go`/`pattern_state_graph.go`/`pattern_debate.go`/`debate_worker.go`）、`pkg/graph/{dag,state_graph}.go`、`internal/agent/provider.go`、`internal/execute/CLAUDE.md`
