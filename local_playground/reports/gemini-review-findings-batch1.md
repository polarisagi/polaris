# Polaris 审核报告 - 批次 1（代码轨道）

置信度分布声明: 本批次所有 4 条发现均经过源码与 SSoT 规范 / state.yaml / CHANGELOG 逐行对照及 §2-A 反证动作，确定属于逻辑缺陷或规范漂移，无需假定未验证的运行时条件。

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-1-001 | P1 | internal/store | MutationBus 单写者在超时与版本冲突路径上直接向 ResultCh 写通道引发永久死锁 | 高 | 是 |
| GR-1-002 | P1 | internal/store | OutboxWorker 处理毒丸记录时误返回 nil 导致死字记录被更新为 done 状态 | 高 | 是 |
| GR-1-003 | P2 | internal/observability | TierParameters 仍保留已废弃的 GraphRAGLLMDailyBudget 字段而缺失 GraphRAGConcurrentWorkers | 高 | 是 |
| GR-1-004 | P2 | internal/downloader | downloadResume 多源降级时误把不支持 Range 的截断 partial 文件尺寸当作后续 Candidate 偏移量导致数据损坏 | 高 | 是 |

---

### [GR-1-001] MutationBus 单写者在超时与版本冲突路径上直接向 ResultCh 写通道引发永久死锁
- 严重级: P1
- 模块: internal/store（层: L0）
- 位置: internal/store/mutation_bus_execute.go:177, internal/store/mutation_bus_execute.go:258, internal/store/mutation_bus.go:264, internal/store/mutation_bus.go:280
- 违反规则: HE-6 | R1.16
- 置信度: 高
- 可机械化: 是（建议规则: ast 检查 dw.ResultCh <- expr 未包裹在 select-default 块中）
- 反证: 已查 cmd/polaris/boot_server.go、internal/bootstrap/ 拓扑及 DatabaseWriter 全量调用链。AppendEvent/AppendDecision 均在与 ctx.Done() 的 select 中等待 ResultCh。若调用 context 先超时退出，ResultCh 将无人接收。flushBatch/drainCh/drainPriorityCh 统一执行 r.intent.ResultCh <- r.err，无 select/default 防护，将在 0 容量 channel 写入时永久卡死 DatabaseWriter 单写者协程，导致全局 events 与 decision_log 无法写入。已与 failAll (355行) 的 select-default 结构对比确认差异。
- 问题: DatabaseWriter 在处理批次提交成功 (line 258)、Lease 校验失败 (line 177) 以及 priorityCh/ch 排空 (lines 264, 280) 路径上，直接使用无缓冲 channel 发送操作 `r.intent.ResultCh <- r.err`。当 Submit 调用方（如 AppendEvent/AppendDecision）传入 0 容量 channel 且由于 context 超时/取消而提前退出 select 接收块时，DatabaseWriter 作为全局唯一的 SQLite 写入协程将被永久阻塞在 channel 发送操作上，卡死后续所有 events/decision_log/tasks 等写入操作。
- 证据: internal/store/mutation_bus_execute.go:257-260
  ```go
  for _, r := range results {
  	if r.intent.ResultCh != nil {
  		r.intent.ResultCh <- r.err
  	}
  }
  ```
- 修复方向提示: 在 ResultCh 发送点使用 `select { case r.intent.ResultCh <- r.err: default: }` 结构（对齐 failAll 方法中的安全写法），防止因调用方超时退场卡死单写者协程。

### [GR-1-002] OutboxWorker 处理毒丸记录时误返回 nil 导致死字记录被更新为 done 状态
- 严重级: P1
- 模块: internal/store（层: L0）
- 位置: internal/store/outbox_worker.go:362
- 违反规则: HE-6
- 置信度: 高
- 可机械化: 是（建议规则: 检查 CrashRecoveryCount >= 3 分支返回值是否为非 nil 错误）
- 反证: 已查 cmd/polaris/boot_server.go、internal/bootstrap/、002_outbox.sql 及 spec/state.yaml §outbox 状态机约束。state.yaml outbox_inv_03 明确规定 crash_recovery_count >= 3 时必须直接转移至 dead 状态。OutboxWorker.Process() 在 lines 362-366 检测到 record.CrashRecoveryCount >= 3 时返回 nil，上层 processAndMark (line 264) 接收到 err==nil 后误以为处理成功，执行 UPDATE outbox SET status='done'，导致原本崩溃 3 次的毒丸记录在数据库中被错误标为 done 而非 dead。
- 问题: OutboxWorker.Process 在检测到 CrashRecoveryCount >= 3 (毒丸记录) 时，虽然记录了 dead_letter 指标与日志，但函数末尾直接返回了 nil。上层 processAndMark 方法在 err == nil 时会将 SQL 状态更新为 'done' (`UPDATE outbox SET status='done'`)。这导致达到最大崩溃重试次数的异常 Outbox 任务在数据库台账中被伪造为"成功完成"，破坏了 state.yaml §outbox outbox_inv_03 ("crash_recovery_count >= 3 -> directly to dead") 不变量。
- 证据: internal/store/outbox_worker.go:362-366
  ```go
  if record.CrashRecoveryCount >= 3 {
  	metrics.GlobalOutboxDeadLetterTotal.Add(1)
  	slog.Error("outbox message dead (poison pill)", "id", record.ID, "target", record.TargetEngine)
  	return nil
  }
  ```
- 修复方向提示: 在 Process 中检测到毒丸记录时返回 ErrUnknownTargetEngine 或专用 ErrPoisonPill 哨兵错误，使 processAndMark 正确更新 status='dead'。

### [GR-1-003] TierParameters 仍保留已废弃的 GraphRAGLLMDailyBudget 字段而缺失 GraphRAGConcurrentWorkers
- 严重级: P2
- 模块: internal/observability（层: L0）
- 位置: internal/observability/probe/tier_parameters.go:35, internal/observability/auto_config_tiers.go:30
- 违反规则: SSoT-L0
- 置信度: 高
- 可机械化: 是（建议规则: AST 检查 probe.TierParameters 结构体中是否存在废弃字段 GraphRAGLLMDailyBudget）
- 反证: 已查 docs/specs/CHANGELOG.md (2026-06-09 记录) 与 docs/arch/M03-Observability.md §5.3。规范变更明确记载: "TierParameterTable 中 GraphRAGLLMDailyBudget 参数与 M10 inv_M10_05 '已取消财务日预算限制' 直接冲突，改为 GraphRAGConcurrentWorkers（资源维度并发数）"。实测 internal/observability/probe/tier_parameters.go:35 与 auto_config_tiers.go (lines 30, 57, 84, 111) 仍定义并填充了 GraphRAGLLMDailyBudget，未按照架构裁决订正为 GraphRAGConcurrentWorkers。
- 问题: TierParameters 结构体及其自动配置映射逻辑与 SSoT 规范脱节。CHANGELOG.md (2026-06-09) 已裁决取消 GraphRAGLLMDailyBudget 财务限制并替换为资源维度的 GraphRAGConcurrentWorkers。但 probe/tier_parameters.go:35 与 auto_config_tiers.go 仍保留并硬编码填充旧字段，导致硬件层级的并发调优无法传递给 GraphRAG 模块。
- 证据: internal/observability/probe/tier_parameters.go:35
  ```go
  GraphRAGLLMDailyBudget int `json:"graphrag_llm_daily_budget"`
  ```
- 修复方向提示: 将 TierParameters 中的 GraphRAGLLMDailyBudget 替换为 GraphRAGConcurrentWorkers，并在 auto_config_tiers.go 中配置各 Tier 的并发数。

### [GR-1-004] downloadResume 多源降级时误把不支持 Range 的截断 partial 文件尺寸当作后续 Candidate 偏移量导致数据损坏
- 严重级: P2
- 模块: internal/downloader（层: L0）
- 位置: internal/downloader/http.go:42, internal/downloader/http.go:84
- 违反规则: E
- 置信度: 高
- 可机械化: 是（建议规则: 检查 downloadChunk 中 os.O_TRUNC 标志设置时是否清空 offset 或删除旧 part 文件）
- 反证: 已查 cmd/polaris/boot_tools.go、internal/bootstrap/ 及 sysmgr/updater 全量调用链。downloadResume 为资源/模型/插件下载的核心底层。在 CandidateURLs 循环中（line 80），若首个 Candidate 不支持 Range 响应 200 OK，downloadChunk 将使用 os.O_TRUNC (line 42) 重新从头写入 partPath。若下载中途网络中断或出错，partPath 保留了部分写入的字节数。后续循环尝试 Candidate 2 时，downloadResume 重新读取 fi.Size() (line 85) 作为 offset 并向 Candidate 2 发起 Range: bytes=offset- 请求。若 Candidate 2 支持 Range，将在已截断/损坏的 offset 处追加数据，造成最终重命名文件的数据损坏。已查 boot_tools.go/sysmgr 无上层校验层。
- 问题: downloadResume 在遍历 CandidateURLs 时，若某个候选源返回 HTTP 200 OK（不支持 HTTP Range 请求），downloadChunk 会使用 os.O_TRUNC 覆盖打开 partPath 重新写入。若本次下载中途中断返回错误，downloadResume 在 lines 84-86 重新通过 os.Stat(partPath) 更新 offset 为当前已写入字节数。在接下来的 Candidate 循环中，后续支持 Range 的候选源将从该中途截断的 offset 处发起 Range 请求并追加，导致前段内容丢失或数据错位位移，破坏下载文件完整性。
- 证据: internal/downloader/http.go:80-87
  ```go
  for _, url := range candidates {
  	if err := downloadChunk(ctx, client, url, partPath, offset); err != nil {
  		slog.Warn("downloader: source failed, trying fallback", "url", url, "err", err)
  		if fi, statErr := os.Stat(partPath); statErr == nil {
  			offset = fi.Size()
  		}
  ```
- 修复方向提示: 在 downloadChunk 中若服务端返回 200 OK（不支持 Range），重置写入偏移量并在失败时将 offset 归零或清理不完整的 .part 文件。

---

## 已审文件清单

- `internal/downloader/extract.go`
- `internal/downloader/git.go`
- `internal/downloader/http.go`
- `internal/downloader/proxy.go`
- `internal/downloader/sysproxy_unix.go`
- `internal/downloader/sysproxy_windows.go`
- `internal/downloader/types.go`
- `internal/ffi/dlopen_unix.go`
- `internal/ffi/dlopen_windows.go`
- `internal/ffi/dylib.go`
- `internal/ffi/llama.go`
- `internal/ffi/vec_ops.go`
- `internal/observability/auto_config.go`
- `internal/observability/auto_config_pressure.go`
- `internal/observability/auto_config_tiers.go`
- `internal/observability/budget/budget.go`
- `internal/observability/doc.go`
- `internal/observability/logger.go`
- `internal/observability/metrics/cardinality_guard.go`
- `internal/observability/metrics/cognitive.go`
- `internal/observability/metrics/instruments.go`
- `internal/observability/metrics/instruments_init.go`
- `internal/observability/metrics/metrics.go`
- `internal/observability/metrics/metrics_handler.go`
- `internal/observability/metrics/metrics_surprise.go`
- `internal/observability/metrics/metrics_tokenburn.go`
- `internal/observability/metrics/performance_drift.go`
- `internal/observability/metrics/record.go`
- `internal/observability/probe/feature_gate.go`
- `internal/observability/probe/feature_gate_degradation.go`
- `internal/observability/probe/hardware_probe.go`
- `internal/observability/probe/memory_probe.go`
- `internal/observability/probe/memory_probe_darwin.go`
- `internal/observability/probe/memory_probe_linux.go`
- `internal/observability/probe/memory_probe_wasip1.go`
- `internal/observability/probe/memory_probe_windows.go`
- `internal/observability/probe/process_rss.go`
- `internal/observability/probe/process_rss_darwin.go`
- `internal/observability/probe/process_rss_linux.go`
- `internal/observability/probe/process_rss_wasip1.go`
- `internal/observability/probe/process_rss_windows.go`
- `internal/observability/probe/tier_parameters.go`
- `internal/observability/trace/exporter.go`
- `internal/observability/trace/otlp_exporter.go`
- `internal/observability/trace/record.go`
- `internal/observability/trace/tracer.go`
- `internal/store/audit/chain.go`
- `internal/store/audit/decisionlog.go`
- `internal/store/audit/eventlog.go`
- `internal/store/mutation_bus.go`
- `internal/store/mutation_bus_execute.go`
- `internal/store/outbox_worker.go`
- `internal/store/provider.go`
- `internal/store/repo/repo_app.go`
- `internal/store/repo/repo_audit.go`
- `internal/store/repo/repo_automation.go`
- `internal/store/repo/repo_budget.go`
- `internal/store/repo/repo_channel.go`
- `internal/store/repo/repo_chat.go`
- `internal/store/repo/repo_cron.go`
- `internal/store/repo/repo_event.go`
- `internal/store/repo/repo_extension.go`
- `internal/store/repo/repo_extension_apps.go`
- `internal/store/repo/repo_extension_mcp.go`
- `internal/store/repo/repo_mock.go`
- `internal/store/repo/repo_modelversion.go`
- `internal/store/repo/repo_provider.go`
- `internal/store/repo/repo_system.go`
- `internal/store/repo/repo_task.go`
- `internal/store/repo/repo_task_checkpoint.go`
- `internal/store/repo/repo_workflow.go`
- `internal/store/schema_manager.go`
- `internal/store/search/batcher_adapter.go`
- `internal/store/search/corpus_stats.go`
- `internal/store/search/embedding_batcher.go`
- `internal/store/search/hybrid_retrieve.go`
- `internal/store/search/hybrid_retriever.go`
- `internal/store/search/reranker.go`
- `internal/store/search/semantic_cache.go`
- `internal/store/search/surreal_cache_store.go`
- `internal/store/storage_router.go`
- `internal/store/store.go`
- `internal/store/store_ops.go`
- `internal/store/surreal_store.go`
- `internal/store/surreal_store_ext.go`
- `internal/store/surreal_store_helpers.go`
- `internal/sysinfo/sysinfo.go`
- `internal/sysinfo/sysinfo_unix.go`
- `internal/sysinfo/sysinfo_wasip1.go`
- `internal/sysinfo/sysinfo_windows.go`
- `pkg/apperr/apperr.go`
- `pkg/apperr/sentinel.go`
- `pkg/concurrent/safe_go.go`
- `pkg/graph/dag.go`
- `pkg/graph/state_graph.go`
- `pkg/types/consts.go`
- `pkg/types/doc.go`
- `pkg/types/enums_agent.go`
- `pkg/types/enums_llm.go`
- `pkg/types/enums_security.go`
- `pkg/types/enums_storage.go`
- `pkg/types/enums_swarm.go`
- `pkg/types/enums_tool.go`
- `pkg/types/ext_type.go`
- `pkg/types/models_agent.go`
- `pkg/types/models_db.go`
- `pkg/types/models_eval.go`
- `pkg/types/models_event.go`
- `pkg/types/models_headless.go`
- `pkg/types/models_llm.go`
- `pkg/types/models_memory.go`
- `pkg/types/models_other.go`
- `pkg/types/models_skill.go`
- `pkg/types/models_tool.go`
- `pkg/types/models_user.go`
- `pkg/util/fts5.go`
- `pkg/util/id.go`
- `pkg/version/version.go`

## 明确未覆盖的范围

无

## 审了但无发现的模块

- `pkg/`（`apperr` / `concurrent` / `graph` / `types` / `util` / `version`）
- `internal/sysinfo`
- `internal/ffi`
