# ADR-0052: 2026-07-21 全仓库 deadcode 复核收尾

- **状态**: Accepted（已执行）
- **日期**: 2026-07-21
- **决策者**: MrLaoLiAI
- **相关模块**: `internal/agent/`、`internal/channel/adapter/`、`internal/gateway/`、
  `internal/learning/`、`internal/memory/retrieval/`、`internal/observability/metrics/`、
  `internal/protocol/`、`internal/sandbox/`、`internal/security/network/`

## 上下文

`deadcode ./cmd/polaris/...` 复核（承接 ADR-0051 2026-07-14 Phase 1-4 收尾，两次
运行之间无新 deadcode 相关提交）。本次运行报告 106 项 unreachable func/method。
逐条与 ADR-0051 及历史 memory 记录比对后：约 19 个簇（63 个符号）是 ADR-0051 已
判定 DEFER 的既有结论（沿用，不重新展开）；约 24 个簇（43 个符号）此前从未被
审查过。本 ADR 只记录这 24 个新簇的处置结果。

方法：每簇独立派发 research-only 调研（grep 全仓库调用方，区分生产/测试调用，
读文档，判断是否有专门测试覆盖），禁止批量机械处理，禁止仅凭 grep 命中数判断
（2026-07-13 memory 记录过 `downloader.Get` 的教训：常见名字 grep 噪音大，必须
以包自身测试是否失败为准）。

## 决策

### WIRE（真实缺口，已接入）

| 条目 | 接入点 | 理由 |
|------|--------|------|
| `FeishuVerifyWebhookSignature` | `channelsadmin/webhook_verify.go` 新增 `case "feishu"` | `verifyWebhookSource` 此前落入 `default` 分支要求 `webhook_secret`（飞书表单从不写入），导致飞书 webhook 100% fail-closed 拒绝——是可用性缺口非安全漏洞。仅完整支持"仅 Verification Token"明文模式（与 `extractFeishuWebhook` 现有明文解析能力匹配）；Encrypt Key 加密模式因缺 AES-256-CBC 解密器，显式返回 `CodeUnimplemented` 而非伪装成功 |
| `optimizer.NewBlindZoneDetector` + 两处 `InjectBlindZoneDetector` | `boot_memory.go` 构造 + 注入 `FallacyPool`；`boot_agent.go` 注入每个 Agent | 构造函数与双路注入点均已完整实现（V8-S4），但 `boot_memory.go` 只建了 `fallacyPool`/`heuristics` 漏了这一步——S_PLAN 生产盲区强制 System2 路由从未真正生效 |
| `types.TaintLevelFromContext` | `server_handlers.go` `handleAgentQuery` 的 `IntentTaint` | `middleware_auth.go` 已按 clientType 算出 TaintMedium/TaintHigh 并注入 ctx，但此处硬编码 `types.TaintMedium`，外部 api 调用本该按 TaintHigh 处理的信号被静默丢弃（HE-2 相关） |
| `skill.WithMaxCodeSize` | `boot_tools.go` `NewSkillValidationPipeline` | Option 从未传入，`maxCodeSize` 恒为零值导致大小校验被跳过；复用 `codeact` 侧已有的同一份 `M7Tool.MaxCodeSizeBytes` 阈值 |
| `authcontext.WithWorkDir` | `server_lifecycle.go` `NewContextRefExpander` | 未传时 `@file` 引用解析退化为相对进程 CWD；同一 Dependencies 结构体的其他字段早已能拿到 `s.dataDir` |
| `PerformanceDriftDetector.CurrentPassRate`/`Baseline` | `metrics_handler.go` legacy + otel 两套 `/metrics` handler 新增 gauge | 检测器本体（Record/RegisterListener→KillSwitch）已生产接入，仅缺 gauge 暴露，与 SurpriseIndex/TokenBurnRate 等一等公民信号的可观测标准不一致（HE-1） |

### DELETE（确认废弃）

| 条目 | 理由 |
|------|------|
| `agent.TaintGate`/`defaultTaintGate`/`Agent.taintGate` 字段 | 全仓库零读取；真正的 S_VALIDATE 污点防线是 `execute/dag.validateTaintGate`（ADR-0046），此为被架构拆分淘汰的孤立抽象。`NewAgent` 签名同步去掉该参数（全部调用方本就传 nil） |
| `internal/agent/fsm/fallback_fsm.go`（整文件） | `FallbackFSM` 全仓库零调用（生产+测试）；`M04-Agent-Kernel.md` 提及的 `use_flowy` build tag 从未在代码中真正出现，属过时文档描述，已同步订正 |
| `feishu.go` `feishuHMACVerify`/`feishuGetAccessTokenForWebhook` | 前者算法与飞书官方签名机制不符的自承"备用"桩；后者是 `FeishuGetTenantToken` 的冗余包装，reply 流程走 `ChannelMgr.SendReply`（渠道无关），无独立调用意图 |
| `learning.Engine.ReportOutcome`/`.SurpriseIndex()`/`.Stop()` | 生产侧分别用直写 channel、`metrics.GlobalSurpriseIndex().Current()`、ctx 取消替代，三者均零调用 |
| `surprise.Route` | 已被 ADR-0022 `metrics.SelectThinkingMode`（实际接入 `fsm/transitions.go`）取代，函数自身注释承认非权威实现 |
| `retrieval.NewWriteFilterWithThreshold` | 零调用（含测试）的幻影档位构造函数，与 ADR-0051 Phase1 同类模式 |
| `metrics.DecisionLog`/`DecisionLogStore`/`DecisionLogger`/`NewDecisionLogger`/`.Log()` | 与 `internal/store/audit/decisionlog.go` 的 `SQLiteDecisionLog`（实现 `protocol.DecisionLogger`，被 `execute/orchestrator/pipeline.go` 实际使用）撞名但完全独立、零调用的平行实现；`M03-Observability.md §10` 引用已订正指向真实实现 |
| `internal/protocol/trust.go` + `trust_test.go`（整文件） | `TrustFromSignatureValid` 自承"用于数据库迁移 021_skill_trust_tier.sql"，该迁移文件全仓库不存在（Schema 变更走直接改写原始 DDL，符合 CLAUDE.md 上线前策略），无数据源可转换 |
| `sandbox_container.go` `bytes2ReadCloser`/`noopReadCloser` + 对应测试 | `CmdRunnerCfg` 无 stdin 字段，`RunScript` 的 `input []byte` 参数从未通过此封装消费，ContainerSandbox 从未真正支持 stdin 传递 |
| `local_only.go` `NoopTransport`/`goTransport` 字段/`httpRequest`/`httpResponse`/`IsLoopbackIP` | `RoundTrip` 签名用局部定义类型，结构性不满足 `http.RoundTripper`；`Enable()` 的真实 L2 防线是直接改写 `http.DefaultTransport.DialContext`（内联等效实现），字段/函数完全冗余 |

### 复核后确认无需处理（有测试覆盖的合法 API，非死代码）

以下符号虽然生产环境零调用，但均有专门测试覆盖真实行为（而非仅测试符号本身存在），
且各自文档/注释已明确定位为"测试/开发专用变体"，不属于需要清理的废弃代码：

- `agent.NewAgentWithDefaults`（ADR-0046 记载的测试专用构造器）
- `credential.NewVault`（`server_lifecycle.go` 注释明确禁止生产使用，边界已注明）
- `search.NilReranker.Rerank`（有意的 null-object 测试替身，与 `execute/orchestrator.Blackboard` 同类 false positive）
- `llm.NewSingleCredentialPool`（`credential_pool_test.go` 用其覆盖多个安全回归场景，含 ADR-0051 Phase2 的防御性拷贝 bug 回归测试）
- `planner.DefaultSpawner`（自身注释承认"生产环境真实 spawner 见其注入点"，测试覆盖真实 `PlannerPool.Run()` 行为）
- `supervisor.Supervisor.Wait`（`Stop()` 已含等效 `wg.Wait()`，但为低风险、有测试覆盖的对称 API，不强行删除）
- `downloader.Get`/`DownloadExtractTarGz`/`ResolveURL`/`ProxyStatus`（2026-07-13 memory 已确认，测试覆盖真实行为）

### DEFER（本轮新发现，需要真实设计，不强行接入）

| 条目 | 理由 |
|------|------|
| `agent.ContextWindowManager.NeedsCompaction` | 无生产构造函数、无 `currentUsage` 赋值路径；且需要决定 M4 Agent kernel 主循环如何跨层调用 M5 `SessionCompressor`（后者操作 chat_messages 表行，前者操作 agent 内核内部 token 计数），是架构设计问题非一行接线 |
| `reflexion.NewReflectionWorkerWithConfig` | `reflection_worker_test.go` 覆盖真实白名单覆盖行为（非仅测试构造器本身），但生产侧无 `ReflectionConfig` 配置来源——`internal/config/thresholds.go` 无对应字段，需产品侧决定阈值归属的 threshold 结构体与默认值 |
| `surprise.DriftDetector` 全套（`NewDriftDetector`/`AddAnchor`/`anchorCosineDist`/`scoreAnchors`/`Detect`/`EmbeddingVersionTracker.Update`） | 实现完整且与 `M05-Memory-System.md §12.3` 设计逐字段吻合，但缺失整条"漂移响应编排器"——周期性喂 anchor、调用 `Detect()`、按 task_type 降级 BM25、触发 Blue-Green 重嵌——这条编排链全仓库不存在 |
| `adapter.SteeringAdapter.SteerActivations`/`.ClearSteering` | 适配器骨架 + Tier 门控已完整（`boot_substrate.go` 构造），但 `/steer` 用户命令解析面全仓库不存在，`M01-Inference-Runtime.md §1.3` "已实现"表述需订正为"适配器完成，命令面待补" |
| `adapter.QLoRAAdapter.Train`/`PRMAdapter.Train`/`postJSON` | 同上模式；缺 M9 curriculum/reflexion 产出训练样本并决定触发时机的上游流水线 |
| `modelregistry.DecideMigration`/`MigrationDecision.String` | 纯函数 + 测试完整，但上游 `OnModelUpgrade`/`DeprecateModel` 无生产触发入口，缺"新模型版本发现"运营流程 |
| `consolidation.ConsolidationPipeline.MarkColdEpisodicEvents` | 与 `memory/facade.go:ArchiveEpisodic` 是两条平行封装，均零调用；缺 session-close 钩子或独立 cron 触发的设计决策 |
| `optimizer.ExtractTaskType` | 与 `agent/agent_execute_util.go` 内私有 `extractTaskType` 逻辑完全相同；是否消除重复实现需要架构决策（agent 包注释所称"避免 L1→L2 依赖"已过期，optimizer 现属 L1） |
| `protocol.SetReplayMode` | `IsReplaying()` 读侧已在 4 处生产路径接入护栏，但 Setter 无启动期/崩溃恢复触发点；`M04-Agent-Kernel.md` 已定义崩溃恢复重放设计但从未落地驱动逻辑 |
| `guard.NewSICCleanerWithDetector` | 零生产调用点，且不存在任何符合注入签名的 LLM 检测器实现 |
| `network.Allowlist.Add`/`.IsAllowed` | 配套的 Ed25519 签名 TOML 加载器（`local_only_network_allowlist.toml`）从未实现，字段构造后从未被填充/查询 |
| `types.BuildIdempotencyKey` | 实现/文档齐全，但生产 10+ 调用点全部用 ad-hoc 拼接格式；统一需评估已落盘 outbox 记录的兼容性，属一致性重构而非一行接线 |
| `skill.NewSkillCreator`/`.GenerateSkill`/`extractJSON` | 功能完整（与 LogicCollapse 轨迹驱动生成平行的"用户意图驱动"生成管线），但缺"用户请求生成技能"的触发入口设计（HTTP/CLI/tool 三选一待决） |
| `graphrag.NewGraphTraverser` | 与 ADR-0051 已判定 DEFER 的 `GraphWriter` 是同一次架构决策的另一半（`HybridRetriever` 图接入路径已被整体拆除，`hr.graph` 生产环境恒为 nil），非独立新问题 |
| `mcp.TaintedJSONNode.AllStrings` | 注释承诺的两个消费者（`Spotlighting()`、`SICCleaner`）均已改用扁平化字符串方案绕开 JSON 树形结构，粒度化污点标记设计从未真正实现 |

## 判断依据

延续 ADR-0051 的分界线：WIRE 与 DELETE 的区分是"真实实现是否已存在、是否被
测试独立验证"；DEFER 一律给出具体缺失的设计对象（触发入口/上游流水线/配置
归属/编排器），供未来会话定位起点，不发明业务逻辑。新增一条方法论：对有专门
测试覆盖的候选，先判断测试是否验证真实业务行为（如 `NewSingleCredentialPool`
覆盖安全回归场景）还是仅验证符号存在本身，前者一律不删除。

## 后果

- **正向**：`go build ./...`、`go test ./...`（152 个包，无 FAIL）、
  `golangci-lint run ./...`（含 wasip1 子 lint）全绿；6 处真实悬空接线点已激活
  （含 1 处 HE-2 相关的污点级别丢失修复、1 处飞书 webhook 从 100% 不可用恢复为
  可用、1 处 PerformanceDrift 可观测性缺口补齐）；10 个簇的确认废弃代码已清理
  （含 2 处文档订正：`M03-Observability.md` DecisionLog 引用、`M04-Agent-Kernel.md`
  FallbackFSM/use_flowy 引用）；7 个曾疑似死代码的符号复核后确认是合法的
  测试覆盖 API，避免了误删。`kernel_manifest.json` 已重新生成
  （触达 `internal/agent/`、`internal/security/`、`internal/observability/`
  三个 ImmutableKernelPackages）。
- **负向**：15 个新发现的 DEFER 条目仍是未接入状态，需要后续会话在真实需求
  驱动下重新设计。加上 ADR-0051 沿用的 11 个既有 DEFER 条目，当前仍有 26 个
  已归因、可追溯、非误删风险的未接入符号。

## 引用代码

- `internal/gateway/server/sysadmin/channelsadmin/webhook_verify.go`（`verifyFeishuWebhook`）
- `cmd/polaris/boot_memory.go`/`boot_agent.go`（`BlindZoneDetector` 双路注入）
- `internal/gateway/server/server_handlers.go`（`IntentTaint` 修复）
- `cmd/polaris/boot_tools.go`（`skill.WithMaxCodeSize`）
- `internal/gateway/server/server_lifecycle.go`（`authcontext.WithWorkDir`）
- `internal/observability/metrics/metrics_handler.go`（PerformanceDrift gauge）
- `internal/agent/agent.go`（`TaintGate` 清理）
- `internal/agent/fsm/fallback_fsm.go`（已删除）
- `internal/protocol/trust.go`（已删除）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-21 | 初稿，`deadcode ./cmd/polaris/...` 106 项复核，6 项 WIRE + 10 簇 DELETE + 7 项确认无需处理 + 15 项新 DEFER |
