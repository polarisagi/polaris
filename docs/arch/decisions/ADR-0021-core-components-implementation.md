# ADR-0021: 核心机制实现（SurpriseIndex / WasmTester / BM25 / FSM）

**状态**: 已接受 (Accepted)  
**日期**: 2026-06-09  

## 背景
在推进 Polaris AGI 的核心能力时，需要补全四个占位符组件，同时严格遵守 Tier-0 架构的硬件门槛与模块间的隔离纪律（防止循环依赖和超前抽象）。

## 决策

1. **SurpriseIndex 的单例与状态收敛 (L0)**
   - **实现机制**：在 `pkg/substrate/observability/metrics.go` 实现 `SurpriseIndex` 的基础版本。计算过程结合了基于 `embedding` 的 Cosine 距离（采用 EMA 平滑）以及基于 `toolSeq` 的 Jaccard 距离。
   - **并发设计**：使用 `sync.RWMutex` 确保该单例在高并发情况下的读写安全。这不仅满足了可观测的一等公民诉求，同时也提供了 S_PERCEIVE 阶段无思考触发 (FastPath) 的路由基础。

2. **WasmTester 与沙箱能力下沉 (L1 -> L0 接口)**
   - **实现机制**：在 `pkg/cognition/skill_pipeline.go` 定义 Consumer-side `WasmExecutor` 接口。
   - **解耦逻辑**：解耦了认知层对 `pkg/action` Wasm 沙箱实例的直接依赖。`WasmTester` 提供运行时冷启动时间以及内存消耗测试，满足可观测原则与隔离要求。

3. **BM25 混合检索中的全局状态统计 (L0)**
   - **实现机制**：在 `pkg/substrate/hybrid_retrieve.go` 引入了 `CorpusStats` 结构。动态记录总文档数、分词频次、平均长度。
   - **架构对齐**：使用 Go 读写锁包裹状态更新，移除了外部复杂依赖，在内存约束极高的 Tier-0 约束下，完成快速近实时动态 IDF 分数反馈。

4. **Agent FSM 边界与 Gateway SSE 的双向通信 (L3 -> L1)**
   - **实现机制**：在 `internal/protocol/interfaces.go` 抽象 `AgentController`。`Agent` 实现了该接口并在内部完成事件注入流转 (`SendIntent` / `SetTaskIntent`)。
   - **解耦逻辑**：`pkg/gateway/server/sse.go` 再也无需直接引用 `*kernel.Agent`。在会话注入 Intent 时异步驱动 FSM 扭转；并且 SSE Stream 可通过 FSM `CurrentState()` 监测生命周期，FastPath 完成后由 SSE 发起 `TriggerExecuteDone` 推演。

## 后果
- **正面**：成功串联了四大模块（观测、沙箱、检索、状态机）。消除了模块循环依赖的隐患，并且完全维持 8GB 以下内存的 Tier-0 可观测标准和错误处理策略 (`internal/errors`)。
- **负面**：`CorpusStats` 状态缓存在内存中存在节点宕机数据丢失的风险；未来在需要持久化或跨节点扩容时，需要进一步与 M2 Storage 接驳（暂作为二阶段改进项）。
