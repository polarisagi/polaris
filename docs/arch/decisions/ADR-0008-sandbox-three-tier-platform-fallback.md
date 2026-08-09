# ADR-0008: Sandbox 与代码安全防线合集（三级基座 + Logic Collapse L3 运行时 + L4 长驻进程池 + 三层代码安全防线，含原 ADR-0024/0026/0078/0079）

- **状态**: Accepted（已执行）| **日期**: 2026-05-16（扩展 2026-06/06-13/2026-07-25，合并 2026-07-28）| **模块**: M7 `internal/action` / `internal/sandbox/` / `internal/extension/skill/` / `internal/action/codeact/` / `internal/swarm/agents`

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

## 决策四：GovernanceAgent 代码安全三层防线（原 ADR-0024，AST + 正则 + 单次 ThinkingMax LLM）

LLM 生成代码（CodeAct/Wasm）进沙箱前经三层串行防线，取代原三路 goroutine LLM 投票（成本 3×、收益边际低）：

| 层 | 性质 | 机制 |
|----|------|------|
| Layer 0 | 同步 <5ms | Go AST 解析 + import 白名单，拦截 `os/exec`/`syscall`/`unsafe` 等危险包（gpython AST + mvdan.cc/sh 语法树） |
| Layer 1 | 同步 <1ms | 正则规则集，邻近匹配距离 ≤200 字节防跨行误报 |
| Layer 2 | 异步 | 单次 LLM + `ThinkingMax` 深度审计，超时 fail-closed |

本决策与决策一/二/三的关系：决策一/二/三管代码**在哪运行**（沙箱隔离层级），本决策管代码**能否放行运行**（执行前静态+LLM 审计门），两者是同一 CodeAct 管线的串联防线，非替代关系。

## 决策五：Wasm Component Model 当前状态与后续路线图（GD-14-008）

### 当前状态（2026-07-29 核实）

`rust/substrate/src/wasmtime_engine.rs`（第77行）已通过 `config.wasm_component_model(true)` 启用 Wasmtime 的 Component Model 基础配置开关。然而，这仅是底层引擎配置项的激活，**并不意味着功能已完整落地**：

- **未实现**：WIT（Wasm Interface Types）接口绑定代码（通过 `wit-bindgen` 生成）
- **未实现**：组件组合（component linking）与跨组件调用支持
- **未实现**：WASI 0.2 Preview2 完整的 I/O 接口适配（`wasi:io@0.2.0` / `wasi:filesystem@0.2.0` 等 World 定义）
- **当前效果**：所有插件/技能仍以 Shell 脚本、Python 或经典 Wasm 模块（非 Component Model）形式运行，不依赖 Component Model 功能

### 后续路线图（Tier-1+ 解锁，Tier-0 不作硬依赖）

如需引入 Wasm Component Model 完整支持，后续应：

1. **引入 `wit-bindgen`**：为插件市场定义标准 WIT World（如 `polaris:plugin/world`），生成 Rust/Python/JS 宾端绑定
2. **补齐组件实例化**：在 `wasmtime_engine.rs` 中添加 `Component::from_file` + `Linker::define` 组件加载路径，与现有 `Module::from_file`（经典 Wasm）并行
3. **WASI 0.2 适配**：接入 `wasmtime-wasi` crate 的 `WasiCtxBuilder` preview2 API
4. **插件格式迁移**：允许插件市场以 `.wasm component` 格式（替代当前 `.wasm module`）分发，实现更严格的接口契约与更快的冷启动（无需 Python 运行时启动）

**安全约束延续**：Component Model 不改变本 ADR 决策一/四的沙箱隔离层级与代码审计要求；组件实例仍须在 L2（wasmtime deny-by-default）内运行，不得提升到 L1 原生层。

## 决策六：可信来源的 InProcess 回退需显式 opt-in（追加，引用 ADR-0087）

可信来源（TrustOfficial 及以上）在 Wasm/Container/Remote 均不可用时，**默认不**允许降级到 InProcess 执行（`configs/defaults.toml` `[sandbox] allow_trusted_inprocess_fallback = false`）。

理由：可信 ≠ 稳定——可信来源的代码仍可能死循环/内存爆炸/panic，InProcess 执行会直接拖垮宿主进程；这是**稳定性维度**的风险，与"可信"这一**安全维度**判断是两个正交问题（阶段03 R-05）。仅开发环境未编译 Wasm 引擎、或运维明确接受该风险时可显式置 `true`。该开关与其余 fail-closed 降级同属 ADR-0087"降级必须显式"总原则的一个实例。

## 反例守护（更新）

拒绝"为方便所有工具降到 L1"——L1 仅限内置确定性工具。拒绝"LLM 生成技能用 L1 兼容"——至少 L2。拒绝改用 Go/Rust 直接编译执行 Logic Collapse 产物——蒸馏产物是动态生成代码。拒绝在 Wasm(L1) 中运行 Python。拒绝伪造 L4 checkpoint/restore 后端——ADR-0078 的安全分析结论仍然有效。拒绝恢复多视角 ensemble 投票——单次 ThinkingMax 推理质量已优于三路无 thinking 投票，且成本更低。**拒绝在 Component Model 未完整落地前对外声称"已支持 WASI 0.2"**——当前仅配置开关已开启，完整实现尚未完成。

## 引用代码

`internal/action/sandbox/`、`internal/extension/skill/compile.go`（`ValidatePython`）、`internal/extension/skill/skill_pipeline.go`、`internal/sandbox/sandbox_persistent.go`、`internal/tool/sandbox/argv_wrapper_adapter.go`、`internal/action/codeact/code_act.go`（三层同步编排）、`internal/action/codeact/code_act_checker.go`（Layer 0 AST）、`internal/swarm/agents/security_audit_agent.go`（Layer 2）、`rust/substrate/src/wasmtime_engine.rs`（L2 Wasmtime 引擎）

> 2026-08-09 追记：重新评估触发条件——① Wasm Component Model 若要从"配置开关已开"
> 推进到"可对外声称支持"，须先完成决策五路线图四步（wit-bindgen/组件实例化/WASI 0.2/
> 插件格式迁移），逐步验收，不得跳步宣称；② L4 长驻进程池的"已知边界"（会话内串行、
> 未接 OOM Guard）任一被真实场景触发问题，需回来加固而非放大回收阈值绕过；
> ③ 决策六的 `allow_trusted_inprocess_fallback` 默认值若要改真，须先有稳定性维度的
> 独立评估（资源限额/超时熔断），不能因为"来源可信"就跳过该评估。

