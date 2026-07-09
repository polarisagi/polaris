# ADR-0021: 核心机制实现（SurpriseIndex / WasmTester / BM25 / FSM）

**状态**: 已接受 (Accepted)  
**日期**: 2026-06-09  

## 上下文

在推进 Polaris AGI 的核心能力时，需要补全四个占位符组件，同时严格遵守 Tier-0 架构的硬件门槛与模块间的隔离纪律（防止循环依赖和超前抽象）。

## 决策

1. **SurpriseIndex 的单例与状态收敛 (L0)**
   - **实现机制**：在 `internal/observability/metrics/metrics.go` 实现 `SurpriseIndex` 的基础版本。计算过程结合了基于 `embedding` 的 Cosine 距离（采用 EMA 平滑）以及基于 `toolSeq` 的 Jaccard 距离。（注：在 ADR-0028 修复后，`SurpriseCalculator` 接管了主路径 cosine 计算，`ComputeBasic` 保留为 L0 备用路径）
   - **并发设计**：使用 `sync.RWMutex` 确保该单例在高并发情况下的读写安全。这不仅满足了可观测的一等公民诉求，同时也提供了 S_PERCEIVE 阶段无思考触发 (FastPath) 的路由基础。

2. **ScriptTester 与沙箱能力下沉 (L1 -> L0 接口)**
   - **实现机制**：在 `internal/extension/skill/skill_pipeline.go` 定义 Consumer-side `ScriptExecutor` 接口。
   - **解耦逻辑**：解耦了认知层对 `internal/action` 脚本沙箱实例的直接依赖。`ScriptTester` 提供运行时冷启动时间以及内存消耗测试，满足可观测原则与隔离要求。

3. **BM25 混合检索中的全局状态统计 (L0)**
   - **实现机制**：在 `internal/store/search/hybrid_retrieve.go` 引入了 `CorpusStats` 结构。动态记录总文档数、分词频次、平均长度。
   - **架构对齐**：使用 Go 读写锁包裹状态更新，移除了外部复杂依赖，在内存约束极高的 Tier-0 约束下，完成快速近实时动态 IDF 分数反馈。

4. **Agent FSM 边界与 Gateway SSE 的双向通信 (L3 -> L1)**
   - **实现机制**：在 `internal/protocol/interfaces.go` 抽象 `AgentController`。`Agent` 实现了该接口并在内部完成事件注入流转 (`SendIntent` / `SetTaskIntent`)。
   - **解耦逻辑**：`internal/gateway/server/logstream.go` 再也无需直接引用 `*kernel.Agent`。在会话注入 Intent 时异步驱动 FSM 扭转；并且 SSE Stream 可通过 FSM `CurrentState()` 监测生命周期，FastPath 完成后由 SSE 发起 `TriggerExecuteDone` 推演。

## 后果

- **正向**：成功串联了四大模块（观测、沙箱、检索、状态机）。消除了模块循环依赖的隐患，并且完全维持 8GB 以下内存的 Tier-0 可观测标准和错误处理策略 (`internal/errors`)。
- **负向**：`CorpusStats` 状态缓存在内存中存在节点宕机数据丢失的风险；未来在需要持久化或跨节点扩容时，需要进一步与 M2 Storage 接驳（暂作为二阶段改进项）。
- **反例守护**: 未来如有人提议在 L0（`internal/observability/metrics/`）实现完整业务逻辑（如 SurpriseCalculator 的嵌入计算），引用本 ADR 拒绝——L0 必须保持纯净，复杂计算属 L1/L2；未来如有人提议 L1 直接 import L2 包（如 `internal/learning/`），引用本 ADR 拒绝，consumer-side 接口是唯一合法跨层路径。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 在 L0 metrics 包实现完整 SurpriseCalculator（含 embedding 计算） | 破坏 L0 纯净性；observability 包不应含业务逻辑，计算逻辑属 L2 |
| `internal/agent/` 直接 import `internal/learning/` 包 | 产生 L1→L2 非法跨层依赖，违反层级隔离；consumer-side 接口（`SurpriseReader`）是唯一合规跨层路径 |
| `CorpusStats` 持久化到 SQLite（实时 IDF 分数持久化） | Tier-0 内存约束下持久化开销不合理；CorpusStats 是近实时统计，节点重启后从内存重建代价可接受，列为二阶段改进 |

## 引用代码

- `internal/observability/metrics/metrics.go`（`SurpriseIndex` 结构体 + `NewSurpriseIndex`/`ComputeBasic`/`computeCosineDist`/`computeJaccardDist`，`sync.RWMutex` 并发安全，对应决策第 1 点）
- `internal/extension/skill/skill_pipeline.go`（`ScriptExecutor` consumer-side 接口，对应决策第 2 点）
- `internal/store/search/hybrid_retrieve.go`（`CorpusStats` 结构，对应决策第 3 点）
- `internal/protocol/interfaces.go`（`AgentController` 接口抽象，对应决策第 4 点）
- `internal/gateway/server/logstream.go`（SSE 侧通过 `AgentController` 解耦、`TriggerExecuteDone` 驱动，对应决策第 4 点）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-09 | 初稿，Accepted |
| 2026-06-13 | WasmTester/WasmExecutor → ScriptTester/ScriptExecutor（与 skill_pipeline.go 实际命名对齐） |
| 2026-06-17 | 修正文件路径：skill_pipeline.go → internal/extension/skill/skill_pipeline.go；hybrid_retrieve.go → internal/store/search/hybrid_retrieve.go |
| 2026-07-03 | 代码引用复核补全 |
| 2026-07-09 | 补充说明：ADR-0028 BUG-D 修复后，`SurpriseCalculator`（L2 learning 层）已接管主路径 cosine 计算，`ComputeBasic` 保留为 L0 备用路径 |
