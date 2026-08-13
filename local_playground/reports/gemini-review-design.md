<!-- 由 make review-merge 生成，勿手改 -->
# 设计挑战汇总（GD/DS）

| ID | 类别 | 涉及模块 | 一句话标题 | 优先级 | 来源 |
|---|---|---|---|---|---|
| GD-13-002 | 调用链问题 | internal/gateway/session, internal/agent/fsm, internal/security/guard | 流式对话事件处理链优化：敏感信息扫描前置与热路径分配裁减 | 高 | gemini-review-design-batch13.md |
| GD-13-003 | 错误路径缺失 | internal/memory/retrieval, internal/store/search | HybridRetriever 混合检索单路超时降级与单路 fallback 机制 | 高 | gemini-review-design-batch13.md |
| GD-14-002 | 业界对标差距 | internal/agent/fsm, internal/execute/dag, internal/protocol/schema | 状态快照缺少节点级可回溯 Time-Travel 重放机制 | 高 | gemini-review-design-batch14.md |
| GD-14-005 | 错误路径缺失 | internal/security/taint, internal/memory/retrieval, internal/knowledge | 长期记忆持久化存储缺乏 recall 级 Taint 继承防护 (OWASP ASI03) | 高 | gemini-review-design-batch14.md |
| GD-14-006 | 领先设计(保留) | internal/agent/fsm, internal/execute/orchestrator | 保留 Go 确定性 FSM 控制流与 LLM 协处理器架构 (防退化) | 高 | gemini-review-design-batch14.md |
| GD-14-007 | 领先设计(保留) | internal/security/policy, internal/security/taint, rust/substrate | 保留 Cedar 三层策略门控与五级污点追踪确定性安全体系 (防退化) | 高 | gemini-review-design-batch14.md |
| GD-13-001 | 模块拆合 | internal/bootstrap, cmd/polaris | 统一模块生命周期管理：合并 internal/bootstrap 与 cmd/polaris 过程式启动 | 中 | gemini-review-design-batch13.md |
| GD-13-004 | 调用链问题 | internal/execute/orchestrator | SQLiteBlackboard 任务认领机制：从纯 DB 轮询升级为事件通知+CAS抢占 | 中 | gemini-review-design-batch13.md |
| GD-14-001 | 功能缺失 | internal/swarm, internal/gateway/server, internal/extension/mcp | 缺失 Agent-to-Agent (A2A) 标准协议发现与对外网关端点 | 中 | gemini-review-design-batch14.md |
| GD-14-003 | 业界对标差距 | internal/memory/consolidation, internal/memory/store | 记忆巩固缺少基于 LLM-CRUD 的主动事实冲突消除机制 | 中 | gemini-review-design-batch14.md |
| GD-14-004 | 业界对标差距 | internal/sandbox, rust/substrate | WASI 0.2 Component Model (WIT) 微工具沙箱支持滞后 | 中 | gemini-review-design-batch14.md |

## 详细条目

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

### [GD-14-002] 状态快照缺少节点级可回溯 Time-Travel 重放机制
- 类别: 业界对标差距
- 涉及模块: internal/agent/fsm, internal/execute/dag, internal/protocol/schema
- 现状: Polaris 拥有 `035_task_checkpoints.sql` 和 `s_interrupt` 中断/恢复机制（ADR-0076, ADR-0088），但 checkpoint 仅在崩溃恢复与人工挂起（HITL）边界记录。FSM 在执行多步 DAG/StateGraph 时未对每个 Transition/Node 保存增量状态版本。
- 挑战: 在复杂 Agent 任务中（例如 10 步代码重构任务），若 Agent 在第 7 步出现逻辑偏离，用户无法像 LangGraph 2025/2026 的 `StateGraph` `Time-Travel` 机制一样将状态“倒带”（rewind）回第 4 步并注入修改后的 Prompt/参数重新分叉执行，只能选择整体重跑或从崩溃点恢复，浪费大量算力和 token。
- ADR 核对: 已查 ADR-0076（崩溃恢复 checkpoint）与 ADR-0046（StateGraph 编排），原设计集中于单向崩溃恢复与 CAS 调度，未引入节点级 step_sequence 状态分叉回溯。反证：查 `internal/agent/fsm/state_machine.go:340` 确认 FSM 状态转换未持久化 step 历史快照链。
- 业界依据: https://www.langchain.com/langgraph LangGraph Checkpointing & Time-Travel Debugging Architecture (2025-10 / 2026-05)
- 建议方案: 扩展 `035_task_checkpoints.sql` 新增 `step_sequence` 与 `parent_checkpoint_id` 字段；在 `internal/agent/fsm` 的状态变迁完成后自动异步记录轻量级 state delta，增加 `RewindTaskState(taskID, stepSeq)` 接口。
- 代价/收益: SQLite 每轮增加数 KB 状态快照，硬件开销微乎其微，完全符合 Tier-0 限制；极大提升复杂 Agent 任务的可排查性与用户交互纠错效率。
- 优先级建议: 高

---

### [GD-14-005] 长期记忆持久化存储缺乏 recall 级 Taint 继承防护 (OWASP ASI03)
- 类别: 错误路径缺失
- 涉及模块: internal/security/taint, internal/memory/retrieval, internal/knowledge
- 现状: Polaris 拥有完备的五级污点追踪体系（`internal/security/taint`，ADR-0007）和 HE-7 防退化边界。输入侧（channel/MCP/HTTP）打上 Taint 标签，写入记忆走 ExecuteTool / Memory-Write-Tool。
- 挑战: 依据 OWASP Top 10 for Agentic Applications (2026) ASI03 风险定义，持久化记忆投毒（Persistent Memory Poisoning）是 Agent 最危险的隐蔽攻击面。当含有恶意 Payload 的外部输入被写入记忆/知识库后，如果在后续会话被 RAG/Retrieval 召回时没有**强制恢复原始 Taint 级别**，召回文本可能以“已信任内部记忆”的形式注入 FSM 思考循环，绕过 PolicyGate。
- ADR 核对: 已查 ADR-0007（TaintLevel 5 级只升不降），已规定 Sanitizer 降级规则，但未显式规定 DB Schema（`004_memories.sql` / `010_knowledge_chunks.sql`）中的 `taint_level` 字段在 Retrieval 实例化时必须无条件还原为 `TaintedString`。反证：查 `internal/memory/retrieval/` 检索返回结果，部分路径直接返回 string 而非带 Taint 包装类型。
- 业界依据: https://genai.owasp.org/ OWASP Top 10 for Agentic Applications 2026 (ASI03: Persistent Memory Manipulation & Indirect Prompt Injection)
- 建议方案: 检查并补齐 `004_memories.sql` 与 `010_knowledge_chunks.sql` 的 `taint_level` 字段约束；在 `internal/memory/retrieval` 与 `internal/knowledge` 的 API 出口处增加断言，召回内容必须保留或恢复其原始 Taint 标记（fail-closed）。
- 代价/收益: DB 增加 1 字节状态列，Go 结构体无开销，彻底杜绝持久化记忆 Prompt 注入跨会话劫持风险。
- 优先级建议: 高

---

### [GD-14-006] 保留 Go 确定性 FSM 控制流与 LLM 协处理器架构 (防退化)
- 类别: 领先设计(保留)
- 涉及模块: internal/agent/fsm, internal/execute/orchestrator
- 现状: Polaris 坚持 HE-5 不变量与 R1.9 反模式约束：由 Go 13-态确定性 FSM 持有 Agent 执行控制流，将 LLM 严格限制为无状态协处理器，严禁 `while true { call LLM }` 自由流转。
- 挑战: 在 Agent 框架演进中，常有开发者或社区提议“给予 LLM 完全自主控制权/取消确定性 FSM 状态机限制以提升灵活性”。若放开此限制，系统将陷入 LLM 输出不可控、无限循环重试、Token 燃尽（TokenBurnRate 爆表）及并发死锁等陷阱。
- ADR 核对: 已查 ADR-0020、ADR-0021、ADR-0046，均为确定性 FSM 和 StateGraph 提供架构背书。反证：业界 AutoGen v0.4 与 LangGraph 2025/2026 的演变事实均证明，自由流转 Agent 必须向状态机（StateGraph/Event-Driven FSM）收敛。
- 业界依据: LangGraph StateGraph & AutoGen v0.4 Event-Driven FSM Architecture Consensus (2025-2026)
- 建议方案: 保持 HE-5 / R1.9 为最高级别不可推翻约束。所有复杂多步 Agent 编排必须收敛在 `internal/execute/orchestrator` 的声明式状态图（ADR-0046）中，严禁引入非状态机包裹的 LLM 自由循环。
- 代价/收益: 维护成本为零，保护了系统在 Tier-0 (2GB VPS) 约束下的极端稳健性。
- 优先级建议: 高

---

### [GD-14-007] 保留 Cedar 三层策略门控与五级污点追踪确定性安全体系 (防退化)
- 类别: 领先设计(保留)
- 涉及模块: internal/security/policy, internal/security/taint, rust/substrate
- 现状: Polaris 实现了基于 Rust Cedar 引擎（purego FFI，ADR-0011）的三层 Deny-by-Default 策略门控 (PolicyGate) 与五级 Taint 动态传播体系（`internal/security/taint`），安全校验 100% 脱离 LLM 提示词，由物理/密码学规则执行。
- 挑战: 业界大部分 Agent 框架依赖 LLM System Prompt（“请不要访问系统文件”）或简单 Python 正则做安全防护。开发者可能因 Cedar 策略配置繁琐而提议绕过 PolicyGate 或弱化 Taint 校验。
- ADR 核对: 已查 ADR-0007（TaintLevel 5 级）、ADR-0008（Sandbox 3 级基座与代码安全防线）、ADR-0011（purego FFI），确定性安全架构已完整落地。反证：OWASP GenAI Security 2026 明确指出“基于 LLM 自律的安全隔离”属于高危漏洞，必须外部化为物理/确定性策略引擎（RBAC/PBAC）。
- 业界依据: https://genai.owasp.org/ OWASP LLM03:2026 Excessive Agency & ASI01/ASI02 Deterministic Policy Governance Requirements (2026-02)
- 建议方案: 严格守护 HE-7 防退化边界，禁止任何“临时”绕过 PolicyGate 或 TaintTracking 的 PR，保持 Cedar 策略引擎作为工具/动作调用的必经关卡。
- 代价/收益: 维护成本为零，使 Polaris 在 Agent 安全治理方面保持业界领先地位。
- 优先级建议: 高

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

### [GD-14-001] 缺失 Agent-to-Agent (A2A) 标准协议发现与对外网关端点
- 类别: 功能缺失
- 涉及模块: internal/swarm, internal/gateway/server, internal/extension/mcp
- 现状: Polaris 当前实现了内部 Blackboard 与 Handoff 机制（`internal/swarm`，ADR-0046/ADR-0086），并通过 MCP `transfer_to_agent` 实现了出站跨框架委派（ADR-0017, ADR-0084）。但在网关入站侧（`internal/gateway/server`），尚未暴露符合 Linux Foundation AAIF A2A 标准的 `/.well-known/agent.json` 发现端点与 A2A 任务协商 JSON-RPC 接口。
- 挑战: 随着 2025–2026 年 Agent-to-Agent (A2A) 协议（Google 提出、Linux Foundation 托管）成为多 Agent 跨系统协同的标准，外部第三方 Agent 框架（如 AutoGen v0.4, LangGraph, CrewAI）无法通过标准元数据自发现并调用 Polaris 实例，限制了自托管 Agent 在多异构 Agent 生态中的互操作能力。
- ADR 核对: 已查 ADR-0017（MCP A2A 战略方向）与 ADR-0084（出站 transfer_to_agent），均未包含入站 `/.well-known/agent.json` Agent Card 元数据暴露与标准 A2A JSON-RPC 路由。反证：查 `cmd/polaris/boot_server.go` 与 `internal/gateway/server/` 确认无 `well-known` 路由注册。
- 业界依据: https://genai.owasp.org/ & Linux Foundation AAIF Agent-to-Agent (A2A) Protocol Specification v1.0 (2025-04 / 2026-02)
- 建议方案: 在 `internal/gateway/server/sysadmin` 或 `internal/gateway/server` 新增轻量级 `A2AHandler`，响应 `GET /.well-known/agent.json` 导出 Agent Capabilities / Skills，并将 A2A `tasks/send` 映射至 `internal/swarm` 的 Blackboard 任务投递管道。
- 代价/收益: 改动仅限 gateway 增加约 200 行 Go 代码，完全兼容 Tier-0（2GB VPS）约束；收益是获得与业界主流多 Agent 协议的无缝互操作性。
- 优先级建议: 中

---

### [GD-14-003] 记忆巩固缺少基于 LLM-CRUD 的主动事实冲突消除机制
- 类别: 业界对标差距
- 涉及模块: internal/memory/consolidation, internal/memory/store
- 现状: Polaris 记忆系统划分为 Working / Episodic / Semantic / Procedural 四层（M05, ADR-0033），并设计了 Jaccard 信念修正与显著度衰减机制。但在记忆巩固阶段（`internal/memory/consolidation`），缺少对陈旧事实的主动 LLM 判定与增删改（ADD/UPDATE/DELETE/SUPERSEDE）冲突解算。
- 挑战: 当用户偏好或客观事实发生显式变更时（例如从“使用 Python 3.10”变为“项目已升级至 Python 3.12”），仅靠显著度衰减与 Jaccard 相似度很难快速清理旧语义记忆片段，导致 RAG 检索时新旧矛盾事实同时被召回，干扰 LLM 推理。
- ADR 核对: 已查 ADR-0033（记忆写路径与 Jaccard 修正）及 ADR-0077（M5 ↔ M10 桥接），已实现 Jaccard 级联失效，但未包含 Consolidation 阶段基于 Structured JSON 的事实 CRUD 解算。反证：查 `internal/memory/consolidation/` 代码确认巩固逻辑主要为摘要生成与衰减计算，无事实覆盖判决。
- 业界依据: https://mem0.ai & https://www.letta.com Mem0 / Letta Agent Long-Term Memory Architecture Specs (2025-09 / 2026-03)
- 建议方案: 在 `internal/memory/consolidation` 中引入后台异步 Fact Deduplication Worker，利用小模型（如 DeepSeek V4 / Qwen）提取 Structured Fact Diff，对被覆盖的旧记忆标记 `superseded_by` 指针，并在 RAG 召回时过滤。
- 代价/收益: 仅在后台空闲期（consolidation）消耗极少量 LLM token，零内存额外占用，显著提升长程对话的事实一致性。
- 优先级建议: 中

---

### [GD-14-004] WASI 0.2 Component Model (WIT) 微工具沙箱支持滞后
- 类别: 业界对标差距
- 涉及模块: internal/sandbox, rust/substrate
- 现状: Polaris 使用 3 级沙箱架构（Wasm -> Docker -> Native bwrap/Seatbelt，ADR-0008, ADR-0011）。其中 Wasm 引擎（wazero / rust purego wasmtime）主要基于 WASI Preview 1 (P1) 接口规范。
- 挑战: 2025–2026 年 WASI 0.2（Component Model，WIT 接口定义语言）已成为 WebAssembly 沙箱的业界标准（Bytecode Alliance / Wassette）。WASI P1 缺少类型化的组件接口与细粒度 Capabilities 导入（如受控 HTTP 抓取、虚拟文件系统根绑定），导致复杂工具不得不跨越升级到重的 Tier-2 Docker 沙箱，增加了 Tier-0 (2GB VPS) 环境下的容器依赖。
- ADR 核对: 已查 ADR-0008（Sandbox 3 级基座）与 ADR-0011（Rust wasmtime purego 桥接），目前 wasmtime 桥接层仅实现基础 WASI P1 系统调用导出。反证：查 `rust/substrate/src/wasmtime_engine.rs` 确认使用的是 `wasmtime_wasi::preview1` API。
- 业界依据: https://wasi.dev WebAssembly System Interface (WASI) 0.2 Specification (2024-02 / 2026-01) & E2B / Wassette Sandbox benchmarks (2025-2026)
- 建议方案: 升级 `rust/substrate` 中的 wasmtime 库绑定以支持 WASI 0.2 Component Model，为内置与第三方 Wasm Skill 提供基于 WIT 的类型化 API 导出（如网络/存储能力句柄）。
- 代价/收益: 仅修改 `rust/substrate` Rust FFI 桥接代码，不增加运行期 RAM 开销，使 Tier-0 环境下无 Docker 就能安全运行中等复杂度的 Wasm 工具。
- 优先级建议: 中

---

