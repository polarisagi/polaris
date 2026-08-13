<!-- 由 make review-merge 生成；状态列人工/修复轮维护，合并时保留 -->
| ID | 模块 | 位置 | 一句话标题 | 状态(open/fixed/rejected) | 处置说明 |
|---|---|---|---|---|---|
| GR-10-002 | internal/eval（层: L3） | internal/eval/analysis/shadow_executor.go:281 | ShadowExecutor.scoreShadow 在 llmProvider 为 nil 时降级放行导致未评估候选版本自动确认上线 | fixed | WP-1 fail-closed 修复，commit 3cef8d5 |
| GR-10-004 | internal/channel（层: L3） | internal/channel/adapter/email.go:105 | EmailAdapter.Send 经 EmailSendMessage 直调 smtp.SendMail 绕过 SafeDialer SSRF 防护 | fixed | WP-3 Egress 硬化，commit f0641d0 |
| GR-6-001 | internal/vfs（层: L1） | internal/vfs/workspace_manager_ephemeral.go:53 | StageEphemeralFile 缺失 filename 路径穿越校验导致任意文件写突破沙箱隔离 | fixed | WP-1 fail-closed 修复，commit 3cef8d5 |
| GR-7-001 | internal/knowledge（层: L2） | internal/knowledge/rag_retrieval.go:340 | KnowledgeBase.Search 缺失 TaintMax 校验导致高污染数据穿越安全隔离区 | fixed | WP-2 Taint & Stream Security，commit 005e5ce |
| GR-9-002 | internal/gateway/authcontext（层: L3） | internal/gateway/authcontext/contextref.go:239-251 | ContextRefExpander.resolveFile 未校验 workDir 隔离边界导致任意文件读取 | fixed | WP-1 fail-closed 修复，commit 3cef8d5 |
| GR-1-001 | internal/store（层: L0） | internal/store/mutation_bus_execute.go:177, internal/store/mutation_bus_execute.go:258, internal/store/mutation_bus.go:264, internal/store/mutation_bus.go:280 | MutationBus 单写者在超时与版本冲突路径上直接向 ResultCh 写通道引发永久死锁 | fixed | WP-4 非阻塞 select 包裹，commit fd09cd1 |
| GR-1-002 | internal/store（层: L0） | internal/store/outbox_worker.go:362 | OutboxWorker 处理毒丸记录时误返回 nil 导致死字记录被更新为 done 状态 | fixed | WP-4 毒丸修复，commit fd09cd1 |
| GR-10-001 | internal/automation（层: L3） | internal/automation/queue.go:168 | SQLiteScheduler.scanAndDispatch 零过滤 running 状态导致长耗时任务重复并发调度 | fixed | WP-6 调度幂等，commit 164be51 |
| GR-11-001 | rust/substrate（层: rust） | rust/substrate/src/surreal_store/fts.rs:25 | surreal_store FFI 导出函数缺乏 NULL 指针前置校验导致潜在空指针解引用未定义行为 | fixed | WP-8 FFI NULL 守卫，commit f9f86bd |
| GR-2-001 | internal/llm（层: L0） | internal/llm/circuit_breaker.go:70 | circuitBreaker 在 HalfOpen 探测失败时未恢复到 circuitOpen 状态，导致熔断器探活失败后卡死在全量放行状态 | fixed | WP-7 熔断器并发泄漏，commit 75701d9 |
| GR-3-001 | internal/bootstrap（层: 契约） | docs/specs/04-Module-Boundary.md:46 | 04-Module-Boundary.md 声明 internal/bootstrap 被 cmd/polaris 引用与代码/架构文档矛盾 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-3-002 | internal/bootstrap（层: 契约） | internal/bootstrap/bootstrapper.go:134 | Bootstrapper 四阶关停按 map 随机序遍历破坏逆序依赖优雅关停 | fixed | WP-5 生命周期修复，commit 92039ea |
| GR-4-001 | internal/action（层: L1） | internal/action/codeact/code_act_stateful.go:97 | CodeAct Python Stateful 模式在 Sbx-L3 容器内打开宿主快照路径因未捕获 `FileNotFoundError` 导致脚本退出报错 | fixed | WP-10.2 GR-4-001 try/except，commit cb7ca5d |
| GR-4-002 | internal/agent（层: L1） | internal/agent/context/memory_context.go:361 | BuildReflectContext 组装反思 Prompt 时硬编码 ExecuteResult 污点等级为 TaintMedium 导致高污点标记静默丢失 | fixed | WP-2 动态污点，commit 005e5ce |
| GR-5-004 | internal/tool（层: L1） | internal/tool/builtin/run_command/run_command.go:49 | run_command 与 bash 内置工具对 RiskHITL 风险指令静默放行，绕过 HITL 审批 | fixed | WP-1 fail-closed，commit 3cef8d5 |
| GR-6-002 | internal/execute/orchestrator（层: L1） | internal/execute/orchestrator/pattern_state_graph.go:300 | StateGraphExecutor 无条件多节点循环边被误入 AND-Join 硬依赖集合导致状态图死锁 | fixed | WP-6 StateGraph 死锁预防，commit 164be51 |
| GR-7-002 | internal/learning（层: L2） | internal/learning/engine.go:234 | Engine.Start 对 taskEvents/versionEvents 通道关闭误用 return nil 导致后台自进化引擎提前异常关停 | fixed | WP-5 生命周期，commit 92039ea |
| GR-8-001 | internal/extension/marketplace（层: L2） | internal/extension/marketplace/marketplace.go:40 | NewMCPMarketplaceClient 中的 SafeHTTPClient 校验缺乏 nil 空值防护导致 panic 崩溃 | fixed | WP-1 fail-closed，commit 3cef8d5 |
| GR-8-002 | internal/extension/skill（层: L2） | internal/extension/skill/skill_creator.go:234 | skill_creator 与 plugin_creator 的 extractJSON 贪婪正则 (?s)\{.*\} 导致多括号 LLM 响应解析失败 | fixed | WP-10.2 括号计数扫描 pkg/util，commit cb7ca5d |
| GR-9-001 | internal/gateway/server（层: L3） | internal/gateway/server/server_handlers_hitl.go:131 | handleAgentInterrupt 未将 WebUI 默认客户端类型纳入 whitelist 导致中断请求遭 403 拒绝 | fixed | WP-9 ClientType 枚举 SSoT，commit 4cf0aa4 |
| GR-1-003 | internal/observability（层: L0） | internal/observability/probe/tier_parameters.go:35, internal/observability/auto_config_tiers.go:30 | TierParameters 仍保留已废弃的 GraphRAGLLMDailyBudget 字段而缺失 GraphRAGConcurrentWorkers | fixed | WP-10.2 Tier 参数修复，commit cb7ca5d |
| GR-1-004 | internal/downloader（层: L0） | internal/downloader/http.go:42, internal/downloader/http.go:84 | downloadResume 多源降级时误把不支持 Range 的截断 partial 文件尺寸当作后续 Candidate 偏移量导致数据损坏 | fixed | WP-10.2 跨源续传同一性校验，commit cb7ca5d |
| GR-10-003 | internal/cli（层: L3） | internal/cli/cli.go:20 | internal/cli 模块包含 AgentREPL 等导出类型但零生产接线且未列入白名单 | fixed | WP-10.1 deadcode-allowlist 备案+文档更新，commit 03bd9c6 |
| GR-10-005 | internal/automation（层: L3） | internal/automation/idle_evolution.go:115 | IdleEvolutionScheduler 在后台任务完成时未清理 cancelFuncs 导致后续空闲周期永久阻断 | fixed | WP-5 生命周期，commit 92039ea |
| GR-12-001 | docs/arch（层: L0） | docs/arch/M02-Storage-Fabric.md:433 | M02 §16 DDL 全量文件数与末尾编号与 schema 现状漂移 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-12-002 | docs/arch（层: L1） | docs/arch/M04-Agent-Kernel.md:46 | M04 §1 状态枚举声明列表缺漏且权威源文件名标注错误 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-12-003 | docs/arch（层: 契约） | docs/arch/INDEX.md:101 | INDEX §1 场景加载预算中多份核心文档 est_tok 估算严重低估 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-12-004 | docs/specs（层: 契约） | docs/specs/00-Constitution.md:101 | 宪法 R2.5 错误码字典缺失 CodeStorageUnavailable 权威定义 | fixed | WP-12 已存在，无需改动 |
| GR-12-005 | docs/arch（层: L0） | docs/arch/00-Global-Dictionary.md:628 | 全局字典 §12 追溯表中各模块 DDL 文件范围陈旧 | fixed | WP-12 已修正，commit 14847e7 |
| GR-12-006 | docs/arch（层: L0） | docs/arch/M02-Storage-Fabric.md:178 | M02 §2 tasks 表结构列说明遗漏 spawn_depth 字段 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-12-007 | docs/arch（层: L3） | docs/arch/M13-bis-Extension-Registry.md:72 | M13-bis §2.2 表格中 origin 枚举值与 DDL/类型定义自相矛盾 | fixed | WP-12 文档漂移，commit 14847e7 |
| GR-2-002 | internal/security（层: L0） | internal/security/network/safe_dialer.go:353 | SafeDialer.dnsCache 缺乏容量上限与淘汰机制，长时运行下存在无界内存泄露风险 | fixed | WP-10.2 DNS 缓存容量限制，commit cb7ca5d |
| GR-2-003 | internal/security（层: L0） | internal/security/provider.go:27 | security/provider.go 声明的 AuditRepo / KillSwitchMetrics / GuardProvider 接口全仓零消费方 | fixed | WP-10.1 删除过渡接口，commit 03bd9c6 |
| GR-3-003 | internal/bootstrap（层: 契约） | internal/bootstrap/bootstrapper.go:93 | Bootstrapper.Ignite 模块初始化中途失败缺乏已初始化模块的回滚清理 | fixed | WP-5 Bootstrapper 回滚，commit 92039ea |
| GR-5-001 | internal/memory（层: L1） | internal/memory/memory.go:72 | MemImpl.InjectRelevantMemory 接口与实现未接线，零生产调用方 | fixed | WP-10.1 删除冗余接口，commit 03bd9c6 |
| GR-5-002 | internal/memory（层: L1） | internal/memory/memory.go:214 | MemImpl.ConfigureWorkingMemBudget 与 InjectSkillRegistry 未接线 | partial | InjectSkillRegistry 已接线；ConfigureWorkingMemBudget 因 config 无对应阈值暂缓，登记 99-遗留线索.md |
| GR-5-003 | internal/memory（层: L1） | internal/memory/memory_system.go:149 | MemorySystemImpl/MemoryFacadeImpl 的 Forget 方法无生产调用方 | fixed | WP-10.1 删除冗余实现（ForgettingManager 已覆盖），commit 03bd9c6 |
| GR-5-005 | internal/tool（层: L1） | internal/tool/catalog/composite.go:234 | CompositeCatalog.CleanupSession 未在会话终态接入，存在 activeSessions 内存泄漏风险 | fixed | WP-10.1 Pool.OnSessionClose 钩子接线，commit 03bd9c6 |
| GR-6-003 | internal/vfs（层: L1） | internal/vfs/provider.go:1 | provider.go 存留 2 行无符号声明空文件 | fixed | WP-10.2 删除空文件，commit cb7ca5d |
| GR-7-003 | internal/swarm（层: L2） | internal/swarm/planner/pool.go:175 | PlannerPool.workerEngineA 派生超时 context 后未校验 parent context 导致取消信号下仍然运行高消耗沙箱构建 | fixed | WP-5 PlannerPool context 修复，commit 92039ea |
| GR-8-003 | internal/extension/skill（层: L2） | internal/extension/skill/skill_executor.go:85 | ScriptSkillExecutor 限流触发时错误码误用 CodeInternal 替代 CodeResourceExhausted | fixed | WP-10.2 错误码修正，commit cb7ca5d |
| GR-8-004 | internal/extension/mcp（层: L2） | internal/extension/mcp/mcp_manager_tools.go:211 | makeMCPToolAsyncFn 异步变体工具返回结果未标注 TaintLevel 退化为 TaintNone | rejected | 误报，见 00-核验结论.md §1.1 |
| GR-8-005 | internal/extension/mcp（层: L2） | internal/extension/mcp/mcp_client_stdio.go:72 | stdio readLoop 的 bufio.Scanner 硬编码 1MB 缓冲区上限导致大载荷 MCP 响应断连 | fixed | WP-10.2 从 config 读取，commit cb7ca5d |
| GR-9-003 | internal/gateway/server（层: L3） | internal/gateway/server/server_routes.go:203 | HandleTogglePluginMCP 路由与 handler 路径参数不匹配导致永远 400 | fixed | WP-9 路由参数修复，commit 4cf0aa4 |
