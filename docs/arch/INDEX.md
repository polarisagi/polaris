# Arch Index

> **一句话定位**：AI 编程入口。先读本文件，按场景按需加载下方文档。`docs/arch/` 全量加载 ≈ 390K token（见 §1 表），必爆 200K 上下文。
>
> **实现语言**：无特定 | **代码位置**：根目录

## §0 症状索引（故障排查入口，先于 §1/§2）

> §2/§3 解决"我要改什么，该读哪个文件"；本列表解决"我看到一个症状，该读哪个文件"。命中即跳对应文件精读，不要从症状关键词开始全仓库 grep 通读。**排查完一个新根因后必须往这里加一项**（哪怕一行）。

### 症状 1：网关只读 GET 接口在浏览器 DevTools 显示 Pending
- **症状特征**：浏览器 Pending，直接 `curl` 同样卡住/超时（非仅浏览器排队）。
- **归类模块**：M02
- **根因类别**：SQLite 连接池被长耗时批量写独占。
- **排查起点**：`internal/store/store.go`（writer/reader 连接池分离）；检查后台批量写任务（插件市场同步 `bootMarketplaceInit`/向量回填）。

### 症状 2：日志反复出现 context deadline exceeded
- **症状特征**：`context deadline exceeded`（尤其 `episodic_mem: scan high salience failed` / `MemoryAgent.scan failed`）。
- **归类模块**：M02
- **根因类别**：同症状 1，DB 连接池争用，后台周期任务抢不到连接。
- **排查起点**：`internal/memory/store/episodic_mem.go` `ScanHighSalience` + `internal/store/store.go`。

### 症状 3：某个 GET 接口在浏览器一直 Pending
- **症状特征**：仅该接口 Pending，同一时刻其它接口正常。
- **归类模块**：—
- **根因类别**：先排除浏览器同源并发连接数上限（HTTP/1.1 通常 6），不要默认怀疑接口本身。
- **排查起点**：DevTools Network → 请求 Timing 分栏，看是 `Stalled/Queueing` 还是 `Waiting (TTFB)`。

### 症状 4：重启后插件市场/插件目录页面短暂为空
- **症状特征**：重启后页面为空，一两分钟后自己恢复。
- **归类模块**：M13-bis
- **根因类别**：1) 启动期后台全量同步未跑完（约 1~2 分钟）；2) 多个 polaris 进程共用同一个 `~/.polaris/data/polaris.db`（如常驻实例 + 本地测试构建）。
- **排查起点**：`internal/gateway/server/server_init.go` `bootMarketplaceInit`；`pgrep -fl "polaris serve"` 确认是否多实例共用数据目录（`scripts/restart.sh` 直接热部署常驻服务，不另起沙箱实例）。

### 症状 5：怀疑外部阻塞式调用导致卡死
- **症状特征**：怀疑"持有 DB 连接/Tx/未关闭 Rows 期间发起阻塞式外部调用（LLM/网络）"。
- **归类模块**：—
- **根因类别**：反模式排查——先看代码是否真在事务/Rows 作用域内调用外部请求，不凭"改动时间点接近"下结论。
- **排查起点**：`docs/specs/00-Constitution.md R1.16`。

### 症状 6：安全/资源门控未生效
- **症状特征**：Cedar 策略、CC-2 三维门控等代码存在，但观察不到应有的拦截效果。
- **归类模块**：M07 / M11 / M09
- **根因类别**：构造函数把真实依赖换成零值/nil（`&Xxx{}` 空结构体，或 `_ = New(nil, nil, ...)` 丢弃返回值），门控静默跳过而非报错。
- **排查起点**：顺调用链找构造点，查零值结构体/丢弃的 `_ =` 返回值；案例 `ADR-0025` BUG-1/BUG-2。

### 症状 7：Task/请求卡在中间状态不推进
- **症状特征**：卡在某个中间状态（如 Executing）永久不推进，怀疑协程死锁或 Blackboard 卡住。
- **归类模块**：M08
- **根因类别**：裸 `go func()` 跑关键状态机主循环，panic 时清理逻辑（如 `defer close(done)`）不执行。
- **排查起点**：检查驱动 goroutine 是否用 `concurrent.SafeGo`；案例 `ADR-0025` BUG-3 / BUG-B。

### 症状 8：并发会话间 Agent 状态互相覆盖
- **症状特征**：多标签页/并发会话之间 Agent 状态覆盖、串话。
- **归类模块**：M04 / M13
- **根因类别**：Agent 实例是全服务器单例，并发请求共享同一份状态字段。
- **排查起点**：检查是否走 `AgentPool`（per-session 实例）而非单例；案例 `ADR-0025` §E。

### 症状 9：Blackboard 事件消费方读到的 ev.Payload 恒为空
- **症状特征**：`SQLiteBlackboard.Subscribe` 消费方在 `task_completed` 事件读 `ev.Payload` 得空字节，即便 `CompleteTask` 传了非空 `result`。
- **归类模块**：M08
- **根因类别**：某状态转换方法的 `broadcast` 忘了把参数写进 `BlackboardEvent.Payload`（对照兄弟方法，如 `FailTask` 有 `Payload: errBytes`）。
- **排查起点**：`internal/execute/orchestrator/sqlite_blackboard.go`（2026-07-12 前在 `internal/swarm/orchestrator/`，ADR-0046）对比 `PostTask`/`PostBatch`/`ClaimTask`/`StartExecution`/`CompleteTask`/`FailTask` 六个方法的 `bb.broadcast(...)` 是否都设置了 `Payload`；案例 ADR-0046 §2 第 6 点。

### 症状 10：AgentPool 会话 SendIntent 后 FSM 无任何响应/流式无输出
- **症状特征**：per-session 请求 `SendIntent` 不报错，但 `SubscribeStream`/`CurrentState()` 永不变化。
- **归类模块**：M04
- **根因类别**：Agent 已构造但无 goroutine 消费 intent channel——`a.intent` 带缓冲（cap=10）掩盖了 FSM 从未被驱动；构造点忘了启动 `Run()` 事件循环。
- **排查起点**：确认构造点有配套 `concurrent.SafeGo(..., func(ctx) { agent.Run(ctx) })`；案例 `ADR-0025` §E Addendum（`internal/agent/pool.go` `newPoolEntry`）。

### 症状 11：Worker 认领任务后 PeekTask 读不回任务意图内容
- **症状特征**：`PostTask` 传入的 `TaskEntry.Intent`，Worker `ClaimTask` 后 `PeekTask` 读到恒为空。
- **归类模块**：M08
- **根因类别**：`PostTask`/`PostBatch` 的 INSERT 从未写 `intent` 列，`TaskEntry.Intent` 在写路径被静默丢弃；`task_posted` 事件同样不带 payload。
- **排查起点**：`007_tasks.sql` 是否有 `intent` 列 + `sqlite_blackboard.go` INSERT 字段列表是否含 `task.Intent`；案例 `ADR-0046` §6 Addendum。

### 症状 12：`transfer_to_agent` 委派链深度上限从未生效
- **症状特征**：`PostTask` 的 `resolveMaxDepth`/`MaxSpawnDepth` 校验代码存在，但委派链再深也不触发 `CodeForbidden`。
- **归类模块**：M04 / M08
- **根因类别**：`TaskEntry.SpawnDepth` 无调用方赋值（恒 0）；传播链路（`agent_handoff.go` 构造 `TaskEntry` → `007_tasks.sql` 列 → `PeekTask` 回填 → `StateContext.SpawnDepth` → `SetSpawnDepth`）曾完全缺失。
- **排查起点**：`007_tasks.sql` 是否有 `spawn_depth` 列 + `sqlite_blackboard.go`/`sqlite_blackboard_ops.go` 是否读写 + `agent_handoff.go` 构造时是否为 `a.sCtx.SpawnDepth + 1`；案例 `ADR-0111`（原 0084）§引用代码。

### 症状 13：对话回复里夹带 JSON / DAG / TaskModel 等内部结构，或多轮对话"失忆"
- **症状特征**：回复出现 ```json 围栏的 `nodes`/`dag`/`Goal` 结构、同一段话重复两遍，日志伴随 `schema_ref=plan_dag ... invalid JSON` 与 `from=2 to=9`（S_PLAN→S_FAILED）；或第二轮起模型不记得上一轮。
- **归类模块**：M04 / M13
- **根因类别**：内部阶段 token 被当作用户回复推送（LLMFillEffect 受众未隔离）/ 无回复合成态 / 记忆路径 prompt 未加载阶段 Schema / session 未向内核注入对话历史（ADR-0098，2026-09-24 修复）。
- **排查起点**：`internal/agent/agent_stream_infer.go` `doStreamInfer`（按 `Audience` 发布）→ `internal/agent/fsm/transitions_respond.go` → `internal/agent/context/respond_context.go`；历史注入看 `internal/gateway/session/orchestrator_fsm.go` 与 `internal/agent/agent_turn.go` `SetConversationHistory`。

### 症状 14：侧边栏某几个页面（如插件/自动化/技能）显示完全空白，其余页面正常
- **症状特征**：点击「插件」或「自动化」等页面主内容区完全空白，对话、监控、设置正常。
- **归类模块**：M13
- **根因类别**：`web/src/index.html` 用 `<page-fragment>` 扁平拼装，前面某页面（如 `settings.html`）遗漏闭合 `</div>`，后续页面被吞入父级 `<div x-show="...">`，非父页面路由激活时父容器 `display: none`。
- **排查起点**：`web/src/pages/` 下做 HTML 标签平衡检查找未闭合文件；DevTools DOM 树看空白页面的父节点是否属于其他页面。

### 症状 15：CI 单测报 TempDir RemoveAll cleanup: directory not empty 或 nil pointer dereference
- **症状特征**：`TempDir RemoveAll cleanup: unlinkat ...: directory not empty`，或 `SafeGo` 捕获的空指针 panic（如 `downloader.downloadResumeP`）。
- **归类模块**：M00 / L0
- **根因类别**：测试触发异步后台协程（如 `Install()`）但提前退出，协程仍向 `t.TempDir()` 写文件，与清理并发争用；且未注入测试用 `HTTPClient` 致下载组件空指针。
- **排查起点**：检查测试是否通过 channel/轮询等待后台协程终态（如 `installing.Load() == false`）；检查 service 构造是否注入模拟离线客户端与可取消 context。

### 症状 16：Agent 用 cron_create 建的定时任务从不执行
- **症状特征**：Agent 会话中通过 `cron_create` 创建了定时任务，但到期从不触发执行。
- **归类模块**：M13 / M07
- **根因类别**：`cron_jobs`（014）无调度器消费（调度器 `cronTick` 仅扫 `automations` 表）；ADR-0114：cron_jobs 无调度器消费，已改为 automation_* 写 automations/workflows。
- **排查起点**：`docs/arch/decisions/ADR-0114-info-acquisition-automation-pipeline.md`、`docs/arch/M13-Interface-Scheduler.md §8.7`。

**维护规范**：
- 每项只留高命中率的路由信息，排查过程留给排查起点指向的文件/章节。
- 相似症状合并成一项（用"/"列举变体）。
- 超过 ~30 项时按"归类模块"用 `###` 分组，不拆新文件——保持单一入口。
- 根因已彻底消除、代码路径被重构的项应删除，不留死索引。

## §1 文档清单（按 token 量降序）

<!-- BEGIN GENERATED: arch-index-table · 源: docs/arch/*.md · 勿手改，改源后跑 make docs-gen -->
| 文件 | 域 | est_tok | 内容摘要 |
|------|----|---------|----------|
| `M13-Interface-Scheduler.md` | L3 接口 | 31K | 见下方 [M13 补充](#m13-补充) |
| `spec/state.yaml` | SSoT（Single Source of Truth，唯一权威源） 规约 | 27K | 状态机 + 全模块阈值（唯一权威） |
| `M04-Agent-Kernel.md` | L1 内核 | 24K | 状态机 14 态、S_VALIDATE 四层、System 1/1.5/2 路由、Saga |
| `00-Global-Dictionary.md` | 字典 | 23K | 全 `[Concept]` 标签定义、XR-01~07 跨模块规则、公理 |
| `M11-Policy-Safety.md` | L0 策略 | 23K | 五防线、Cedar、TaintedString、KillSwitch、PII（Personally Identifiable Information，个人可识别信息） Vault、SSRFGuard |
| `M07-Tool-Action-Layer.md` | L1 工具 | 23K | 见下方 [M07 补充](#m07-补充) |
| `M05-Memory-System.md` | L1 记忆 | 22K | 四层记忆、PromptBuilder、HybridRetriever、Consolidation |
| `M13-bis-Extension-Registry.md` | L3 扩展 | 15K | 见下方 [M13-bis 补充](#m13-bis-补充) |
| `M08-Multi-Agent-Orchestrator.md` | L2 协同 | 14K | Blackboard、CAS（Compare-And-Swap，比较并交换） 认领、Reaper、Supervisor Tree、7 编排模式 |
| `M01-Inference-Runtime.md` | L0 推理 | 14K | Provider Router、Model Pool、CircuitBreaker、SemanticCache |
| `M10-Knowledge-RAG.md` | L2 知识 | 14K | 文档树、6 阶段摄入、GraphRAG、IncrementalIndexer |
| `M09-Self-Improvement-Engine.md` | L2 自演化 | 13K | 五条无梯度路线、SurpriseIndex 完整版、MEMF（Memory of Errors and Mistakes Framework，错误记忆框架）、Auto-Curriculum |
| `M02-Storage-Fabric.md` | L0 存储 | 13K | 三轴存储、EventLog、MutationBus、Outbox、SchemaManager |
| `M06-Skill-Library.md` | L1 技能 | 12K | 技能三件套、Logic Collapse（Python+ContainerSandbox）、三级检索 |
| `M03-Observability.md` | L0 可观测 | 11K | OTel（OpenTelemetry）、TokenBurnRate（CANONICAL）、SurpriseIndex 基础、AutoConfig |
| `M12-Eval-Harness.md` | L3 评测 | 10K | EvalCase、五层 Evaluator、TrajectoryReplayer、CI 门控 |
| `ARCHITECTURE.md` | 总览 | 8K | 见下方 [ARCHITECTURE 补充](#architecture-补充) |
| `ROADMAP.md` | 路线 | 7K | 时间敏感项 / 工程现状 / 未完成研究方向 / 工程纪律 / 拒绝清单（**人类参考**，AI 默认不加载） |
| `DIAGRAMS.md` | 图谱 | 6K | 时序图（**人类参考**，AI 默认不加载） |
| `Module-Dependency-Axioms.md` | 依赖公理 | 3K | 包间依赖方向、防循环依赖底线、领域模型正交性 |
<!-- END GENERATED: arch-index-table -->

#### M07 补充
MCP/A2A、Rust 沙箱三级分级、Capability Token、Workspace Bridge。

#### ARCHITECTURE 补充
SSoT 锚点：定位/硬约束、Staging 7 阶段、HT0 预算、变更控制、配置层。

#### M13 补充
HTTP/SSE、HITLGateway、ResourceGovernor、TaskQueue、Web UI 规约（Alpine.js+Tailwind）。

#### M13-bis 补充
三层模型（Market/Instances/Runtime）、安装流、信任门控、文件系统、调用路由、M9 归并。

## §2 场景加载预算

| 任务类型 | 必读组合 | 总 tok |
|----------|----------|--------|
| 修改存储 / EventLog / DDL | `00` + `M02` + `state.yaml`(§outbox) | ~80K |
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
| 修改包结构 / 引入新依赖 | `00` + `ARCHITECTURE` + `Module-Dependency-Axioms` | ~50K |

## §2.5 章节级跳读

每个 `M_X.md` 文件头第 4~6 行有 **`§跳读`** 单行索引，格式 `id:line title`，列出每个章节起始行号 + SKIP/SOFT 标记：
- **SKIP** = rationale（选型/拒因/风险/快照），AI 编程不读
- **SOFT** = 故障矩阵/降级，修改时按需
- 其余 = 默认读

读取流程：先 `Read offset=1 limit=10` 拿 §跳读 → 按行号 `Read offset=N limit=M` 精读目标章节。

**行号机器维护**：由 `tools/sync_doc_toc.go` 从实际 `## N` headers 生成，禁手动编辑。增删章节后 `make docs-sync` 刷新，提交前 `make docs-check` 确认无 drift（CI `docs-toc` job 同跑）。人工只维护 title 文案和 (SKIP)/(SOFT) 标记。

**header id 可用形态**（`tools/sync_doc_toc.go` `docIDPattern` 为 SSoT）：`N` / `N.M`（可多级）/ `N-bis`、`N-ter`、`N-quater`、`N-quinquies`…（`-[a-z]+` 后缀）/ `§N-xxx`（带 § 前缀的补录节）；id 后的 `.` 可有可无。不对应真实 `## ` header 的子节锚保持不动。

`state.yaml` 同样有 §跳读（前 14 行注释块），按 `meta/par/staging/taint/...` 偏移精读。

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
4. `state.yaml` 体量大，按章节用 Read offset/limit 局部加载。
5. `ROADMAP.md` `DIAGRAMS.md` 是人类参考层，AI 默认不加载。
6. 跨模块概念用 §3 表定位，不要重复加载多个文档查同一概念。
7. **代码层约束见 `docs/specs/INDEX.md`**：改 Go 加 01-Go-Code.md，改 Rust 加 02-Rust-FFI.md，改 Agent 层加 03-Agent-Pattern.md。加载优先级见根 `CLAUDE.md` `## 文档加载协议`。
