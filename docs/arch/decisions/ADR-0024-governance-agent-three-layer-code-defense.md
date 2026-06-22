# ADR-0024: GovernanceAgent 代码安全三层防线（AST + 正则 + 单次 ThinkingMax LLM）

- **状态**: Accepted
- **日期**: 2026-06-13
- **决策者**: 架构组
- **相关模块**: M07 (Tool/Action), M11 (Policy/Safety), internal/swarm/agents

## 上下文

LLM 生成代码（CodeAct / LLMGenerated Wasm）在进入沙箱前需经代码安全审查。原有 `SecurityAuditAgent` 实现采用三路 goroutine 并发（`perspectives = ["security", "integrity", "reliability"]`）+ 各调一次 LLM + `mergeVotes()` 投票聚合的"ensemble"架构。该方案存在以下问题：

1. 三路 LLM 调用成本为单次 3 倍，但聚合收益有限——三路调用使用相同 prompt 基础，输出高度相关，多数情况下 3 票一致
2. `LLMInferFunc` 类型定义缺少 `opts ...protocol.InferOption` 参数，无法传递 `ThinkingMax` 等高级选项，导致 SecurityAudit 无法使用推理增强
3. Layer 0（静态 AST/import 扫描）从未实现，危险包导入（`os/exec`/`syscall`/`unsafe`）仅靠正则捕获，存在绕过风险

## 决策

**建立三层串行防线，将审查从"三路 LLM 投票"改为"同步静态卡口 + 单次 ThinkingMax LLM 深度审计"。**

| 层 | 性质 | 机制 | 失败结果 |
|----|------|------|---------|
| Layer 0 | 同步卡口，<5ms | Go AST 解析 + import 路径白名单扫描，拦截危险包导入 | 硬拒绝，不进 L1 |
| Layer 1 | 同步卡口，<1ms | 正则规则集（`code_validator.go`），邻近匹配距离 ≤200 字节防跨行误报（废弃 `(?s)` 全文匹配） | 硬拒绝，不进 L2 |
| Layer 2 | 异步，独立 goroutine | 单次 LLM 调用 + `ThinkingMax`，`SecurityAuditAgent` 出具结构化安全报告 | 高风险拒绝执行 |

Layer 0/1 为同步物理卡口，任一失败直接阻断代码进入沙箱。Layer 2 在 L0/L1 通过后并发启动，审计结论必须在沙箱执行前到达（超时 fail-closed）。

同步更新 `LLMInferFunc` 签名为 `func(ctx context.Context, prompt string, opts ...protocol.InferOption) (string, error)`，向后兼容已有调用方。

## 后果

- **正向**: 三路 LLM 调用降为一路，成本降 67%；ThinkingMax 的推理深度显著优于三路无 thinking 调用投票；Layer 0 AST 卡口从根本上阻断危险 import，不依赖正则的字符串匹配
- **负向**: Layer 2 为单点——若 ThinkingMax 调用超时，当前策略为 fail-closed（拒绝执行），可能影响合法代码的执行流程
- **反例守护**: 未来若有人提议"恢复多视角 ensemble 投票"，引用本 ADR 拒绝——单次 ThinkingMax 的推理质量已优于三次无 thinking 投票聚合，且成本更低；若需提升准确率，应优化 Layer 0/1 规则而非增加 LLM 调用次数

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 三路 LLM ensemble（原方案） | 成本 3×，收益边际低；无法传递 ThinkingMax；规则命中率不高于单次 ThinkingMax |
| 仅依赖 Cedar 策略（M11 Layer 2 forbid） | Cedar 规则为语义级权限控制，不能替代代码内容的静态安全扫描 |
| 仅正则，不加 AST | 正则无法可靠识别语言构造（注释内的 import 字符串、字符串拼接混淆等），AST 解析是唯一可靠的结构性检查 |

## 引用代码

- `internal/swarm/agents/governance_agent.go`（`ValidateCodeWithAudit`，三层调用编排）
- `internal/swarm/agents/code_validator.go`（Layer 0 AST + Layer 1 正则）
- `internal/swarm/agents/security_audit_agent.go`（Layer 2 单次 ThinkingMax LLM）
- `internal/swarm/agents/memory_agent.go`（`LLMInferFunc` 签名变更）
- `docs/arch/M07-Tool-Action-Layer.md §7.5`

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-13 | 初稿 |
