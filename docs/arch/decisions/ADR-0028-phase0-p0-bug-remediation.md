# ADR-0028: Phase 0 P0 Bug 修复（Scheduler 防抖 / FSM SafeGo / SurpriseCalculator 接入）

**状态**: 已接受 (Accepted)
**日期**: 2026-06-25

## 背景

代码核验发现 4 项影响系统正确性的 P0 缺陷，均属"已有基础设施但未接线"类型，改动量极小（每项 ≤ 10 行）。

## 缺陷台账

| 编号 | 文件 | 根因 | 影响 |
|------|------|------|------|
| BUG-A | `automation/queue.go:scanAndDispatch` | `gate` 字段注入后从未调用 `BackgroundPermit()`，CC-2 门控对 Scheduler 无效 | 高认知负载期间后台任务仍全量触发，与 CC-2 设计意图相悖 |
| BUG-B | `swarm/orchestrator/worker.go:107` | 裸 `go func()` 运行 FSM 主循环，panic 时 `close(done)` 不执行 | Task 永久卡在 Executing 状态，Blackboard 死锁 |
| BUG-C | `security/policy/gate.go:447` | Cedar evaluate 裸 goroutine 无 recover | panic → goroutine 泄漏；10ms timeout fail-closed 可接受，但泄漏持续积累 |
| BUG-D | `agent/agent.go:430` | `ComputeBasic(nil, toolSeq)` embedding 永远为 nil，cosine 分量恒 0 | SurpriseIndex 退化为纯 Jaccard，三档路由质量严重下降；`learning/surprise/SurpriseCalculator` 完整实现闲置 |

## 决策

### BUG-A — 内稳态防抖（Allostatic Suppression）

**方案**：`scanAndDispatch` 在 CAS 写入 `"running"` 前调用 `s.gate.BackgroundPermit(taskPriority)`。负载过高时，原地累积 `storedTask.MissedExecutions` 计数并写回 KV store（不推进为 running），跳过本轮；焦点解除时仅触发一次补偿执行并清零计数。

**不采纳**：直接 `continue` 跳过（会导致任务永久饥饿）；写入 Dead Letter Queue（引入额外内存压力，与 Allostatic Suppression 的"零内存"目标相悖）。

**`storedTask` 结构体新增字段**：`MissedExecutions int`，JSON 序列化透明持久化到 KV store，无需 DDL 变更。

### BUG-B — FSM 主循环 SafeGo 包装

**方案**：用 `concurrent.SafeGo(ctx, "worker.kernel.run", fn)` 替换裸 goroutine，`defer close(done)` 移入 SafeGo 内部，panic 时 defer 依然执行，防止 `tryClaimAndExecute` 死锁。

**依据**：`pkg/concurrent.SafeGo` 已完整实现（ADR-0027 BUG-3 同类问题），此处属未迁移遗漏。

### BUG-C — Cedar evaluate SafeGo 包装

**方案**：与 BUG-B 同策，用 `concurrent.SafeGo` 包装 Cedar 评估 goroutine。`ch <- result{...}` 在 SafeGo 内执行，panic 时 ch 不接收，调用方等到 `EvalTimeout` 后 fail-closed（已有 timeout 保护，行为不变）。

### BUG-D — SurpriseCalculator 接入 Agent 主路径

**方案**：
1. `Agent` struct 新增 `surpriseCalc SurpriseReader`（consumer-side 接口，防 L1→L2 包循环）。
2. `populateSessionContext` 中，当 `surpriseCalc != nil` 时调用 `Submit(CalcRequest)` + `CurrentSurprise()`；结果同步写入 `metrics.GlobalSurpriseIndex().SetLastValue(v)`，保持 `SelectThinkingMode`（transitions.go）读值一致。
3. `boot_agent.go` 构造 `NewSurpriseCalculator(nil)` 并注入。

**为什么用 consumer-side 接口而非直接依赖 `*surprise.SurpriseCalculator`**：`internal/agent/`（L1）不应直接引用 `internal/learning/`（L2），与已有的 `ToolCallRecorder`、`BlindZoneDetector` 模式一致。

**不采纳**：在 L0 metrics 包实现完整 SurpriseCalculator（破坏 L0 纯净性）；在 FSM S_PERCEIVE 之外新增 SLM 预路由（引入额外延迟和模型依赖，Gap 根因在计算质量而非路由层数）。

## 后果

- **正面**：CC-2 门控在 Scheduler 真正生效；FSM panic 不再死锁 Task；Cedar goroutine 泄漏消除；SurpriseIndex 恢复三分量计算，三档路由质量提升。
- **负面**：`Agent` struct 新增一个接口字段；`metrics.SurpriseIndex` 新增 `SetLastValue` 方法（接口扩展可向后兼容）。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Accepted |
