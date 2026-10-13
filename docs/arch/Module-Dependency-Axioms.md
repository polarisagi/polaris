# 模块依赖层级公理

> **[定位]**：仓库内所有模块 import 方向与接口定义权的唯一权威约束源，CI lint（`internal/lint/`）按本文检查违规。
> 与 `docs/arch/00-Global-Dictionary.md §1-ter` XR 规则互补：XR 规则描述"怎么做"，本文描述"谁不能 import 谁"。

---

## 1. 层级定义

`ls internal` 实测 29 个目录，全部归入下列层级（`internal/lint` 为 CI 测试包，无生产代码，不入层）：

```text
Arch-L0: pkg/                        ← 纯数据基座，全系统可 import
Arch-L1: internal/store              ← 基础设施层
          internal/observability
          internal/security
          internal/config            ← 配置加载 + 编译期不可变常量
          internal/ffi               ← Rust dylib 零 CGO 桥接（purego）
          internal/runtimeinfo       ← run/ 运行时状态文件（端口/令牌/PID，ADR-0096）
          internal/sysinfo           ← 2026-07 自 sysmgr 迁出
          internal/downloader        ← 2026-07 自 sysmgr 迁出（被 L2/L3 广泛引用）
Arch-L1bis: internal/execute         ← 单/多 Agent 执行引擎（ADR-0046）
          （dag: 工具链 DAG 执行 + S_VALIDATE 四层校验；orchestrator: Blackboard +
          Worker + 多模式编排。只负责"如何跑完已确定的计划"，不做决策；被 L2 agent
          与 L3 swarm 消费，仅可 import Arch-L0/LX）
Arch-L2: internal/agent              ← 认知/执行层（核心业务）
          internal/action
          internal/memory
          internal/tool
          internal/sandbox
          internal/vfs
          internal/llm
          internal/prompt
Arch-L3: internal/swarm              ← 协同/知识层
          internal/learning
          internal/knowledge
          internal/extension
Arch-L4: internal/gateway            ← 接口/治理层
          internal/automation
          internal/eval
          internal/channel
          internal/sysmgr            ← 现含 locale / osutils / updater
Arch-LX: internal/protocol           ← 跨层共享契约（特殊，不属任意业务层）
```

---

## 2. 强制约束（MUST）

### 2.1 Arch-L0 净化约束

- **[MUST]** `pkg/` 仅允许 POD：struct、enum、const、纯内存方法
- **[MUST NOT]** `pkg/` 内定义任何 `interface`
- **[MUST NOT]** `pkg/` import 任何 `internal/` 包

### 2.2 单向下沉约束

- **[MUST]** 高层只能 import 低层或同层的 `internal/protocol/` 契约
- **[MUST NOT]** 低层 import 高层（如 `store` 禁止 import `agent`）
- **[MUST NOT]** Arch-L2 模块之间跨包直接 import 具体实现
  - 正确：`agent` 在自身包内声明接口，装配层注入 `action` 的具体实现
  - 错误：`agent` 直接 `import internal/action/codeact`
- **[MUST NOT]** `internal/execute`（L1bis）反向 import `internal/agent`（L2）或 `internal/swarm`（L3）的内部实现；`execute` 仅通过 `agent/provider.go` 的 `DAGRunner`/`DAGValidator` 消费端接口被动接线，自身只 import `internal/protocol`

### 2.3 跨 Arch-L2 通信规范

两种合法路径，选一：

**路径 A（Consumer-side Interface，推荐）**：
- 调用方（如 `agent`）在自身包内 `provider.go` 定义极简接口，实现方（如 `action`）隐式满足，装配层物理注入
- 标杆：`internal/agent/provider.go`

**路径 B（Protocol 共享契约）**：
- 接口定义在 `internal/protocol/interfaces_*.go`（按域拆分，如 `interfaces_store.go` / `interfaces_memory.go` / `interfaces_swarm.go`）
- 适用于被 3 个以上模块共享的通用接口；每个接口必须标注 `@consumer` 和 `@producer`

### 2.4 Arch-L8 装配层特权

- **[MUST]** 装配层是全仓库唯一允许跨层引用的位置
- **[MUST]** 所有具体实现与接口的注入，必须且仅能在装配层完成
- **[MUST NOT]** 其他任何包通过全局变量或 `init()` 做隐式依赖注入

> **装配层的物理落点是 `cmd/polaris/boot_*.go`**。曾有的 bootstrap 包（自动装配契约）与 `PhasedStartup` 已删除（ADR-0112 决策八 #1，2026-10-13，可从 git 历史取回）。上述三条 [MUST] 约束的是**职责归属**。详见 `ARCHITECTURE.md §8`。

### 2.5 `internal/protocol/` 特殊规则

- `protocol/` 是跨模块共享的只读契约层，不持有业务状态
- **[MUST NOT]** `protocol/` import 任何 Arch-L1 ~ Arch-L4 的具体实现包
- `protocol/interfaces.go` 中的接口仅用于"3 个以上消费者"的通用场景；单一消费者优先路径 A

### 2.6 M7 专项：tool / sandbox / action / extension 边界（GD-5）

裁决：**不采纳**把 M7 物理合并为单一 `internal/action/` 包（会把 `internal/extension` 承载的 M6 / M13-bis 职责卷入，制造新越界）；收紧的是 import 方向而非目录结构。记录见 `docs/arch/decisions/ADR-0081-arch-doc-structure-governance.md`（决策三，含原 ADR-0044）。

| 从 → 到 | 是否允许 | 说明 |
|---|---|---|
| `extension` → `tool`/`sandbox` | ✅ 允许 | 符合 §2.2（L3 → L2），消费 tool 注册的工具与 sandbox 执行环境 |
| `extension` → `action` | ✅ 允许 | 通过 action 暴露的消费端接口触发 CodeAct/LAM/Hook 执行 |
| `tool`/`sandbox` → `extension` | ❌ 禁止 | L2 不得依赖 L3 |
| `action` → `extension` | ❌ 禁止 | 同上 |
| `extension` 定义本应属于 `tool`/`sandbox` 的接口类型 | ❌ 禁止 | 定义权属于 L2（谁执行谁定义），extension 只能消费 |
| `tool` ↔ `sandbox` ↔ `action` 同层互引 | 参照 §2.2 | 同层跨包直接 import 具体实现仍走路径 A/B，不因同属 M7 而豁免 |

---

## 3. 常见违规示例

- **❌ 违规 1**：L2 直接 import L2 具体实现
  `internal/agent/agent.go` 引用 `internal/action/codeact`。
  **✅ 正确**：在 `agent/provider.go` 声明消费者接口，由装配层注入。

- **❌ 违规 2**：`pkg/types` 定义业务接口（如 `StoreWriter`）。
  **✅ 正确**：接口定义在消费方包内（如 `internal/agent/provider.go`）。

- **❌ 违规 3**：低层依赖高层，如 `internal/store/store.go` 引用 `internal/agent`。

---

## 4. 与现有文档的关系

| 文档 | 职责分工 |
|---|---|
| 本文档 | import 方向约束（谁不能 import 谁）|
| `00-Global-Dictionary.md §1-ter XR 规则` | 跨模块协作协议（怎么通信）|
| `internal/protocol/interfaces_*.go` | 具体接口契约代码（权威实现）|
| 各模块 `CLAUDE.md` 权力边界章节 | 单模块内部的禁令清单 |
