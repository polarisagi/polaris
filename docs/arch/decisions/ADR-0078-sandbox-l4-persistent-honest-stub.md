# ADR-0078: Sandbox-L4-Persistent 接线到位、后端诚实留空

## 状态
Accepted（已执行，能力本身未生效）

## 背景

原始设计（GD-14-003，`local_playground/upgrade/gemini-upgrade-prompt.md` D4）提出为长程有状态 CodeAct 会话新增 Tier2+ 可选持久化沙箱（Sandbox-L4-Persistent），底层设想用 CRIU（Linux checkpoint/restore）或 Firecracker microVM snapshot 二选一，替代现有 `internal/action/codeact/code_act_stateful.go` `StatefulSession` 的 pickle/`declare -p` 文本序列化方案（每次调用仍是全新一次性进程，文件句柄/线程/DB 连接等不可序列化对象被静默跳过）。

复核发现两个关键约束：

1. **现有 L3 沙箱架构已不支持虚拟化/容器路径**。`internal/sandbox/sandbox_container.go` 明确注释"统一 Rust 沙箱，废弃 Linux namespace / Firecracker 路径"（ADR-0008/ADR-0011），实际后端是 bwrap（Linux namespace 隔离）/Seatbelt（macOS sandbox profile），通过 `CmdRunner` 抽象跨平台 shell 调用，不是 Docker/gVisor/microVM（`pkg/types/enums_tool.go` 历史注释仍写着"gVisor / microVM"，属于未同步的文档漂移，本次一并订正）。
2. **bwrap/Seatbelt 都没有对应的 checkpoint/restore 原语**。CRIU 理论上能对 bwrap 派生的普通 Linux 进程树做 dump，但需要额外的 PID namespace 隔离才能拿到干净的 dump 边界，工作量显著；macOS 的 Seatbelt 完全没有等价机制。本仓库也没有任何 CRIU/Firecracker 的真实实现或已验证选型（全仓搜索确认，仅有已废弃方案的历史注释残留）。在没有真实 Tier2+ Linux 宿主验证 CRIU 端到端可用性的前提下，实现一个"看起来能持久化但实际不 dump/restore 任何东西"的假后端，等同于伪造安全边界——HE-Rule-2（可验证执行）明令禁止"概率过滤/伪装当安全边界"，伪装能力比不实现更危险。

用户在被告知上述约束后，明确选择"接线到位，后端诚实留空"这一方案（而非重新引入 Docker checkpoint 依赖、或投入更大工作量给 bwrap 套 CRIU namespace、或本轮完全不做）。

## 决策

1. **新增枚举** `types.SandboxPersistent`（`pkg/types/enums_tool.go`），作为 `SandboxTier` 的第 6 个取值。命名沿用设计文档"Sandbox-L4-Persistent"，但注释明确澄清：枚举序号与既有注释中散称的"L4"（`SandboxRemote`/`SandboxNativeOS` 的口语化标签，非严格序号）不是同一含义。

2. **骨架实现** `internal/sandbox/sandbox_persistent.go`：`PersistentSandbox` 实现 `SandboxProvider` 接口（满足 `Run` 方法）。
   - `Available() bool` **恒定返回 `false`**——这是本 ADR 的核心可验证承诺：在真实 checkpoint/restore 后端接入之前，任何调用方都不会被引导至一个假装可用的 L4 实现。配套单测 `TestPersistentSandbox_AvailableIsAlwaysFalse` 锁定该不变量。
   - `Run()` 作为纵深防御第二道闸门：即便调用方绕过 `Available()` 检查直接调用，也显式返回 `apperr.CodeUnimplemented`，不会静默假装执行成功。

3. **路由接线** `internal/sandbox/sandbox_router.go`：`RouteByTier` 新增 `case types.SandboxPersistent`，`persistent.Available()==true` 时路由至 L4；否则按 `SandboxContainer` 同等的降级链回退（Container → Remote → fail-closed）。这与设计文档"否则保持现状回退到既有 StatefulSession 序列化路径"的要求一致——调用方拿到 Container/Remote 后仍走原有一次性进程执行路径，`StatefulSession` 的样板注入逻辑完全不受影响。`WithPersistent` 注入方法遵循既有 `WithRemote`/`WithNativeOS` 同一模式。

4. **硬件门控 + 配置阈值**：`internal/config/thresholds.go` `M7ToolThresholds` 新增 `SandboxL4Enabled bool`（默认 `false`）/`SandboxL4Backend string`（默认 `"unimplemented"`，仅用于日志诊断，不影响 `Available()` 判定）。`cmd/polaris/boot_tools.go` 仅在 `SandboxL4Enabled && hwTier>=2` 时构造并注入 `PersistentSandbox`；`state.yaml m7_tool` 段新增对应的 `sbx_l4_persistent_enabled`/`sbx_l4_persistent_backend` 阈值项。`make gen-threshold-examples` 已重新生成并提交 `configs/threshold-examples/m7_tool.toml`。

5. **范围声明**：本 ADR 只交付"接线"，不交付"能力"。任何环境下 `PersistentSandbox.Available()` 都返回 `false`，L4 tier 在生产中永远走 Container/Remote 降级路径，Tier-0/Tier-1 行为零回归、零风险。

## 后果

- 未来若要真正接入 CRIU/Docker-checkpoint/等价机制，只需替换 `sandbox_persistent.go` 的 `Available()`（换成真实宿主能力探测）与 `Run()`（实现真正的 dump/restore），路由层/配置层/硬件门控均无需改动——变更面被限制在单一文件内（R1.4 消费方接口 + 组合原语的设计价值）。
- 顺带订正了 `pkg/types/enums_tool.go` 中 `SandboxContainer` 的过时注释（"gVisor / microVM" → bwrap/Seatbelt 实际实现），属于本次改动直接相邻代码的最小订正，未扩大到 `docs/arch/M07-Tool-Action-Layer.md` §4.1/§4.2 中同类型的历史文档漂移（那是更大范围的独立文档清理工作，超出本 ADR 范围，已在 M07 文档 §4.7 中就近标注供未来处理）。
- 不引入 Docker/CRIU 依赖，不违反 ADR-0008/ADR-0011 关于 L3 沙箱统一收敛为 bwrap/Seatbelt 的既有决策；本 ADR 与那两份 ADR 是互补关系（L4 是完全独立于 L3 的可选 tier），不推翻它们。
- 代价：D4 本次交付不提供真实持久化能力，`StatefulSession` 的 pickle/`declare -p` 局限（不可序列化对象静默丢失）依然存在，留待未来专项评估 CRIU/Docker-checkpoint 选型时一并解决。
