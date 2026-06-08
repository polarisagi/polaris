# ADR-0008: Sandbox 三级 + Tier-0 平台特化降级

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M7 / `pkg/action`
- **实现详情**: [M07 §4 Sandbox Provider](../M07-Tool-Action-Layer.md) | [00-Dict §5 Sandbox-L1/L2/L3](../00-Global-Dictionary.md)

## 上下文

工具执行风险跨度大:内置工具零风险 / LLM 生成技能中等风险 / shell/CodeAct 高风险。单一沙箱方案要么过重(影响内置性能)要么过轻(无法约束高风险)。同时 Tier 0 macOS/Windows 无 gVisor 支持。

## 决策

**三级 Sandbox 抽象 + Tier-0 平台特化降级。**

三级完整定义见 [00-Dict §5](../00-Global-Dictionary.md):
- **L1 InProc**(Go function): 零隔离,仅限受信内置工具
- **L2 wazero Wasm**: deny-by-default WASI,第三方技能默认级
- **L3 gVisor**: 用户态内核 syscall 拦截,高风险/CodeAct 强制

Tier-0 平台特化:
- macOS: L2 + Apple Sandbox profile(`sandbox-exec`)
- Windows: L2 + Job Objects(AppContainer)
- Tier-2+ Linux 可选 Firecracker microVM 升级(需硬件 KVM)
- Tier-0 全平台 L3 不可用(gVisor 启动门槛 ≥256MB)

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| 单一 Sandbox 级别 | L3 太重(内置工具性能);L1 太轻(高风险无隔离) |
| 全平台 gVisor | macOS/Windows 不支持;Tier-0 内存预算不足 |
| Docker 容器 | 启动秒级;不便单二进制分发 |
| 仅 Wasm(无 L3) | 高风险 CodeAct 缺少 syscall 隔离 |

**反例守护**:
- 未来如有人提议"为方便给所有工具降到 L1"—本 ADR 拒绝。L1 仅限**内置确定性工具**
- 未来如有人提议"为兼容性给 LLM 生成技能用 L1"—本 ADR 拒绝。LLM 生成内容至少 L2
