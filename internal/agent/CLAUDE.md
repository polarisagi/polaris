# internal/agent — 模块规范

> 对应架构文档：`docs/arch/M04-Agent-Kernel.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

Agent 核心状态机（Arch-L2）。驱动 FSM 状态转移、思考循环（DAG 执行）、
上下文感知（MemoryFacade 检索）。LLM 是协处理器，Go FSM 是主控制流。
禁止 `while True: call LLM` 范式。

## 权力边界 [MUST]

### 拥有
- FSM 状态转移的唯一触发权（状态机是 agent 核心）
- 执行结果的语义成功/失败判定权（action 层只执行，agent/fsm 做判定）
- AgentContext（SystemPrompt / SkillSet / ToolSet）的组装权
- Budget（Token_Burn_Rate）的监控与熔断触发权

### 禁止 [MUST NOT]
- **[MUST NOT]** 直接 import `internal/action/codeact`、`internal/action/lam`、
  `internal/extension/skill` 等具体实现包（必须通过 `agent/provider.go` 中声明的接口）
- **[MUST NOT]** 直接 import `internal/memory` 具体实现（通过 MemoryFacade 接口）
- **[MUST NOT]** 直接调用 LLM API（必须通过注入的 `protocol.Provider` 接口）
- **[MUST NOT]** 在状态机转移函数中执行阻塞 I/O（FSM 转移必须是非阻塞的，
  I/O 交给 Effect 异步处理）
- **[MUST NOT]** 将 LLM 幻觉/失败响应静默视为成功（必须产生对应的错误状态转移）
- **[MUST NOT]** 在单次 Agent 运行中无限循环（必须有 MaxSteps 上限，由 Budget 控制）

## 消费端接口声明位置

`internal/agent/provider.go` — 已声明：CodeActEngine、ScriptSkillCache、
LAMPolicyChecker、WorldModelUpdater。
新增外部依赖时先在此文件声明接口，由 `bootstrap` 注入，禁止直接 import 具体实现。

## FSM 并发约束

单个 Agent 实例的 FSM 转移必须串行（禁止并发触发 Dispatch）。
多 Agent 并发由 `swarm` 层管理，不在 agent 包内处理。
AgentContext 缓存 TTL 不超过 60s（防止 Prompt 版本晋升后继续使用旧版本）。
