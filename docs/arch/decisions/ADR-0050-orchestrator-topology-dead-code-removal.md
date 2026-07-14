# ADR-0050: 删除中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter/CapabilityRegistry/TopologyEvolverService

- **状态**: Accepted（已执行）
- **日期**: 2026-07-14
- **决策者**: MrLaoLiAI
- **相关模块**: M08（`internal/execute/orchestrator/`、`internal/swarm/topology/`）

## 上下文

`deadcode ./cmd/polaris/...` 复核（247→237 条）中，两簇死代码呈现相同结构：production
代码里存在两套解决同一问题的实现，一套被真实使用，一套完全没有生产调用点。用户要求
不受既有架构文档/ADR 约束，纯以系统最优第一性原理判断"用的"和"没用的"哪个更好，判断
后删除较劣者；并要求考虑是否存在更优的合并/整合方案，以 2026 年 7 月的 AI Agent 领域
现状为参照。

### 簇一：任务派发——中心化 push-dispatch vs 自订阅 pull + CAS 认领

- `internal/execute/orchestrator/orchestrator.go`（`Orchestrator`/`RegisterWorker`/
  `ListenLoop`/`dispatchPendingTasks`/`queryAgentLoads`）+ `worker.go`（`Worker`/
  `tryClaimAndExecute`）+ `blackboard.go`/`blackboard_lifecycle.go`（内存版 `Blackboard`）：
  `cmd/polaris/boot_agent.go` 曾构造 `orch := orchestrator.NewOrchestrator(...)` 并存入
  `AgentBundle.Orch`，但代码自带注释明确说明 `orch.ListenLoop` **从未**注册为 Supervisor
  Worker——"其内部 dispatchPendingTasks 在生产环境下 100% 无法成功派发任务（RegisterWorker
  从未调用、agent-0 的 Skills=["general"] 与真实任务类型永远不匹配）"。`orchestrator.NewWorker`
  全仓库零调用。内存版 `Blackboard`（`NewBlackboard()`）同样零生产调用点，仅自身单测使用，
  且违反本仓库 HE-6（State-in-DB）不变量（状态仅存于进程内存）。
- 与此同时，`internal/execute/orchestrator/default_worker.go`（`DefaultTaskWorker`）与
  `internal/gateway/server/sysadmin/workflowadmin/workflow_step_worker.go`
  （`RunStepWorkerLoop`）**各自独立**收敛出同一种模式："自订阅 `SQLiteBlackboard` 的
  `task_posted` 事件 + `ClaimTask` CAS 原子认领"，且两个文件的代码注释都明确写明这是因为
  中心化 `Orchestrator`/`Worker` 机制"在生产环境从未被激活"而采取的替代方案。这是当前
  唯二两条真实生产任务分派路径。

### 簇二：多 Agent 拓扑路由——运行时动态切换 vs 结构显式声明

- `internal/swarm/topology/swarm.go`（`CapabilityRegistry`）+ `swarm_router.go`
  （`SwarmRouter`，按注册 Agent 数量 <10/≥10 自动切换 Hierarchy/Mesh 拓扑）+
  `evolver_service.go`（`TopologyEvolverService`，Shadow→A/B→Gradual→Commit 拓扑灰度
  演化状态机）：`NewSwarmRouter`/`NewTopologyEvolverService`/`NewCapabilityRegistry`
  全仓库零生产调用点，仅测试文件构造过。`docs/arch/M08-Multi-Agent-Orchestrator.md`
  §8 此前记载"✅ 已接入：TopologyEvolverService……Orchestrator 任务完成后驱动演化与
  自动回滚"，与代码不符——`Orchestrator.evolverSvc` 字段在生产环境永远为 nil，
  `SetTopologyEvolverService` 从未被调用过，属于本仓库既有的"文档断言早于代码/与代码
  脱节"通病的又一实例。
- 真实生产的多 Agent 编排走 M08 §3 十种模式（PatternDAG/StateGraph/MapReduce/Parallel/
  Sequential/Swarm 等，均已接入 `sysadmin` REST API，构造于 `boot_agent.go`）+
  `internal/swarm/agents/` 三个硬编码常驻角色（governance/security_audit/memory，
  M08 §2-bis 明确记载为常驻 goroutine，"不经过 Orchestrator 的 tasks 表"）。两者均是
  **结构显式声明**（DAG 边 / StateGraph 转移表 / 固定角色清单），不存在"同一角色多实例
  竞争、需要运行时按数量动态选路"的场景。

## 决策

**两簇均删除较劣（未使用）的一侧，保留并巩固已在生产验证的一侧，不做合并/整合。**

删除清单：

- `internal/execute/orchestrator/orchestrator.go`、`worker.go`、`blackboard.go`、
  `blackboard_lifecycle.go` 及对应测试文件（`orchestrator_extra_test.go`、
  `worker_test.go`、`blackboard_test.go`、`blackboard_extra_test.go`）。
- `AgentRegistry.FindBestAgent`（+ `AgentStats`、`containsAll`）——仅被已删除的
  `dispatchPendingTasks` 消费，`AgentRegistry` 其余方法（`Register`/`Deregister`/
  `MarkUnreachable`/`Get`）因 `SQLiteBlackboard.SetRegistry` 做 SpawnDepth 校验的
  真实生产依赖而保留；`AgentCard`/`AgentHandle` 类型随 `AgentRegistry` 一并保留
  （由 `blackboard_lifecycle.go` 迁至 `registry.go`）。
- `internal/swarm/topology/` 整包（`swarm.go`/`swarm_router.go`/`evolver_service.go`
  及对应测试）。
- `cmd/polaris/boot_agent.go`：移除 `AgentBundle.Orch` 字段与 `orch :=
  orchestrator.NewOrchestrator(...)` 构造，`bb.SetRegistry(agentRegistry)` 改为在
  `boot_agent.go` 直接调用（此前隐藏在 `NewOrchestrator` 内部）。

### 判断依据（系统最优，不受既有 ADR 约束）

1. **簇一不是"两种同样有效的架构在竞争"，而是"同一问题的旧实现被新实现事实性淘汰"**：
   `default_worker.go`/`workflow_step_worker.go` 的注释显示，这不是我主观判断——是本仓库
   过去的开发者已经两次独立得出"中心化推送不可用，改用自订阅+CAS"的结论并落地，我只是
   把这个已经发生的架构迁移在代码层面完成收尾。中心化 push-dispatch 需要维护"谁存活、
   谁能接收"的额外状态（`workers map[string]*Worker`、`RegisterWorker`），且引入单点：
   `Orchestrator` 崩溃或未启动，全部任务停摆；自订阅 pull + CAS 认领没有这个单点，语义
   等价于 Kafka consumer group / Temporal task queue 的 competing-consumers 模式——
   多个 Worker 独立监听、谁先 CAS 成功谁执行，天然支持水平扩展与故障隔离。这在分布式
   系统里是成熟共识，不是本次讨论才发明的判断。
2. **2026 年 7 月 AI Agent 编排领域现状印证而非推翻这一判断**：检索显示 2026 年生产
   多 Agent 系统的主流做法是 Supervisor（中心化任务*分解*）与 P2P/竞争消费（去中心化
   任务*执行*）分层组合——顶层用显式 Supervisor/DAG 决定"做什么"，底层用 pull 式队列
   决定"谁来做"，去中心化路由仅在"central orchestrator 会成为瓶颈或单点故障"的大规模
   场景下才被采用。这与本仓库现状完全吻合：PatternDAG/StateGraph（结构化顶层编排）+
   SQLiteBlackboard 自订阅认领（执行层任务分派）已经是这个分层组合本身，中心化
   `Orchestrator` 是这个组合之外多余的第三层，不是缺失的一层。
3. **簇二删除的理由不是"没人用"，而是"要解决的问题在当前架构里不存在"**：
   `CapabilityRegistry`/`SwarmRouter` 解决的是"同一能力有多个候选 Agent 实例，运行时
   按负载/数量选一个"——但本仓库当前不存在这种"同构多实例"场景：`internal/swarm/agents/`
   三个角色是唯一单例，M08 §3 的编排模式走结构显式的 DAG/StateGraph，不是运行时按数量
   自动升级/降级拓扑。`TopologyEvolverService` 的 Shadow/A-B/Gradual/Commit 灰度机制
   本身是合理的工程模式（类比 M13 TrafficSplitter），但它评估的对象（"多套拓扑候选"）
   在当前系统里同样不存在——先有需要 A/B 的多个候选拓扑，才谈得上要不要灰度切换，本仓库
   目前只有一套编排层实现。
4. **是否存在合并/整合空间：论证后认为不存在**。`CapabilityRegistry` 的"负载感知选主"
   能力若未来真的需要（例如出现同构 Agent 池），正确的落点是扩展已验证生产可用的
   自订阅+CAS 模式（`DefaultTaskWorker` 已支持 `excludeTypes` 按能力类型过滤，可平行
   扩展为按负载优先级过滤），而不是复活一个从未跑通过的独立路由决策层——合并两者只会
   把"从未验证过"的代码路径继续背在"已验证"的代码路径旁边，不产生任何当前可用的新能力，
   反而增加认知负担和维护面。`TopologyEvolverService` 同理：没有多拓扑候选就没有可评估
   的对象，合并只是把状态机搬个位置，不解决"评估对象不存在"这个根本问题。这与本仓库
   CLAUDE.md R1 反模式（禁止超前抽象、臆测开发）直接吻合，无需援引即可独立成立——这不是
   "因为文档禁止所以删"，而是"投机性抽象在工程上本来就不该在需求出现前存在"。

## 后果

- **正向**：`internal/execute/orchestrator/`、`internal/swarm/topology/` 不再存在"文档
  说已接入、代码从未跑过"的认知负担；`deadcode ./cmd/polaris/...` 237→177 条；`AgentBundle`
  不再持有一个构造出来但 ListenLoop 永远不跑的 `Orch` 字段；`go build`/`go test`/
  `golangci-lint`/`internal/lint` 不变量测试全绿。
- **负向**：若未来真的出现"同一能力多实例竞争路由"或"多拓扑候选灰度评估"的真实场景，
  需要重新设计（不能简单恢复被删代码，因其从未在真实负载下验证过）；本 ADR 不提供
  面向该未来场景的接口预留。
- **反例守护**：未来若有人在 `internal/execute/orchestrator/` 或 `internal/swarm/`
  下看到类似"中心化调度器 + 独立注册表"的设计冲动，先确认是否已存在自订阅+CAS 的
  同类实现（`default_worker.go`/`workflow_step_worker.go`），避免重蹈"两套实现只有一套
  被真正驱动"的覆辙。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 合并 `CapabilityRegistry` 的负载评分能力进 `DefaultTaskWorker`/`RunStepWorkerLoop` | 论证后无当前真实场景需要"多候选择优"（当前均为单例角色/结构化 DAG），合并只是搬运从未验证过的代码，不产生可用能力；真需要时应基于已验证的自订阅+CAS 模式做增量扩展，而非整体复活 |
| 保留 `TopologyEvolverService` 作为"未来拓扑 A/B 实验"的预留基础设施 | 当前系统只有一套编排层实现，没有可供灰度评估的"多拓扑候选"对象，预留的是一个评估对象不存在的空壳状态机，违反 R1 反模式（禁止超前抽象） |
| 保留中心化 `Orchestrator`/`Worker` 作为自订阅 CAS 模式的"降级备份" | 两者是同一问题的互斥解，`Orchestrator` 从未被真实流量验证过，"备份一个未经验证的实现"不提供实际可靠性收益，反而制造"两条路径可能同时被触发产生 CAS 竞争"的新风险（`worker.go` 注释已明确指出此风险） |
| 只删代码不改文档，留给下次会话处理 | `docs/arch/M08-Multi-Agent-Orchestrator.md` §8 的"✅ 已接入"断言若不同步修正，会误导下一次基于文档做决策的会话，属于本仓库已反复出现的文档漂移模式，本次顺手修正成本低于放任其扩散 |

## 引用代码

- `internal/execute/orchestrator/default_worker.go`（`DefaultTaskWorker`，文件头注释
  明确记载中心化机制"在生产环境从未被激活"）
- `internal/gateway/server/sysadmin/workflowadmin/workflow_step_worker.go`
  （`RunStepWorkerLoop`，文件头注释同上）
- `internal/execute/orchestrator/registry.go`（`AgentRegistry`，删除 `FindBestAgent`
  后保留 `Register`/`Deregister`/`MarkUnreachable`/`Get`）
- `internal/execute/orchestrator/sqlite_blackboard.go`（`SetRegistry`，SpawnDepth
  校验的真实生产消费方）
- `cmd/polaris/boot_agent.go`（`AgentBundle`、`agentRegistry`/`blackboard.SetRegistry`
  构造顺序）
- `docs/arch/M08-Multi-Agent-Orchestrator.md`（§1.1/§3-bis/§4/§8 同步订正）
- `docs/arch/M07-Tool-Action-Layer.md`（§4.6-bis `orchestrator.Blackboard` →
  `orchestrator.SQLiteBlackboard` 引用订正）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-14 | 初稿，记录中心化 Orchestrator/Worker/内存 Blackboard 与 SwarmRouter/CapabilityRegistry/TopologyEvolverService 删除决策 |
