# Arch Index

> **AI 编程入口**。先读本文件，按场景按需加载下方文档。全量加载 ≈ 500K token，必爆 200K 上下文。

## §1 文档清单（按 token 量降序）

| 文件 | 域 | est_tok | 内容摘要 |
|------|----|---------|----------|
| `spec/state.yaml` | SSoT 规约 | 52K | 状态机 + 全模块阈值（唯一权威） |
| `M11-Policy-Safety.md` | L0 策略 | 41K | 五防线、Cedar、TaintedString、KillSwitch、PII Vault、SSRFGuard |
| `M07-Tool-Action-Layer.md` | L1 工具 | 38K | MCP/A2A、wazero 三级沙箱、Capability Token、Workspace Bridge |
| `M02-Storage-Fabric.md` | L0 存储 | 29K | 三轴存储、EventLog、MutationBus、Outbox、SchemaManager |
| `M05-Memory-System.md` | L1 记忆 | 31K | 四层记忆、ContextAssembler、HybridRetriever、Consolidation |
| `ARCHITECTURE.md` | 总览 | 10K | SSoT 锚点: 定位/硬约束、Staging 7 阶段、HT0 预算、变更控制、配置层 |
| `M04-Agent-Kernel.md` | L1 内核 | 26K | 状态机 10 态、S_VALIDATE 四层、System 1/1.5/2 路由、Saga |
| `M13-Interface-Scheduler.md` | L3 接口 | 28K | HTTP/SSE、HITLGateway、ResourceGovernor、TaskQueue、Web UI 规约（Alpine.js+Tailwind） |
| `M13-bis-Extension-Registry.md` | L3 扩展 | 5K | 三层模型（Market/Instances/Runtime）、安装流、信任门控、文件系统、调用路由、M9 归并 |
| `M10-Knowledge-RAG.md` | L2 知识 | 25K | 文档树、6 阶段摄入、GraphRAG、IncrementalIndexer |
| `M09-Self-Improvement-Engine.md` | L2 自演化 | 25K | 五条无梯度路线、SurpriseIndex 完整版、MEMF、Auto-Curriculum |
| `M06-Skill-Library.md` | L1 技能 | 23K | 技能三件套、Logic Collapse、Wasm 编译、三级检索 |
| `M03-Observability.md` | L0 可观测 | 22K | OTel、TokenBurnRate（CANONICAL）、SurpriseIndex 基础、AutoConfig |
| `00-Global-Dictionary.md` | 字典 | 23K | 全 `[Concept]` 标签定义、XR-01~07 跨模块规则、公理 |
| `M01-Inference-Runtime.md` | L0 推理 | 19K | Provider Router、Model Pool、CircuitBreaker、SemanticCache |
| `M08-Multi-Agent-Orchestrator.md` | L2 协同 | 17K | Blackboard、CAS 认领、Reaper、Supervisor Tree、7 编排模式 |
| `M12-Eval-Harness.md` | L3 评测 | 17K | EvalCase、五层 Evaluator、TrajectoryReplayer、CI 门控 |
| `ROADMAP.md` | 路线 | 7K | 时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单（**人类参考**，AI 默认不加载） |
| `DIAGRAMS.md` | 图谱 | 14K | 时序图（**人类参考**，AI 默认不加载） |

## §2 场景加载预算

| 任务类型 | 必读组合 | 总 tok |
|----------|----------|--------|
| 修改存储 / EventLog / DDL | `00` + `M02` + `state.yaml`(§Storage) | ~80K |
| 修改 Agent 状态机 / Saga | `00` + `M04` + `state.yaml`(§Kernel) | ~80K |
| 修改记忆 / 上下文组装 | `00` + `M05` | ~56K |
| 修改 RAG / 知识图谱 | `00` + `M10` (+ `M05` 如涉混合检索) | ~50~83K |
| 修改工具 / 沙箱 / MCP | `00` + `M07` (+ `M11` 如涉策略边界) | ~63~107K |
| 修改策略 / 安全 / Taint | `00` + `M11` + `state.yaml`(§Safety) | ~120K |
| 修改可观测 / 指标 | `00` + `M03` + `state.yaml`(§Metrics) | ~75K |
| 修改 Provider 路由 / Model Pool | `00` + `M01` | ~44K |
| 修改技能 / Logic Collapse | `00` + `M06` (+ `M09` 如涉蒸馏) | ~48~75K |
| 修改 Orchestrator / Blackboard | `00` + `M08` (+ `M04` 如涉认领协议) | ~42~71K |
| 修改自演化 / 评估循环 | `00` + `M09` (+ `M12` 如涉 CI) | ~50~69K |
| 修改 HTTP API / HITL | `00` + `M13` (+ `M11` 如涉 Auth) | ~46~90K |
| 添加新模块 | `00` + `ARCHITECTURE` + 相邻 1~2 个 `M_X` | ~58~108K |
| 修改 Test-Time Compute / 推理深度 | `00` + `M01` §5.2-bis + `M04` §5 §7.1(两维度模型见 00 §9-ter) | ~62K |
| 修改扩展市场 / 安装流 / extension_instances | `00` + `M13-bis` (+ `M11` 如涉信任门控) | ~10~50K |
| 修改 Plugin Registry / Hook 框架 | `00` + `M07` §14 §15 (+ `M11` 如涉 Taint) | ~63~107K |
| 修改 Custom Agent / CSV Fan-out | `00` + `M08` §12 §13 (+ `M07` 如涉 Plugin) | ~42~80K |
| 修改 AgentSkills 技能格式适配 | `00` + `M06` §9 (+ `M07` §14 如涉 Plugin) | ~48~75K |
| 修改输出真实性 / 引用核验 | `00` + `M11` §6.5 + `M10` §4.1 §4.2 | ~95K |
| 修改即时代码执行 / CodeAct | `00` + `M07` §7.4 + `M04` §5 | ~73K |
| 修改用户中断 / 长程任务控制 | `00` + `M04` §1 + `M13` §1.2.5 | ~75K |
| 修改反思记忆 / Reasoning State | `00` + `M05` §3.4 §3.1 + `M04` §7.1 | ~80K |
| 修改运行时漂移检测 | `00` + `M03` §10.1 + `M12` (§11 RegressionDetector 对比) | ~65K |

## §2.5 章节级跳读

每个 `M_X.md` 文件头第 4~6 行有 **`§跳读`** 单行索引，格式 `id:line title`，列出每个章节起始行号 + SKIP/SOFT 标记：
- **SKIP** = rationale（选型/拒因/风险/快照），AI 编程不读
- **SOFT** = 故障矩阵/降级，修改时按需
- 其余 = 默认读

读取流程：先 `Read offset=1 limit=10` 拿 §跳读 → 按行号 `Read offset=N limit=M` 精读目标章节。

**行号机器维护**：§跳读 行号由 `tools/sync_doc_toc.go` 从实际 `## N.` headers 自动生成，禁手动编辑。改 markdown 后跑 `make docs-sync` 重写；CI `docs-toc` job 跑 `make docs-check`，drift 即 fail。新增章节 / 编辑结构后流程：
1. 自由增删 `## N. Title` headers
2. `make docs-sync` 刷新所有文件头 §跳读 行号
3. 提交前 `make docs-check` 确认无 drift

人工只维护 §跳读 中的 **title 文案** 和 **(SKIP)/(SOFT) 标记**，行号 100% 由脚本接管。子节锚（如 `10.1 PerformanceDrift`，无对应 `## 10.1.` header）保持不动。

`state.yaml` 同样有 §跳读（前 14 行注释块），按 `meta/par/staging/taint/...` 偏移精读。

**节省效果**: §0 决策（多数 SKIP）+降级章节 ≈ 13% 文档体量；典型任务在 §2 基础上再省 ~4K tok。

---

## §3 概念定位（防止重复加载）

> `[Concept]` 标签定义见 `00-Global-Dictionary.md`；下方仅指向**展开实现**所在文档。

| 概念 | 定义 | 实现 |
|------|------|------|
| `[TaintLevel]` `[TaintedString]` `[SafeString]` | 00 | M11 |
| `[SurpriseIndex]` | 00 | M03（基础两组件）+ M09（完整三阶段） |
| `[TokenBurnRate]` `[KillSwitch]` | 00 | M03（CANONICAL） |
| `[Cedar-Gate]` | 00 | M11 |
| `[EventLog]` `[MutationBus]` | 00 | M02 |
| `[Blackboard]` | 00 | M08 |
| `[ContextAssembler]` `[Spotlighting]` `[ImmutableCore]` | 00 | M05 |
| `[HybridRetriever]` | 00 | M05 + M10 |
| `[ReplayMode]` `[Sub-agent-Isolation]` | 00 | M04 |
| `[Sandbox-L1/L2/L3]` | 00 | M07 |
| `[MEMF]` `[FallacyMemoryPool]` | 00 | M09 |
| `[Logic Collapse]` | 00 | M06 |
| `[ExtensionInstance]` `[ExtensionOrigin]` | 00 | M13-bis |
| `[SSRFGuard]` `[SafeDialer]` | 00 | M11 |
| `[Capability Token]` | 00 | M07 + M11 |
| `[ReasoningEffort]` `[ReasoningTokens]` `[BestOfN]` `[SelfConsistency]` `[TTC]` | 00 §9-ter | M01 §5.2-bis |
| `[ReasoningState]` | 00 | M04 §7.1 + M05 §3.1 |
| `[FactualityGuard]` `[CitationValidator]` | 00 | M11 §6.5 + M10 §4.1 |
| `[CodeAct]` | 00 | M07 §7.4 |
| `[UserInterrupt]` | 00 | M04 §1 (S_INTERRUPT) + M13 §1.2.5 |
| `[ReflectionMemory]` | 00 | M05 §3.4 |
| `[PerformanceDrift]` | 00 | M03 §10.1 |
| `[KnowledgeConflict]` | 00 | M10 §4.2 |

## §4 AI 加载纪律

1. **禁止全量加载** `docs/arch/*.md`，会爆 200K 上下文。
2. 入会先读：本文件 + `00-Global-Dictionary.md`（合计 ~26K）。
3. 按 §2 场景表选择 1~3 个 `M_X` 加载。
4. `state.yaml` 体量大，需要时按章节用 Read offset/limit 局部加载。
5. `ROADMAP.md` `DIAGRAMS.md` 是人类参考层，AI 默认不加载。
6. 跨模块概念使用 §3 表定位，不要重复加载多个文档查同一概念。
7. **代码层约束见 `docs/specs/INDEX.md`**。按场景联动加载对应 spec 文件：改 Go 加 01-Go-Code.md，改 Rust 加 02-Rust-FFI.md，改 Agent 层加 03-Agent-Pattern.md。加载优先级见 AGENTS.md `## 开发规范系统`。
