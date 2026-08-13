# Polaris 审核报告 - 批次 13（全局设计评审：8 条端到端链路 + 功能完备性 + 模块拆合）

## 批次汇总

| ID | 类别 | 涉及模块 | 一句话标题 | 优先级建议 |
|---|---|---|---|---|
| GD-13-001 | 模块拆合 | internal/bootstrap, cmd/polaris | 统一模块生命周期管理：合并 internal/bootstrap 与 cmd/polaris 过程式启动 | 中 |
| GD-13-002 | 调用链问题 | internal/gateway/session, internal/agent/fsm, internal/security/guard | 流式对话事件处理链优化：敏感信息扫描前置与热路径分配裁减 | 高 |
| GD-13-003 | 错误路径缺失 | internal/memory/retrieval, internal/store/search | HybridRetriever 混合检索单路超时降级与单路 fallback 机制 | 高 |
| GD-13-004 | 调用链问题 | internal/execute/orchestrator | SQLiteBlackboard 任务认领机制：从纯 DB 轮询升级为事件通知+CAS抢占 | 中 |

## 端到端链路走查结论

- 链路1: 已走通 `cmd/polaris/main.go:88-199` → `bootSubstrate` → `bootMemory` → `bootTools` → `bootKnowledge` → `bootAgent` → `bootServer` → system ready
- 链路2: 已走通 `internal/gateway/server/provider/providers.go:127-189` → `ProviderRepo` → SQLite DB → `cmd/polaris/server_provider_loader.go:22-97` `LoadProvidersFromDB` → `internal/llm/adapter/`
- 链路3: 已走通 `internal/gateway/session/orchestrator_fsm.go:21-68` `runFSMTurn` → `internal/agent/fsm/` → `internal/llm/` → `sink.Emit` 回传
- 链路4: 已走通 `internal/tool/tool.go:149-250` `ExecuteTool` → `checkPreExecution` → PolicyGate / `ExecEnvelope.Execute` → `internal/sandbox/` → `ToolResult` → FSM 回注
- 链路5: 已走通 `internal/tool/builtin/memory_tools.go:43-75` → `ExclusiveWriter` → `internal/memory/consolidation/` → `internal/memory/retrieval/retriever.go:105-183` `HybridRetrieverImpl.Search`
- 链路6: 已走通 `internal/swarm/planner/decomposer.go:58-73` → `internal/execute/orchestrator/pattern_dag.go` / `pattern_state_graph.go` → `SQLiteBlackboard` → Workers → Result aggregation
- 链路7: 已走通 `cmd/polaris/boot_crash_recovery.go:77-115` `recoverCrashedSessions` / `boot_handoff_reconciler.go` → `schema/035_task_checkpoints.sql` → `SetReplayMode` → FSM 幂等重放
- 链路8: 已走通 `internal/sysmgr/updater/updater.go:211-323` `TriggerUpdate` → `applyUpdate` 二进制替换 → 进程重启 → `bootSubstrate` → `sysstore.OpenSQLite` schema DDL 版本核对/初始化

---

### [GD-13-001] 统一模块生命周期管理：合并 internal/bootstrap 与 cmd/polaris 过程式启动
- 类别: 模块拆合
- 涉及模块: internal/bootstrap, cmd/polaris
- 现状: internal/bootstrap/bootstrapper.go:27-130 实现了 Bootstrapper 及其 Kahn 拓扑排序算法 topologicalSort 和四阶关停 gracefulShutdown。然而 cmd/polaris/main.go:105-180 中，启动序列未调用 Bootstrapper，而是硬编码手写了 bootSubstrate -> bootMemory -> bootTools -> bootKnowledge -> bootAgent -> bootServer 过程式调用，且关停流程在 main.go:201-229 散落手写 httpSrv.Shutdown, ab.ReaperStop(), sb.DBWriter.Close() 等清理操作。
- 挑战: 架构规范 docs/arch/00-Global-Dictionary.md §XR-12 和 internal/bootstrap/CLAUDE.md 明确声明系统依赖拓扑与四阶优雅关停（Phase1 停流 → Phase2 排干 → Phase3 刷盘 → Phase4 释放）通过 Bootstrapper 统一管理。现状导致架构规范定义的生命周期抽象在生产入口被绕过，测试环境与生产环境启动关停行为脱节，手写 defer 无法保证复杂的四阶拓扑逆序清扫。
- ADR 核对: 已查 ADR-0081, ADR-0094 未涉及禁止统一 Bootstrapper；docs/arch/Module-Dependency-Axioms.md 明确指出 bootstrap 是全系统唯一允许跨层引用的 DI 容器。
- 业界依据: —
- 建议方案: 将 cmd/polaris/boot_*.go 导出的子系统结构体适配为 bootstrap.Bootable（及 Stage1Stopper..Stage4Closer 接口），统一由 bootstrap.Bootstrapper 注册并按 Kahn 拓扑排序进行 Ignite 和 ListenAndServe 优雅关停。
- 代价/收益: 改动仅限于 cmd/polaris/ 启动桥接逻辑，不影响核心模块内部实现；收益是彻底收敛生命周期管理，消除死抽象并保证四阶关停的确定性。
- 优先级建议: 中

### [GD-13-002] 流式对话事件处理链优化：敏感信息扫描前置与热路径分配裁减
- 类别: 调用链问题
- 涉及模块: internal/gateway/session, internal/agent/fsm, internal/security/guard
- 现状: internal/gateway/session/orchestrator_fsm.go:21-122 在 runFSMTurn 中订阅 FSM 流事件 AgentStreamEvent，并在 handleFSMEvent 阶段逐 Token 调用 systemPromptGuard.Scan(ev.Content, true)（第 95 行）。
- 挑战: 1) 泄露防护滞后：Scan 发生在 handleFSMEvent 内，而 sink.Emit(Event{Kind: KindDelta, Text: cleaned})（第 98 行）在泄露检测触发前可能已经向前端推送了部分 Token 碎片，无法做到“零泄露”物理阻断；2) 热路径分配过高：每个 Token 传输均构造临时 map (map[string]any{"type": "info"}) 和 slice append，高并发 SSE 吐字时 GC 压力显著。
- ADR 核对: 已查 ADR-0085, ADR-0094，确认 SessionOrchestrator 零 HTTP 依赖与 Fail-Closed 门控要求，但未限制扫描位置的前置优化。
- 业界依据: —
- 建议方案: 1) 将 SystemPromptGuard 扫描下沉/前置至 agent/fsm 的 Token 产出边界，在事件推送到 SubscribeStream channel 之前完成原子拦截与清理；2) 在 session/ 流传输热路径中使用零分配事件结构或对象池。
- 代价/收益: 需重构 guard.Scan 在 FSM 层的调用点；收益是做到真正物理零泄露拦截，同时显著降低高并发 SSE 对词包的内存分配开销。
- 优先级建议: 高

### [GD-13-003] HybridRetriever 混合检索单路超时降级与单路 fallback 机制
- 类别: 错误路径缺失
- 涉及模块: internal/memory/retrieval, internal/store/search
- 现状: internal/memory/retrieval/retriever.go:105-183 的 HybridRetrieverImpl.Search 依赖 search.HybridSearch 进行 BM25 + Dense Vector + Graph 三路并发召回与 RRF 融合。在第 160-172 行直接传入父 ctx，当其中任意单路（如 SurrealDB-Core 图遍历或向量检索）超时或出现死锁/慢查询时，整体 HybridSearch 直接返回 nil, err。
- 挑战: 混合检索是 Agent 思考循环与记忆工具（memory_search）的核心热路径。若 SurrealDB-Core FFI 图遍历或向量计算受高并发或内存压力拖慢，单个分路的超时会导致整条混合检索报错失败返回空结果，拖垮 Agent 决策链。系统缺少单分路超时（如 Vector/Graph 设定 300ms 独立 deadline）与退化为纯 BM25 召回的 Fallback 降级机制。
- ADR 核对: 已查 ADR-0010, ADR-0020，且 docs/specs/09-LLM-Agent-Production.md §四 明确要求 RAG 链路具备容错与降级评估。
- 业界依据: —
- 建议方案: 1) 为 Vector 检索与 Graph 召回分路引入独立的 context.WithTimeout 硬超时（如 300ms）；2) 当子分路超时或报错时，记录 Prometheus 降级指标，并自动降级为已完成的 BM25 结果进行 RRF 融合，保证检索主干高可用。
- 代价/收益: 需修改 retriever.go 及 search/ 召回调度逻辑；收益是显著提升 Memory RAG 链路在极端负载下的健壮性与可用性。
- 优先级建议: 高

### [GD-13-004] SQLiteBlackboard 任务认领机制：从纯 DB 轮询升级为事件通知+CAS抢占
- 类别: 调用链问题
- 涉及模块: internal/execute/orchestrator
- 现状: internal/execute/orchestrator/sqlite_blackboard.go:198-244（ClaimTask）与 reaper.go:41 使用每秒 Reaper 扫描与 Worker 定期 PeekTask 轮询来认领任务。
- 挑战: 在 StateGraphExecutor 或 PatternDAGExecutor 处理多节点串行/条件工作流时，前驱节点调用 CompleteTask（第 312 行）后，后继节点无法实时感知，必须等待 Worker 的下一次 PeekTask 轮询间隔（1s~2s），为 DAG/Graph 多步编排引入了秒级调度延迟；同时高频 SELECT * FROM tasks WHERE status='pending' 轮询占用了有限的 SQLite 读连接池（MaxOpenConns=4）。
- ADR 核对: 已查 ADR-0041, ADR-0062，确认 Blackboard 的 CAS+Lease 机制不能被完全替换，但 ADR-0062 建议引入内存通知作为辅助。
- 业界依据: —
- 建议方案: 保留 SQLite CAS 认领与 Lease 事务性，在 SQLiteBlackboard 内部增加 sync.Cond 或 chan struct{} 内存广播。当 PostTask / CompleteTask 写入成功时触发广播，使同进程的 Worker 能够 <1ms 内醒来执行 CAS 认领，实现“事件驱动 + CAS 抢占”双重保证。
- 代价/收益: 改动控制在 sqlite_blackboard.go 内部；收益是将同进程多 Agent / DAG 节点间的调度延迟从秒级缩短至亚毫秒级（<1ms），并大幅降低 SQLite 连接池轮询开销。
- 优先级建议: 中

## 已审文件清单

- `cmd/polaris/main.go`
- `cmd/polaris/boot_substrate.go`
- `cmd/polaris/boot_memory.go`
- `cmd/polaris/boot_tools.go`
- `cmd/polaris/boot_knowledge.go`
- `cmd/polaris/boot_agent.go`
- `cmd/polaris/boot_server.go`
- `cmd/polaris/boot_crash_recovery.go`
- `cmd/polaris/boot_handoff_reconciler.go`
- `cmd/polaris/server_provider_loader.go`
- `internal/bootstrap/bootstrapper.go`
- `internal/bootstrap/bootable.go`
- `internal/gateway/server/provider/providers.go`
- `internal/gateway/server/provider/providers_probe.go`
- `internal/gateway/server/provider/providers_models.go`
- `internal/gateway/session/orchestrator_fsm.go`
- `internal/agent/fsm/state_machine.go`
- `internal/agent/fsm/transitions.go`
- `internal/tool/tool.go`
- `internal/tool/builtin/memory_tools.go`
- `internal/memory/consolidation/consolidation.go`
- `internal/memory/retrieval/retriever.go`
- `internal/swarm/planner/decomposer.go`
- `internal/execute/orchestrator/sqlite_blackboard.go`
- `internal/execute/orchestrator/pattern_dag.go`
- `internal/execute/orchestrator/pattern_state_graph.go`
- `internal/sysmgr/updater/updater.go`
- `internal/sysmgr/updater/updater_install.go`
- `internal/protocol/schema/035_task_checkpoints.sql`

## 明确未覆盖的范围

- `web/` 前端代码（按 §5.0-A 规则不在此次代码审查范围内）
- `testdata/` 与各包下 `*_test.go` 文件

## 审了但无发现的模块

- `internal/sysinfo/`
- `internal/downloader/`
