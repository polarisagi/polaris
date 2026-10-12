# ADR-0006: state.yaml SSoT + 一致性回归测试

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0012 ｜ 模块：M4 / 全系统阈值 / `internal/protocol`
> 实现详情：`docs/arch/spec/state.yaml`（文件头 §跳读索引）

## 决策一：state.yaml 为状态机 + 全模块阈值的单一权威源

`docs/arch/spec/state.yaml` 是跨模块枚举、转移表、数值阈值的唯一权威源。Go 代码读取/复制时须 cite `state.yaml §N`；一致性由决策二强制（CI fail-closed）。

## 决策二：显式断言映射的一致性回归测试（原 ADR-0012）

`internal/protocol/spec_consistency_test.go` 加载 `state.yaml` 反序列化后与 Go 枚举/常量精确对照，双向集合等值（机制 A：显式断言，非反射、非代码生成）。
- Tier 1（必测，fail-closed）：M4 状态枚举/转移、TaintLevel 五级、KillSwitch 三阶段
- Tier 2（warning）：Blackboard 时间窗等数值阈值

## 反例守护 / 已驳方案

- Go 中硬编码新跨模块阈值 — 必须先改 `state.yaml`
- Go const 集中定义 — Rust FFI 侧不易读取
- Protobuf/JSON 定义 — 不便注释
- 反射 — 无 stringer 工具链，维护成本更高；代码生成 — 改 yaml 后须跑生成器才能编译
- 为省维护成本移除一致性测试 / 用宽松断言替代精确等值 — 让漂移重新成为可能

## 重新评估触发条件

显式断言映射维护成本高于反射/代码生成（如阈值数量级增长致断言列表失控），才重议机制切换。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版（决策一、二） |
| 2026-07-28 | 合并原 ADR-0012 |

## 引用代码

`internal/protocol/interfaces_agent.go`（state.yaml §par 状态枚举镜像点）、`internal/protocol/spec_consistency_test.go`
