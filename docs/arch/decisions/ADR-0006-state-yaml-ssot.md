# ADR-0006: state.yaml SSoT + 一致性回归测试合集（含原 ADR-0012）

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M4 / 全系统级阈值 / `internal/protocol`
- **实现详情**: `docs/arch/spec/state.yaml`（文件头 §跳读索引）

## 决策一：state.yaml 作为状态机 + 全模块阈值的单一权威源（原决策）

`docs/arch/spec/state.yaml` 是跨模块共享枚举、转移表、数值阈值的单一权威源。Go 代码读取/复制时须 cite `state.yaml §N`。一致性由决策二 `spec_consistency_test` 强制守护（CI fail-closed）。

## 决策二：state.yaml ↔ Go 代码一致性回归测试设计（原 ADR-0012）

新增 `internal/protocol/spec_consistency_test.go`，CI 强制门控守护决策一 SSoT 一致性。**采用机制 A：显式断言映射**（非反射，非代码生成）——测试加载 `state.yaml` 反序列化后与 Go 枚举/常量精确对照，双向集合等值。

Tier 1（必测，CI fail-closed）：M4 状态枚举/转移、TaintLevel 五级、KillSwitch 三阶段。Tier 2（warning）：Blackboard 时间窗等数值阈值。

不用反射（无 stringer 工具链，维护成本更高）；不用代码生成（编辑 state.yaml 后必须跑生成器才能编译，IDE 路径复杂）。

## 反例守护

禁止在 Go 中硬编码新跨模块阈值——必须先改 `state.yaml`，Go 代码引用。拒绝 Go const 集中式定义（Rust FFI 侧不易读取）、Protobuf/JSON 定义（不便注释、编辑笨重）。拒绝为减少维护成本移除一致性测试——测试维护成本就是 SSoT 守护的物理体现。拒绝用宽松断言替代精确等值——会让漂移重新成为可能。

## 引用代码

`internal/protocol/interfaces_agent.go`（state.yaml §par 状态枚举镜像点，原 `constants.go` 已拆分不复存在）、`internal/protocol/spec_consistency_test.go`

> 2026-08-09 追记：重新评估触发条件——若 `spec_consistency_test.go` 的显式断言映射
> 维护成本被证明高于反射/代码生成方案（例如阈值数量级增长导致断言列表本身失控），
> 才重议机制切换；当前 Tier 1/Tier 2 分级门控策略仍成立时不重议。
