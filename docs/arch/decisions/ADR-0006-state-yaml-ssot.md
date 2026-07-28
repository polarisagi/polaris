# ADR-0006: state.yaml 作为状态机 + 全模块阈值的单一权威源（SSoT）

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M4 / 全系统级阈值
- **实现详情**: `docs/arch/spec/state.yaml`（文件头 §跳读索引）

## 决策

`docs/arch/spec/state.yaml` 是跨模块共享枚举、转移表、数值阈值的单一权威源。Go 代码读取/复制时须 cite `state.yaml §N`。一致性由 [ADR-0012](./ADR-0012-spec-consistency-test.md) `spec_consistency_test` 强制守护（CI fail-closed）。

## 反例守护

禁止在 Go 中硬编码新跨模块阈值——必须先改 `state.yaml`，Go 代码引用。拒绝 Go const 集中式定义（Rust FFI 侧不易读取）、Protobuf/JSON 定义（不便注释、编辑笨重）。

## 引用代码

`internal/protocol/constants.go`
