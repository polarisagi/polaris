# ADR-0021: 核心机制实现（SurpriseIndex / ScriptTester / BM25 / FSM）

> 状态：Accepted ｜ 首次：2026-06-09 ｜ 最近复核：2026-10-12 ｜ 模块：L0/L1 跨层

## 决策：四个占位组件补全，严格层级隔离

1. **SurpriseIndex 单例**（L0 `internal/observability/metrics`）：cosine（embedding EMA）+ Jaccard（toolSeq）距离，`sync.RWMutex`。
2. **ScriptTester 沙箱下沉**（L1→L0）：`skill_pipeline.go` 定义 consumer-side `ScriptExecutor` 接口，认知层不直接依赖沙箱实例。
3. **BM25 混合检索全局状态**（L0 `hybrid_retrieve.go`）：`CorpusStats`，读写锁，近实时动态 IDF。
4. **Agent FSM 边界与 Gateway SSE**（L3→L1）：`internal/protocol/interfaces_agent.go` 抽象 `AgentController`，`logstream.go` 不再直接引用 `*kernel.Agent`。

## 反例守护

- L0 metrics 包实现完整业务逻辑（如 embedding 计算）— L0 必须纯净
- L1 直接 import L2（如 `internal/learning/`）— consumer-side 接口是唯一合法跨层路径

## 重新评估触发条件

- `ComputeBasic`（L0 备用路径）长期无调用场景可移除，需先确认无降级/离线场景依赖。2026-10 核实：`SurpriseCalculator`（L2，ADR-0025 BUG-D 后接管主路径）经 `SetSurpriseCalc` 注入，未注入时降级 `ComputeBasic`；`internal/execute/dag/executor_node.go` 仍直接调用，故保留
- 四条层级隔离边界不因业务需求松动；新增跨层直接 import 须先过 depguard

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-06-09 | 初版 |
| 2026-07-09 | ADR-0025 BUG-D 修复后 `SurpriseCalculator` 接管主路径 |

## 引用代码

`internal/observability/metrics/metrics.go`、`internal/extension/skill/skill_pipeline.go`、`internal/store/search/hybrid_retrieve.go`、`internal/protocol/interfaces_agent.go`、`internal/gateway/server/logstream.go`
