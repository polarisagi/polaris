# internal/memory — 模块规范

> 架构文档：`docs/arch/M05-Memory-System.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

记忆系统（L1）。Working/Episodic/Semantic/Procedural 四层记忆的物理存储（`store/`）、检索（`retrieval/` HybridRetriever：BM25 + 向量 + 图三路融合，`graph/`）、巩固（`consolidation/` Worker）、压缩（`compact/`）以及大载荷工作区卸载（`vfs_offloader.go`）。是 agent 的外部认知存储，不参与决策。

## 权力边界 [MUST]

### 拥有
- 四层记忆的读写权
- 工具输出等大载荷的工作区卸载与 `workspace_vfs` 索引登记权（经注入的 `WorkspaceProvider`）
- 记忆巩固（Mem-L1→L2 summarize）的触发权

### 禁止 [MUST NOT]
- 直接 import `internal/agent`、`internal/action`（防循环）
- 跨 memory 包写入其他模块的数据表
- 持有 LLM Provider 具体实现引用（经注入的 `LLMSummarizer`）
- 在 `memory.go` / `facade.go` 中直接 `sql.Open` 或持有裸 `*sql.DB`（存储访问经注入的 Store/Repo 接口）
- 在热检索路径全量加载向量至 Go 堆（分页/流式）
- 对检索结果做语义判定（属 `agent`）

## 消费端接口声明位置

`internal/memory/provider.go`：`EmbedProvider`、`CognitiveStoreProvider`、`LLMSummarizer`、`WorkspaceProvider`。新增外部依赖先在此声明，由 `cmd/polaris/boot_*.go` 注入。

## 大载荷卸载约束

超限载荷必须经 `ToolRefOffloader`（`vfs_offloader.go`）卸载到任务隔离工作区，DB 内仅留引用；卸载前走 `WithQuota` 预占配额（门控 `Test_inv_VFS_QuotaMustUseWithQuota`）。memory 层禁止直接 `os.WriteFile`。
