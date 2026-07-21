# ADR-0053: ADR-0051 遗留 11 项 DEFER 复核 + MCPKnowledgeConnector 接入

- **状态**: Accepted（已执行）
- **日期**: 2026-07-21
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/knowledge/connector/`、`internal/extension/mcp/`、
  `internal/extension/lifecycle/`、`docs/arch/M01-Inference-Runtime.md`

## 上下文

承接 ADR-0051（2026-07-14）与 ADR-0052（2026-07-21）。用户要求：对 ADR-0051
遗留的 11 个 DEFER 条目逐一复核是否有新证据推翻原判定，"只要需要的，都开发
完成"。逐条重新读代码+文档后，1 项判定翻转为 WIRE 并已实现，1 项发现原 DEFER
理由本身已过期（需订正但暂不实现），其余 9 项复核后确认原判定依然成立。

## 决策

### WIRE（本次翻转，已实现）

**`MCPKnowledgeConnector.List`/`.Fetch`**（`internal/knowledge/connector/mcp_connector.go`）：

原 ADR-0051 判定 DEFER 的理由是"`List`/`Fetch`/`Watch` 是 `CodeUnimplemented` 桩
代码"。复核发现 `internal/extension/mcp.MCPClient` 已有 `ListTools`/`CallTool`
的 JSON-RPC 调用模式（`c.call(ctx, method, params)`），MCP 协议的
`resources/list`/`resources/read` 是标准方法，与 `tools/list`/`tools/call` 同构，
并非需要凭空发明的业务逻辑。本次新增：

- `mcp.MCPClient.ResourcesList`/`.ResourcesRead`（`mcp_client_protocol.go`），
  与 `ListTools`/`CallTool` 同一 RPC 调用约定。
- `connector.MCPKnowledgeConnector.List`/`.Fetch` 改为调用上述两个真实方法，
  `MCPClient` 本地接口收窄为消费方只用到的两个方法（HE-3/R1.4）。
- `lifecycle.MCPInstaller.Install`：原先"检测到 knowledge-source 能力声明 →
  仅 `slog.Warn` 跳过注册"的硬拦截，改为通过既有 `MCPConnector.GetClient`
  取出真实客户端、类型断言满足 `connector.MCPClient` 接口后
  `registry.Register(...)`；`Uninstall` 对称补上 `registry.Unregister`
  （原实现遗漏，卸载后 connector 会残留继续被 `SyncScheduler` 调度）。
- `Watch` 保持不变（`SyncConfig.SupportsWatch` 仍硬编码 `false`，MCP
  `resources/subscribe` 通知订阅未实现——这是比 List/Fetch 更大的独立设计
  问题，本次不展开，`Watch()` 原有的 fail-closed 行为已经是正确的降级）。

验证：新增 `mcp_connector_test.go`（List/Fetch/空内容三个用例），
`go build ./...`/`go test ./...`/`golangci-lint` 全绿，`deadcode
./cmd/polaris/...` 从 70 项降至 63 项（`MCPKnowledgeConnector` 全部 7 个符号
不再出现）。

### 订正但不实现（原 DEFER 理由已过期，但激活风险/工作量超出本次范围）

**`types.WithSemanticCacheHints`**（连带 `WithThinkingBudget` 一并复核）：

原 ADR-0051 理由"消费侧已在 router.go 完整接入，但构造侧需要会话/任务类型
上下文，当前通用调用点不具备"——复核发现这个理由本身不准确：

1. `docs/arch/M01-Inference-Runtime.md` §6.2 声称"向量索引后端待激活，
   `store=nil` 时安全空操作"已过期——`SurrealCacheStore` 是完整实现，
   `boot_substrate.go` 在 `surrealStore != nil` 时已构造真实 cache 并注入
   Router，文档已订正（见本次 M01 §6.2 修改）。
2. 真正的阻塞点不是"缺上下文"：`ContextHintFingerprint` 需要的 SHA-256
   指纹其实已经在 `internal/agent/fsm/epoch.go`（`epochTracker.check`）
   逐条算出，只是只返回 epoch 计数器、指纹字符串本身被丢弃（`sCtx.ContextEpoch`
   写入后也零读取方——同一种"生产者/消费者都在、中间没接"模式）。
3. 但即便补齐 `WithSemanticCacheHints` 构造，缓存 Get/Put 逻辑只存在于
   `InferenceRouter.Infer`（非流式），主对话轮走的 `StreamInfer`
   （`router_stream.go`）完全没有等价的缓存查询/写入分支——接入需要给
   `StreamInfer` 设计一条"缓存命中时如何在流式 channel 里一次性返回"的
   对称逻辑，且 `wrapStreamChannel` 已经叠了 `StreamBudgetGuard`/
   `TokenBurnDetector`/`TrackStreamCost` 等多层机制，是全系统调用频率最高、
   风险最高的路径之一。

判定：**不在本次实现**，原因是工作量与风险都明显超出"一次调用点接线"的范畴，
且触达全系统最热路径，贸然改动的下行风险不对称于本次复核的既定范围。已把
准确的现状写回 M01 §6.2（订正"待激活"为"已激活但主对话轮不走缓存"），供未来
会话直接定位起点，不再需要重新调研一遍。

### 复核后确认原判定依然成立（无新证据，DEFER 维持）

| 条目 | 复核方式 | 结论 |
|------|---------|------|
| `SkillEvolutionEngine.EvaluateAndEvolve` | grep 全仓库调用点 | 仍零生产调用；与 `SkillEvolutionMonitor` 重叠问题域未变 |
| `ActiveContext.Rebuild` | 读代码 | 循环体仍 `_ = e` 丢弃事件；全仓库仍零处 `ActiveContext{}` 实例化 |
| `SynapticPlasticityManager`/`EvidenceSubgraphExtractor` | 读代码 | `Extract` 仍是拼字符串的 mock；`FeedbackCalibrate`/`PeriodicPrune` 仍是空实现桩 |
| `temporal.go` 三个函数 | grep `ValidFrom:`/`ValidUntil:` | 全仓库仍零处构造带时序有效窗口的实体 |
| `LlamaEmbed`/`LlamaRerank` | 沿用 | 仍需产品侧模型选型决策，本次未重新展开 |
| `MCPRetryPolicy` | grep `MCPErrorCode` 枚举值生产侧构造 | 全仓库仍零处把原始 transport error 分类为 `MCPErrorCode`，错误分类器仍不存在 |
| `QLearner.Update` | 读代码 | `salienceThreshold` 仍是构造时写死的 0.15 常量，从未被自适应调整过；奖励信号（"遗忘决策的好坏"）仍无任何定义依据 |
| `PromptBuilder.WriteSkillContext`/`WriteUserInstruction` | grep `types.Skill{` | 全仓库仍零处构造该类型 |
| `graphrag.GraphWriter`/`NewGraphTraverser` | 读 `GraphBuildPipeline.Run` | 仍走 `ClusterEntities` + `synthesizeConcepts`，未使用 `GraphWriter.UpsertEntity` |

## 判断依据

延续 ADR-0051/0052 的方法论：WIRE 需要"真实实现已存在，只缺一处可定位的桥接
逻辑"；本次 `MCPKnowledgeConnector` 符合这一标准（`ListTools`/`CallTool` 提供了
现成的 RPC 调用范式，`resources/list`/`resources/read` 是有明确外部规范的标准
方法，不是发明业务逻辑）。`WithSemanticCacheHints` 一项的特殊性在于：原判定
理由本身有误（doc drift），但订正后暴露出的真实工作量（StreamInfer 对称缓存
分支 + 触达最热路径）比原判定描述的"缺上下文"更大，因此维持不接入，但必须
把订正后的真实理由写回文档，避免未来会话重复"以为只是缺上下文"式的低估。

## 后果

- **正向**：`go build`/`go test`/`golangci-lint` 全绿；MCP 知识源连接器
  子系统从"注册即被拒绝"恢复为可用（含卸载生命周期对称修复）；`deadcode`
  70→63；M01-Inference-Runtime.md §6.2 订正两处过期表述。
- **负向**：9 个条目仍是 DEFER，`WithSemanticCacheHints`/`WithThinkingBudget`
  仍未接入（但复核理由已更新为准确表述）。

## 引用代码

- `internal/extension/mcp/mcp_client_protocol.go`（`ResourcesList`/`ResourcesRead`）
- `internal/knowledge/connector/mcp_connector.go`（`List`/`Fetch` 实现 +
  `mimeTypeToSourceType`）
- `internal/extension/lifecycle/mcp_installer.go`（`Install`/`Uninstall`
  的 registry 接入）
- `docs/arch/M01-Inference-Runtime.md` §6.2（SemanticCache 现状订正）
- `internal/agent/fsm/epoch.go`（`epochTracker.check` 指纹丢弃现状）
- `internal/llm/router.go`/`router_stream.go`（`Infer` vs `StreamInfer` 缓存
  分支缺失对比）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-21 | 初稿：ADR-0051 遗留 11 项复核，1 项 WIRE（MCPKnowledgeConnector）+ 1 项理由订正（SemanticCacheHints）+ 9 项维持 DEFER |
