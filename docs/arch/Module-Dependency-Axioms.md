# 模块依赖层级公理

> **[定位]**：本文档是 polaris 仓库内所有模块 import 方向与接口定义权的
> 唯一权威约束源。CI lint（`internal/lint/`）按本文档检查违规。
> 与 `docs/arch/00-Global-Dictionary.md §1-ter` 的 XR 规则互为补充：
> XR 规则描述"怎么做"，本文档描述"谁不能 import 谁"。

---

## 1. 层级定义

```text
Arch-L0: pkg/                        ← 纯数据基座，全系统可 import
Arch-L1: internal/store              ← 基础设施层（存储）
          internal/observability
          internal/security
          internal/sysinfo           ← 2026-07-07 从 internal/sysmgr/sysinfo 迁出
          internal/downloader        ← 2026-07-07 从 internal/sysmgr/downloader 迁出
          （二者原归类 Arch-L4 sysmgr 下，但被 Arch-L2/L3 广泛引用，分类与实际
          用途不匹配，复核后物理迁移为独立 Arch-L1 包，见 CLAUDE.md 项目结构）
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
          internal/sysmgr
Arch-L8: internal/bootstrap          ← 装配层（DI 容器）
          internal/cli
Arch-LX: internal/protocol           ← 跨层共享契约（特殊，不属于任意业务层）
```

---

## 2. 强制约束（MUST）

### 2.1 Arch-L0 净化约束

- **[MUST]** `pkg/` 仅允许 POD（Plain Old Data）：struct、enum、const、纯内存方法
- **[MUST NOT]** `pkg/` 内定义任何 `interface`
- **[MUST NOT]** `pkg/` import 任何 `internal/` 包

### 2.2 单向下沉约束

- **[MUST]** 高层只能 import 低层或同层的 `internal/protocol/` 契约
- **[MUST NOT]** 低层 import 高层（如 `store` 禁止 import `agent`）
- **[MUST NOT]** Arch-L2 模块之间跨包直接 import 具体实现
  - 正确：`agent` 在自身包内声明接口，`bootstrap` 注入 `action` 的具体实现
  - 错误：`agent` 直接 `import internal/action/codeact`

### 2.3 跨 Arch-L2 通信规范

跨 Arch-L2 模块通信有两种合法路径，选一：

**路径 A（Consumer-side Interface，推荐）**：
- 调用方（如 `agent`）在自身包内的 `provider.go` 定义极简接口
- 实现方（如 `action`）隐式满足该接口
- `bootstrap` 做物理绑定注入
- 参考标杆：`internal/agent/provider.go`（已实现）

**路径 B（Protocol 共享契约）**：
- 接口定义在 `internal/protocol/interfaces.go`
- 适用于被 3 个以上模块共享的通用接口
- 每个接口必须标注 `@consumer` 和 `@producer`

### 2.4 Arch-L8 装配层特权

- **[MUST]** `internal/bootstrap/` 是全仓库唯一允许跨层引用的包
- **[MUST]** 所有具体实现与接口的注入，必须且仅能在 `bootstrap` 中完成
- **[MUST NOT]** 其他任何包通过全局变量或 `init()` 做隐式依赖注入

### 2.5 `internal/protocol/` 特殊规则

- `protocol/` 是跨模块共享的只读契约层，不持有任何业务状态
- **[MUST NOT]** `protocol/` import 任何 Arch-L1 ~ Arch-L4 的具体实现包
- `protocol/interfaces.go` 中的接口仅用于"3个以上消费者"的通用场景
- 单一消费者场景优先选用路径 A（Consumer-side Interface）

### 2.6 M7 专项：tool / sandbox / action / extension 边界（GD-5）

> 背景：`gemini-review-design.md` GD-13-002 提出 M7（工具执行层）物理目录碎片化，
> 建议合并为单一 `internal/action/` 包。复核（`local_playground/upgrade/
> 01-架构设计变更规范.md` GD-5）结论：**不采纳物理合并**——`internal/action/
> CLAUDE.md` 已有清晰的"拥有/禁止"边界文档，物理合并会把 `internal/extension`
> 承载的 M6（Skill）/M13-bis（Registry）职责一并卷入，制造新的越界。真正需要
> 收紧的是 import 方向，而非目录结构。裁决记录见
> `docs/arch/decisions/ADR-0044-m7-boundary-deferred.md`。

显式 import 方向约束（Arch-L2 `tool`/`sandbox`/`action` vs Arch-L3 `extension`）：

| 从 → 到 | 是否允许 | 说明 |
|---|---|---|
| `extension` → `tool`/`sandbox` | ✅ 允许 | 符合 §2.2 单向下沉（L3 → L2），extension 消费 tool 注册的工具、sandbox 提供的执行环境 |
| `extension` → `action` | ✅ 允许 | 同上，extension 通过 action 暴露的消费端接口触发 CodeAct/LAM/Hook 执行 |
| `tool`/`sandbox` → `extension` | ❌ 禁止 | 违反单向下沉，L2 不得依赖 L3 |
| `action` → `extension` | ❌ 禁止 | 同上 |
| `extension` 直接定义本应属于 `tool`/`sandbox` 的接口类型 | ❌ 禁止 | 类型/接口定义权属于 L2（谁执行谁定义），extension 只能消费，不得越权定义后反向要求 L2 遵从 |
| `tool` ↔ `sandbox` ↔ `action` 同层互引 | 参照 §2.2 | 同层跨包直接 import 具体实现仍需走路径 A/B，不因同属 M7 而豁免 |

---

## 3. 常见违规示例

- **❌ 违规 1**：L2 直接 import L2 具体实现
  `internal/agent/agent.go` 引用 `github.com/polarisagi/polaris/internal/action/codeact`
  **✅ 正确**：在 `agent/provider.go` 声明消费者接口（如 `CodeActEngine` 及其方法），由外部组装并注入。

- **❌ 违规 2**：`pkg/types` 包定义业务接口
  在 `pkg/types/models.go` 中定义诸如 `StoreWriter` 等接口契约。
  **✅ 正确**：接口必须定义在消费方包内（如 `internal/agent/provider.go`）。

- **❌ 违规 3**：低层依赖高层（逆向引用）
  `internal/store/store.go` 引用 `github.com/polarisagi/polaris/internal/agent`。

---

## 4. 与现有文档的关系

| 文档 | 职责分工 |
|---|---|
| 本文档 | import 方向约束（谁不能 import 谁）|
| `00-Global-Dictionary.md §1-ter XR 规则` | 跨模块协作协议（怎么通信）|
| `internal/protocol/interfaces.go` | 具体接口契约代码（权威实现）|
| 各模块 `CLAUDE.md` 权力边界章节 | 单模块内部的禁令清单 |
