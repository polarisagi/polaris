# ADR-0024: GovernanceAgent 代码安全三层防线（AST + 正则 + 单次 ThinkingMax LLM）

- **状态**: Accepted | **日期**: 2026-06-13 | **模块**: M07 / M11 / `internal/swarm/agents`

## 决策

LLM 生成代码（CodeAct/Wasm）进沙箱前经三层串行防线，取代原三路 goroutine LLM 投票（成本 3×、收益边际低）：

| 层 | 性质 | 机制 |
|----|------|------|
| Layer 0 | 同步 <5ms | Go AST 解析 + import 白名单，拦截 `os/exec`/`syscall`/`unsafe` 等危险包（gpython AST + mvdan.cc/sh 语法树） |
| Layer 1 | 同步 <1ms | 正则规则集，邻近匹配距离 ≤200 字节防跨行误报 |
| Layer 2 | 异步 | 单次 LLM + `ThinkingMax` 深度审计，超时 fail-closed |

## 反例守护

拒绝恢复多视角 ensemble 投票——单次 ThinkingMax 推理质量已优于三路无 thinking 投票，且成本更低。

## 引用代码

`internal/action/codeact/code_act.go`（三层同步编排）、`internal/action/codeact/code_act_checker.go`（Layer 0 AST）、`internal/swarm/agents/security_audit_agent.go`（Layer 2）
