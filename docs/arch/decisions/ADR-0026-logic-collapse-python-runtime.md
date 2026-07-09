# ADR-0026 — Logic Collapse 执行运行时：Python + ContainerSandbox

- **状态**: Accepted
- **日期**: 2026-06
- **决策者**: 架构组
- **相关模块**: M6 Logic Collapse

## 上下文

Logic Collapse（M6 §2.2）的目标是将 System 2 成功轨迹蒸馏为可快速复用的 System 1 技能。
蒸馏产物需要一个执行运行时。历史上架构文档写的是"TinyGo → Wasm"，实际落地为"TypeScript → npx tsx"。
两者均存在问题，需要做出最终决策。

## 决策

### 采纳方案：Python + ContainerSandbox

Logic Collapse 生成语言改为 **Python**，执行路径改为 **ContainerSandbox（L3）**。

**1. 零增量依赖**

CodeAct（M7 §7.4）已强制依赖 ContainerSandbox + Python 运行时。凡能运行 CodeAct 的节点（`FeatureL3Sandbox` 启用），Python 运行时已经存在。Logic Collapse 使用 Python 不引入任何新的运行时依赖。

**2. LLM 代码生成质量最高**

Python 是 LLM 代码生成质量最高的语言，生成成功率、安全性和可读性均优于 TypeScript 和 TinyGo。

**3. 与插件生态一致**

Polaris 官方插件市场的插件以 Python 编写。Logic Collapse 蒸馏产物（`src/skill.py`）与插件格式一致，未来可直接发布到市场形成闭环。

**4. 安全隔离已满足 HE-2**

ContainerSandbox（Firecracker/VZ/WSL2）提供进程级、网络级、文件系统级隔离，满足 HE-2（可验证执行）。与 TinyGo Wasm 相比，实际安全差距可忽略（两者均需 L3 门控）。

**5. System 1 语义成立**

Python 技能通过 ContainerSandbox 执行，比 System 2（LLM 推理 2~10 秒）快 10~50 倍（容器执行 100~500ms），满足 System 1 的功能定位。

**实现约束**

- Logic Collapse 现在依赖 `FeatureLogicCollapse` **AND** `FeatureL3Sandbox` 双门控
- L3 不可用时：不触发蒸馏，仅存技能元数据（SKILL.md 形式降级）
- 蒸馏产物文件名：`src/skill.py`，函数签名固定为 `def execute(input: dict) -> dict:`
- 静态分析：`skill/compile.go:ValidatePython`（禁止 `import os`/`subprocess`/`socket`/`eval`/`exec`）
- 执行 ABI：stdin/stdout JSON（与 CodeAct `writeTempScript` 路径一致）

## 后果

- **正向**: Python 生态丰富，Logic Collapse 蒸馏产物可直接运行；ContainerSandbox 提供强隔离保证
- **负向**: 需要容器运行时（Docker/Podman），Tier-0 裸机部署需额外初始化
- **反例守护**: 未来如有人提议将 Logic Collapse 蒸馏产物改用 Go/Rust 直接编译执行，引用本 ADR 拒绝——蒸馏产物是动态生成代码，静态编译路径不可行；未来如有人提议在 Wasm 沙箱中运行 Python，引用 ADR-0008 三级沙箱体系，Wasm 是 L1，Python+Container 是 L2，两者沙箱等级不同，不可替换

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 方案 A：TinyGo/Wasm | TinyGo 是编译时依赖，需在宿主机安装；LLM 生成 TinyGo 代码质量差；Wasmtime 安全隔离边界已由 L3 沙箱覆盖 |
| 方案 B：TypeScript + npx tsx | Node.js 是运行时依赖；首次运行下载包有副作用；安全边界弱于 L3 沙箱；与插件生态不一致 |

## 引用代码

- `internal/extension/skill/compile.go`（`ValidatePython` 静态分析函数）
- `internal/extension/skill/skill_pipeline.go`（蒸馏管线，产出 `src/skill.py`）
- `internal/action/codeact/code_act.go`（`writeTempScript` 执行 ABI，与蒸馏产物执行路径一致）
- `internal/observability/probe/feature_gate.go`（`FeatureLogicCollapse` 依赖 `FeatureL3Sandbox` 的双门控逻辑）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06 | 初稿，Accepted |
| 2026-07-03 | 补充决策者署名与代码引用复核 |
