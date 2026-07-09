# internal/memory — 模块规范

> 对应架构文档：`docs/arch/M05-Memory-System.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

记忆系统（Arch-L2）。负责 Working/Episodic/Semantic/Procedural 四层记忆的物理存储、
检索（HybridRetriever BM25+向量+图三路融合）、巩固（consolidation Worker）
以及 VFS 大载荷卸载。是 agent 的外部认知存储，不参与决策。

## 权力边界 [MUST]

### 拥有
- 四层记忆（Working/Episodic/Semantic/Procedural）的读写权
- VFS Blob 的读写权（通过注入的 VFSProvider 接口）
- 记忆巩固（Mem-L1→L2 summarize）的触发权

### 禁止 [MUST NOT]
- **[MUST NOT]** 直接 import `internal/agent`、`internal/action`（防循环）
- **[MUST NOT]** 跨 memory 包写入其他模块的数据表（memory 只管 memory 相关表）
- **[MUST NOT]** 持有 LLM Provider 的具体实现引用（通过注入的 LLMSummarizer 接口调用）
- **[MUST NOT]** 在 `memory.go` / `facade.go` 中直接 `sql.Open` 或持有裸 `*sql.DB`
  （存储访问必须通过注入的 Store/Repo 接口）
- **[MUST NOT]** 在热检索路径全量加载向量至 Go 堆（使用分页/流式加载）
- **[MUST NOT]** 对检索结果做语义判定（检索结果的语义使用权属于 `agent`）

## 消费端接口声明位置

`internal/memory/provider.go` — 已声明：VFSProvider、EmbedProvider、CognitiveStoreProvider、
LLMSummarizer、WorkspaceProvider。
新增外部依赖时先在此文件声明接口，由 `bootstrap` 注入。

## VFS 卸载约束

单行载荷超 4KB 必须卸载至 VFS（通过 VFSProvider.WriteBlob）。
DB 内仅保留 `vfs_ref` 游标。禁止在 memory 层直接调用 `os.WriteFile`。
