# ADR-0008: Sandbox 与代码安全防线（三级基座 + Logic Collapse L3 + L4 长驻池 + 三层代码审计）

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0024/0026/0078/0079 ｜ 模块：M7 `internal/sandbox/` / `internal/action/codeact/` / `internal/extension/skill/` / `internal/swarm/agents/`

## 决策一：三级 Sandbox + Tier-0 平台降级

定义见 [00-Dict §5](../00-Global-Dictionary.md)：
- **L1 原生层**：仅内置确定性工具，Fail-Closed（隔离组件缺失直接失败，禁向无沙箱原生 exec 静默降级）
- **L2 Rust 脚本沙箱**（wasmtime，deny-by-default）：Wasm 二进制专用
- **L3 容器级隔离（现状）**：`ContainerSandbox` 经 Rust `CmdRunner` 调用 Linux bwrap（namespace+seccomp）/ macOS Seatbelt（Windows 经 WSL2），无 microVM 后端。microVM（Linux Firecracker/gVisor 回退、macOS Virtualization.framework）为 Tier2 规划，未实现；`probe` 中的 `firecracker` 仅是 Tier2+ Linux 的能力标签，无后端消费

Tier-0 规则（2026-09-20 修订，取代原"一律 `ErrTier0SandboxLimit`"）：L3 在 Tier-0 不可用（≥256MB/实例超预算）。Container 请求在 Tier-0 降级为 `SandboxNativeOS`（Rust FFI → bwrap namespace+seccomp / Seatbelt，物理可验证隔离，满足 HE-2，不属"无沙箱原生 exec"）；`CapPrivileged` 在 Tier-0 仍返回 `ErrTier0SandboxLimit`（bwrap/Seatbelt 无法替代 microVM 硬件边界）。实现 `internal/sandbox/assign.go`。

## 决策二：Logic Collapse 运行时（原 ADR-0026）

产物语言 **Python**，执行于 **ContainerSandbox（L3）**，取代"TinyGo→Wasm"与"TypeScript→npx tsx"。理由：CodeAct 已依赖 ContainerSandbox+Python、LLM 生成质量最高、与插件市场一致、隔离满足 HE-2。
- `FeatureLogicCollapse` **AND** `FeatureL3Sandbox` 双门控；L3 不可用时降级为仅存 SKILL.md 元数据
- 产物固定 `src/skill.py`，签名 `def execute(input: dict) -> dict`；静态分析禁 `import os`/`subprocess`/`socket`/`eval`/`exec`

## 决策三：Sandbox-L4-Persistent 长驻解释器进程池（原 ADR-0079，推翻 ADR-0078 "Available() 恒 false"）

CRIU/Firecracker checkpoint/restore 在 bwrap/Seatbelt 下无 OS 原语（ADR-0078 此安全分析仍成立，伪造后端违反 HE-2）；改让解释器进程跨调用不退出（Jupyter Kernel/E2B 同款），不依赖缺失原语。
- 经 `ArgvWrapper` 复用 L3 同一 Rust FFI 沙箱封装，隔离强度不降级
- Python 用显式 JSON 协议（非 REPL）；Bash 用长驻 `bash --noprofile --norc -s` + 哨兵行
- 10 分钟空闲回收 / 8 会话上限 / 30 秒单次超时；默认 `SandboxL4Enabled=false`，`Tier>=2` 才装配（`cmd/polaris/boot_tools.go`）
- 已知边界：沙箱边界（AllowedPaths/网络策略）会话创建时固化；单会话串行；未接 OOM Guard

## 决策四：代码安全三层防线（原 ADR-0024）

LLM 生成代码进沙箱前三层串行，取代三路 goroutine LLM 投票（成本 3×、收益低）。与决策一~三是串联关系：前者管在哪运行，本决策管能否放行。

| 层 | 性质 | 机制 |
|----|------|------|
| Layer 0 | 同步 <5ms | Go AST + import 白名单，拦 `os/exec`/`syscall`/`unsafe`（gpython AST + mvdan.cc/sh） |
| Layer 1 | 同步 <1ms | 正则规则集，邻近匹配 ≤200 字节防跨行误报 |
| Layer 2 | 异步 | 单次 LLM + `ThinkingMax` 审计，超时 fail-closed |

## 决策五：Wasm Component Model 状态与路线（GD-14-008）

2026-10 核实：`rust/substrate/src/wasmtime_engine.rs:77` 仅 `config.wasm_component_model(true)` 配置开关；无 WIT 绑定、无组件链接、无 WASI 0.2（现仅 `wasmtime_wasi::p1`）。所有插件/技能仍以脚本或经典 Wasm 模块运行。
路线（Tier-1+，Tier-0 不硬依赖）：`wit-bindgen` 标准 World → `Component::from_file`+`Linker` 并行于 `Module::from_file` → `wasmtime-wasi` preview2 → 插件市场 `.wasm component` 分发。
约束：不改决策一/四的隔离与审计要求；组件仍须在 L2 内运行，不得提升到 L1。

## 决策六：可信来源 InProcess 回退须显式 opt-in（引用 ADR-0087）

TrustOfficial 及以上在 Wasm/Container/Remote 均不可用时，默认不降级到 InProcess（`configs/defaults.toml` `[sandbox] allow_trusted_inprocess_fallback = false`，`internal/sandbox/sandbox_router.go`）。可信是安全维度，InProcess 死循环/OOM/panic 拖垮宿主是稳定性维度，二者正交（阶段03 R-05）。仅开发环境未编译 Wasm 引擎或运维明确接受风险时置 `true`。

## 反例守护

- 所有工具降到 L1 — L1 仅限内置确定性工具；LLM 生成技能至少 L2
- Go/Rust 直接编译执行 Logic Collapse 产物 — 蒸馏产物是动态代码
- 在 Wasm(L1) 中运行 Python
- 伪造 L4 checkpoint/restore 后端 — 违反 HE-2
- 恢复多视角 ensemble 投票 — 单次 ThinkingMax 质量更优且更便宜
- Component Model 未完整落地前对外声称"已支持 WASI 0.2"

## 重新评估触发条件

- Component Model 对外声称支持前，须按决策五四步逐步验收，不得跳步
- L4 已知边界（会话内串行、未接 OOM Guard）被真实场景触发 → 加固，不放大回收阈值绕过
- `allow_trusted_inprocess_fallback` 默认值改真前，须有稳定性维度独立评估（资源限额/超时熔断）

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版（决策一） |
| 2026-06 | 决策二（ADR-0026）、决策四（ADR-0024） |
| 2026-07-25 | 决策三（ADR-0079 推翻 ADR-0078） |
| 2026-07-28 | 合并 ADR-0024/0026/0078/0079 |
| 2026-07-29 | 决策五（Component Model 现状核实） |
| 2026-08-09 | 决策六（引用 ADR-0087） |
| 2026-09-20 | 决策一 Tier-0 规则修订：Container 降级 NativeOS，仅 Privileged 拒绝 |
| 2026-10-12 | L3 microVM 描述订正为现状 bwrap/Seatbelt；记录先改代码后补文档的偏差（ADR-0112 决策八 #4） |

## 引用代码

`internal/sandbox/`（`assign.go`、`sandbox_router.go`、`sandbox_persistent.go`）、`internal/extension/skill/compile.go`（`ValidatePython`）、`internal/extension/skill/skill_pipeline.go`、`internal/tool/sandbox/argv_wrapper_adapter.go`、`internal/action/codeact/code_act.go`（三层编排）、`internal/action/codeact/code_act_checker.go`（Layer 0）、`internal/swarm/agents/security_audit_agent.go`（Layer 2）、`rust/substrate/src/wasmtime_engine.rs`
