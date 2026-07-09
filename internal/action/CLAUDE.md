# internal/action — 模块规范

> 对应架构文档：`docs/arch/M07-Tool-Action-Layer.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

动作执行层（Arch-L2）。负责工具调用的物理执行（CodeAct / LAM / Hook）、
能力令牌签发与验证、工具使用策略（PolicyGate 五阶段）。
LLM 不在此层做决策；执行结果的语义判定权属于 `agent/fsm`。

## 权力边界 [MUST]

### 拥有
- 工具调用的物理执行权（CodeAct / LAM / Hook 三条路径）
- 能力令牌（CapabilityToken）的签发与验证权
- 工具使用策略的持久化读写权（PolicyStoreReader 接口）

### 禁止 [MUST NOT]
- **[MUST NOT]** 持有或直接调用 `store.*` / `store/repo.*`（写操作必须通过注入的接口）
- **[MUST NOT]** 判定执行结果的语义成功/失败（判定权属于 `agent/fsm`）
- **[MUST NOT]** 静默丢弃工具 Schema 校验失败。校验失败必须封装为结构化错误返回上层，
  不允许私自将任务降级或跳过
- **[MUST NOT]** 感知 taint 传播逻辑（taint 计算属于 `security/taint` + `tool/policy` 层，
  action 层只透传和继承 taint）
- **[MUST NOT]** 直接 import `internal/agent`（防循环）
- **[MUST NOT]** 持有长生命周期状态（action 层是无状态执行引擎）

## 消费端接口声明位置

`internal/action/provider.go` — 声明 action 包对外部能力的消费端接口（ToolExecutor、PolicyStoreReader）。
新增外部依赖时，必须先在 `provider.go` 声明接口，由 `bootstrap` 注入，禁止直接 import 具体实现。

## 并发约束

CodeAct 执行路径必须携带独立的 context 取消传播。
子进程/容器必须挂载进程组（PGroup），确保随 context 取消而原子终止。
禁止无限期阻塞等待执行结果（必须有 deadline）。
