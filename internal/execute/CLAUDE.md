# internal/execute — 模块规范

> 对应架构文档：`docs/arch/M04-Agent-Kernel.md`（dag 子包）、
> `docs/arch/M08-Multi-Agent-Orchestrator.md`（orchestrator 子包）
> 决策档案：`docs/arch/decisions/ADR-0046-execute-module.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

单/多 Agent 执行引擎层（2026-07-12 新增，物理整合自
`internal/agent/dag` 与 `internal/swarm/orchestrator`）。只负责"如何把一份
已确定的计划/图跑完"——拓扑调度、并发执行、Saga 补偿、条件路由、扇入语义、
四层安全校验管线。不做任何决策：目标分解交给 `swarm/planner`，单 Agent 控制流
交给 `agent/fsm`，跨 Agent 编排策略（谁该做什么）交给 `swarm/supervisor`/
`swarm/topology`。

```text
internal/execute/
  dag/           单 Agent 内工具链 DAG 执行器（DAGExecutor）+ S_VALIDATE
                 四层校验管线（ValidateDAG）。消费方：internal/agent（经
                 provider.go 的 DAGRunner/DAGValidator 消费端接口）。
  orchestrator/  跨 Agent Blackboard（任务黑板 CAS）+ 多模式编排执行引擎
                 （Sequential/Parallel/MapReduce/PatternDAG/StateGraph/
                 CSV-Fanout）。消费方：internal/swarm（拓扑/督导/规划）、
                 internal/gateway/server/sysadmin/workflowadmin（Workflow
                 DAG 并行执行）。
```

## 为何是两个子包而非合并为单一执行引擎

`dag` 与 `orchestrator` 均以"Executor"命名结尾（DAGExecutor / PatternDAGExecutor /
StateGraphExecutor），但作用域不同：`dag` 是单个 Agent 内部工具调用序列的执行
（M04 §5.3，输入是 LLM 一次 S_PLAN 产出的 ExecNode 列表），`orchestrator` 是跨
多个 Agent 实例的任务编排（M08，输入是 Blackboard 上多个独立 Agent 认领的任务）。
二者的调度对象（工具调用 vs. Agent 任务）、生命周期（一次 FSM 步骤内同步执行 vs.
跨多个异步 Agent 生命周期）、失败语义（Saga 补偿 vs. Blackboard 任务重试/Reaper）
均不同，不应假借同名强行合并为一个包——参照 2026 年 Planner-Executor 分离的行业
共识（详见 ADR-0046），"执行引擎"内部按调度对象边界拆分子包，而非拍平为一个
巨石包。

## 权力边界 [MUST]

### 拥有
- DAG/图拓扑校验、并发调度、Saga 逆序补偿的唯一实现权（`dag`）
- Blackboard 任务全生命周期（PostTask/ClaimTask/Complete/Fail/Reaper）的唯一
  实现权（`orchestrator`，2026-07-12 前隶属 `swarm/orchestrator`，物理迁出但
  接口契约不变，见 `internal/swarm/CLAUDE.md`）
- S_VALIDATE 四层校验管线（L0 拓扑/L1 Taint/L1 Policy/L2 Heuristic/L3 LLM
  看门狗）的唯一实现权（`dag`）

### 禁止 [MUST NOT]
- **[MUST NOT]** 持有任务/计划应该"做什么"的决策权（分解目标属于
  `swarm/planner`；LLM 生成 DAG 内容属于 `agent/fsm` 的 S_PLAN 状态）
- **[MUST NOT]** 直接 import `internal/agent` 具体实现（防止反向依赖它服务的
  认知核心；`internal/agent` 通过 `agent/provider.go` 声明的 DAGRunner/
  DAGValidator 消费端接口反向注入本模块的具体实现，方向不可颠倒）
- **[MUST NOT]** 在 Reaper 扫描路径（HeartbeatInterval=15s）中调用 LLM
  （沿用原 `swarm/orchestrator` 约束）
- **[MUST NOT]** 在 DAG 节点执行/Blackboard CAS 认领路径中做非幂等的静默重试
  （HE-2 可验证执行：失败必须产生可观测的结构化错误，不得吞掉后重试）

## 消费端接口声明位置

- `internal/agent/provider.go` — `DAGRunner`/`DAGValidator`（`dag` 子包实现，
  `execute/dag.Runner`/`execute/dag.Validator` 由 `cmd/polaris/boot_agent.go`
  构造并注入，二者均无状态，可全局共享同一实例）
- `internal/swarm/provider.go` — `orchestrator` 子包的消费方声明（Blackboard/
  LLMInfer/OutboxWriter 等，见该文件 `@consumer` 注释）

## 不可变内核保护

`internal/execute/dag/` 已列入 `internal/config.ImmutableKernelPackages()`
白名单（L1 TaintGate + L1 Cedar PolicyGate 是 HE-2 安全边界，随 2026-07-12
从 `internal/agent/dag/` 迁出时一并迁移保护范围）。`internal/execute/orchestrator/`
不在白名单内（沿用其前身 `internal/swarm/orchestrator/` 此前从未受此保护的
既有边界，迁移未扩大保护范围）。
