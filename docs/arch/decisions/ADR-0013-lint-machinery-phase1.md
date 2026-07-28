# ADR-0013: lint 机械化 Phase 1（执行带 1 落地）

- **状态**: Accepted（已执行完毕）| **日期**: 2026-05-16 | **模块**: 全 pkg / `.golangci.yml` / CI

## 决策

Phase 1 启用四个低成本高 ROI linter 机械化守护宪法规则：`depguard`（B1 层依赖方向 + R6 隔离）、`errorlint`（R1.2 错误包装）、`nestif`（R7 嵌套深度≤3，起始放宽后续收紧）、`gocyclo`（R7 圈复杂度≤15）。`funlen`/`wrapcheck`/`gochecknoglobals` 推迟到 Phase 2（各自需要先做既有违规盘点/白名单设计）。

**不采用 baseline 模式**锁定既有违规——baseline 等于规则空转，违背执行带 1 哲学；既有违规按优先级修复或显式 `//nolint` + 关联 ADR。

## 反例守护

拒绝加 baseline 锁定既有违规。拒绝无 ADR 佐证的 `//nolint:depguard` 跨层 import。

## 修订记录

2026-07-04：`funlen` 判定不采用——与已启用的 `gocyclo` 高度冗余，且 Go 错误处理惯例拉长物理行数但不代表真实复杂度，误报率偏高；复杂度治理职责收敛到 `gocyclo` 一家。

## 引用代码

`.golangci.yml`、`.github/workflows/ci.yml`
