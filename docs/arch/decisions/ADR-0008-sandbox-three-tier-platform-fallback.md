# ADR-0008: Sandbox 分级架构合集（三级基座 + Logic Collapse L3 运行时 + L4 长驻进程池，含原 ADR-0026/0078/0079）

- **状态**: Accepted（已执行）| **日期**: 2026-05-16（扩展 2026-06/2026-07-25，合并 2026-07-28）| **模块**: M7 `internal/action` / `internal/sandbox/` / `internal/extension/skill/` / `internal/action/codeact/`

## 决策一：三级 Sandbox + Tier-0 平台特化降级（原决策）

三级 Sandbox（定义见 [00-Dict §5](../00-Global-Dictionary.md)）：
- **L1 原生层**：内置确定性工具专用，Fail-Closed（隔离组件缺失直接失败，禁向无沙箱原生 exec 静默降级）
- **L2 Rust 脚本沙箱**（wasmtime，deny-by-default）：Wasm 二进制专用；Logic Collapse Python 走 L3（决策二）
- **L3 平台原生 microVM**：Linux Firecracker/gVisor 回退、macOS Virtualization.framework、Windows WSL2+Hyper-V

Tier-0 平台特化：全平台 Tier-0 L3 不可用（≥256MB/实例超预算）；`CapWriteNetwork`/`Privileged` 在 Tier-0 → `ErrTier0SandboxLimit`，禁降级到原生子进程。

## 决策二：Logic Collapse 执行运行时（原 ADR-0026）

Logic Collapse（System 2 轨迹蒸馏为 System 1 技能）产物语言定为 **Python**，执行路径为 **ContainerSandbox（L3）**，取代早期"TinyGo→Wasm"设计与实际落地的"TypeScript→npx tsx"。

理由：CodeAct 已强依赖 ContainerSandbox+Python，零增量依赖；Python 是 LLM 代码生成质量最高的语言；与插件市场生态一致（同为 Python）；ContainerSandbox 隔离强度满足 HE-2。

实现约束：`FeatureLogicCollapse` **AND** `FeatureL3Sandbox` 双门控，L3 不可用时降级为仅存 SKILL.md 元数据；产物固定为 `src/skill.py`，函数签名 `def execute(input: dict) -> dict`；静态分析禁止 `import os`/`subprocess`/`socket`/`eval`/`exec`。

## 决策三：Sandbox-L4-Persistent 长驻解释器进程池（原 ADR-0079，推翻原 ADR-0078"诚实留空"结论）

原 ADR-0078 因 CRIU/Firecracker checkpoint/restore 在本仓库 L3 沙箱架构（bwrap/Seatbelt 进程级隔离）下无对应 OS 原语，判定 `PersistentSandbox.Available()` 恒为 `false`。复核发现 checkpoint/restore 只是达成"状态不因进程重启丢失"这一目标的一种手段，另一手段是**让解释器进程在多次调用间根本不退出**（Jupyter Kernel / E2B / Modal 同款机制），不依赖任何缺失的 OS 原语。

推翻"Available() 必须恒为 false"结论，**保留** ADR-0078 的安全分析本身（bwrap/Seatbelt 无 checkpoint/restore 原语、伪造后端违反 HE-2 均属实）。session-scoped 长驻进程经 `ArgvWrapper` 消费方接口复用与 L3 `ContainerSandbox` 同一 Rust FFI 沙箱封装（bwrap/Seatbelt），隔离强度不降级——只是生命周期更长。Python 用显式 JSON 协议（非交互式 REPL，避免解析提示符的脆弱性）；Bash 用长驻 `bash --noprofile --norc -s` + 哨兵行。生命周期：10 分钟空闲回收 / 8 会话上限 / 30 秒单次超时。默认 `SandboxL4Enabled=false`，`hwTier>=2` 才装配，Tier-0/1 零回归。

**已知边界**：沙箱边界（AllowedPaths/网络策略）会话创建时固化，不可中途更改；单会话同一时刻只能串行处理一次调用；未接入 OOM Guard 联动。

## 反例守护

拒绝"为方便所有工具降到 L1"——L1 仅限内置确定性工具。拒绝"LLM 生成技能用 L1 兼容"——至少 L2。拒绝改用 Go/Rust 直接编译执行 Logic Collapse 产物——蒸馏产物是动态生成代码。拒绝在 Wasm(L1) 中运行 Python。拒绝伪造 L4 checkpoint/restore 后端——ADR-0078 的安全分析结论仍然有效。

## 引用代码

`internal/action/sandbox/`、`internal/extension/skill/compile.go`（`ValidatePython`）、`internal/extension/skill/skill_pipeline.go`、`internal/sandbox/sandbox_persistent.go`、`internal/tool/sandbox/argv_wrapper_adapter.go`
