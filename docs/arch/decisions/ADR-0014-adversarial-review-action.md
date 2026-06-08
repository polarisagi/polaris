# ADR-0014: 对抗审查 GitHub Action（执行带 3 落地）

- **状态**: Accepted（**已执行完毕** 2026-05-16）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: CI / `.github/workflows/` / `scripts/`

## 上下文

执行带 3 目标：**用独立、与开发者会话隔离的 AI agent，对照宪法对 PR 做违例审查**。设计哲学：
- "AI 自查 = 让被告判自己" → 写代码 AI 与审代码 AI 必须物理隔离
- 模型族区隔降低共谋（开发用 Opus，审查用 Sonnet）
- Reviewer 仅看 Diff + 00-Constitution，不看其他上下文 → 防被开发者"叙事 framing"污染
- 输出严格机器化（"R<编号> | 文件:行 | 说明" 或 "NONE"）→ 防 reviewer 假装 helpful 给建议反而稀释信号

带 1 (lint) + 带 2 (golden) 覆盖**机械可检测**反模式；带 3 覆盖**语义级**反模式（R1.4 / R1.8 / R1.9 / R1.10 等 lint 不可解者）。

## 决策

**新建 `.github/workflows/constitutional-review.yml` + `scripts/constitutional_review.sh`，PR 触发独立 Anthropic API 调用。**

| 决策项 | 内容 |
|--------|------|
| 触发 | `pull_request: [opened, synchronize, reopened]` to `main` |
| 必备 secret | `ANTHROPIC_API_KEY`；未设置时 no-op + warning（防 fork PR 误阻断） |
| 模型 | 默认 `claude-sonnet-4-6`（开发 Opus 4.7），可经 `REVIEWER_MODEL` 覆盖；**不可与开发者用模型同型号** |
| 输入 | 00-Constitution.md 全文（system prompt）+ PR diff（user message，>100KB 截断头部） |
| 提示词约束 | 仅输出违例或 "NONE"；禁建议/表扬/推理；ADR 已豁免（如 ADR-0001）跳过；不假设；优先级 R1>R7>B1>R5 |
| 决策 | warning-only（不阻断 CI），人类 review 兜底；reviewer 是 LLM 可能误报，硬阻断会让团队凭关键词钝化 |

### 安全考虑

- diff 拼入 prompt → prompt injection 风险低（reviewer 任务是检测违例，最坏情况漏报由人类兜底）
- secret 通过 `env:` 传入，不在 `run:` 命令中明文展开
- `gh comment` 失败时仅 warning（避免 review 失败级联）

### 演化路径

| 阶段 | 内容 |
|------|------|
| Phase 1（本 ADR） | 单 reviewer，warning-only，输出 PR comment |
| 远期 | 多 reviewer 投票（不同模型族）；高置信违例自动阻断；reviewer 自我评测（红队 prompt 集） |

## 后果

- **正向**: 执行带 3 从设计文档进入物理实现；R1.4/1.8/1.9/1.10 等 lint 不可解的语义反模式有自动检测路径
- **负向**: 每 PR 触发一次 API 调用（成本 ~$0.05~0.5/PR by Sonnet 4.6 token rate）；reviewer 误报概率非零（warning-only 缓解）
- **反例守护**:
  - "为省成本改用同一开发者 LLM 会话作 review" → 拒绝；同会话审查 = 自我证伪
  - "reviewer 给详细 fix 建议" → 拒绝；信号稀释
  - "reviewer 报告违例就 fail CI" → 拒绝；误报会让团队设法绕过 (`//nolint`-style hack)；warning + 人类 ack 更可持续

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 同一开发者会话做 self-review | 自我证伪困境，违反执行带 3 核心 |
| 用 anthropic/claude-code-action 官方 Action | 绑定外部 Action 演进；自维护 bash + curl 透明便于审计与定制 |
| Reviewer 给详细 fix 建议 | 信号稀释；提示词应严格 |
| 多 reviewer 并行 + 投票 | Phase 1 不必要复杂；先 1 reviewer 看效果 |
| Fail CI on any violation | 误报让团队学会绕过；warning + 人类 ack 更稳 |

## 引用代码

- `.github/workflows/constitutional-review.yml`（实施落地）
- `scripts/constitutional_review.sh`（128 行 bash + curl + jq + gh pr comment）
- `docs/specs/00-Constitution.md`（reviewer 上下文输入）
- `docs/specs/05-Coding-Workflow.md W6.4`（PR review 与 AI reviewer 并行）

## 关联 ADR

- [ADR-0012](./ADR-0012-spec-consistency-test.md): 同执行带框架（带 2）
- [ADR-0013](./ADR-0013-lint-machinery-phase1.md): 同执行带框架（带 1）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted；代码执行待 B 阶段批准 |
| 2026-05-16 | B 阶段完成：workflow + script 落地；secret 通过 env 传入；100KB diff 截断；warning-only `exit 0`；实际触发待用户配 secret + 真实 PR |
