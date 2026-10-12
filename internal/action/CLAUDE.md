# internal/action — 模块规范

> 架构文档：`docs/arch/M07-Tool-Action-Layer.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

动作执行层（L1）。负责工具调用的物理执行（`codeact/` `lam/` `hook/`）、能力令牌签发与验证（`capability_token.go`）、工具使用策略建议（`tool_usage_policy.go`）。PolicyGate 五阶段在 `internal/tool`，不在本包。LLM 不在此层决策；执行结果的语义判定权属于 `agent/fsm`。

## 权力边界 [MUST]

### 拥有
- 工具调用的物理执行权（CodeAct / LAM / Hook 三条路径）
- 能力令牌（CapabilityToken）的签发与验证权
- `ToolUsagePolicy`（参数建议/适用场景）的定义权

### 禁止 [MUST NOT]
- 持有或直接调用 `store.*` / `store/repo.*`（写操作经注入接口）
- 判定执行结果的语义成功/失败（属 `agent/fsm`）
- 静默丢弃工具 Schema 校验失败：必须封装为结构化错误返回上层，不得私自降级或跳过
- 感知 taint 传播逻辑（计算属 `security/taint` + `tool/policy`；本层只透传和继承 taint）
- 直接 import `internal/agent`（防循环）
- 持有长生命周期状态（本层是无状态执行引擎）

## 消费端接口

就近声明于使用处（如 `codeact.AuditRecorder`）；新增外部依赖时在调用方包内声明接口，由 `bootstrap`/`cmd/polaris/boot_*.go` 注入，禁止直接 import 具体实现。本包不设 `provider.go`（原 ToolExecutor/PolicyStoreReader 全仓零实现零消费，已删）。

## 并发约束

CodeAct 执行路径携带独立 context 取消传播；子进程/容器须随 context 取消整组原子终止；禁止无 deadline 地阻塞等待执行结果。
