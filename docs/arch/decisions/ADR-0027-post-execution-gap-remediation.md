# ADR-0027: Gemini 执行后遗留实现缺口修复（BUG-1~4）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-25

## 背景

`local_playground/upgrade/04-remediation-final-complete.md`（Claude Opus 主导，2026-06-24）定义了 R1–R11 共 11 项缺陷的最终修复方案。Gemini 执行后经逐文件代码核验，发现 **4 项实现缺口**：接口签名与编译均通过，但逻辑是空操作或局部遗漏，属"假完成"范畴。本 ADR 为这 4 项缺口的唯一决策记录。

## 缺口台账

| 编号 | 关联 | 根因 | 严重度 |
|------|------|------|--------|
| BUG-1 | R3 LAM PolicyGate | `NewComputerUseEngine` 构造后直接 `_ =` 丢弃，provider=nil（干跑模式），且引擎从未注入 Agent；Cedar 策略预检成死代码 | P0 |
| BUG-2 | R1 CC-2 ResourceBudget | `m9Engine`、audit-chain 周期任务、`graphPipeline`、`consolidationPipeline` 四处均用 `&budget.ResourceBudget{}`（零值），三维门控（TBR/OSMemoryGuard/FeatureGate）全部跳过，实际只检查 `cog < threshold` | P1 |
| BUG-3 | R4 XR-14 SafeGo | `boot_agent.go` Blackboard→M9 事件桥仍为裸 `go func()`，panic 不会被 recover，违反 XR-14 | P1 |
| BUG-4 | R2 XR-16 taint_level 读路径 | `SemanticMem.GetEntity` SELECT 缺 `taint_level` 列，Scan 未绑定 `ent.TaintLevel`；写路径（UpsertFact/UpsertRelation）已有 only-up 语义，读路径断路 | P1 |

## 决策

### BUG-1 — LAM 引擎完整接入 Agent Kernel

**变更文件**：`internal/action/lam/lam.go`、`internal/agent/agent.go`、`internal/agent/agent_execute.go`、`cmd/polaris/boot_agent.go`

1. `ComputerUseEngine` 新增导出方法 `CheckPolicy(ctx, actionJSON []byte) error`，供 Agent Kernel 无循环依赖地调用。
2. `Agent` struct 新增字段 `lamEngine *lam.ComputerUseEngine`，配套 `SetLAMEngine(e *lam.ComputerUseEngine)` setter。
3. `interceptComputerUse`（`agent_execute.go`）在 HITL 审批前插入 Cedar 策略预检：`a.lamEngine.CheckPolicy(ctx, args)`；`lamEngine==nil` 时 nil-safe 跳过（兼容无 LAM 场景）。
4. boot 端从 `_ = lam.NewComputerUseEngine(nil, nil, ...)` 改为：
   - `provider = sb.Router`（真实 VLM provider，非 nil）
   - `executor = nil`（当前无 GUI 执行器，保留 dry-run 模式）
   - `agent.SetLAMEngine(lamEngine)` 真实注入

**架构影响**：Cedar `browser_automate/lam/{allow_net:true}` 策略预检现在对每次 computer_use/browser_use 工具调用强制生效，deny-by-default 语义完整落地。与 M07 §7.1 HITL 的关系：Cedar 预检在先（快速拒绝），HITL 审批在后（人工确认）。

### BUG-2 — ResourceBudget 四处零值注入修复

**变更文件**：`cmd/polaris/boot_agent.go`、`cmd/polaris/boot_substrate.go`、`cmd/polaris/boot_knowledge.go`、`cmd/polaris/boot_tools.go`

全部 `&budget.ResourceBudget{}`（零值）替换为 `budget.NewResourceBudget(tbr/sb.TBR, guard, gate)`，其中：
- `guard *probe.OSMemoryGuard` 和 `gate *probe.FeatureGate` 从 `sb.AutoConf`（`boot_substrate`：从局部 `autoConf`）nil-safe 提取
- `NewResourceBudget` 对三个参数均 nil-safe：某依赖 nil 时对应维度跳过，保守放行

修复前：零值结构体 `p95==0`，token 维度 `burn < 0×2.0` 恒真；`guard==nil` 内存维度跳过——退化为只检查 `cog < threshold`，三维门控变成单维。修复后：CC-2 三维门控（认知压力 + 内存降级等级 + TBR P95 倍数）在 M9/audit-chain/M10 GraphRAG/M5 Consolidation 四条路径全部生效。

### BUG-3 — m9-bb-bridge 裸 goroutine

**变更文件**：`cmd/polaris/boot_agent.go`

Blackboard→M9 TaskCompleteEvent 事件桥由裸 `go func()` 改为 `concurrent.SafeGo(ctx, "m9-bb-bridge", func(ctx context.Context){...})`，满足 XR-14：panic 由 `SafeGo` recover 并自增 `polaris_goroutine_panic_total`。

### BUG-4 — GetEntity 读路径 taint_level 断路

**变更文件**：`internal/memory/store/semantic_mem.go`

`GetEntity` SELECT 新增 `, COALESCE(taint_level, 0)`，`row.Scan` 追加 `(*int)(&ent.TaintLevel)`。修复前写路径已有 only-up 语义（`MAX(taint_level, excluded.taint_level) ON CONFLICT`），读路径未绑定导致 `ent.TaintLevel` 恒为零，XR-16 读过滤形同虚设。

## 不采纳方案

- **将 CheckPolicy 调用直接内联在 gate.go** — 职责混乱，LAM 的 GUI 动作语义判断属于 M07，不属于 M11 Cedar 引擎。
- **boot_substrate 用 SubstrateBundle 字段传递 guard/gate** — 此时 SubstrateBundle 尚未构造完毕，局部变量是正确的引用点。
- **ResourceBudget 零值兼容处理（让空壳变成"Tier0 宽松模式"）** — 违背 CC-2 设计意图；Tier0 正确行为由 `NewResourceBudget` 的 nil-safe 路径保证，而非零值 struct。

## 后果

- **正面**：R1/R2/R3/R4 四项修复从"编译通过但逻辑空操作"提升为真实生效；Cedar deny-by-default 完整覆盖 GUI 自动化路径；CC-2 三维门控在全部后台任务路径落地；XR-16 读写污点对称。
- **负面**：`Agent` struct 新增 `lam` 包依赖（`internal/action/lam`），但 lam 包无上层业务依赖，无循环依赖风险。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Accepted |
