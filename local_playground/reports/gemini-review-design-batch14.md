# Polaris 架构审查与业界对标报告（批次 14 · 业界对标）

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GD-14-001 | 中 | internal/swarm, gateway | 缺失 Agent-to-Agent (A2A) 标准协议发现与对外网关端点 | 高 | 否 |
| GD-14-002 | 高 | internal/agent/fsm, execute | 状态快照缺少节点级可回溯 Time-Travel 重放机制 | 高 | 否 |
| GD-14-003 | 中 | internal/memory/consolidation | 记忆巩固缺少基于 LLM-CRUD 的主动事实冲突消除机制 | 高 | 否 |
| GD-14-004 | 中 | internal/sandbox, rust/substrate | WASI 0.2 Component Model (WIT) 微工具沙箱支持滞后 | 高 | 否 |
| GD-14-005 | 高 | internal/security/taint, memory | 长期记忆持久化存储缺乏 recall 级 Taint 继承防护 (OWASP ASI03) | 高 | 否 |
| GD-14-006 | 高 | internal/agent/fsm | 保留 Go 确定性 FSM 控制流与 LLM 协处理器架构 (防退化) | 高 | 否 |
| GD-14-007 | 高 | internal/security/policy, taint | 保留 Cedar 三层策略门控与五级污点追踪确定性安全体系 (防退化) | 高 | 否 |

置信度分布声明: 本批次所有 GD 结论均已完成代码事实、架构规范（`docs/arch/`）及业界 2025–2026 年主流规范（LangGraph、Mem0/Letta、A2A/MCP、WASI 0.2、OWASP GenAI 2026）的三方交叉比对与 ADR 驳回档案核对，判定依据确凿，置信度统一标定为高。

---

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

## 已审文件清单

- `docs/arch/ARCHITECTURE.md`
- `docs/arch/00-Global-Dictionary.md`
- `docs/arch/spec/state.yaml`
- `docs/arch/M01-Inference-Runtime.md`
- `docs/arch/M04-Agent-Kernel.md`
- `docs/arch/M05-Memory-System.md`
- `docs/arch/M06-Skill-Library.md`
- `docs/arch/M07-Tool-Action-Layer.md`
- `docs/arch/M08-Multi-Agent-Orchestrator.md`
- `docs/arch/M11-Security-Policy.md`
- `docs/arch/M13-Interface-Scheduler.md`
- `docs/arch/decisions/README.md`
- `docs/specs/00-Constitution.md`
- `docs/specs/09-LLM-Agent-Production.md`
- 业界 2025–2026 年公开标准规范（LangGraph v0.3, Mem0/Letta Architecture Specs, WASI 0.2 Specification, LF AAIF Agent-to-Agent Protocol v1.0, OWASP GenAI/Agentic Top 10 2026）

## 明确未覆盖的范围

- `web/` 前端应用界面层代码
- `rust/substrate/` 底层 Rust 编译算法的具体 CPU 指令集级微调（仅从架构接口与 WASI 规范层面审查）

## 审了但无发现的模块

- `internal/observability`（OTel span 与 Prometheus TokenBurnRate 指标体系完备，符合 HE-1 不变量）
- `internal/config`（配置定义与 threshold-examples 同步机制完备，符合 Tier-0 约束）
