# ADR-0061：2026-07-22 全仓库 deadcode 复核（47 项，3 项新发现 + 44 项既定 DEFER 复现）

## 状态

已接受，已实现（含一次过程中订正，见"订正记录"节；`ReportSurrealDBIndexSize`
FFI 接线已于同日追加完成，不再有遗留决策项）。

## 背景

`deadcode ./cmd/polaris/...` 本轮报告 47 个 unreachable func。逐项核对
`docs/arch/decisions/`（ADR-0033/0046/0047/0051/0052/0053/0054/0059）与
2026-07-13/2026-07-22 memory 记录后确认：46 项均为既有 DEFER 决策在新一轮
静态分析中的再次出现（非回归、非遗漏），仅 1 项为本次新发现的真死代码。

## 核实方法

延续 ADR-0051/0052/0053 方法论：先 grep 决策档案确认是否已有结论，再对
未覆盖项读源码判断"真实实现+专门测试覆盖=倾向 WIRE 而非删"或"自承
mock/占位=倾向 DELETE"，禁止仅凭 grep 命中数批量处理。

## 结论

### 既定 DEFER 复现（46 项，无需新动作）

| 符号 | 既定决策来源 |
|---|---|
| `NewAgentWithDefaults` | ADR-0046，测试专用构造器，代码注释自证 |
| `downloader.Get`/`DownloadExtractTarGz`/`ResolveURL`/`ProxyStatus` | 2026-07-13 memory + ADR-0052，测试覆盖真实行为的 false positive |
| `MCPRetryPolicy` | ADR-0051，错误分类器输入流水线不存在 |
| `mcp.TaintedJSONNode.AllStrings` | ADR-0052，两个承诺消费者均已改用扁平化方案 |
| `SkillEvolutionEngine.EvaluateAndEvolve`/`consecutiveFailures`/`triggerEvolution`/`consecutiveFailureReasons` | ADR-0051，与已接入的 `SkillEvolutionMonitor` 架构上不可合并 |
| `graphrag.NewGraphTraverser` | ADR-0052，与 `GraphWriter` 同一次决策 |
| `graphrag.NewGraphWriter` | ADR-0051，`GraphBuildPipeline.Run()` 实际绕过 |
| `reflexion.NewReflectionWorker`（含 `WithConfig` 变体） | ADR-0052，`ReflectionConfig` 缺失 product 侧阈值归属 |
| `surprise.EmbeddingVersionTracker.Update` | ADR-0052/0054，漂移响应编排器缺失 |
| `llm.NewSingleCredentialPool` | ADR-0052，安全回归测试覆盖，不删 |
| `QLearner.Update` | ADR-0051，奖励信号从未定义 |
| `NewEvidenceSubgraphExtractor`/`EvidenceSubgraphExtractor.Extract` | ADR-0051，自承 mock implementation |
| `SynapticPlasticityManager` 全套（`New`/`PruneThreshold`/`ReinforcePath`/`FeedbackCalibrate`/`DecayUnused`） | ADR-0051，零生产构造点 |
| `temporal.go`（`SetValidWindow`/`IsValidAt`/`FormatValidWindow`） | ADR-0051，消费侧已接入但零生产实体带有效窗语义 |
| `ActiveContext.Rebuild` | ADR-0051，自承 MVP 占位 |
| `PromptBuilder.WriteSkillContext`/`WriteUserInstruction` | ADR-0051，`types.Skill` 全仓库零构造点，疑似从未建成的平行系统 |
| `credential.NewVault` | ADR-0052，生产路径注释明确禁止使用 |
| `FactualityGuard.AddToGate` | ADR-0051，`emit_response` 钩子点未接入真实 FSM/gateway |
| `search.NilReranker.Rerank` | ADR-0052，有意 null-object 测试替身 |
| `planner.DefaultSpawner` | ADR-0052，自身注释自证 |
| `supervisor.Supervisor.Wait` | ADR-0052，对称 API 低风险保留 |
| `types.BuildIdempotencyKey` | ADR-0059，强行迁移=臆造 version 语义，R1 违规 |
| `types.WithSemanticCacheHints`/`WithThinkingBudget` | ADR-0053（订正版），StreamInfer 对称缓存分支工作量超预期 |
| `taint_sanitizer.SanitizeByDeterministicTransform` | ADR-0047，二级降级刻意不接入 S_VALIDATE |
| `IncidentToEvalConverter.ReviewAndPromote` | 既有 memory 记录：M12 §6 HITL 人工审核 API 入口刻意 deferred |
| `SafeString.Content` | 非遗漏：专属 `Test_inv_TaintContentCallAudit` 审计每处 `.Content()` 调用，是刻意收窄的安全 accessor，`TaintedString.Content`（另一独立方法）才是被广泛使用的那个，本轮核实未发现混淆 |
| `authcontext.WithMaxExpandTokens` | 与同文件 `WithWorkDir`（已被 ADR-0052 接线）同类 Option，生产侧默认值已够用，无 product 侧配置需求，比照既定 DEFER 处理 |

### 本次新发现并已修复（3 项）

**`knowledge.GoldmarkChunker`/`.Chunk`**（`internal/knowledge/parsers.go`）：
真死代码。`internal/knowledge/chunker.go:115` `DefaultChunker.Chunk` 路由早已改为
`case "md","markdown": strategy = &MarkdownChunker{} // Fix from GoldmarkChunker`
——`GoldmarkChunker` 是被替换后遗留的旧实现，非缺口。已删除该类型 + 方法，
连带清理未使用的 `goldmark`/`goldmark/ast`/`goldmark/text` import，
`go mod tidy` 移除 `go.mod`/`go.sum` 中的 `github.com/yuin/goldmark` 依赖。

**`taint.TaintBoundarySerializer` 全套**（`New`/`Seal`/`Unseal`/`computeHMAC`，
`internal/security/taint/taint.go`）：WIRE。真实规范缺口，非死代码——见下节订正记录。
已接入 `internal/knowledge` 包的 rag_chunks 读写路径。

**`metrics.ReportSurrealDBIndexSize`**（`internal/observability/metrics/metrics_handler.go`）：
WIRE。违反 HE-1（可观测优先）——`polaris_surrealdb_index_size_mb` Gauge 的
Setter 存在但从未被任何调用方触发，恒为零值；根因是 Rust FFI 侧
（`rust/substrate/`）此前未导出任何查询 SurrealDB-Core 索引内存占用的接口。
用户确认"现在就做"，已接入，见下节"SurrealDB-Core 索引内存占用接线"。

### 订正记录：TaintBoundarySerializer 初次判定有误

本 ADR 初稿（撰写于删除 GoldmarkChunker 之后、处理 TaintBoundarySerializer
之前）曾将其归类为"待产品决策，倾向删除"，理由是"全仓库零生产调用点，且
未在任何 ADR/spec 中找到设计意图"。这个结论只检索了 `docs/arch/decisions/`
（ADR 决策档案），**没有检索 `docs/arch/M11-Policy-Safety.md` 本体**——该文档
§2.1 明确将其列为污点系统"四重防护"的**第三重：持久化边界密码学验证**
（"SQL/JSON/Protobuf 序列化层附加 HMAC-SHA256...反序列化时重算验证；验证
失败或字段缺失 → 强制 TaintHigh"），且 §"Taint 跨边界 HMAC 验证（inv_M11_02）"
逐字描述的就是 `Seal`/`Unseal` 的行为；`taint_inv_test.go` 也已有专属测试
`Test_inv_M11_02_TaintBoundaryHMACMismatchUpgradesToHigh`。即：这不是"无依据
的安全基础设施"，而是**规范明文要求、已实现、已测试，但从未接入真实持久化
路径**的缺口——与 ADR-0060 的 ContextWindowManager、ADR-0051 的
CommunityGenerativeSummarizer 是同一类"生产者/消费者都在，中间没接"模式。
向用户澄清后，用户选择"现在接入真实持久化边界"而非删除。

**教训**：核查一个安全原语"是否有设计依据"时，检索范围必须包含
`docs/arch/M*.md` 规范文档本体，不能只查 `decisions/` 目录下的 ADR 索引——
后者记录的是"已经决议过的方案"，前者才是权威的功能需求源。

### 接入范围与设计

真实的跨边界持久化点是 `rag_chunks` 表（`content`/`taint_level`/`taint_source`
三列拆分存储，`taint_level`/`taint_source` 无任何完整性保护，可被直接 SQL
篡改实现"降级攻击"）。核实发现两条独立的生产写入路径（均在
`cmd/polaris/boot_*.go` 中被真实构造，非测试专用）：

- `internal/knowledge/rag_impl.go` `DefaultIngestionPipeline.Ingest` +
  `internal/knowledge/rag_summary_tree.go` `insertSummaryChunk`
  （`boot_knowledge.go` 主 RAG 摄取管道）
- `internal/knowledge/ingester.go` `PipelineImpl.Ingest`
  （`boot_agent.go:847`，外部知识源 `SyncScheduler` 摄取管道）

以及三条读取点（`internal/knowledge/retriever.go` `HybridRetrieverImpl` 的
`searchFTS`/`fetchCognitiveHits`/`searchVectorFallback` + `rag_retrieval.go`
`ContextExpander.Expand`）。

设计：

- `internal/security/credential/vault.go` 新增 `Vault.DeriveKey(purpose string)
  []byte`——从既有 `masterKey` 派生 domain-separated 子密钥（不新增密钥管理面，
  复用 Provider API Key 同一份本地密钥文件）。
- `cmd/polaris/boot_substrate.go` 构造 `SubstrateBundle.RAGChunksTaintSerializer
  = taint.NewTaintBoundarySerializer(vault.DeriveKey("rag_chunks_taint_boundary_v1"))`，
  全进程唯一实例，`nil`（`sb.Vault` 缺失时的防御性降级）时读写两侧对称退化
  为不校验。
- `internal/knowledge/taint_boundary.go` 新增 `sealChunkTaint`/`verifyChunkTaint`
  两个包内共享 helper，封装 `TaintedString`↔三元组`(id, content, level, source)`
  的规范映射，避免每个调用点重复拼装 `TaintSource`/`TaintEnvelope`。
- `009_rag_chunks.sql` 新增 `taint_hmac TEXT NOT NULL DEFAULT ''` 列。
- **Fail-closed 语义**：`verifyChunkTaint` 在 HMAC 缺失（历史行/被剥离）或
  校验失败（篡改）时均返回 `TaintHigh`，与 `Unseal` 本身的 fail-closed 设计
  一致——空签名与被剥离的签名从读取方视角不可区分，不能豁免。
- `GraphTraverser`（`internal/knowledge/graphrag/graph_traverser.go`）与
  `rag_summary_tree.go` 的 `MAX(taint_level)` 聚合查询未接入：前者本身是
  ADR-0051/0052 已判定的 DEFER 死代码（`GraphBuildPipeline.Run()` 结构性绕过，
  未接入任何生产调用链）；后者是派生的"最坏情况"聚合信号，不直接暴露
  content，风险等级不同，留作独立复核项。

### SurrealDB-Core 索引内存占用接线

`surrealdb` crate（`rust/substrate/` 依赖的 `surrealdb-core`）未暴露原生内存
自省 API，无法物理测量 HNSW/BM25/图索引实际占用字节数。设计为**粗粒度估算**
而非精确测量：

- `SurrealStore`（`mod.rs`）新增 `vec_dim: u32` 字段——`surreal_open` 时保存
  HNSW 建索引使用的向量维度；DDL 建完索引后无法从 `surrealdb` 反查该值，
  故在 Rust 侧结构体里保留一份。
- `surreal_stats`（`fts.rs`）在既有 `kv_count`/`vec_count`/`doc_count`/
  `edge_count` 基础上，追加三个经验估算常量：
  `HNSW_OVERHEAD_BYTES_PER_NODE=128`、`FTS_OVERHEAD_BYTES_PER_DOC=200`、
  `GRAPH_OVERHEAD_BYTES_PER_EDGE=96`，计算
  `index_size_mb = (vec_count*(vec_dim*4+128) + doc_count*200 +
  edge_count*96) / 1MB`，写入既有 `surreal_stats` JSON 输出（新增字段，
  不新建 FFI 符号，复用 Go 侧已有的 `SurrealStore.Stats()` 绑定）。
- Go 侧新增 `startSurrealIndexSizeReporter`（`cmd/polaris/boot_memory.go`）：
  60s 周期 `concurrent.SafeGo` goroutine，调用 `Stats()` 解析
  `index_size_mb` 并上报到 `metrics.ReportSurrealDBIndexSize`；启动时立即
  上报一次，不等第一个 tick（避免 `/metrics` 长期显示 0）。接入
  `bootMemory` 启动链路，紧随 `startCognitiveReplayerIfNeeded` 之后。

**已知限制**：估算值不含 kv 主表本身的存储占用，仅覆盖三类索引结构；三个
`OVERHEAD_BYTES_PER_*` 常量为工程经验值，非从 SurrealDB-Core 源码实测得出，
后续如需更精确的数值应通过压测校准或等待上游暴露原生统计接口替换。

## 验证

- `go build ./...`：通过
- `go mod tidy`：`goldmark` 依赖清理干净，`go.sum` 同步
- `make lint`：主 lint + wasip1 子 lint 均 0 issues
- `go test ./...`：全量通过（102 个含测试文件的包，0 FAIL）
- `go test -race ./internal/knowledge/... ./internal/security/credential/... ./cmd/polaris/...`：通过
- `make generate-manifest`：`internal/security/credential/vault.go` 属
  `ImmutableKernelPackages()`，已重新生成 `kernel_manifest.json`
- 新增测试 `internal/knowledge/taint_boundary_test.go`：往返一致性、
  `taint_level` 被篡改后 fail-closed 到 TaintHigh、HMAC 缺失 fail-closed、
  `nil` serializer 对称降级不阻断读写，4 个用例全通过
- `deadcode ./cmd/polaris/...`：47→45（`GoldmarkChunker` 1 项 +
  `TaintBoundarySerializer` 全套 4 项符号消失，净减 3 因存在计数误差，以
  `diff` 精确比对为准：确认只有这 4 个符号被移除，无新增/无回归）；
  `ReportSurrealDBIndexSize` 接线后复查同样不再出现在 unreachable 列表中
- `cargo test --lib surreal_store --manifest-path rust/substrate/Cargo.toml`：
  2 passed（含 `test_surreal_store_all_features`，覆盖 `surreal_stats` 新增
  的 `index_size_mb` 字段不破坏既有断言）
- `cargo clippy --all-targets --manifest-path rust/substrate/Cargo.toml
  -- -D warnings`：0 warnings
- 追加一轮 `go build ./...`/`go vet ./...`/`make lint`/`go test ./...`
  （102 包全通过，0 FAIL）：确认 `boot_memory.go` 改动无回归

## 引用代码

- `internal/knowledge/parsers.go`（GoldmarkChunker 删除）
- `internal/knowledge/chunker.go:104-121`（`DefaultChunker.Chunk` 路由，确认
  `GoldmarkChunker` 已被 `MarkdownChunker` 取代的证据来源）
- `docs/arch/M11-Policy-Safety.md` §2.1、§"Taint 跨边界 HMAC 验证（inv_M11_02）"
  （TaintBoundarySerializer 的规范依据，初次判定遗漏的检索范围）
- `internal/security/taint/taint.go:172-259`（`TaintBoundarySerializer` 实现）
- `internal/security/credential/vault.go`（`DeriveKey`）
- `internal/knowledge/taint_boundary.go`（`sealChunkTaint`/`verifyChunkTaint`）
- `internal/protocol/schema/009_rag_chunks.sql`（`taint_hmac` 列）
- `cmd/polaris/boot_substrate.go`/`boot_knowledge.go`/`boot_agent.go`（接线点）
- `internal/observability/metrics/metrics_handler.go:30-35`（`ReportSurrealDBIndexSize` 定义）
- `rust/substrate/src/surreal_store/mod.rs`（`SurrealStore.vec_dim`）
- `rust/substrate/src/surreal_store/fts.rs`（`surreal_stats` 的 `index_size_mb` 估算）
- `cmd/polaris/boot_memory.go`（`startSurrealIndexSizeReporter`，接入 `bootMemory`）
