# ADR-0013: CI 质量门禁合集（lint 机械化 Phase 1 + 对抗审查 GitHub Action，含原 ADR-0014）

- **状态**: Accepted（已执行完毕）| **日期**: 2026-05-16 | **模块**: 全 pkg / `.golangci.yml` / CI / `.github/workflows/`

## 决策一：lint 机械化 Phase 1（执行带 1 落地，原决策）

Phase 1 启用四个低成本高 ROI linter 机械化守护宪法规则：`depguard`（B1 层依赖方向 + R6 隔离）、`errorlint`（R1.2 错误包装）、`nestif`（R7 嵌套深度≤3，起始放宽后续收紧）、`gocyclo`（R7 圈复杂度≤15）。`funlen`/`wrapcheck`/`gochecknoglobals` 推迟到 Phase 2（各自需要先做既有违规盘点/白名单设计）。

**不采用 baseline 模式**锁定既有违规——baseline 等于规则空转，违背执行带 1 哲学；既有违规按优先级修复或显式 `//nolint` + 关联 ADR。

## 决策二：对抗审查 GitHub Action（原 ADR-0014，执行带 3 落地）

新建 `.github/workflows/constitutional-review.yml` + `scripts/constitutional_review.sh`，PR 触发独立 Anthropic API 调用做宪法违例审查，覆盖带1(lint)/带2(golden) 机械不可检的语义级反模式（R1.4/R1.8/R1.9/R1.10 等）。

- 审查模型与开发模型**不可同型号**（防"被告判自己"共谋），默认 `claude-sonnet-4-6`（开发用 `claude-opus-4-8`）。
- Reviewer 仅看 diff + `00-Constitution.md`，不看其他上下文（防叙事 framing 污染）。
- 输出严格机器化（"R<编号> | 文件:行 | 说明" 或 "NONE"），禁建议/表扬/推理——防信号稀释。
- **warning-only，不阻断 CI**——reviewer 是 LLM 可能误报，硬阻断会让团队学会绕过（`//nolint`-style hack）。

## 反例守护

拒绝加 baseline 锁定既有违规。拒绝无 ADR 佐证的 `//nolint:depguard` 跨层 import。拒绝同一开发者会话做 self-review（自我证伪困境）。拒绝 reviewer 给详细 fix 建议（信号稀释）。拒绝 fail CI on any violation（误报致团队学会绕过）。

## 修订记录

2026-07-04：`funlen` 判定不采用——与已启用的 `gocyclo` 高度冗余，且 Go 错误处理惯例拉长物理行数但不代表真实复杂度，误报率偏高；复杂度治理职责收敛到 `gocyclo` 一家。

## 引用代码

`.golangci.yml`、`.github/workflows/ci.yml`、`.github/workflows/constitutional-review.yml`、`scripts/constitutional_review.sh`
