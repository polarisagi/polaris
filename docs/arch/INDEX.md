# Arch Index

> **一句话定位**：AI 编程入口。先读本文件，按场景按需加载下方文档。全量加载 ≈ 500K token，必爆 200K 上下文。
>
> **实现语言**：无特定 | **代码位置**：根目录

## §0 症状索引（故障排查入口，先于 §1/§2）

> §2/§3 解决"我要改什么，该读哪个文件"；本列表解决"我看到一个症状，该读哪个文件"——两个不同的检索方向，不能互相替代。
> 命中即跳对应文件精读，不要从症状关键词开始全仓库 grep 通读。**排查完一个新根因后必须往这里加一项**（哪怕只有一行），否则下次同类问题又要从头查一遍。

### 症状 1：网关只读 GET 接口在浏览器 DevTools 显示 Pending
- **症状特征**：网关只读 GET 接口在浏览器 DevTools 显示 Pending，但直接 `curl` 打服务端同样卡住/超时（非仅浏览器排队）。
- **归类模块**：M02
- **根因类别**：SQLite 连接池被长耗时批量写独占。
- **排查起点**：`internal/store/store.go`（writer/reader 连接池分离）；检查是否有后台批量写任务在跑（插件市场同步 `bootMarketplaceInit`/向量回填）。

### 症状 2：日志反复出现 context deadline exceeded
- **症状特征**：日志反复出现 `context deadline exceeded`（尤其 `episodic_mem: scan high salience failed` / `MemoryAgent.scan failed`）。
- **归类模块**：M02
- **根因类别**：同上，DB 连接池争用，后台周期任务抢不到连接。
- **排查起点**：`internal/memory/store/episodic_mem.go` `ScanHighSalience` + `internal/store/store.go`。

### 症状 3：某个 GET 接口在浏览器一直 Pending
- **症状特征**：某个 GET 接口在浏览器一直 Pending，但同一时刻其它接口正常返回。
- **归类模块**：—
- **根因类别**：先排除浏览器同源并发连接数上限（HTTP/1.1 通常 6），不要默认怀疑该接口本身。
- **排查起点**：DevTools Network → 点开该请求看 Timing 分栏，是 `Stalled/Queueing` 还是 `Waiting (TTFB)`。

### 症状 4：重启后插件市场/插件目录页面短暂为空
- **症状特征**：重启后插件市场/插件目录页面短暂为空，过一两分钟自己又有了。
- **归类模块**：M13-bis
- **根因类别**：1) 启动期后台全量同步尚未跑完（约 1~2 分钟）；2) 多个 polaris 进程共用同一个 `~/.polarisagi/polaris/data/polaris.db`（如 launchd 常驻实例 + 本地测试构建同时跑）。
- **排查起点**：`internal/gateway/server/server_init.go` `bootMarketplaceInit`；`scripts/restart.sh` 的 `POLARIS_DATA_DIR` 隔离机制。

### 症状 5：怀疑外部阻塞式调用导致卡死
- **症状特征**：怀疑"持有 DB 连接/Tx/未关闭 Rows 期间发起阻塞式外部调用（LLM/网络）"导致卡死。
- **归类模块**：—
- **根因类别**：反模式排查（先看具体代码是否真的在事务/Rows 作用域内调用外部请求，不要仅凭"改动时间点接近"下结论）。
- **排查起点**：`docs/specs/00-Constitution.md R1.16`。

### 症状 6：安全/资源门控未生效
- **症状特征**：某个安全/资源门控（Cedar 策略、CC-2 三维门控等）代码看着存在，但实际观察不到应有的拦截效果。
- **归类模块**：M07 / M11 / M09
- **根因类别**：构造函数把真实依赖换成了零值/nil（`&Xxx{}` 空结构体，或 `_ = New(nil, nil, ...)` 丢弃返回值），门控静默跳过而非报错。
- **排查起点**：顺着调用链找构造点，检查是否有零值结构体/丢弃的 `_ =` 返回值；历史真实案例见 `ADR-0025（Architecture Decision Record，架构决策记录）` BUG-1/BUG-2。

### 症状 7：Task/请求卡在中间状态不推进
- **症状特征**：Task/请求卡在某个中间状态（如 Executing）永久不再推进，怀疑协程死锁或 Blackboard 卡住。
- **归类模块**：M08
- **根因类别**：裸 `go func()` 跑关键状态机主循环，panic 时清理逻辑（如 `defer close(done)`）不会执行。
- **排查起点**：检查该状态机驱动 goroutine 是否用 `concurrent.SafeGo` 包装；历史真实案例见 `ADR-0025` BUG-3 / BUG-B。

### 症状 8：并发会话间 Agent 状态互相覆盖
- **症状特征**：多个浏览器标签页/并发会话之间出现 Agent 状态互相覆盖、串话。
- **归类模块**：M04 / M13
- **根因类别**：Agent 实例是全服务器单例，并发请求共享同一份状态字段。
- **排查起点**：检查是否走 `AgentPool`（per-session 实例）而非单例 `ChatHandler.Agent`；历史真实案例见 `ADR-0029` §E。

### 症状 9：Blackboard 事件消费方读到的 ev.Payload 恒为空
- **症状特征**：订阅 `SQLiteBlackboard.Subscribe` 的消费方（如编排执行器）在 `task_completed` 事件里读 `ev.Payload` 得到空字节，即便 `CompleteTask` 明明传了非空 `result` 参数。
- **归类模块**：M08
- **根因类别**：某个状态转换方法的 `broadcast` 调用忘了把参数写进 `BlackboardEvent.Payload` 字段（对照同类兄弟方法，如 `FailTask` 有 `Payload: errBytes`，遗漏方法却没有）。
- **排查起点**：`internal/execute/orchestrator/sqlite_blackboard.go`（2026-07-12 前路径为 `internal/swarm/orchestrator/`，见 ADR-0046）里对比 `CompleteTask`/`FailTask`/`SuspendTask` 等同类状态转换方法的 `bb.broadcast(...)` 字面量，看是否每个都对齐设置了 `Payload`；真实案例见 ADR-0041 §2 第6点（`CompleteTask` 此前遗漏）。

### 症状 10：AgentPool 会话 SendIntent 后 FSM 无任何响应/流式无输出
- **症状特征**：走 `AgentPool.Acquire` 的 per-session 请求，`SendIntent` 本身不报错，但 `SubscribeStream`/`CurrentState()` 永远不变化，像是石沉大海。
- **归类模块**：M04
- **根因类别**：Agent 实例已构造但没有任何 goroutine 消费其 intent channel——`a.intent` 带缓冲（cap=10），短期内写入不报错，掩盖了 FSM 从未被驱动的事实；构造点忘记为该实例启动 `Run()` 事件循环。
- **排查起点**：确认该 Agent 实例的构造点是否有配套的 `concurrent.SafeGo(..., func(ctx) { agent.Run(ctx) })`；历史真实案例见 `ADR-0029` §E Addendum（`internal/agent/pool.go` `newPoolEntry` 此前遗漏）。

### 症状 11：Worker 认领任务后 PeekTask 读不回任务意图内容
- **症状特征**：`PostTask` 时通过 `TaskEntry.Intent` 传入的意图数据（如编排执行器编码的节点 ID/模板），Worker `ClaimTask` 之后调用 `PeekTask` 却读不到——`Intent` 字段恒为空。
- **归类模块**：M08
- **根因类别**：`SQLiteBlackboard.PostTask`/`PostBatch` 的 INSERT 语句从未写入 `intent` 列（`tasks` 表此前也没有这一列），`TaskEntry.Intent` 在写路径上被静默丢弃；`task_posted` 事件同样不携带 payload。此前未暴露是因为没有真实生产 Worker 消费过 `Pattern*Executor` 投递的任务。
- **排查起点**：确认 `007_tasks.sql` 是否有 `intent` 列 + `sqlite_blackboard.go` PostTask/PostBatch 的 INSERT 字段列表是否包含 `task.Intent`；真实案例见 `ADR-0041` §6 Addendum（workflow 接入 StateGraphExecutor 时发现，2026-07-12）。

**维护规范**（避免列表随项目变大而失控）：
- 每项只留"高命中率的路由信息"，具体排查过程留给排查起点指向的文件/章节，不要在列表里展开叙述。
- 相似症状优先合并成一项（用"/"列举变体），不要为每个变体开新项。
- 超过 ~30 项时，按"归类模块"拆分成多个小列表（同一 `## §0` 标题下用 `###` 分组），而不是拆成新文件——保持"先读本文件"的单一入口。
- 对应代码路径被重构、根因已彻底消除的项应删除，不要无限期保留成"死索引"。

## §1 文档清单（按 token 量降序）

| 文件 | 域 | est_tok | 内容摘要 |
|------|----|---------|----------|
| `spec/state.yaml` | SSoT（Single Source of Truth，唯一权威源） 规约 | 52K | 状态机 + 全模块阈值（唯一权威） |
| `M11-Policy-Safety.md` | L0 策略 | 41K | 五防线、Cedar、TaintedString、KillSwitch、PII（Personally Identifiable Information，个人可识别信息） Vault、SSRFGuard |
| `M07-Tool-Action-Layer.md` | L1 工具 | 38K | 见下方 [M07 补充](#m07-补充) |
| `M02-Storage-Fabric.md` | L0 存储 | 29K | 三轴存储、EventLog、MutationBus、Outbox、SchemaManager |
| `M05-Memory-System.md` | L1 记忆 | 31K | 四层记忆、PromptBuilder、HybridRetriever、Consolidation |
| `ARCHITECTURE.md` | 总览 | 10K | 见下方 [ARCHITECTURE 补充](#architecture-补充) |
| `M04-Agent-Kernel.md` | L1 内核 | 26K | 状态机 12 态、S_VALIDATE 四层、System 1/1.5/2 路由、Saga |
| `M13-Interface-Scheduler.md` | L3 接口 | 28K | 见下方 [M13 补充](#m13-补充) |
| `M13-bis-Extension-Registry.md` | L3 扩展 | 5K | 见下方 [M13-bis 补充](#m13-bis-补充) |
| `M10-Knowledge-RAG.md` | L2 知识 | 25K | 文档树、6 阶段摄入、GraphRAG、IncrementalIndexer |
| `M09-Self-Improvement-Engine.md` | L2 自演化 | 25K | 五条无梯度路线、SurpriseIndex 完整版、MEMF（Memory of Errors and Mistakes Framework，错误记忆框架）、Auto-Curriculum |
| `M06-Skill-Library.md` | L1 技能 | 23K | 技能三件套、Logic Collapse（Python+ContainerSandbox）、三级检索 |
| `M03-Observability.md` | L0 可观测 | 22K | OTel（OpenTelemetry）、TokenBurnRate（CANONICAL）、SurpriseIndex 基础、AutoConfig |
| `00-Global-Dictionary.md` | 字典 | 23K | 全 `[Concept]` 标签定义、XR-01~07 跨模块规则、公理 |
| `M01-Inference-Runtime.md` | L0 推理 | 19K | Provider Router、Model Pool、CircuitBreaker、SemanticCache |
| `M08-Multi-Agent-Orchestrator.md` | L2 协同 | 17K | Blackboard、CAS（Compare-And-Swap，比较并交换） 认领、Reaper、Supervisor Tree、7 编排模式 |
| `M12-Eval-Harness.md` | L3 评测 | 17K | EvalCase、五层 Evaluator、TrajectoryReplayer、CI 门控 |
| `ROADMAP.md` | 路线 | 7K | 时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单（**人类参考**，AI 默认不加载） |
| `DIAGRAMS.md` | 图谱 | 14K | 时序图（**人类参考**，AI 默认不加载） |

#### M07 补充
MCP（Model Context Protocol，模型上下文协议）/A2A（Agent-to-Agent，智能体间通信）、Rust 沙箱三级分级、Capability Token、Workspace Bridge。

#### ARCHITECTURE 补充
SSoT 锚点：定位/硬约束、Staging 7 阶段、HT0 预算、变更控制、配置层。

#### M13 补充
HTTP/SSE、HITLGateway、ResourceGovernor、TaskQueue、Web UI 规约（Alpine.js+Tailwind）。

#### M13-bis 补充
三层模型（Market/Instances/Runtime）、安装流、信任门控、文件系统、调用路由、M9 归并。

## §2 场景加载预算

| 任务类型 | 必读组合 | 总 tok |
|----------|----------|--------|
| 修改存储 / EventLog / DDL（Data Definition Language，数据定义语言） | `00` + `M02` + `state.yaml`(§outbox) | ~80K |
| 修改 Agent 状态机 / Saga | `00` + `M04` + `state.yaml`(§par) | ~80K |
| 修改记忆 / 上下文组装 | `00` + `M05` | ~56K |
| 修改 RAG / 知识图谱 | `00` + `M10` (+ `M05` 如涉混合检索) | ~50~83K |
| 修改工具 / 沙箱 / MCP | `00` + `M07` (+ `M11` 如涉策略边界) | ~63~107K |
| 修改策略 / 安全 / Taint | `00` + `M11` + `state.yaml`(§taint) | ~120K |
| 修改可观测 / 指标 | `00` + `M03` + `state.yaml`(§signals) | ~75K |
| 修改 Provider 路由 / Model Pool | `00` + `M01` | ~44K |
| 修改技能 / Logic Collapse | `00` + `M06` (+ `M09` 如涉蒸馏) | ~48~75K |
| 修改 Orchestrator / Blackboard | `00` + `M08` (+ `M04` 如涉认领协议) | ~42~71K |
| 修改自演化 / 评估循环 | `00` + `M09` (+ `M12` 如涉 CI) | ~50~69K |
| 修改 HTTP API / HITL（Human-In-The-Loop，人在回路） | `00` + `M13` (+ `M11` 如涉 Auth) | ~46~90K |
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

<!-- §跳读: 此处原本含有每个章节起始行号和标记的信息，供机器读取。改写为 HTML 注释以符合规范。 -->

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
| `[Spotlighting]` `[ImmutableCore]` | 00 | M05 |
| `[HybridRetriever]` | 00 | M05 + M10 |
| `[ReplayMode]` `[Sub-agent-Isolation]` | 00 | M04 |
| `[Sandbox-L1/L2/L3]` | 00 | M07 |
| `[MEMF]` `[FallacyMemoryPool]` | 00 | M09 |
| `[Logic Collapse]` | 00 | M06 |
| `[ExtensionInstance]` `[ExtensionOrigin]` | 00 | M13-bis |
| `[SSRFGuard]` `[SafeDialer]` | 00 | M11 |
| `[Capability Token]` | 00 | M07 + M11 |
| `[ReasoningEffort]` `[ReasoningTokens]` `[BestOfN]` `[SelfConsistency]` `[TTC（Test-Time Compute，推理时计算）]` | 00 §9-ter | M01 §5.2-bis |
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
7. **代码层约束见 `docs/specs/INDEX.md`**。按场景联动加载对应 spec 文件：改 Go 加 01-Go-Code.md，改 Rust 加 02-Rust-FFI.md，改 Agent 层加 03-Agent-Pattern.md。加载优先级见根 `CLAUDE.md` `## 文档加载协议`。
