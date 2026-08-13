# Polaris 代码轨道批次 5 审核报告

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-5-001 | P2 | internal/memory | MemImpl.InjectRelevantMemory 接口与实现未接线，零生产调用方 | 高 | 是 |
| GR-5-002 | P2 | internal/memory | MemImpl.ConfigureWorkingMemBudget 与 InjectSkillRegistry 未接线 | 高 | 是 |
| GR-5-003 | P2 | internal/memory | MemorySystemImpl/MemoryFacadeImpl 的 Forget 方法无生产调用方 | 高 | 是 |
| GR-5-004 | P1 | internal/tool | run_command 与 bash 内置工具对 RiskHITL 风险指令静默放行，绕过 HITL 审批 | 高 | 是 |
| GR-5-005 | P2 | internal/tool | CompositeCatalog.CleanupSession 未在会话终态接入，存在 activeSessions 内存泄漏风险 | 高 | 是 |

置信度分布声明: 本批次所有 5 条发现均附带明确的源码路径与行号，且已按 §2-A 执行完整的四处反证核对（确认 boot_*.go、bootstrap、注册表、反射及 struct tag 驱动点无生产调用），无需假定运行时条件，故置信度均判定为高。

### [GR-5-001] MemImpl.InjectRelevantMemory 接口与实现未接线，零生产调用方
- 严重级: P2
- 模块: internal/memory（层: L1）
- 位置: internal/memory/memory.go:72
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/protocol/ 接口声明处、反射/结构体 Tag 驱动点均无 InjectRelevantMemory 生产调用。
- 问题: MemImpl 中实现了 InjectRelevantMemory 方法（并在 internal/agent/agent_wiring.go 中定义了同名接口），用于根据 query 检索相关记忆片段组装上下文。然而该方法全仓零生产调用方，导致 Memory 层的相关记忆主动注入能力处于死代码状态。
- 证据:
  ```go
  // InjectRelevantMemory 提取与 query 相关的高价值实体与文档片段，组装为上下文供 LLM 注入。
  func (m *MemImpl) InjectRelevantMemory(ctx context.Context, sessionID string, query string) (string, error) {
  	if query == "" {
  ```
- 修复方向提示: 在 Agent 思考循环上下文组装路径中接入 InjectRelevantMemory 或清理无用导出接口声明。

### [GR-5-002] MemImpl.ConfigureWorkingMemBudget 与 InjectSkillRegistry 未接线
- 严重级: P2
- 模块: internal/memory（层: L1）
- 位置: internal/memory/memory.go:214
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、注册表/工厂注册点、反射驱动点均无 ConfigureWorkingMemBudget 与 InjectSkillRegistry 的生产调用。
- 问题: MemImpl.ConfigureWorkingMemBudget 负责设置工作记忆 Token 预算并关联 EpisodicMem 以触发 WorkingMem 溢出换页，InjectSkillRegistry 负责将技能注册表注入 ProceduralMem。两者在 internal/memory/memory.go 导出但生产环境引导逻辑中未被调用，导致工作记忆预算配置与过程记忆技能注入未接入。
- 证据:
  ```go
  // ConfigureWorkingMemBudget sets the token budget and episodic memory for WorkingMem paging.
  func (m *MemImpl) ConfigureWorkingMemBudget(budget int) {
  	m.working.SetTokenBudget(budget)
  	m.working.SetEpisodic(m.episodic)
  }
  ```
- 修复方向提示: 在 boot_memory.go 的 BootMemory 或初始化链中补充 WorkingMem 预算与 SkillRegistry 注入逻辑。

### [GR-5-003] MemorySystemImpl/MemoryFacadeImpl 的 Forget 方法无生产调用方
- 严重级: P2
- 模块: internal/memory（层: L1）
- 位置: internal/memory/memory_system.go:149
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、gateway/sysadmin、automation 调度处均无 Forget() 生产调用，仅在 memory_system_test.go 中被测试调用。
- 问题: Forget(ctx context.Context) (int, error) 在 MemorySystemImpl、MemoryFacadeImpl 及 EpisodicMem 中均有完整实现，用于清理遗忘过期或低显著度情景记忆，但在生产逻辑中没有任何定时器、调度器或 API 调用该方法，导致记忆主动遗忘机制物理失联。
- 证据:
  ```go
  func (ms *MemorySystemImpl) Forget(ctx context.Context) (int, error) {
  	n, err := ms.episodic.Forget(ctx)
  	return n, err
  }
  ```
- 修复方向提示: 在 ForgettingManager 或 idle_evolution 自动维护任务中接通 Memory.Forget 周期调度。

### [GR-5-004] run_command 与 bash 内置工具对 RiskHITL 风险指令静默放行，绕过 HITL 审批
- 严重级: P1
- 模块: internal/tool（层: L1）
- 位置: internal/tool/builtin/run_command/run_command.go:49
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: AST check for switch cases on classifier.RiskHITL missing HITL prompt or return）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/tool/builtin/run_command/run_command.go 与 bash/bash.go：Phase1 模式下 switch 匹配 RiskHITL 仅打印 slog.Warn，未调用 hitl.Prompt 或返回挂起错误，即落入后续子进程执行逻辑；从外部输入入口到 ExecuteTool -> MakeRunCommandFn 均可直接达该代码行。
- 问题: run_command.go 与 bash.go 中的安全规则审查在匹配到 classifier.RiskHITL（需要人工审批的高风险指令）时，仅在日志中输出 executing in Phase1 mode 警告，随后直接透传执行命令，未能通过 HITLGateway 发起人机交互审批，违反 HE-2（可验证执行）与防退化安全边界。
- 证据:
  ```go
  case classifier.RiskHITL:
  	slog.Warn("run_command: command requires human approval (HITL) — executing in Phase1 mode",
  		"cmd", args.Command, "reason", verdict.Reason)
  ```
- 修复方向提示: 在 RiskHITL 分支中接入 HITLGateway.Prompt 发起审批或当 HITL 未配置时 fail-closed 拦截。

### [GR-5-005] CompositeCatalog.CleanupSession 未在会话终态接入，存在 activeSessions 内存泄漏风险
- 严重级: P2
- 模块: internal/tool（层: L1）
- 位置: internal/tool/catalog/composite.go:234
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/gateway/session/、internal/agent/ 各种会话关停与垃圾回收点，均无 CompositeCatalog.CleanupSession 调用，仅在 composite_test.go 中被调用。
- 问题: CompositeCatalog 维护了 activeSessions map[string]map[string]bool 用于追踪各会话经 search_tools 动态激活的工具列表。然而 CleanupSession 未在会话结束或清除时被调用，长期运行的服务随着会话增加将导致 activeSessions map 无限累积，造成内存泄漏。
- 证据:
  ```go
  // CleanupSession cleans up activated tools when a session ends to prevent memory leaks.
  func (c *CompositeCatalog) CleanupSession(sessionID string) {
  	if sessionID == "" {
  ```
- 修复方向提示: 在 SessionOrchestrator 或 Agent 关停/清理生命周期钩子中接入 catalog.CleanupSession(sessionID)。

## 已审文件清单

- `internal/memory/compact/compact.go`
- `internal/memory/consolidation/consolidation.go`
- `internal/memory/consolidation/consolidation_archive.go`
- `internal/memory/consolidation/consolidation_background.go`
- `internal/memory/consolidation/consolidation_extract.go`
- `internal/memory/consolidation/consolidation_profile.go`
- `internal/memory/consolidation/consolidation_skills.go`
- `internal/memory/consolidation/consolidation_summary.go`
- `internal/memory/consolidation/disk_probe_unix.go`
- `internal/memory/consolidation/disk_probe_windows.go`
- `internal/memory/consolidation/episodic_projector.go`
- `internal/memory/consolidation/event_archiver.go`
- `internal/memory/consolidation/per_message_extractor.go`
- `internal/memory/consolidation/retrieval_reinforcer.go`
- `internal/memory/consolidation/semantic_compress_handler.go`
- `internal/memory/consolidation/summarizer.go`
- `internal/memory/facade.go`
- `internal/memory/graph/edge_weight.go`
- `internal/memory/graph/episodic_graph_bridge.go`
- `internal/memory/graph/mmd_canvas.go`
- `internal/memory/graph/temporal.go`
- `internal/memory/memory.go`
- `internal/memory/memory_system.go`
- `internal/memory/provider.go`
- `internal/memory/retrieval/cascade_invalidator.go`
- `internal/memory/retrieval/cognitive_replayer.go`
- `internal/memory/retrieval/exclusive_closure.go`
- `internal/memory/retrieval/online_reindexer.go`
- `internal/memory/retrieval/query_classifier.go`
- `internal/memory/retrieval/query_classifier_semantic.go`
- `internal/memory/retrieval/retriever.go`
- `internal/memory/retrieval/retriever_construct.go`
- `internal/memory/retrieval/retriever_helpers.go`
- `internal/memory/retrieval/source.go`
- `internal/memory/retrieval/write_filter.go`
- `internal/memory/store/core_memory.go`
- `internal/memory/store/durative_mem.go`
- `internal/memory/store/episodic_mem.go`
- `internal/memory/store/episodic_mem_lifecycle.go`
- `internal/memory/store/episodic_mem_overflow.go`
- `internal/memory/store/immutable_core_prompt.go`
- `internal/memory/store/notes_store.go`
- `internal/memory/store/reflection_mem.go`
- `internal/memory/store/semantic_mem.go`
- `internal/memory/store/semantic_mem_query.go`
- `internal/memory/store/semantic_relation_temporal.go`
- `internal/memory/store/sql_reflection_mem.go`
- `internal/memory/store/types.go`
- `internal/memory/store/working_mem.go`
- `internal/memory/testutil/mock.go`
- `internal/memory/util/simhash.go`
- `internal/memory/vfs_offloader.go`
- `internal/tool/builtin/a2a_tools.go`
- `internal/tool/builtin/audio_convert.go`
- `internal/tool/builtin/bash/bash.go`
- `internal/tool/builtin/bash/sandboxed_exec.go`
- `internal/tool/builtin/builtin_tools.go`
- `internal/tool/builtin/core_memory_edit.go`
- `internal/tool/builtin/core_memory_edit_exec.go`
- `internal/tool/builtin/cron_tools.go`
- `internal/tool/builtin/csv_parse/csv_parse.go`
- `internal/tool/builtin/data_query/data_query.go`
- `internal/tool/builtin/diff_text/diff_text.go`
- `internal/tool/builtin/execute_wasm/tool.go`
- `internal/tool/builtin/fetch_url/fetch_url.go`
- `internal/tool/builtin/get_datetime/get_datetime.go`
- `internal/tool/builtin/get_task_result/get_task_result.go`
- `internal/tool/builtin/git_text_tools.go`
- `internal/tool/builtin/glob/glob.go`
- `internal/tool/builtin/grep/grep.go`
- `internal/tool/builtin/guard/guard.go`
- `internal/tool/builtin/list_a2a_agents.go`
- `internal/tool/builtin/list_a2a_agents_exec.go`
- `internal/tool/builtin/list_dir/list_dir.go`
- `internal/tool/builtin/memory_tools.go`
- `internal/tool/builtin/memory_tools_exec.go`
- `internal/tool/builtin/multi_edit/multi_edit.go`
- `internal/tool/builtin/notebook_edit/notebook_edit.go`
- `internal/tool/builtin/notebook_read/notebook_read.go`
- `internal/tool/builtin/read_file/read_file.go`
- `internal/tool/builtin/read_tool_ref/read_tool_ref.go`
- `internal/tool/builtin/run_command/run_command.go`
- `internal/tool/builtin/sandboxenv/sandboxenv.go`
- `internal/tool/builtin/skill_tools.go`
- `internal/tool/builtin/str_replace_editor/str_replace_editor.go`
- `internal/tool/builtin/sys_probe/sys_probe.go`
- `internal/tool/builtin/todo_read/todo_read.go`
- `internal/tool/builtin/todo_write/todo_write.go`
- `internal/tool/builtin/tts_edge/tts_edge.go`
- `internal/tool/builtin/video_analysis/video_analysis.go`
- `internal/tool/builtin/web_search/web_search.go`
- `internal/tool/builtin/write_file/write_file.go`
- `internal/tool/catalog/catalog.go`
- `internal/tool/catalog/composite.go`
- `internal/tool/catalog/memory_catalog.go`
- `internal/tool/catalog/skill_catalog.go`
- `internal/tool/dispatch/dispatcher.go`
- `internal/tool/dispatch/interceptors.go`
- `internal/tool/dispatch/provider.go`
- `internal/tool/loader.go`
- `internal/tool/sandbox/argv_wrapper_adapter.go`
- `internal/tool/sandbox/cmd_runner_adapter.go`
- `internal/tool/sandbox/rust_native_sandbox.go`
- `internal/tool/sandbox/rust_wasmtime_sandbox.go`
- `internal/tool/sandbox/wasm_quota.go`
- `internal/tool/sandbox/wasmtime_sandbox.go`
- `internal/tool/tool.go`
- `internal/tool/tool_helpers.go`
- `internal/tool/tool_outcome.go`
- `internal/tool/tool_pii.go`
- `internal/tool/tool_search.go`
- `internal/tool/tool_taint_egress.go`

## 明确未覆盖的范围

无

## 审了但无发现的模块

- `internal/memory/compact`
- `internal/memory/consolidation`
- `internal/memory/graph`
- `internal/memory/retrieval`
- `internal/memory/store`
- `internal/memory/util`
- `internal/memory/vfs_offloader.go`
- `internal/tool/dispatch`
- `internal/tool/sandbox`
