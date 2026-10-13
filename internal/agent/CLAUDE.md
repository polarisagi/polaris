# internal/agent — 模块规范

> 架构文档：`docs/arch/M04-Agent-Kernel.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

Agent 核心状态机（L1）：驱动 FSM 状态转移、思考循环控制流、上下文感知（MemoryFacade 检索）。LLM 是协处理器，Go FSM 是主控制流，禁止 `while True: call LLM`。

DAG 执行（拓扑调度/并发/Saga 补偿）与 S_VALIDATE 四层校验在 `internal/execute/dag`（ADR-0046，见 `internal/execute/CLAUDE.md`）：agent 只经 `provider.go` 声明的 `DAGRunner`/`DAGValidator` 消费端接口驱动，FSM 仍是决定"何时进入 S_EXECUTE/S_VALIDATE"的唯一控制流。子包：`fsm/`（状态机与转移）、`context/`（感知与召回上下文）、`schemavalidate/`。

## 权力边界 [MUST]

### 拥有
- FSM 状态转移的唯一触发权
- 执行结果语义成功/失败的判定权（action 层只执行）
- AgentContext（SystemPrompt / SkillSet / ToolSet）的组装权
- Budget（Token_Burn_Rate）的监控与熔断触发权

### 禁止 [MUST NOT]
- 直接 import `internal/action/codeact`、`internal/action/lam`、`internal/extension/skill`、`internal/execute/dag` 等具体实现包（经 `provider.go` 接口）。**唯一例外**：`agent.go` 的 `NewAgentWithDefaults`（测试/开发默认构造器）import `internal/execute/dag` 注入默认无状态 Runner/Validator；生产路径由 `cmd/polaris/boot_agent.go` 显式注入覆盖，例外范围严格限于该函数
- 直接 import `internal/memory` 具体实现（经 MemoryFacade 接口）
- 直接调用 LLM API（经注入的 `protocol.Provider`）
- 在状态机转移函数中执行阻塞 I/O（转移必须非阻塞，I/O 交给 Effect 异步处理）
- 将 LLM 幻觉/失败响应静默视为成功（必须产生对应错误状态转移）
- 单次运行无限循环（必须有 `MaxSteps` 上限，由 Budget 控制）

## 消费端接口声明位置

`internal/agent/provider.go`：`CodeActEngine`、`ScriptSkillCache`、`LAMPolicyChecker`、`DAGRunner`、`DAGValidator`、`HandoffPoster`、`SkillForker`；世界模型消费端接口 `WorldModel` 声明于 `agent.go`。新增外部依赖先在此声明，由 `cmd/polaris/boot_*.go` 注入。

## FSM 并发约束

- 单个 Agent 实例的 FSM 转移必须串行（禁止并发触发 Dispatch）；多 Agent 并发由 `swarm` 管理
- AgentContext 缓存 TTL ≤ 60s（防止 Prompt 版本晋升后沿用旧版本）
- 转移产生的 Effect 经 `executeEffect`（`agent_execute_effect.go`）在转移外异步执行；确定性 Effect 走 `executeDeterministicEffect`

## 结构体分组（SecurityBundle）

`Agent` 字段多，相关组件按领域内聚分组。`SecurityBundle`（`agent.go`）封装 `PolicyGate`、`TaintReviewChecker`、`TaintAuditor`、`TokenVault`、`PIIDetector`、`PIIDesensitizer`、`AnomalyFilter`，不改变既有依赖注入逻辑。
