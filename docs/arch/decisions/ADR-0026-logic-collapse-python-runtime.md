# ADR-0026 — Logic Collapse 执行运行时：Python + ContainerSandbox

**状态**: 已采纳（2026-06）
**范围**: M6 Logic Collapse 蒸馏产物的代码生成语言与执行路径

---

## 背景

Logic Collapse（M6 §2.2）的目标是将 System 2 成功轨迹蒸馏为可快速复用的 System 1 技能。
蒸馏产物需要一个执行运行时。历史上架构文档写的是"TinyGo → Wasm"，实际落地为"TypeScript → npx tsx"。
两者均存在问题，需要做出最终决策。

---

## 被驳回方案

### 方案 A：TinyGo/Wasm

- **优点**：Wasm 二进制自包含、Wasmtime 沙箱隔离最强（WASI capability-based）、运行时无外部依赖
- **缺点**：
  - TinyGo 是编译时依赖，需在触发 Logic Collapse 的宿主机安装（运维负担）
  - LLM 生成 TinyGo 代码质量差（TinyGo 限制多：无 reflect、goroutine 受限、stdlib 子集）
  - Wasmtime 在 L2 沙箱，ContainerSandbox（L3）的隔离边界已覆盖其安全需求

### 方案 B：TypeScript + npx tsx（旧实现）

- **优点**：LLM 生成 TypeScript 质量较高、npx 无需预装全局工具
- **缺点**：
  - **Node.js 是运行时依赖**，2GB VPS 最小部署目标不保证存在（与 Tier-0 兼容性承诺矛盾）
  - `npx tsx` 首次运行会从网络下载包（副作用、离线不可用）
  - 安全边界弱于 ContainerSandbox（Node.js 进程继承宿主权限）
  - 与官方插件市场（Python 插件）生态不一致

---

## 采纳方案：Python + ContainerSandbox

### 决策

Logic Collapse 生成语言改为 **Python**，执行路径改为 **ContainerSandbox（L3）**。

### 理由

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

---

## 实现约束

- Logic Collapse 现在依赖 `FeatureLogicCollapse` **AND** `FeatureL3Sandbox` 双门控
- L3 不可用时：不触发蒸馏，仅存技能元数据（SKILL.md 形式降级）
- 蒸馏产物文件名：`src/skill.py`，函数签名固定为 `def execute(input: dict) -> dict:`
- 静态分析：`skill/compile.go:ValidatePython`（禁止 `import os`/`subprocess`/`socket`/`eval`/`exec`）
- 执行 ABI：stdin/stdout JSON（与 CodeAct `writeTempScript` 路径一致）

---

## 关联

- **ADR-0008**：沙箱三级回退（L1/L2/L3 平台差异）
- **ADR-0021**：核心机制实现状态（Logic Collapse 已实现条目需同步更新）
- **M06-Skill-Library.md §2.2**：Logic Collapse 详细流水线
- **M07-Tool-Action-Layer.md §7.4**：CodeAct 共用 ContainerSandbox 路径
