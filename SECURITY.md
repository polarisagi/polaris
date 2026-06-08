# 安全策略 / Security Policy

## 上报渠道

**首选**：GitHub Private Vulnerability Reporting
- 访问 https://github.com/polarisagi/polaris/security/advisories/new
- 仅维护者可见，公开披露前可协调修复

**备用**：邮件 — polarisagi.online@gmail.com（或通过 GitHub 个人主页）

**不要**：在公开 Issue / Discussion / PR 中提交未公开的安全漏洞。

## 在范围内（In Scope）

AI Agent 项目的高敏感面：

| 类别 | 示例 |
|------|------|
| **Prompt Injection** | 绕过 M11 Taint Tracking 五道防线、Spotlighting 失效、注入 instruction slot |
| **Sandbox Escape** | wazero/microVM 逃逸、Capability Token 提权、WASI 权限矩阵绕过 |
| **Taint 降级路径滥用** | 非 Sanitizer 路径让 `TaintedString` 降级为 `SafeString` |
| **Cedar Policy 绕过** | 静态规则缺陷、动态决策注入 |
| **PII / 凭证泄漏** | SessionPIIVault 边界击穿、`[CredentialVault]` 明文落盘 |
| **SSRF / DNS Rebinding** | `[SafeDialer]` 五阶段防护遗漏 |
| **KillSwitch / HITL 绕过** | 不可逆操作未经 Phase 2 HITL、`.fullstop` 文件被无视 |
| **审计链篡改** | hash chain 断裂、Append-only 约束破坏 |
| **Eval Holdout Set 泄漏** | M9 通过 L1/L2/L3 三层防护读到 Holdout |
| **依赖供应链** | Wasm skill 签名校验失效、MCP server OAuth 流程缺陷 |

## 不在范围内（Out of Scope）

- 用户主动启用 `local_only=false` 后第三方 LLM Provider 的服务端漏洞（请上报至对应 Provider）
- 用户绑定到 `0.0.0.0` 后未启 TLS + capability 的暴露面（违反硬约束 `[Tier-0-Limit]`）
- 个人 Workspace 路径下用户自己生成的代码副作用
- DoS（资源耗尽类）— 已在 `[Tier-0-Limit]` + `OSMemoryGuard` 范围内自我防御

## 响应承诺

- **确认收到**：5 个工作日内
- **初步定级**：10 个工作日内（Critical / High / Medium / Low）
- **修复目标**：Critical 30 天、High 60 天、Medium 90 天
- **协调披露**：默认 90 天，可与上报者协商缩短/延长

本项目为社区维护，无 7×24 待命；以上为**尽力承诺**，无 SLA 强制力。

## 漏洞奖励

目前**无金钱奖励计划**。已确认有效的安全报告将在 Release Notes 中致谢（除非上报者要求匿名）。

## 安全实践参考

- 架构防御纵深：[M11-Policy-Safety](./docs/arch/M11-Policy-Safety.md)
- Taint Tracking 主防线：[00-Global-Dictionary §4](./docs/arch/00-Global-Dictionary.md)
- ADR-0007（TaintLevel 五级）、ADR-0008（Sandbox 三级降级）、ADR-0009（KillSwitch 三阶段）：[decisions/](./docs/arch/decisions/)
