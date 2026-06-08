# ADR-0012: state.yaml ↔ Go 代码一致性回归测试设计

- **状态**: Accepted（**已执行完毕** 2026-05-16）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: 全系统级 / `internal/protocol` / `docs/arch/spec/state.yaml`

## 上下文

ADR-0006 决策 `state.yaml` 作为 SSoT，但无 CI 守护一致性 → SSoT 是纸面约束。

漂移模式：
- 改 Go 常量但忘改 state.yaml → 设计文档过时
- 改 state.yaml 但忘改 Go 常量 → 运行时行为偏离规约
- 双侧并行修改但语义不一致 → 难肉眼发现

ADR-0006 反例守护"必须先改 state.yaml"无机械守护时等同纸面（与 ADR-0002 R1.4 纸面化同类）。

## 决策

**新增 `internal/protocol/spec_consistency_test.go`，作为 CI 强制门控的一致性回归测试。**

### 测试范围分级

**Tier 1（必测，CI fail-closed）**:

| 项 | state.yaml 路径 | Go 侧 |
|----|----------------|------|
| M4 状态枚举 | `par.states` | `protocol.AgentState` |
| M4 状态转移 | `par.transitions` | `pkg/cognition/kernel/state_machine.go` Transitions |
| TaintLevel 五级 | `taint` | `pkg/substrate/taint.go` |
| KillSwitch 三阶段 | `kill_switch.stages` | `pkg/substrate/killswitch.go` |

**Tier 2（warning，不阻断）**: Blackboard 时间窗 / Memory consolidation / Tool RateLimiter QPS 等数值阈值。
**Tier 3（远期）**: M9 / M10 / M12 子项目阈值。

### 测试机制

**采用机制 A：显式断言映射**（非反射，非代码生成）。

- 测试加载 `state.yaml` → 反序列化到 stateSpec struct → 用 expected map 与 Go 枚举/常量精确对照
- 双向集合等值：state.yaml 字段数 ≠ Go 枚举数 → fail
- 单点位置：`internal/protocol/spec_consistency_test.go`（与 yaml meta `go_package` 对齐，避免分散加载）

**为何不用反射 (B)**：项目无 stringer 工具链；反射枚举受限；维护成本反高于显式映射。
**为何不用代码生成 (C)**：Go 可读性受损；编辑 state.yaml 必须跑生成器才能编译，CI/IDE 路径复杂。

### CI 集成

`.github/workflows/ci.yml` test job 内分类步骤：`go test -run "^TestSpec" ./internal/protocol/... -v`。Tier 1 失败 → PR 阻断。Tier 2 用 `t.Logf` warning 不阻断。

## 后果

- **正向**: ADR-0006 SSoT 决策从纸面进入物理执行；漂移在 PR 提交时立即暴露
- **负向**: 新增阈值需同时改 state.yaml + Go + 测试映射三处——但**这正是 SSoT 本意**，刻意增加跨源同步成本以保一致
- **反例守护**:
  - 未来如有人提议"为减少测试维护成本去掉 spec_consistency_test" → 本 ADR 拒绝；测试维护成本就是 SSoT 守护的物理体现
  - 未来如有人提议"用宽松断言替代精确等值" → 本 ADR 拒绝；宽松断言让漂移再次成为可能

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 不实施一致性测试，依赖 PR review | review 不可机械化；与执行带 1 lint 自动化原则矛盾 |
| 完全反射机制 | 当前无 stringer 工具链；维护成本反而高 |
| 单边代码生成（state.yaml → Go consts） | Go 可读性受损；编辑 state.yaml 后必须跑代码生成器才能编译 |
| 仅测枚举不测阈值 | 阈值漂移是更高发的实际问题（如 KillSwitch 倍数被悄悄改） |

## 引用代码

- `docs/arch/spec/state.yaml`（权威源）
- `internal/protocol/spec_consistency_test.go`（实施落地）
- 涉及对齐的 Go 文件：`internal/protocol/interfaces.go`（AgentState）/ `pkg/cognition/kernel/state_machine.go`（Transitions）/ `pkg/substrate/taint.go` / `pkg/substrate/killswitch.go`

## 关联 ADR

- [ADR-0006](./ADR-0006-state-yaml-ssot.md): SSoT 决策，本 ADR 是其守护实施
- [ADR-0002](./ADR-0002-skill-registry-consolidation.md): 同类"消除规则纸面化"模式

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted；代码执行待 B 阶段批准 |
| 2026-05-16 | B 阶段完成：4 Tier-1 测试落地（TaintLevels / ParStates / ParTransitionsReferenceKnownStates / KillSwitchStages）；CI step 集成；副发现 yaml `kill_switch.stages.trigger` 是人类可读文本非数值字段 → BurnRate 倍数 / KillSwitch 失败计数当前未在 yaml 作机器可读字段，Tier-2 阈值精确等值守护待先重构 yaml（独立 ADR） |
