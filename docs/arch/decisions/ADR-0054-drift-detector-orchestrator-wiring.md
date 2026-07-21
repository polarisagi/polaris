# ADR-0054: DriftDetector 漂移响应编排器接线 + EmbeddingVersionTracker 范围订正

- **状态**: Accepted（已执行）
- **日期**: 2026-07-21
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/learning/surprise/`、`internal/memory/retrieval/`、
  `internal/memory/memory.go`、`cmd/polaris/boot_memory.go`、
  `internal/config/thresholds.go`

## 上下文

承接 ADR-0052 DEFER 条目："`surprise.DriftDetector` 全套（`NewDriftDetector`/
`AddAnchor`/`anchorCosineDist`/`scoreAnchors`/`Detect`/`EmbeddingVersionTracker.Update`）
实现完整且与 `M05-Memory-System.md §12.3` 设计逐字段吻合，但缺失整条'漂移响应
编排器'——周期性喂 anchor、调用 `Detect()`、按 task_type 降级 BM25、触发
Blue-Green 重嵌——这条编排链全仓库不存在"。

用户指令："剩下的也都要处理...只要需要的，都开发完成"。读 `docs/arch/
M05-Memory-System.md` §12.2-12.3 + `drift_detector.go` 全文后确认：原判定描述
的编排链缺口是真实的，且发现一个比原判定更细的问题——`Detect()`/`scoreAnchors()`
从未读取 `AnchorSample.TaskType` 字段，只做全局聚合，与 §12.3 降级表"该
task_type 降级纯 BM25，其余不受影响"的设计不符（原设计要求按 task_type 隔离
判断，实现却是全局判断）。

## 决策

### WIRE（本次实现）

**编排链**：`Search() 采样 anchor → DetectByTaskType() 按 task_type 评分 →
DriftDowngradeRegistry 同步降级状态 → Search() 读降级状态零权重 VectorWeight
→ 存在降级时尽力触发 OnlineReindexer 批次`。

1. `DriftDetector.DetectByTaskType()`（新增，`drift_detector.go`）：按
   `AnchorSample.TaskType` 分组后复用既有 `scoreAnchorsFiltered` 评分逻辑
   （从 `scoreAnchors` 提炼出可按 task_type 过滤的版本，`scoreAnchors` 本身
   保留作为全局聚合的向后兼容路径）。样本数 <5 的组跳过，避免小样本噪声。
2. `DriftDetector.RecordAnchor(taskType, query string, embedding []float32,
   expected []string)`（新增）：以原始参数追加 `AnchorSample`，语义等价于
   `AddAnchor(AnchorSample{...})`，供 L1 消费方以接口方法调用（见下方分层
   说明）。
3. `DriftDowngradeRegistry`（新文件 `drift_downgrade_registry.go`）：
   `IsDowngraded`/`SetDowngraded`/`ClearAll`/`Downgraded`，线程安全 map，
   承载"当前处于降级状态的 task_type 集合"这一最小跨模块状态。
4. `DriftOrchestrator`（新文件 `internal/learning/surprise/drift_orchestrator.go`）：
   `RunOnce` 每轮用当前 anchor 窗口重新评分并**覆盖**（而非"触发一次即假设
   已修复"）降级状态；存在新增降级 task_type 时尽力触发一次
   `OnlineReindexer` 批次（复用 `cmd/polaris.startOnlineReindexer` 已返回的
   同一闭包，与 `ModelRegistry.DeprecateModel` 同款触发模式）。降级解除
   依据下一轮 `Detect` 重新评分，不臆测"重嵌已完成"（R1：不构造无法从现有
   信号验证的结论）。`Start` 启动周期 ticker（默认 168h，可配置）。
5. `HybridRetrieverImpl.Search()`（`retriever.go`）新增两处逻辑：
   - Stage 0.6：`taskType := optimizer.ExtractTaskType(query)`（内部计算，
     不改 `RetrievalConfig` 签名，不影响调用方）。
   - RRF 权重计算处：`driftRegistry.IsDowngraded(taskType)` 为真时
     `VectorWeight` 归零（"降级纯 BM25"）。
   - Stage 5（`sampleDriftAnchor`，`retriever_helpers.go`）：采样率命中时
     以本次检索 Top-5 结果 Source 作为自参照 `Expected` 基线记录 anchor
     （漂移定义为"相对历史自身基线的偏离"，非外部标注真值）。
6. `internal/config/thresholds.go` `M5MemoryThresholds` 新增
   `DriftCheckIntervalHours`(168)/`DriftThreshold`(0.05)/
   `DriftAnchorSampleRate`(0.02)，`configs/threshold-examples/m5_memory.toml`
   已用 `make gen-threshold-examples` 同步。
7. `cmd/polaris/boot_memory.go`：`sb.Embedder != nil` 时构造
   `DriftDetector`/`DriftDowngradeRegistry`/`DriftOrchestrator` 并注入 `mem`、
   启动周期 goroutine；`sb.Embedder == nil`（纯 BM25 部署）时整套跳过——
   向量空间漂移问题域本就不存在于纯 BM25 路径。

### 架构分层修正（HE-3/R1.7）

初版实现直接在 `internal/memory`（L1）import `internal/learning/surprise`
（L2），被 `Test_inv_NoCrossLayerImport` 拦截（L0←L1←L2←L3 单向依赖，低层
禁止反向 import 高层）。修正为消费方接口模式：

- `internal/memory/retrieval` 本地声明 `DriftAnchorRecorder`/`DriftGate`
  两个只含所需方法签名的接口（`retriever_construct.go`），`HybridRetrieverImpl`
  持有接口类型字段而非 `*surprise.DriftDetector`/`*surprise.DriftDowngradeRegistry`
  具体类型。
- `*surprise.DriftDetector.RecordAnchor`/`*surprise.DriftDowngradeRegistry.IsDowngraded`
  方法签名与上述接口精确匹配，`cmd/polaris`（组合根，不受分层限制）构造
  具体实例后直接传入满足接口。
- `DriftOrchestrator` 改放 `internal/learning/surprise`（L2）而非
  `internal/memory/retrieval`（L1）：其 `reindex` 触发参数是裸函数类型
  `func(context.Context)(int,bool,error)`，不需要 import `retrieval` 包，
  放在 L2 不产生依赖，且 L2→L1 方向本就合法（若确实需要引用 retrieval 类型）。
- `internal/memory/memory.go` 的 `InjectDriftDetector`/`InjectDriftRegistry`
  委托方法同步改为接口参数类型，不再 import `surprise`。

### 测试

新增 `internal/learning/surprise/drift_detector_test.go`（`DetectByTaskType`
按 task_type 分组隔离、小样本组跳过、`RecordAnchor` 等价性、
`DriftDowngradeRegistry` 全部方法）、`drift_orchestrator_test.go`
（降级+触发重嵌、漂移消失后自动恢复、nil-safe）、
`internal/memory/retrieval/drift_wiring_test.go`（注入接线、
`sampleDriftAnchor` 采样/截断/各类跳过条件）。

### 保留但订正范围：EmbeddingVersionTracker.Update 不在本次接线范围内

`deadcode` 复查后 `EmbeddingVersionTracker.Update` 仍未被引用。原 ADR-0052
把它与 DriftDetector 编排链缺口归为同一条 DEFER，但读代码后确认这是一个
**功能上独立**的问题：`EmbeddingVersionTracker` 是"跨 embedding 版本混合检索
时的分数归一化"机制（`M05-Memory-System.md` §12.3："每索引维护
P50/P95/P99/Min/Max 滚动统计，跨版本检索走 min-max 归一化 → RRF 融合"），
用于 Blue-Green 重嵌过渡期——部分行是旧版本 embedding、部分行已重嵌为新
版本，两者原始 cosine 分数尺度不可比，需按版本归一化后才能公平参与 RRF
（呼应 inv_M5_05"RRF 融合不裸加权"）。

这与本次接好的"漂移检测→降级→触发重嵌"编排链是两套独立机制，不能顺带
接入，原因：

1. `protocol.CognitiveSearcher.VecUpsert(id string, embedding []float32) error`
   （SurrealDB HNSW 写入接口）没有版本参数——Tier1+ 主路径的向量索引本身
   不携带 per-vector embed_model_version，`VecKNN` 返回的
   `types.CognitiveSearchResult` 命中也不带版本标签。
2. Tier0 SQL 回退路径（`fetchVectorResultsFromSQL`）的查询语句
   `SELECT content, embedding FROM episodic_events ...` 同样没有选出
   `embed_model_version` 列，尽管该列在表中存在（`OnlineReindexer` 用它
   判断是否需要重嵌）。
3. 真正接好 `EmbeddingVersionTracker` 需要：（a）扩展
   `protocol.CognitiveSearcher`/`types.CognitiveSearchResult` 携带版本
   标签（跨 Go/Rust FFI 的 SurrealDB 存储层改动），（b）Tier0 SQL 查询补
   selectembed_model_version 并线程穿透到 `ScoredFragment`，（c）在 RRF
   融合前按版本查表做 min-max 归一化。这是至少涉及一个共享跨层接口签名
   的架构变更，不是"一次调用点接线"，勉强只做 Tier0 半覆盖会造成 Tier1+
   主路径（生产推荐配置）完全不覆盖的名不副实状态（R1：不构造"看起来已接入"
   但实际只覆盖降级路径的实现）。

判定：**保留 DEFER**，但订正理由——不是"缺编排链"（编排链本次已解决），而是
"跨版本分数归一化需要先扩展 `CognitiveSearcher` 接口才能做，属独立架构变更"。

## 判断依据

延续 R1：只对"真实实现已存在、只缺一处可定位桥接逻辑"的情形做 WIRE。
DriftDetector 编排链符合——`Detect`/`AddAnchor`/`AnchorSample` 均已按文档
逐字段实现，只缺消费方（Search 的 anchor 采样 + 降级读取）与驱动方
（周期性 orchestrator）。`EmbeddingVersionTracker` 不符合——其"归一化"
职责依赖的版本标签在现有存储接口里根本不存在，接入前必须先做接口扩展，
这是设计问题而非桥接问题。

## 后果

- **正向**：`go build`/`go test ./...`/`golangci-lint`/
  `make gen-threshold-examples` 全绿；`deadcode` 确认
  `DriftDetector`/`DetectByTaskType`/`RecordAnchor`/`DriftDowngradeRegistry`/
  `DriftOrchestrator` 全部可达；纯 BM25 部署（<8GB VPS）不受影响
  （`sb.Embedder == nil` 时整套跳过）。
- **负向**：`EmbeddingVersionTracker.Update` 仍是 deadcode，需要独立的
  `CognitiveSearcher` 接口扩展设计（含 SurrealDB Rust 侧改动）才能真正接入，
  本次不展开。

## 引用代码

- `internal/learning/surprise/drift_detector.go`（`DetectByTaskType`/`RecordAnchor`）
- `internal/learning/surprise/drift_downgrade_registry.go`（新增）
- `internal/learning/surprise/drift_orchestrator.go`（新增）
- `internal/memory/retrieval/retriever_construct.go`（`DriftAnchorRecorder`/`DriftGate` 接口）
- `internal/memory/retrieval/retriever.go`/`retriever_helpers.go`（Stage 0.6/RRF 降级/`sampleDriftAnchor`）
- `internal/memory/memory.go`（`InjectDriftDetector`/`InjectDriftRegistry` 委托）
- `cmd/polaris/boot_memory.go`（构造 + 注入 + 启动）
- `internal/config/thresholds.go`（`DriftCheckIntervalHours`/`DriftThreshold`/`DriftAnchorSampleRate`）
- `internal/protocol/interfaces_memory.go`（`CognitiveSearcher.VecUpsert` 无版本参数，
  `EmbeddingVersionTracker` 阻塞点）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-21 | 初稿：DriftDetector 编排链接线完成；EmbeddingVersionTracker 订正为独立 DEFER（需先扩展 CognitiveSearcher 接口） |
