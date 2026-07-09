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
