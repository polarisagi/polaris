# ADR-0051: 跨模块死代码清理与悬空接线收尾（Phase 1-4）

- **状态**: Accepted（已执行）
- **日期**: 2026-07-14
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/memory/`、`internal/knowledge/`、`internal/prompt/`、`internal/action/lam/`、
  `internal/learning/curriculum/`、`internal/llm/`、`internal/extension/marketplace/`、
  `internal/agent/`、`internal/security/guard/`、`internal/tool/`

## 上下文

`local_playground/reports/open-issues-20260714.md` + `deadcode ./cmd/polaris/...`
复核收尾（承接 ADR-0050 删除 orchestrator/topology 簇之后的剩余条目）。用户明确要求：
不受既有架构文档/ADR 约束，纯以系统最优第一性原理处理全部剩余问题；需要设计的完成
设计后实现；不留技术债、不留遗漏遗留。本 ADR 汇总四个执行阶段的全部决策。

## 决策

### Phase 1：删除（幻影档位构造函数 + 已被淘汰的并行实现）

| 文件 | 删除内容 | 理由 |
|------|---------|------|
| `memory/retrieval/retriever_construct.go` | `NewHybridRetrieverWithGraph`/`NewHybridRetrieverWithDurative` | "有 graph 无 cognitive" 组合在启动装配下结构性不可达：graph 与 cognitive 均派生自同一个 `sb.SurrealStore` |
| `memory/memory_system.go` | `NewMemorySystem`/`NewMemorySystemWithGraph` | 同上；唯一生产构造为 `NewMemorySystemFromMemImpl` |
| `memory/memory.go` | `NewMemImplWithGraph` | 同上幻影档位 |
| `memory/store/episodic_mem.go` | `NewEpisodicMemWithGraph` | 同上 |
| `internal/prompt/builder.go` | 整文件（`PromptBuilder`/`Zone*`/`NewPromptBuilder`） | 零消费者；真实调用方全部使用 `protocol.NewPromptBuilder()`/`protocol.Zone*`，是完全独立的重复实现 |
| `knowledge/retriever.go` | `NewHybridRetriever`/`NewHybridRetrieverWithEmbedder`/`NewHybridRetrieverWithGraph` | 唯一生产构造为 `NewHybridRetrieverWithCognitive`（`boot_knowledge.go:50`） |
| `memory/consolidation/consolidation.go` | 基础版 `NewConsolidationPipeline` | 唯一生产构造为 `NewConsolidationPipelineFull`（`boot_tools.go:426`） |
| `action/lam/continuous_action.go` | `ActionDiscretizer`/`ActionProjector`/`Discretize`/`cosineSim` 等 | 自承"[接口预留]"投机代码，零生产调用点，违反 R1 |
| `learning/curriculum/calibrator.go` | 整文件（`DynamicDifficultyCalibrator`/`CoEvolutionCoordinator`/`AutoConfigOptimizer`） | 与 `internal/prompt/optimizer/memf.go` 的同名 `DynamicDifficultyCalibrator`（真实、被使用）完全并行重复，本文件零调用点 |
| `learning/curriculum/curriculum.go` | `WithBlackboard`/`CurriculumGeneratorAdapter` | 与 `reflexion.CurriculumBridge`（真实，`boot_agent.go:557` 接入）并行重复，本身零调用点 |

判断依据与 ADR-0050 一致：均为"同一问题两套实现，仅一套被真实驱动"模式，删除较劣
（未使用）一侧，不做合并——合并只会把从未验证过的代码路径继续背在已验证路径旁边。

### Phase 2：简单接线修复

| 文件 | 修复内容 | 理由 |
|------|---------|------|
| `llm/adapter/stream.go`、`anthropic_request.go` | tool-call JSON marshal 失败时不再 `payload, _ := json.Marshal(...)` 静默丢弃，改为 `json.Valid` 校验 + `JSONRepair` 恢复 + `StreamError` 事件上报 | 静默丢弃违反 HE-1 可观测优先 |
| `llm/adapter/test.go` → `adapters_infer_test.go` | 重命名（原文件名不带 `_test.go` 后缀，`go test` 从未真正跑过） | 重命名后意外暴露真实生产级安全 bug（见下） |
| `llm/credential_pool.go` | `PooledCredential.CredFn()`/`CredentialPool.CredFn()` 从 `return c.key` 改为返回防御性拷贝 | **安全 bug**：调用方 `defer ClearBytes(apiKey)` 原意清零"本地副本"，实际清零的是池内共享凭证源，导致后续请求使用被清零的 key（Google adapter 因 key 嵌入 URL query string 而报错暴露，Bearer-header 类 provider 静默发送损坏 header） |
| `cmd/polaris/server_provider_loader.go` | `anthropic` 分支补 `WithAnthropicPromptCaching()` | 此前遗漏该 Option，Prompt Caching 从未启用 |
| `llm/router.go`/`router_failover.go`/`router_stream.go` | 接入 `modelregistry.Registry.RecordCallResult`，新增 `InjectModelRegistry` | `ModelVersionRegistry`（`schema/033`）此前只写不读，路由决策未消费模型版本健康度 |
| `gateway/server/server_routes.go` | 补 `POST /v1/eval/run` 路由 | `handleEvalRun` 已实现但路由未注册，接口不可达 |
| `boot_substrate.go` | 补 `slog.Info(autoConf.Summary())` | 硬件探针 Summary 已生成但从未记录，排障时无法追溯启动期分级依据 |
| `gateway/server/sysadmin/tools.go` | `BuildToolSchemas` 接入 `mcp.IsValidLLMName` 过滤 | 部分 MCP 工具名不满足 LLM function-calling 命名约束，此前未过滤会导致下游 provider 报错 |
| `boot_agent.go` + `security/audit_trail.go` + `learning/curriculum/curriculum_scheduler.go` | 新增 `auditTrailLogAdapter` 桥接 `dispatch.AuditLogger` → `security.AuditTrail.RecordAudit`；`BackgroundTaskScheduler` 新增 `audit()` helper，接入 founding-anchor 冻结与 red-team 探针失败两处审计点 | `RecordAudit` 此前文档声称实现 `protocol.AuditLogger`（doc-comment drift，已订正），且课程调度器的关键安全事件仅走 `slog.Error`，未落审计表 |
| `memory/memory.go` | `NewWorkingMemWithDB` → `NewWorkingMemWithBudget`（Tier0=8000, Tier1=32000） | 激活此前休眠的溢出压缩到 EpisodicMem（`AppendAndPage`） |

### Phase 3：安全防线接线

- `internal/agent/agent.go`：`Agent` 新增 `anomalyFilter *guard.AnomalyDistanceFilter`
  字段（`NewAgent` 默认构造，按会话隔离）。
- `internal/agent/agent_execute_util.go`：`withTaskScopeCtx` 新增
  `ctx = context.WithValue(ctx, protocol.CtxAnomalyFilterKey{}, a.anomalyFilter)`，
  与既有 `CtxTaskIDKey{}`/`CtxAgentIDKey{}` 传播模式一致。
- `internal/tool/tool.go`：`ExecuteTool` 成功路径接入
  `f.Record(name, []float64{float64(len(execInput))})`，为 OWASP LLM08
  输入异常检测（M11 §2.2）提供训练信号。
- `internal/security/guard/factuality_guard.go`：**调研后判定 DEFER**。
  `AddToGate` 的 `MatchFn` 检查 `action != "emit_response"`，但全仓库无第二处引用
  `"emit_response"` 这个 Cedar action 字符串——该钩子点本身在真实 FSM/gateway
  响应流水线中尚不存在。接入需要新设计一个响应拦截点（含延迟/UX/fail-open vs
  fail-closed 权衡），非一行接线可完成，不强行接入。

### Phase 4：中大型设计项调研结论

| 条目 | 判定 | 理由 |
|------|------|------|
| `KnowledgeConflictArbiter` | **WIRE（已执行）** | `boot_knowledge.go` 此前恒传 `nil`，`KnowledgeBase.Search` 冲突仲裁分支永久跳过；构造函数零依赖，直接激活（`knowledgepkg.NewKnowledgeConflictArbiter()`） |
| `MCPMarketplaceClient.Install`/`WithInstaller` | **WIRE（已执行）** | `mktClient.Install` 是完整实现（校验和验证+下载+解压），但 `WithInstaller` 从未被调用，`postInstallSteps` 下载分支永久跳过。新增 `mktInstallerAdapter`（`cmd/polaris/adapters_misc.go`）做 `target any` → `*protocol.RegistryEntry` 类型断言适配，`boot_tools.go` 接入 |
| `SkillEvolutionEngine.EvaluateAndEvolve` | **DEFER** | 零非测试调用点，无生产构造函数；`SkillEvolutionMonitor`（`boot_server.go:236` 已接入）覆盖同一"检测低效 Skill 并淘汰"问题域，两者被代码注释明确记载为架构上不可合并。接入 Engine 变体需要新设计"UncontrollableFailure 分类"逻辑（文档承诺但方法体从未实现），属真实设计缺口非接线缺口 |
| `MCPKnowledgeConnector`/`KnowledgeConnRegistry` | **DEFER** | Registry 本身已在 `boot_tools.go`/`boot_agent.go` 接入；真正阻塞点是 `MCPKnowledgeConnector.List/Fetch/Watch` 是 `CodeUnimplemented` 桩代码，`extension/lifecycle/mcp_installer.go` 明确注释记载"故意 fail-fast 拒绝注册 knowledge-source 能力的 MCP server，避免 SyncScheduler 空转"——是刻意的功能门控，不是遗漏 |
| `ActiveContext.Rebuild` | **DEFER** | 自承 MVP 占位（`memory/store/types.go:60`），循环体逐字丢弃事件（`_ = e`）；`ActiveContext` 全仓库零生产实例化。接入需要设计事件类型→状态字段的真实映射规则，无既有 spec 可依据 |
| `SynapticPlasticityManager`/`EvidenceSubgraphExtractor` | **DEFER** | 全仓库零生产构造点；自承 demo/mock 实现（`edge_weight.go:19,98` "真实实现应使用 GraphStore 接口，此处用 Store 演示"/"mock implementation"）。接入需要构建真实图存储层与 BFS/PPR 遍历，属未实现的实质性功能，非调用点缺口 |
| `temporal.go` `SetValidWindow`/`IsValidAt`/`FormatValidWindow` | **DEFER** | 消费侧 `TemporalExpirer.ExpireStale` 已接入（`boot_memory.go:167,176`），但全仓库零处构造带 `ValidFrom`/`ValidUntil` 的语义实体，生产侧无任何数据可供过期。决定"什么触发时序有效窗口"（如 LLM 检测到的时限性事实、矛盾消解 TTL）是产品决策，无既有 spec 依据 |
| `LlamaEmbed`/`LlamaRerank` | **DEFER（沿用既有结论）** | 前序会话已明确标记为需要用户/产品侧输入的选型决策，非单方工程判断可拍板，本次未重新展开 |
| `WithThinkingBudget`/`WithSemanticCacheHints` | **DEFER（沿用既有结论）** | 消费侧已在 `router.go` 完整接入，但构造侧需要会话/任务类型上下文（当前通用调用点不具备），前序会话已判定 |
| `MCPRetryPolicy` | **DEFER（沿用既有结论）** | 输入分类流水线（原始 transport error → `MCPErrorCode`）全仓库不存在，需先设计错误分类器 + 带幂等语义的重试包装 |
| `QLearner.Update`（Q-Learning 熵门控） | **DEFER（沿用既有结论）** | 奖励信号从未在任何文档中被定义，强行发明存在引入语义上任意行为的风险 |
| `PromptBuilder.WriteSkillContext`/`WriteUserInstruction` | **DEFER（沿用既有结论）** | 输入类型 `types.Skill` 全仓库零构造点，该 zone 疑似对应一套从未建成的 Ed25519 签名热插拔 Skill 系统，与实际生产的 `internal/extension/skill` SQLite 系统完全独立，无法找到应接入的真实调用意图 |
| `internal/knowledge/graphrag/writer.go`（`GraphWriter`/`UpsertEntity`） | **DEFER（沿用既有结论）** | 真实、有测试覆盖的代码，但被 `GraphBuildPipeline.Run()` 实际绕过（走 `ClusterEntities` + `semanticMem.UpsertFact` 而非 `Cluster(ctx, gw, ...)`）；接入会改变生产数据写入路径，需要审慎的流水线重设计，非强行调用可完成 |

## 判断依据（贯穿全部 Phase）

1. **WIRE 与 DEFER 的分界线是"真实实现是否已存在"**：`KnowledgeConflictArbiter`
   与 `MCPMarketplaceClient.Install` 均是完整、已测试的实现，仅缺一处调用/一个签名
   适配器；被判定 DEFER 的条目无一例外都存在自承的桩代码/mock 标记，或真正阻塞点
   是一段从未被写出来的业务逻辑（分类规则、奖励函数、事件→状态映射）。
2. **DEFER 不等于"以后也不做"**：每条 DEFER 结论都指出了具体缺失的设计对象
   （见上表理由列），供未来会话在真实需求出现时定位设计起点，符合 R1（禁止超前
   抽象、臆测开发）——先有需求，再造实现，不预先铺一层"看起来接好了但语义空洞"
   的胶水代码。
3. **Phase 2 的 credential_pool.go 修复是本轮清理的副产品而非目标**：仅因
   `adapter/test.go` 重命名为 `_test.go` 后缀使一个此前从未真正跑过的测试首次
   执行，暴露出凭证池防御性拷贝缺失的真实生产级安全 bug，印证"修复死代码/命名
   问题本身能带来超出预期的收益"。

## 后果

- **正向**：`go build ./...`/`go test ./...`/`golangci-lint`（含 `GOOS=wasip1
  GOARCH=wasm` skill/sdk 子模块）全绿；13 处此前"文档/注释声称已实现、实际从未
  被驱动"的悬空接线点中 2 处（`KnowledgeConflictArbiter`、
  `MCPMarketplaceClient.Install`）被真正激活；1 处真实安全漏洞
  （凭证池共享清零）被修复；10 处剩余悬空点均给出可追溯、可复核的 DEFER 理由，
  不再是无归因的"未知状态"。
- **负向**：`SkillEvolutionEngine`/`MCPKnowledgeConnector`/`ActiveContext`/
  `SynapticPlasticityManager`/temporal 有效窗口/`LlamaEmbed`/`WithThinkingBudget`/
  `MCPRetryPolicy`/`QLearner`/`PromptBuilder.WriteSkillContext`/`GraphWriter` 共
  11 个条目仍是未接入状态，需要后续会话在真实需求驱动下重新设计（不可简单
  "取消注释"式恢复）。
- **反例守护**：未来若在这些模块新增功能，先检查本 ADR 的 DEFER 理由列是否已被
  新证据推翻（例如出现了 `ValidFrom`/`ValidUntil` 的真实写入点，或
  `types.Skill` 出现了真实构造方），避免重复"发现悬空代码就地强行调用"的
  R1 反模式。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 为全部 13 处悬空接线点统一"强行调用"以满足"零遗漏"字面要求 | 会把未经验证的业务假设（奖励函数、分类规则、事件映射规则）伪装成"已完成"的功能，制造比死代码更隐蔽的技术债——运行但语义错误的代码比未运行的代码更危险 |
| 将 `learning/curriculum/calibrator.go` 与 `prompt/optimizer/memf.go` 的两套 `DynamicDifficultyCalibrator` 合并保留 | 论证后确认后者已是生产验证实现且覆盖前者全部语义，合并只会引入命名冲突与维护面重复，不产生新能力 |
| `internal/prompt/builder.go` 保留作为未来"备用 PromptBuilder 实现" | `protocol.PromptBuilder` 已是唯一生产路径且功能完整，保留一个零调用点的平行实现违反 R1，且两者接口签名并非严格兼容，"备用"说法不成立 |
| `MCPKnowledgeConnector` 桩代码直接返回 mock 数据以"完成接入" | `extension/lifecycle/mcp_installer.go` 的 fail-fast 拒绝是刻意设计（防止 SyncScheduler 对空转桩代码无限重试），强行放行会破坏这一保护 |

## 引用代码

- `internal/extension/marketplace/manager.go`（`ExtensionInstaller`/`WithInstaller`/
  `postInstallSteps`）
- `internal/extension/marketplace/marketplace.go`（`MCPMarketplaceClient.Install`）
- `cmd/polaris/adapters_misc.go`（`mktInstallerAdapter`）
- `cmd/polaris/boot_tools.go`（`installMgr.WithInstaller` 接入点）
- `cmd/polaris/boot_knowledge.go`（`KnowledgeConflictArbiter` 接入点）
- `internal/llm/credential_pool.go`（凭证池防御性拷贝修复）
- `internal/agent/agent.go`/`agent_execute_util.go`/`internal/tool/tool.go`
  （`AnomalyDistanceFilter` 会话级接线）
- `internal/memory/store/types.go:60`（`ActiveContext.Rebuild` MVP 占位注释）
- `internal/memory/graph/edge_weight.go:19,98`（`SynapticPlasticityManager`/
  `EvidenceSubgraphExtractor` mock 标记）
- `internal/extension/lifecycle/mcp_installer.go:90-102`（knowledge-source
  能力 MCP server 刻意 fail-fast 拒绝注释）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-14 | 初稿，汇总 Phase 1-4 死代码删除、简单接线、安全防线接线、中大型设计项调研结论 |
