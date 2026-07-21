# ADR-0061：2026-07-22 全仓库 deadcode 复核（47 项，1 项新发现 + 46 项既定 DEFER 复现）

## 状态

已接受，部分已实现。

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

### 本次新发现并已修复（1 项）

**`knowledge.GoldmarkChunker`/`.Chunk`**（`internal/knowledge/parsers.go`）：
真死代码。`internal/knowledge/chunker.go:115` `DefaultChunker.Chunk` 路由早已改为
`case "md","markdown": strategy = &MarkdownChunker{} // Fix from GoldmarkChunker`
——`GoldmarkChunker` 是被替换后遗留的旧实现，非缺口。已删除该类型 + 方法，
连带清理未使用的 `goldmark`/`goldmark/ast`/`goldmark/text` import，
`go mod tidy` 移除 `go.mod`/`go.sum` 中的 `github.com/yuin/goldmark` 依赖。

### 需要产品/架构侧决策，本次未处理（2 项，不在既定 DEFER 记录中）

| 符号 | 现状 | 待决策点 |
|---|---|---|
| `taint.TaintBoundarySerializer` 全套（`New`/`Seal`/`Unseal`/`computeHMAC`） | 完整实现的 HMAC-SHA256 跨边界污点信封序列化器，仅自身测试覆盖，全仓库零生产调用点，且未在任何 ADR/spec 中找到设计意图或消费方引用 | 是否有计划中的跨进程/跨节点边界需要它（如 Wasm 沙箱 host↔guest、未来 swarm 跨节点通信）？若无明确消费方，按 R1（禁止无依据发明安全语义）应删除，等真实需求出现再实现 |
| `metrics.ReportSurrealDBIndexSize` | Setter 从未被调用；Rust FFI 侧（`rust/substrate/`）未发现任何查询 SurrealDB-Core 索引内存占用的导出函数 | 违反 HE-1（可观测优先），但修复需要先在 Rust 侧新增 FFI 导出，非一行 Go 接线；是否现在做需要排期确认 |

## 验证

- `go build ./...`：通过
- `go mod tidy`：`goldmark` 依赖清理干净，`go.sum` 同步
- `make lint`：主 lint + wasip1 子 lint 均 0 issues
- `go test ./...`：全量通过（仅无测试文件的包，无 FAIL）
- `deadcode ./cmd/polaris/...`：47→46，仅 `GoldmarkChunker` 消失，其余 46 项
  均为既定 DEFER 的预期复现，无新增/无回归

## 引用代码

- `internal/knowledge/parsers.go`（本次编辑）
- `internal/knowledge/chunker.go:104-121`（`DefaultChunker.Chunk` 路由，确认
  `GoldmarkChunker` 已被 `MarkdownChunker` 取代的证据来源）
- `internal/security/taint/taint.go:184-260`（`TaintBoundarySerializer`，待决策）
- `internal/observability/metrics/metrics_handler.go:30-35`（`ReportSurrealDBIndexSize`，待决策）
