# ADR-0014: 对抗审查 GitHub Action（执行带 3 落地）

- **状态**: Accepted（已执行完毕）| **日期**: 2026-05-16 | **模块**: CI / `.github/workflows/`

## 决策

新建 `.github/workflows/constitutional-review.yml` + `scripts/constitutional_review.sh`，PR 触发独立 Anthropic API 调用做宪法违例审查，覆盖带1(lint)/带2(golden) 机械不可检的语义级反模式（R1.4/R1.8/R1.9/R1.10 等）。

- 审查模型与开发模型**不可同型号**（防"被告判自己"共谋），默认 `claude-sonnet-4-6`（开发用 `claude-opus-4-8`）。
- Reviewer 仅看 diff + `00-Constitution.md`，不看其他上下文（防叙事 framing 污染）。
- 输出严格机器化（"R<编号> | 文件:行 | 说明" 或 "NONE"），禁建议/表扬/推理——防信号稀释。
- **warning-only，不阻断 CI**——reviewer 是 LLM 可能误报，硬阻断会让团队学会绕过（`//nolint`-style hack）。

## 反例守护

拒绝同一开发者会话做 self-review（自我证伪困境）。拒绝 reviewer 给详细 fix 建议（信号稀释）。拒绝 fail CI on any violation（误报致团队学会绕过）。

## 引用代码

`.github/workflows/constitutional-review.yml`、`scripts/constitutional_review.sh`
