# ADR-0008: Sandbox 三级 + Tier-0 平台特化降级

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M7 / `internal/action`
- **实现详情**: [M07 §4 Sandbox Provider](../M07-Tool-Action-Layer.md) | [00-Dict §5 Sandbox-L1/L2/L3](../00-Global-Dictionary.md)

## 上下文

工具执行风险跨度大:文件编辑等内置工具低风险 / LLM 生成技能中等风险 / 通用 shell 或 CodeAct 高风险。单一沙箱方案要么过重(影响内置性能)要么过轻(无法约束高风险)。同时 Tier 0 macOS/Windows 无 gVisor 支持。

## 决策

**三级 Sandbox 抽象 + Tier-0 平台特化降级。**

三级完整定义见 [00-Dict §5](../00-Global-Dictionary.md):
- **L1 原生层**（Go function / 平台原生子进程）: 高性能运行层。包含进程内受限执行（如 str_replace_editor）与挂载平台原生沙箱组件（如 bash/run_command 挂载 bubblewrap/seatbelt），仅限核心系统内置工具
- **L2 Rust 脚本沙箱**（wasmtime_engine.rs FFI）: deny-by-default，用于 Wasm 二进制执行；Logic Collapse Python 技能改走 L3（ADR-0026）；内置工具直接信任，不走 L2
- **L3 平台原生 microVM**（统一 SandboxProvider 接口，调用方平台无感）:
  - **Linux**: Firecracker (~125MB/VM, 需硬件 KVM)；KVM 不可用 → gVisor (runsc) 用户态内核
  - **macOS**: Virtualization.framework (~80MB/VM)
  - **Windows**: WSL2 + Hyper-V (~150MB/VM)

Tier-0 平台特化:
- 全平台 Tier-0 L3 不可用（每个 L3 实例 ≥256MB，8GB 预算不足）
- CapWriteNetwork/Privileged 在 Tier-0 → ErrTier0SandboxLimit，禁止降级到原生子进程
- Tier-1+ 启用完整 L3（内存 ≥512MB 当前平台检测）)

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| 单一 Sandbox 级别 | L3 太重（内置工具性能）;L1 太轻（高风险无隔离） |
| 全平台 gVisor | macOS/Windows 不支持 gVisor；Tier-0 内存预算不足；实际 L3 已按平台拆分（Linux=Firecracker/gVisor、macOS=VZ.framework、Windows=WSL2） |
| Docker 容器 | 启动秒级；不便单二进制分发 |
| 仅 Wasm（无 L3） | 高风险 CodeAct 缺少 syscall 隔离 |

**反例守护**:
- 未来如有人提议"为方便给所有工具降到 L1"—本 ADR 拒绝。L1 仅限**内置确定性工具**
- 未来如有人提议"为兼容性给 LLM 生成技能用 L1"—本 ADR 拒绝。LLM 生成内容至少 L2
