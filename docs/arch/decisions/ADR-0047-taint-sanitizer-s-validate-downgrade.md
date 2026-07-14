# ADR-0047: taint_sanitizer 二级降级接入 S_VALIDATE，复用 ExemptionVault 而非新建存储

- **状态**: Accepted（已执行）
- **日期**: 2026-07-14
- **决策者**: MrLaoLiAI
- **相关模块**: M04（S_VALIDATE）/ M11 §2.5（taint_sanitizer）/ `internal/execute/dag` / `internal/security/token`

## 上下文

M11-Policy-Safety.md §2.5 定义了四个 Taint 降级 Sanitizer：`SanitizeBySchema`（JSON Schema 约束降一级）、`SanitizeBySummarization`（LLM 摘要，硬地板 TaintMedium）、`SanitizeByUserReview`（用户 `/approve` 确认，降至 `TaintUserReviewed`）、`SanitizeByDeterministicTransform`（纯函数转换白名单）。复核发现四者均已定义（`internal/security/taint/taint_sanitizer.go`），但 `internal/execute/dag` 的 S_VALIDATE L1-Taint 校验（`validateTaintGate`）此前只做拦截判断，从未调用任何一个 Sanitizer 尝试降级——四条设计好的降级路径在执行引擎侧全部死路。

同时排查 HITL 人工复核豁免的既有实现：`internal/security/token.ExemptionVault` 已被 M04 §3 网络出口检查（`internal/tool/tool.go` 的 `checkTaintEgress`）消费，用于铸造/校验短寿命豁免令牌（field_hash + TTL）。

## 决策

**S_VALIDATE 新增 `validateNodeTaint` 降级尝试链（先 `attemptSchemaDowngrade` 后 `attemptUserReviewDowngrade`），`SanitizeByUserReview` 复用 `ExemptionVault` 作为唯一的人工复核判定源，不新建第二套存储；`SanitizeByDeterministicTransform` 保持不接入。**

依据：

1. `attemptSchemaDowngrade` 对齐 M11 §2.5 SanitizeBySchema 的既定规则：字段仅当 JSON Schema 定义 `format`/`pattern`/`enum`/`const` 内容约束时才允许降级，裸 `{"type":"string"}` 不降级（`schemaNodeIsStrict` 递归校验）；`attemptUserReviewDowngrade` 仅在降级后仍 `>= TaintHigh` 时触发，避免不必要的人工复核查询开销。
2. `TaintReviewChecker` 接口（`IsReviewed(agentID, content) bool`，定义于消费方 `internal/protocol/dag_validation.go`，遵循 HE-3 接口在调用方定义）由 `ExemptionVault.IsReviewed` 实现（内部复用既有 `Lookup(agentID).Valid(content)`）。两个消费点（M04 网络出口检查 + S_VALIDATE 降级判定）共享同一存储，避免"人工已批准"这一状态出现两份互不同步的真相源——若各建一套，用户 `/approve` 一次只对其中一条路径生效会是更严重的可用性缺陷。
3. `SanitizeByDeterministicTransform` 排查全仓无一处真实生产调用点，且 M11 inv_M11_02（TaintLevel 只升不降，`internal/security/taint/envelope.go`）要求降级路径必须有明确、可审计的触发时机；纯函数转换（base64/hex/gzip/SHA-256 等）在当前工具调用链路中没有一个"转换后语义确定安全"的判定点——强行接入会是"看起来在跑但没有真实触发条件"的假接线，故保持不接入，非遗漏。
4. `internal/memory/consolidation/consolidation_summary.go` 顺带修复：`StoreDocument` 此前硬编码 `types.TaintNone`，现按事件真实最高 Taint 经 `SanitizeBySummarization` 降级后写入，四个 Sanitizer 中 SanitizeBySummarization 从"已定义未调用"变为真实生产路径。

## 后果

- **正向**: 四个 Sanitizer 中三个（Schema/UserReview/Summarization）从"文档定义、代码零调用"变为真实生产路径；HITL 人工复核状态单一存储来源，避免状态分裂。
- **负向**: `SanitizeByDeterministicTransform` 仍是唯一保持未接入的 Sanitizer，未来若出现真实需求（如工具参数需要标准化编码后才可信）需要重新设计触发点，而非直接调用。
- **反例守护**: 未来如有人提议"给 SanitizeByUserReview 单独建一张复核记录表"，引用本 ADR 第 2 条拒绝——`ExemptionVault` 已是跨两个消费点的唯一豁免真相源。如有人提议"顺手把 SanitizeByDeterministicTransform 也接上，反正函数都写好了"，引用第 3 条拒绝——没有真实触发条件的接线是假接线，比不接线更危险（给人已受保护的错觉）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 为 S_VALIDATE 复核判定新建独立存储表 | 与 M04 网络出口检查已用的 `ExemptionVault` 语义完全重复，会产生"用户 approve 一次但只有一条路径生效"的分裂状态 |
| 强行给 `SanitizeByDeterministicTransform` 接一个默认触发点（如所有 write_local 工具调用前都跑一遍） | 转换后的"安全性"取决于具体工具语义，一刀切接入是伪装成安全边界的形式主义，违反 HE-2 可验证执行 |

## 引用代码

- `internal/execute/dag/taint_downgrade.go`（`validateNodeTaint`/`attemptSchemaDowngrade`/`attemptUserReviewDowngrade`/`schemaNodeIsStrict`）
- `internal/protocol/dag_validation.go`（`TaintReviewChecker` 接口 + `DAGValidationContext.ReviewChecker`）
- `internal/security/token/exemption_vault.go`（`ExemptionVault.IsReviewed`）
- `internal/memory/consolidation/consolidation_summary.go`（`summaryTaintLevel`，SanitizeBySummarization 真实生产路径）
- `internal/security/taint/taint_sanitizer.go`（四个 Sanitizer 定义）
- `docs/arch/M11-Policy-Safety.md §2.5`（Sanitizer 设计）、`docs/arch/M04-Agent-Kernel.md §3`（S_VALIDATE TaintGate）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-14 | 初稿，记录 S_VALIDATE 二级降级接线与 ExemptionVault 复用决策 |
