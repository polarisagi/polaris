# ADR-0021: 核心机制实现（SurpriseIndex / ScriptTester / BM25 / FSM）

- **状态**: Accepted | **日期**: 2026-06-09 | **模块**: L0/L1 跨层

## 决策

四个占位符组件补全，严格遵守层级隔离：

1. **SurpriseIndex 单例**（L0 `internal/observability/metrics`）：cosine（embedding EMA）+ Jaccard（toolSeq）距离，`sync.RWMutex` 并发安全。
2. **ScriptTester 沙箱下沉**（L1→L0 接口）：`internal/extension/skill/skill_pipeline.go` 定义 consumer-side `ScriptExecutor` 接口，解耦认知层对 `internal/action` 沙箱实例的直接依赖。
3. **BM25 混合检索全局状态**（L0 `internal/store/search/hybrid_retrieve.go`）：`CorpusStats` 结构，读写锁包裹，近实时动态 IDF。
4. **Agent FSM 边界与 Gateway SSE 双向通信**（L3→L1）：`internal/protocol/interfaces.go` 抽象 `AgentController`，`logstream.go` 不再直接引用 `*kernel.Agent`。

## 反例守护

拒绝在 L0 metrics 包实现完整业务逻辑（如 embedding 计算）——L0 必须纯净。拒绝 L1 直接 import L2（如 `internal/learning/`）——consumer-side 接口是唯一合法跨层路径。

## 修订记录

2026-07-09：ADR-0025 BUG-D 修复后，`SurpriseCalculator`（L2）已接管主路径 cosine 计算，`ComputeBasic` 保留为 L0 备用路径。

## 引用代码

`internal/observability/metrics/metrics.go`、`internal/extension/skill/skill_pipeline.go`、`internal/store/search/hybrid_retrieve.go`、`internal/protocol/interfaces.go`
