# ADR-0046: 新建 internal/execute 模块，收敛单/多 Agent 执行引擎

- **状态**: Accepted
- **日期**: 2026-07-12
- **决策者**: MrLaoLiAI
- **相关模块**: M04 / M08 / `internal/agent`（原 `internal/agent/dag`）/ `internal/swarm`（原 `internal/swarm/orchestrator`）/ `internal/execute`（新）

## 上下文

用户提出："系统经过三次彻底重构，前两次都有一个 Executor 模块，现在是否该重新引入"，
并要求不受旧系统（`polaris-agent`）架构文档/决策文档约束，仅以"AI Agent 系统最优"
为准绳，先调研 2026 年最新 Agent 系统设计实践再判断，且明确排除"把规划（planner）
也并入执行模块"这一选项。

调研 2026 年生产级 Agent 编排实践（LangGraph/AutoGen/CrewAI 等）：
1. **Planner-Executor 强制分离**是行业共识——独立推理循环（甚至不同模型尺寸），
   混合会导致"规划失败传播"（planner 产出错误步骤时 executor 无法自行纠正）。
2. **"Executor"在 2026 术语里特指图/DAG 执行引擎**（LangGraph 的 node+edge+
   conditional edge+checkpoint 模型），强调持久化状态、可审计、确定性回放。

同时排查 polaris 现状发现："Executor"一词在代码库历史/现状中已有三层不同语义：
1. 旧 `polaris-agent/internal/executor`：SENA 架构的盲执行动作层（Router+Executor，
   基底核/动作选择，禁业务逻辑）——对应今天的 `action`+`tool`+`sandbox`。
2. `internal/agent/dag.DAGExecutor`：单 Agent 内一次 S_PLAN 产出的工具调用链
   DAG 执行（M04 §5.3）。
3. `internal/swarm/orchestrator.Pattern*Executor`/`StateGraphExecutor`：跨 Agent
   Blackboard 编排（M08）——本会话前半场刚完成其首个生产接入（workflow DAG）。

`internal/swarm/CLAUDE.md` 早已明文"不包含 Agent 内部执行逻辑，只负责任务分发与
生命周期协调"；`internal/agent/CLAUDE.md` 明文 DAG 执行是 agent 思考循环的一部分。
二者的"执行引擎"实现此前分别物理挂靠在 `agent/`（L1）与 `swarm/`（L2）目录下，
从未被当作独立关注点收拢。

## 决策

**新建 `internal/execute` 顶层模块，物理迁入 `internal/agent/dag` → `internal/execute/dag`
与 `internal/swarm/orchestrator` → `internal/execute/orchestrator`，规划（`swarm/planner`）
不迁入。**

依据：

1. Planner-Executor 分离是 2026 行业验证的最优实践，`swarm/planner` 与执行引擎的
   既有边界本就正确，合并是倒退，故不采纳"规划也并入"的选项。
2. 迁移前 `internal/agent/dag` 被 FSM 核心（`fsm/state_machine.go`、
   `fsm/transitions.go`、`agent_execute_*.go`）直接 import——物理迁出后按
   `agent/provider.go` 既有的消费端接口模式（HE-3）改造：新增 `DAGRunner`/
   `DAGValidator` 接口，`execute/dag.Runner`/`execute/dag.Validator` 作为无状态
   适配器由 `cmd/polaris/boot_agent.go` 构造注入，FSM 不再直接依赖具体实现。
3. `internal/swarm/orchestrator` 迁移前仅 6 处组装根引用（`cmd/polaris/boot_agent.go`、
   `server_lifecycle.go`、`sysadmin/handler.go`、`workflowadmin/{admin,workflow_engine}.go`、
   `swarm/startup_test.go`），无深层耦合，纯路径重命名。
4. 迁移过程中一并修复两处发现的遗留缺陷：`internal/protocol/dag_node.go` 的
   `DAGPlan`/`ExecEdge`/`EdgePolarity` 此前已定义却从未被 `agent/dag` 引用（死代码，
   `agent/dag` 保留了独立重复定义）；`ImmutableKernelPackages()` 注释与实际返回值
   长期不一致。

## 后果

- **正向**: 单/多 Agent 执行引擎有了统一、语义清晰的物理归属；FSM 与执行引擎的
  依赖方向通过消费端接口显式化，符合 HE-3；`internal/execute/dag` 保留 L4 不可变
  内核保护（随迁移同步调整白名单，不丢失 L1 TaintGate/PolicyGate 的 CI 保护）。
- **负向**: `internal/agent` 新增一层接口间接性（`DAGRunner`/`DAGValidator`），
  `NewAgentWithDefaults` 保留了一处对 `internal/execute/dag` 的直接 import（测试/
  开发默认构造器的务实例外，生产路径仍由 `boot_agent.go` 显式覆盖注入）。
- **反例守护**: 未来如有人提议把 `swarm/planner` 也物理迁入 `internal/execute`
  （"既然都叫执行引擎，规划也该在一起"），引用本 ADR 第 1 条拒绝——规划与执行的
  分离是经过调研验证的架构决策，不是历史遗留待清理项。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留现状，不做物理重组 | orchestrator 本次会话才完成首个生产接入，用户主动要求评估重组而非"先观察"；调研已给出明确依据，无需再拖延 |
| 仅重命名 `swarm/orchestrator` → `swarm/execute`（子目录级，不动 `agent/dag`） | 用户在看到 `agent/dag` 深耦合 FSM 控制流的具体成本分析后，仍选择"仍按方案搬迁，补接口化改造"，即接受更大改动换取单/多 Agent 执行引擎的统一物理归属 |
| 把 `swarm/planner` 一并迁入 `internal/execute` | 与 2026 年 Planner-Executor 分离的行业共识矛盾，见"决策"第 1 条 |
| `execute/dag` 与 `execute/orchestrator` 合并为单一 Go 包 | 二者调度对象（工具调用 vs. Agent 任务）、生命周期、失败语义均不同，强行合并是巨石化，见 `internal/execute/CLAUDE.md`"为何是两个子包" |

## 引用代码

- `internal/execute/dag/`（原 `internal/agent/dag`，DAGExecutor/ValidateDAG 实现 + `runner.go` 适配器）
- `internal/execute/orchestrator/`（原 `internal/swarm/orchestrator`）
- `internal/agent/provider.go`（`DAGRunner`/`DAGValidator` 消费端接口）
- `internal/protocol/dag_node.go` + `dag_validation.go`（跨模块共享类型，本次迁移一并修复 `DAGPlan`/`ExecEdge` 死代码问题）
- `internal/config/immutable_constants.go`（`ImmutableKernelPackages` 白名单同步调整）
- `internal/execute/CLAUDE.md`（模块权力边界）
- `docs/arch/M04-Agent-Kernel.md §5.3`、`docs/arch/M08-Multi-Agent-Orchestrator.md`（设计文档）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-12 | 初稿，记录 internal/execute 模块化决策与迁移范围 |
