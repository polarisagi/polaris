# docs/specs/ 变更日志

> 规范本身的演进记录。AI 每次会话开头扫描最近 5 条以感知规范增量。

格式：`YYYY-MM-DD | 文件 | 变更摘要`

## 2026-07-12（同步 8 项架构决策复核实现到架构文档）

对 commit `226654d`（Cedar Permit 三档语义修正）/ `618b0b5`（CodeAct VFS 化 + 配额预占）/ `fecc215`（会话持久化可靠性 + AgentPool Run() 补齐）/ `4d72c93`（Jaccard 分支级联失效补齐）/ `3185c43`（EdgeCondition 声明式算子扩展）做架构文档核实与总结性更新，均为对已落地代码的文字补充，不含伪代码：

- `docs/arch/M04-Agent-Kernel.md` | AgentPool 段落补充：`Pool.Acquire` 为每个新建 Agent 启动常驻 `Run()` 循环、`GC()` 回收时调用 `Shutdown()`（此前二者均缺失）
- `docs/arch/M05-Memory-System.md` §4.2/Stage 2 | Jaccard 近似碰撞分支现与精确名称冲突分支共用同一级联失效触发路径（此前 Jaccard 分支不触发 `CascadeInvalidator`）
- `docs/arch/M07-Tool-Action-Layer.md` | CodeAct 执行路径对比表更新：临时脚本改经 `ScriptStagingBackend`（VFS 隔离工作区），未注入时降级为原系统临时目录
- `docs/arch/M08-Multi-Agent-Orchestrator.md` §3-quinquies | `EdgeCondition` 算子集合从 `eq`/`ne` 扩展为含 `gt`/`lt`/`ge`/`le`/`contains`/`exists` + 结构化 `And`/`Or` 复合，仍为声明式扩展、未引入表达式引擎
- `docs/arch/M13-Interface-Scheduler.md` | 移除对已废弃 `ChatHandler.ToolStage` 环节的引用（该字段随孤儿注入点级联清理）
- `docs/arch/INDEX.md` §0 | 新增症状10：AgentPool `SendIntent` 无响应 → 排查 `Run()` 是否启动
- `docs/arch/decisions/ADR-0029-phase1-2-system-hardening.md` §E / `ADR-0041-state-graph-orchestration.md` | 各补充 Addendum 记录本轮复核发现的真实缺口与修复方案

**附带核实**：全部改动均已通过 `gofmt -l .`、`go build ./...`、`golangci-lint run ./...`（含 wasip1 子集）、`go test ./...`（100 包 ok，0 FAIL）验证，详见对应 commit message。

**遗留问题（未在本轮处理）**：项目中并存至少两套独立编号的复核发现 ID 序列，均使用 `GD-13-*`/`GD-14-*` 前缀但指向完全不同的 finding（例如 `GD-14-001`/`GD-14-002` 在 `ADR-0033`/`M05 §2.4`/`00-Global-Dictionary.md` 等既有文档中分别指"多 Agent 共享记忆命名空间"/"上下文分页"，而本轮新增的 `internal/memory/retrieval`、`internal/swarm/orchestrator` 相关注释与提交沿用同名 ID 指向"级联失效 Jaccard 分支"/"EdgeCondition 声明式扩展"）。两套编号均已随各自代码落地，本次未重新编号，避免在未确认权威来源前引入更大范围的误改；建议后续复核时对两份复核报告的 ID 命名空间做一次性协调。

## 2026-07-12（同步任务书08 §8.1/§8.3/§8.4/§8.5 四项实现到架构文档）

对 commit `d65862b`（GD-13-001 通知投递）/ `ec76e1b`（GD-13-003 内存持久化熔断）/ `86b787a`（GD-14-001 多 Agent 共享记忆命名空间）/ `3d5d036`（GD-14-002 上下文分页）做架构文档核实与总结性更新，均为对已落地代码的文字补充，不含伪代码：

- `docs/arch/M04-Agent-Kernel.md` §2 | 插入 GD-13-003 段落：说明"写失败"实为同步 error 而非静默丢失，真正新增的只是 `isMemoryPersistenceFailure` 判定 + `TriggerInterruptReceived` 熔断路由，未新增状态机转换规则
- `docs/arch/M13-Interface-Scheduler.md` §2.1 | 插入 GD-13-001 段落：终态通知复用 `OutboxWorker` 消费框架 + 既有指数退避重试，`Pool=="intent_handler"` 交互式任务排除在外，本轮仅实现 Webhook 一种渠道
- `docs/arch/M05-Memory-System.md` §2（新增 §2.4）+ §3.1 | GD-14-002 分页置换设计（`memory_page_out`/`memory_page_in` 先归档后删除的顺序、`contextPressureHint` 仅暴露信号不强制触发）；GD-14-001 NamespaceID 分区机制（复用既有 `ev.TaskID != q.SessionID` 分区键 + `types.TaskEntry.Namespace` 字段，仅 4 类协同写入参与替换，2PC/FastPath 显式排除）
- `docs/arch/M08-Multi-Agent-Orchestrator.md` §11.1 | 补充 GD-14-001 段落：明确 Namespace 不是新总线，只是既有分区键上的可选联合，`PeekTask`→`SetMemoryNamespace`→`memoryPartitionKey` 的传播链路
- `docs/arch/00-Global-Dictionary.md` | `[Blackboard]` 词条补充 `TaskEntry.Namespace`；`[CoreMemory]` 词条补充分页置换工具

**附带核实**：四项改动均已通过 `go build ./... && go vet ./... && gofmt -l . && go test ./internal/... ./pkg/... ./cmd/...` 与 `make lint`（0 issues），详见对应 commit message。

## 2026-07-11（docs/specs 全量审查：清理两份误入的设计草案）

**问题**：`08-HITL-AskUser.md`/`09-Generative-UI.md` 与既有 `08-Doc-Hygiene.md`/`09-LLM-Agent-Production.md` 编号冲突；`INDEX.md` 加载策略表从未登记两文件（实际未被 AI 主动加载）；`CHANGELOG.md` 无引入记录；正文是 what/why 设计提案文体（散文式"动机与目标"引入 + 完整 Go struct/JSON/HTML 伪代码），与 specs 目录 how/constraints 生成约束文体不符（见 `INDEX.md` arch↔specs 定位表）；全仓库 grep 确认 `AskHuman`/`ClarificationRequest`/`ui_component`/`render_chart` 等零实现、零 ADR。

**处置（按用户选定方案：迁移至 docs/arch/ 并建 ADR）**：

- `docs/specs/08-HITL-AskUser.md`（删除）| 内容基于现有代码重新核校后迁至 `decisions/ADR-0042-hitl-askhuman-consultation.md`：原草案假设的 `ClarificationRequest`/`ErrSuspendForInput` 独立类型体系与实际代码（`types.HITLPrompt`/`types.HITLResponse`、`FSM.SuspendReason` 已有 `capability_gap`/`provider_exhausted` 先例）不符，已修正为复用现有机制（新增 `CheckpointType=clarification_request` + `SuspendReason=awaiting_user_input` + `HITLResponse.Payload` 字段，走 B5.2 破坏性变更流程）
- `docs/specs/09-Generative-UI.md`（删除）| 内容基于现有代码重新核校后迁至 `decisions/ADR-0043-generative-ui-sse.md`：确认前端 Alpine.js + marked 假设成立，但补充核出原方案假设的 DOMPurify 依赖当前不存在（`web/package.json` 未列），列为实现前置事项；渲染工具落点明确为 `protocol.ToolRegistry.ExecuteTool`（`SandboxTier=InProcess`，对齐 `memory_tools.go` 先例）
- `docs/arch/M04-Agent-Kernel.md §2` | 插入一句话 why-do + ADR-0042 锚点（H5 决策迁移规则）
- `docs/arch/M13-Interface-Scheduler.md §2.4` `§8.3`（新增 §8.3.4）| 插入一句话 why-do + 对应 ADR 锚点
- 两份 ADR 状态均标记 `Proposed（未实现，设计草案）`，不代表已落地

**附带发现（未处理，超出本次范围）**：`M04-Agent-Kernel.md`/`M13-Interface-Scheduler.md` 头部 `§跳读` 行号索引与实际 `##` 标题行号已存在大幅漂移（远超本次编辑新增的 3~5 行），且 M04 出现两个重号 `## 13.` 标题（`降级与失败模式` / `跨模块契约`）；`make docs-sync`/`docs-check` 依赖本地 Go 工具链，本次环境不可用，需用户本地跑一次全量同步。

## 2026-07-11（Gemini 升级批次复核修复 + Backlog-2/3 开发）

**Gemini P0~P2 升级提交复核，修复 5 处遗漏（均属"已声明但未接线/未覆盖"类）**：

- `configs/defaults.toml` | 补 `[policy] cedar_enforce_mode`、`[storage] tier0_vector_scan_limit`、整个 `[security]` 段——Gemini/此前批次新增了配置结构体字段，但未同步 TOML 模板，`config_defaults_test.go` 的覆盖检查此前只按结构体名手工枚举、未递归全字段，一并改为反射遍历 `Config{}` 全部顶层字段
- `internal/agent/fsm/state_machine_test.go` | 补齐 P0-5/P0-6（中断入队 stash-queue、扩展激活非阻塞）修复的回归测试，避免相关代码日后被无测试覆盖地改坏
- `internal/eval/analysis/shadow_executor.go` | 补全 `scoreShadow` 的 schema 校验（passed/reason 两字段 fail-closed），Gemini 提交只做了一半
- `internal/extension/mcp/tool_scanner.go` `internal/extension/skill/generate.go` | GR-4-001（正则包级变量改 `sync.OnceValue` 懒加载）Gemini 改到了不相关文件，回填至实际目标文件，并按 `new-from-rev` 基线机制加 `nolint:gochecknoglobals` 豁免（对齐 `entity.go`/`getPIIRegex` 既有先例）

**Backlog-2 Remote Sandbox（Sbx-L4）接线**：

- `internal/config/config_types.go` | 新增 `RemoteSandboxConfig`（`[sandbox.remote]`，默认关闭）
- `cmd/polaris/boot_tools.go` | 条件装配 `sandbox.NewRemoteSandbox` + `sandboxRouter.WithRemote`——此前 `RemoteSandbox`/路由 fallback 逻辑已实现但从未在启动时注入，`WithRemote` 从未被调用
- `docs/arch/00-Global-Dictionary.md` §0 | `Sbx-L` 前缀补 L4 定义
- `docs/arch/M07-Tool-Action-Layer.md` | 新增 §4.8，修正 `remote_sandbox.go` 内引用的失效锚点（原写§4.4「待补」，该号段实际已是 WASI 权限矩阵）

**Backlog-3 StateGraph 编排（GD-8-001）**：

- `internal/protocol/dag_node.go` | `WorkflowEdgeSpec` 新增 `Condition`（声明式字段比较，非脚本引擎）、`WorkflowNodeSpec` 新增 `MaxVisits`/`IsEntry`，均 `omitempty` 向后兼容，`PatternDAGExecutor` 忽略新字段
- `internal/swarm/orchestrator/pattern_state_graph.go`（新建）| 新增编排模式10 `StateGraphExecutor`：在 `PatternDAGExecutor` 之上泛化支持条件路由+有界循环，仍复用 Blackboard 作持久化任务队列/事件总线，不替换其 CAS/Lease/Reaper 机制
- `pkg/graph/state_graph.go`（新建）| `ValidateStateGraphTopology`：允许环，但要求引用完整性/至少一个合法入口/全局访问预算硬上限（终止性由运行时硬计数器保证，非拓扑分析猜测）
- `internal/swarm/orchestrator/sqlite_blackboard.go` | 顺带修复 `CompleteTask` 广播事件遗漏 `Payload` 字段的既有 bug（与同级 `FailTask` 的 `Payload:errBytes` 模式不一致，此前导致下游条件边求值/上游产出传递恒为空）
- `docs/arch/M08-Multi-Agent-Orchestrator.md` | 编排模式表新增第10行，新增 §3-quinquies
- `docs/arch/decisions/ADR-0041-state-graph-orchestration.md`（新建）| 决策记录，含"为何不做完全替换 Blackboard"的评估结论
- `docs/arch/decisions/ADR-0040-cyclic-graph-executor.md` | 标记 Superseded by ADR-0041（此前未落地的同类设计草案，落点/机制与实际实现不一致）
- `docs/arch/INDEX.md` §0 | 新增症状9（Blackboard 事件 Payload 遗漏类根因）

## 2026-07-09（规范全量审查：P0 笔误修正 + P1 信息对齐 + P2 轻量清理）

**规范审查触发，修复 8 处历史遗留问题**：

- `03-Agent-Pattern.md AGENT-7` | **P0 笔误修正**：「唱尔展条件」→「唯一前置条件」（输入法误触字，影响 AI 对 CodeAct 准入规则的解析）
- `00-Constitution.md R1.13` | **P2 边界澄清**：原描述将 Hook `RunScript` 与 Skill `ContainerSandbox L3` 合并为单一豁免描述，实际是两条完全不同的执行路径，AI 生成时容易混淆；拆分为「① Plugin install hook → `internal/action/hook/RunScript`；② Skill 执行 → `internal/extension/skill/ContainerSandbox L3`」
- `03-Agent-Pattern.md AGENT-4` | **P2 冗余清理**：删除第 54 行重复的 Skill 三件套描述，保留第 60 行含完整说明的段落（H2 修饰物清理）
- `04-Module-Boundary.md B1` | **P1 归层补全**：`internal/bootstrap/` 在 B1 依赖图中从未标注层归属，补充说明「跨层初始化编排器，仅被 cmd/ 引用，不属于 L0~L3 业务层」
- `07-Reference-Implementation.md §7.1` | **P1 canonical 补充**：新增 `internal/gateway/authcontext/` 条目（2026-07-08 合并 AuthContext 裂脑后确立的 canonical，对应 contextref.go — ContextRefExpander）
- `09-LLM-Agent-Production.md` | **P3 时间戳**：末行「最后更新」从 2026-07-06 修正至 2026-07-09

## 2026-07-09（UP-05/06/07 升级任务规范变更回写）

**UP-05 ShadowExecutor 接线 Gate 1（commit 1aa2451）**：

- `internal/eval/analysis/shadow_executor.go`（新建）| 基于 `events` 表异步回放的轻量影子执行器：采样（默认 1%，进 `state.yaml`）→ 零副作用回放（工具调用只命中 `032_mock_response_cache`，缓存未命中跳过）→ `meta_eval` 对比评分 → 写 `SQLiteEvalStore` → 发布 `ShadowGateResult` 门控信号
- `docs/arch/M12-Eval-Harness.md §4/§8` | 删除「ShadowExecutor 当前不存在」的复核记载，改为实现说明（Gate 1 Shadow 阶段已接线）
- **规范对齐**：实现遵循 HE-4（Eval 驱动）+ HE-6/R1.16（先读出批次释放连接再推理）+ A-06（mock cache 上限防无界增长）

**UP-06 网关 MVP 直通废除，FSM 收回对话控制权（commits c7185be + f7fd45c，ADR-0039）**：

- `internal/gateway/server/chat/sse.go` | 删除 `runToolInferenceLoop`（网关自带推理+工具循环）和「MVP 直通」注释；Agent 对话路径完整走 FSM；OpenAI 兼容代理路径（`/v1/...` 纯 Provider 代理）保留直通——协议转换器职责不属于认知层
- `internal/agent/` | FSM 思考循环新增流式事件通道（protocol 层 `AgentStreamEvent`，含 taint 标注）；事件发布在锁外（HE-5 FSM 锁内禁 IO）
- `internal/gateway/server/chat/` | 新增基于事件订阅的 SSE 推送路径（feature flag 控制）；输入侧经 `AgentController` 专属通道喂 FSM
- **规范对齐**：HE-5（FSM 持控制流）物理落地；真实对话路径产生完整 FSM 转移 EventLog；SurpriseIndex/ThinkingMode 三档路由（ADR-0022）在真实用户路径上激活
- `docs/arch/M_X` gateway 与 agent 章节已同步更新

**UP-07 领先设计防退化标注（commit 3418b9e）**：

- `docs/specs/07-Reference-Implementation.md §7.1` | 两项领先设计标注为 canonical 并注明「删除或绕过需 ADR 级决策」：① `internal/tool/builtin/memory_tools.go`（主动式即时记忆写入）；② `internal/security/taint/` + `policy/gate.go` + `network/`（污点五级+Cedar三层+KillSwitch+SSRFGuard 纵深防御体系）
- `docs/arch/00-Global-Dictionary.md` | 对应 `[Concept]` 条目补充「防退化」标记



**架构文档同步（代码先修复，文档回写）**：

- `M13-Interface-Scheduler.md` | 新增 §1.2.7"消息预处理：引用展开（ContextRefExpander）"：此前已实现但从未接线的 `@file`/`@url`/`git:` 引用展开器，本轮接入 `sse.go` 请求管道最早阶段（基础校验后、SlashCommandRouter 前）；原 §1.2.7 斜线命令系统章节顺延为 §1.2.8，补充执行顺序说明
- `M10-Knowledge-RAG.md §1.6` | 修正 `buildSummaryTree` 文件路径与行号引用：该函数已从 `rag_impl.go` 抽至 `rag_summary_tree.go`（R7 400 行文件上限拆分），并进一步拆为 `fetchLeafChunks`/`summarizeText`/`insertSummaryChunk`/`generateSummaryLevels` 四个子函数收敛 gocyclo；补充 R1.16 修复说明（查询 leaf chunks 后显式提前 `rows.Close()`，早于后续 LLM 摘要调用）
- `decisions/ADR-0031-tts-multi-provider.md` | `TTSConfig` 引用代码路径从 `internal/config/config.go` 更正为 `internal/config/config_types.go`（R7 拆分后子配置结构体的新落点）

**代码改动（不改变对外行为，未逐条建档，仅摘要）**：

- `internal/gateway/authcontext/` | 合并 `internal/gateway/server/context.go`（`AuthContext`/`ContextRefExpander`）与 `authcontext` 包内的重复实现，消除跨包裂脑
- `internal/knowledge/graphrag/summary_gen_handler.go` | R1.16 复发修复：`rows.Close()` 提前于 LLM 摘要调用之前，nilerr 吞错改为 `apperr.Wrap` 上报可重试失败
- `internal/automation/` `internal/llm/` `internal/knowledge/` `internal/sysmgr/` `internal/gateway/server/` `internal/eval/analysis/` `internal/cli/` `internal/downloader/` | 全仓裸 `go func()` 补 `concurrent.SafeGo` 包装（ADR-0029 §H 后续遗留收尾，全量覆盖）
- `internal/bootstrap/bootstrapper.go` `internal/llm/ollamamgr/manager.go` | `fmt.Errorf` 残留改为 `apperr.Wrap`/`apperr.New`
- `internal/gateway/server/server_lifecycle.go` | 修复 `Shutdown(ctx)` 恒返回非 nil 错误的 bug（`fmt.Errorf("...: %w", nilErr)` 包装 nil 错误仍非 nil）
- `internal/extension/lifecycle/mcp_installer.go` | knowledge-source 能力声明在 `MCPKnowledgeConnector` 补齐真实实现前硬拦截注册，避免 SyncScheduler 对桩实现永久重试空转
- `internal/config/config.go`（410→146 行）/ `internal/knowledge/rag_impl.go`（402→302 行） | 拆出 `config_types.go`（子配置结构体）/ `rag_summary_tree.go`（`buildSummaryTree`），修复本轮编辑触发的 R7 400 行文件上限回归

## 2026-07-04（新增症状索引：给排障场景补一个按现象路由的检索入口）

**背景**：随项目变大，AI 排障时只能从症状关键词开始全仓库 grep/通读多个模块——因为现有 `docs/arch/INDEX.md` §2/§3 和 `docs/specs/05-Coding-Workflow.md` 全套流程都是给"已知道要改什么"的编码场景设计的路由，没有"只有一个症状"的排障场景入口。

- `docs/arch/INDEX.md §0`（新增）| "症状索引"表：症状特征 → 归类模块 → 根因类别 → 排查起点，用本次 SQLite 连接池排查过程中的 5 个真实案例做种子数据（管理接口假死、`context deadline exceeded`、浏览器连接数上限误判、插件市场重启后短暂为空、R1.16 反模式误判案例）
- `docs/specs/05-Coding-Workflow.md`（新增 §W0）| 故障排查工作流：先查 §0 索引 → 未命中走 ADR grep + M_X 精读 → 结论必须有实测依据（日志时间线/直接复现/读实际代码路径，禁止仅凭时间点接近下因果结论）→ 定位新根因后强制回填 §0 一行
- `CLAUDE.md §文档加载协议`（新增一段）| 补"排障场景"提示，指向 §0 症状索引 + W0 流程

**补充（同日第二轮，完善种子数据 + 维护规范）**：

- `docs/arch/INDEX.md §0` | 追加 3 条从既有 ADR 挖出的历史真实案例（非本次会话产生）：安全/资源门控静默失效（`ADR-0027` BUG-1/BUG-2）、状态机裸 goroutine 导致 Task 永久卡死（`ADR-0027` BUG-3 / `ADR-0028` BUG-B）、Agent 单例导致多会话状态互相覆盖（`ADR-0029` §E）
- `docs/arch/INDEX.md §0` | 新增"维护规范"小节：单行只留路由信息不展开叙述、相似症状合并、超 ~30 行按模块拆小表（不拆新文件）、根因消除后的行应删除而非无限累积——防止这张表随项目变大反而失控膨胀

## 2026-07-04（SQLite 读写连接池分离：修复管理只读接口被批量写占死的挂起问题）

**新增反模式**：

- `00-Constitution.md §R1.16` | 新增"持有 DB 连接期间发起阻塞式外部调用"反模式条目：未关闭 `Rows`/未提交 `Tx` 前调用 LLM 推理等耗时不确定外部调用，SQLite 连接池有限时会连带卡死其他读写请求

**架构文档同步（代码先修复，文档回写）**：

- `M02-Storage-Fabric.md §8` | 由"全局单连接、读写共用"改写为"读写双连接池分离"：writer（`MaxOpenConns=1`，MutationBus 单写者）+ reader（`MaxOpenConns=4`，`PRAGMA query_only=1`）；根因是插件市场全量同步等长耗时批量写独占唯一连接时，`/v1/mcp-servers`/`/v1/channels` 等只读管理接口及 `MemoryAgent.ScanHighSalience` 会无限期挂起（无 `http.Server` 超时兜底）

**代码改动**：

- `internal/store/store.go` | `OpenSQLite` 拆分 writer/reader 双 `*sql.DB`；`QueryContext`/`QueryRowContext` 切至 reader，`ExecContext`/`DB()` 保持 writer；新增 `ReadDB()`；`:memory:` 场景 reader 复用 writer（避免独立内存库互不可见）；`Close()` 关闭两个池
- `cmd/polaris/boot_server.go` | `server.NewServer(...)` 的 `db` 参数改用 `sb.Store.ReadDB()`（网关 chat/sysadmin/plugin/provider handler 全仓库确认仅 `QueryContext`/`QueryRowContext`，零 `ExecContext`/`BeginTx`）

## 2026-07-04（全量补救开发提示词收尾审计：文档矛盾清理，任务1/3/9/11/13/15/24）

**新增 ADR**：

- `decisions/ADR-0034-tree-sitter-cgo-exception.md` | 新建，记录 `internal/knowledge/` CodeChunker 依赖 `go-tree-sitter`（CGO）作为 ADR-0011 零 CGO 纪律的受限例外，理由/边界/反例守护齐全（任务15）

**规范矛盾修复（funlen 去留，任务24）**：

- `00-Constitution.md §R7` | 删除"`.golangci.yml` 用 funlen 机械化检查"的过时表述（实际未启用 funlen，与 `.golangci.yml` 行内注释及 ADR-0013 判断一致）
- `06-Review.md C-checklist` | R7 lint 项删除 funlen，改为 "gocyclo / nestif"
- `decisions/ADR-0013-lint-machinery-phase1.md` | 修订记录补 2026-07-04 条目：正式落地 Phase 2 `funlen` 判断结论（不采用，理由与 `gocyclo` 冗余 + Go 惯用法误报率高）

**架构文档同步（代码先修复，文档回写）**：

- `M07-Tool-Action-Layer.md` | `XvfbDisplayServer` 装配逻辑补充 2026-07-04 修复说明：新增 `lam.XvfbAvailable()` 二进制探测（xdotool/xwd/convert），叠加原 FeatureGate OS/Tier 判断，避免"硬件条件满足但依赖未装"导致运行时必错（任务3）
- `M04-Agent-Kernel.md §7` | BudgetManager 条目补充月度预算"写路径与读路径断开"缺陷说明及修复：`AgentController.SetMonthlyBudgetUSD` 新增、`boot_server.go` 启动期回读持久化值、`HandleSetBudget` 热更新闭环（任务11）
- `M10-Knowledge-RAG.md §1.7` | CodeChunker 行补充 ADR-0034 引用、`go.mod` indirect 误标修复（须 `CGO_ENABLED=1 go mod tidy`）、`chunker_treesitter_test.go` 差异化回归测试说明（任务15）

**测试补充**：

- `internal/observability/metrics/metrics_test.go` | 新增 `Test_SurpriseIndex_ComputeBasic_OrderSensitive`（验证 Levenshtein 编辑距离的顺序敏感性，此前任务13验收标准要求但未落地）；`ColdStart` 测试阈值断言由过时的 `<3` 修正为实际生效的 `<10`（任务13）
- `internal/knowledge/chunker_treesitter_test.go` | 新增（`//go:build cgo`），用块注释内嵌 `func ` 字样的样本证明字符串匹配 fallback 会产生虚假切分边界、tree-sitter AST 路径正确处理，填补此前 `TestCodeChunker` 样本过于简单、无法区分两条路径的覆盖缺口（任务15）

## 2026-06-11（架构升级：技能执行迁移至 TypeScript 脚本 + Rust 沙箱）

- `M06/M09/M13-bis` | 技能执行从 TinyGo/impl.wasm/wazero 迁移至 TypeScript/Python 脚本（npx tsx），沙箱从 Go wazero 迁移至 Rust wasmtime（FFI）；内置工具直接信任不走沙箱；官方技能/插件移至独立仓库 polaris-plugins-official

## 2026-06-11（docs/arch/decisions + AGENTS.md + 02-Rust-FFI 全量修订）

**ADR 过时内容修正（4 个 ADR）**：

- `ADR-0005 §决策` | surreal_store.go cgo 状态描述由"历史遗留 P3 待处置"改为"cgo→purego 迁移已由 ADR-0011（2026-05-16）执行完毕"
- `ADR-0010 §关联ADR` | 补入 ADR-0011 引用，删除"cgo 偏离待 P3 处置"过时注释
- `ADR-0014 §决策` | 模型版本 "Opus 4.7"（不存在）→ "`claude-opus-4-8`"（Anthropic 当前最新旗舰）
- `ADR-0015 §状态/§2.1/§2.3/§4/§5` | 标注 §2.1 Plugin 层定位已被 ADR-0016 取代；SignatureValid 方案标注已被 TrustTier 替代；§4 "Plugin 放 M13"条目标注已被 ADR-0016 推翻；§5 补 ADR-0016 引用

**AGENTS.md（= CLAUDE.md）更新（6 处）**：

- Header `6 pkg` → `8 pkg`；ADR 清单补 `0020 DeepSeek V4 · 0021 核心机制实现`；DDL 清单末尾补 `029_workflows`，计数 25→26 张表

**docs/specs/ 规范修订**：

- `02-Rust-FFI.md RUST-1` | 删除过时"单文件结构可维持"描述（已拆分为 4 文件）
- `02-Rust-FFI.md RUST-3` | 更新文件组织为实际结构：`lib.rs`+`surreal_store.rs`+`wasmtime_engine.rs`+`check_wasi.rs`（旧描述为未落地的 cedar.rs/vector.rs 拆分方案）
- `02-Rust-FFI.md RUST-4` | 依赖白名单补充 `wasmtime`+`wasmtime-wasi`+`tokio`+`serde`+`serde_json`+`anyhow`+`bytes`+`lazy_static`（已在 Cargo.toml 实际使用，旧白名单漏列）

## 2026-06-09（Gemini gap 报告核查：修复两处真实差异）

**gap 核查结论（10 条 Gemini 报告）**：

- ID 4 CoEvolutionSubscriber / ID 5 PRM 无文档：假 gap；CoEvSubscriber 存在于 `pkg/swarm/self_improve_calibrator.go`；PRM 有完整文档 M04 §4.6
- ID 9 SemanticCache：真实gap — M01 §6.2 描述"无 Get/Put 实现"与代码实际（已实现）不符，已修复
- ID 8 onboard.html：真实gap — M13 §8.1 目录遗漏 `onboard.html`（14.8K），已修复
- ID 3 OnlineReindexer / ID 6 inv_ 测试：开发阶段已知缺口，暂保持文档意图，待代码补齐
- ID 1/2/7/10：部分真实，属"设计超前于实现"正常状态，不改动文档

commit 28c0915

## 2026-06-09（docs/specs 全量审查 + 架构文档一致性修复）

**docs/specs/ 补充修订**：

- `03-Agent-Pattern.md AGENT-1` | FSM 状态数 "10 态" → "11 态"（M04 §1 已加 S_INTERRUPT 为第 11 态）；流程图补 S_INTERRUPT 触发说明；System 1.5 上界 ≤0.6 → <0.6（与 `[System-1.5]` canonical 一致）
- `03-Agent-Pattern.md AGENT-5` | SurpriseIndex 公式替换为 canonical 三组件引用（旧公式含 `tokenBurnRate/maxBurnRate`+`actionSequenceEntropy`，与 `00-Global-Dictionary §3` canonical 不符）

**架构文档一致性修复（本会话 commit 1dfa428）**：

- `M04-Agent-Kernel.md §14`（原 §13，见下方本轮修复条目：13 号重号已消除）| Schema 引用 `003_tasks` → `007_tasks`
- `M09-Self-Improvement-Engine.md` | EvalGenerator `## 3.` → `## 3-bis.`（消除与主 §3 的重号）；同步更新 §跳读
- `M02-Storage-Fabric.md §16` | DDL 总量 21 份 → 25 份，范围扩至 028_apps
- `AGENTS.md` | DDL 清单补入 022~024/028，计数 20→25 张表

## 2026-06-09（架构文档全量审查 + specs 规范修订）

**架构文档（docs/arch/）深度审查修复（3 次提交，共 7 处）**：

- `M01-Inference-Runtime.md §4` | §4.4 编号重复（ComplexityDeterminer 与 Route 方法同编号），修正第二个为 §4.5，同步更新跳读索引
- `M02-Storage-Fabric.md §16` | DDL 覆盖描述"全部 6 份 DDL"严重低估，实为 21 份（001~021），改为正确引用 `internal/protocol/schema/`
- `M03-Observability.md §5.3` | TierParameterTable 中 `GraphRAGLLMDailyBudget` 参数与 M10 inv_M10_05"已取消财务日预算限制"直接冲突，改为 `GraphRAGConcurrentWorkers`（资源维度并发数）
- `M04~M13`（上轮）| 共修复 22 处：Gate/Stage 流水线映射统一、预算财务与资源门控逻辑冲突消除、跨模块契约不一致等

**ADR 体系修复**：

- `decisions/README.md` | 索引缺失 ADR-0019/0020，补全；ADR-0016 日期空白修复；完善 0016 标题
- `decisions/ADR-0008` | L3 沙箱描述"gVisor"与 M07 §4.1 三平台实现（Firecracker/VZ.framework/WSL2）不符，全面修正；Tier-2+ 改为 Tier-1+
- `decisions/ADR-0019` | 背景中历史草案编号（023/026/027/028）与当前 DDL 目录（001-021）不符，补注说明已重整

**ADR 新增**：

- `decisions/ADR-0020` | 确立 DeepSeek V4 为全系统默认核心模型（Canonical Provider）；开放后台认知任务频率限制；本地模型重定位为延迟极限/物理隔离/灾备特权

**docs/specs/ 规范修订**：

- `02-Rust-FFI.md RUST-4` | 依赖白名单补充 `surrealdb`（ADR-0010 已引入），明确"新增须记录 ADR"
- `03-Agent-Pattern.md AGENT-4` | Wasm 加载描述修正：`EmbedWasmLoader` 从 embed.FS 加载（而非直接文件系统读取），消除歧义
- `04-Module-Boundary.md B1` | `internal/` 层描述逻辑倒装修正（原文语义相反）
- `04-Module-Boundary.md B5.3` | 删除过时的 `cgo` 引用（ADR-0011 已完成全量 purego 迁移）
- `06-Review.md C9` | R8 引用不精确，明确 diff ≤300 行来源
- `07-Reference-Implementation.md §7.1` | 删除 `adapter_anthropic.go` 重复 canonical 条目（行 14 与行 27 重叠）
- `08-Doc-Hygiene.md` | 对象范围补充 `M13-bis-Extension-Registry.md`
- `INDEX.md 守则4` | "doc↔代码冲突"描述精确为"规约文档（docs/arch/ + docs/specs/）与代码冲突以规约为准"

## 2026-06-02（扩展系统全局架构重构）

**架构决策变更（破坏性）**：

- `docs/arch/decisions/ADR-0019` | 推翻"插件子组件不跨边界注入全局表"设计，改为 agentskills.io 标准：安装时子 MCP 写 `mcp_servers`（加 `plugin_id`+`work_dir`），子 Skill 写 `skills`（加 `plugin_id`），生命周期级联管理
- `docs/arch/M13-bis-Extension-Registry.md` | §1/§2/§5.3/§5.7/§6.3/§12 全面更新：Plugin Bundle 安装流重写、卸载流重写、API 表更新、表引用速查补充 FK 关系
- `docs/arch/M06-Skill-Library.md §9` | AgentSkills 标准适配章节完整重写：补 exec_mode 完整 frontmatter、skill name 命名规范（独立/插件/内置三种格式）、plugin_id 级联说明

**DDL 变更（破坏性，需删库重建）**：

- `internal/protocol/schema/015_mcp_servers.sql` | 新增 `plugin_id TEXT NOT NULL DEFAULT ''` + `work_dir TEXT NOT NULL DEFAULT ''`
- `internal/protocol/schema/020_extension_instances.sql` | 删除 `enabled`（废字段）、`parent_id`（死代码）两列
- `internal/protocol/schema/021_plugins.sql` | 注释更新，反映 mcp_policy 仅存附加策略
- `internal/protocol/schema/008_skills.sql` | 新增 `plugin_id TEXT NOT NULL DEFAULT ''`

**代码变更摘要**：

- `pkg/extensions/mcp/mcp_manager.go` | 删除 `LoadFromPlugins` / `LoadOnePlugin` / `readFileBytes`；`LoadFromDB` 读 `work_dir`，注入 `MCPClientConfig.WorkDir`
- `pkg/gateway/server/mcp_servers.go` | 删除 `appendPluginMCPServers`；`handleListMCPServers` 改 LEFT JOIN；`DELETE/PUT` 对插件 MCP 返回 405；`startMCPServerCtx` 传入 WorkDir
- `pkg/gateway/server/plugin_catalog.go` | 安装插件时调 `registerPluginMCPServers`（写 mcp_servers）；独立 skill 改 `skill:{hex}` 命名，统一走 `skillReg.Register`；删 `enabled` 引用
- `pkg/gateway/server/plugin_manage.go` | 完整重写：`handleListPlugins` 从 mcp_servers 读状态；`handleUpdatePlugin` 级联同步；`handleTogglePluginMCP` 操作 mcp_servers.enabled
- `pkg/extensions/marketplace/manager.go` | 补 `case "app"` 删 apps 表；独立 skill 卸载改硬删；`removePluginRuntime` 从 mcp_servers 读 ID，级联硬删；删 `OR parent_id=?` 死引用
- `pkg/gateway/server/plugin_custom.go` | `handleCreateApp` 写 apps 表；全量删 `enabled` 引用
- `pkg/cognition/kernel/agent.go` | 修复 `refreshInstalledExtensions`：删不存在的 `version`/`parent_id = ''` 列
- `pkg/cognition/skill/seeder.go` | 删 `enabled`/`parent_id` 死列引用
- `internal/protocol/interfaces.go` | `SkillMeta` 新增 `PluginID string`
- `pkg/cognition/skill/sqlite_registry.go` | Register/Get/List 支持 plugin_id

## 2026-05-23（初始化链路重构）

**BUG 修复**:
- `cmd/polaris/main.go` | schema 加载从相对路径 OpenSQLiteFromDir 改为 embed.FS OpenSQLite，消灭已安装二进制启动失败
- `internal/config/config.go` | Threshold 加载从 config.Load() 内剥离为独立 LoadThresholds(dataDir)，解决 chicken-and-egg；TOML 默认路径改为 ~/.polarisagi/polaris/config/
- `skills/builtin` | Wasm 加载从 FilesystemWasmLoader 改为 EmbedWasmLoader，impl.wasm embed 进二进制
- `configs/defaults.toml` | interface.host 从 0.0.0.0 修正为 127.0.0.1，符合 ARCHITECTURE.md §1 硬约束
- `pkg/substrate/observability/` | SurrealDB Core 启动条件改为 autoConf != nil &&，防止硬件未知时 OOM

**安全**:
- `cmd/polaris/cli.go` | initPromptSecret 改用 term.ReadPassword 屏蔽 API Key 回显

**可观测性**:
- `internal/config/config.go` | loadModuleTOML 错误不再静默吞噬：文件不存在 Debug，解析失败 Error + Fail-Fast
## 2026-05-22（集成接口规范 + DB 写路径澄清）

**规范新增**：
- `docs/arch/00-Global-Dictionary.md §1-ter` | 新增 XR-08（日志规范）、XR-09（LLM 调用）、XR-10（工具/技能/插件执行）、XR-11（文件系统操作分层）；更新 XR-04（DB 写路径三层规范澄清）；更新 `[Storage-SQLite]` 条目
- `docs/specs/00-Constitution.md §R1` | 新增反模式 R1.11（绕过 Provider）、R1.12（直接打印）、R1.13（绕过沙箱执行命令）
- `docs/specs/01-Go-Code.md` | 新增 F8（日志规范+必选 key 约定+级别表）、F9（HTTP Handler 四段式）、F10（Context 传播+deadline 规范）
- `docs/specs/07-Reference-Implementation.md §7.1` | 新增 canonical：HTTP Handler（channels.go）、LLM 调用（adapter_anthropic.go）、MutationBus 写（mutation_bus.go）、Store 同步写（store.go§Put/Txn）

**代码修复**（同日）：
- `cmd/polaris/main.go` | 接入 MutationBus（DatabaseWriter + EventLog + DecisionLog），修复 MutationBus 从未运行的架构断层；添加优雅退出等待 flush
- `pkg/swarm/sqlite_blackboard.go` | 修正注释（删除"委托 MutationBus"的错误声明，说明 CAS 需要直接写的原因）
- `pkg/substrate/mutation_bus.go` | 修正适用范围注释
- `pkg/substrate/storage/store.go` | 修正 Put/DB() 注释，澄清三层写路径定位

**背景**：AI 编程大模型在以下场景无规范可依，导致生成代码跑偏：(1) 数据库写路径（MutationBus/Store.Put/裸SQL 各自适用场景不清）；(2) 日志（`fmt.Printf` 与 `slog` 混用）；(3) LLM 调用（绕过 Provider 直接构造 HTTP）；(4) 工具/技能执行（绕过 ToolRegistry 直接调用具体实现）；(5) HTTP Handler 结构（SQL 内嵌 handler）。本次规范补全覆盖以上全部缺口。

## 2026-05-22（DDL 修改策略 + Schema 整合）

**规范新增**：
- `CLAUDE.md §编码约定` | 新增 `[强制] DDL 修改策略`：上线前直接修改建表文件，禁止 ALTER TABLE 补丁；上线后走编号迁移文件；Phase 判断 SSoT 为 `§当前阶段`
- `05-Coding-Workflow.md` | 新增 W7（Schema 变更流程）：W7.1 Phase 判断 → W7.2 上线前直改 → W7.3 上线后迁移 → W7.4 阶段 A 契约补充

**背景**：AI（Gemini）反复以 ALTER TABLE 补丁文件叠加 Schema 变更，造成 026_skills.sql 死代码、双写冗余、`getInstalledCatalogIDs` 需 UNION 五表等结构性问题。规则缺失是根因。本次同步完成 35→20 文件 Schema 整合（新增 M13-bis / ADR-0019 / extension_instances SSoT）。

## 2026-05-22（文档卫生规约）

**规范新增**：
- `08-Doc-Hygiene.md` | 新增 docs/arch/ 维护边界。H1 三层判定（契约/决策/实现）+ H2 修饰物清理 + H3 数值双写消除 + H4 EntryPoint 化前置条件 + H5 决策迁 ADR + H6 Tier C 禁区 + H7 锚点化 + H8 五条验收门 + H9 Pilot 协议
- `INDEX.md` | 加载策略表新增第 08 行（改架构文档前加载）

**背景**：评估外部 Schema-first 极简方案后，确认全盘 EntryPoint 化会摧毁 PII/Taint/Capability 顺序契约（违反 [HE-Rule-5]/[HE-Rule-6]）。改采"差分式精简"——清修饰、消双写、保契约。首次 Pilot 选 M04。

**Pilot 反馈（同日修订 H8）**：
- M04 Pilot 实跑显示，契约密集型文件做完合规 A1+A2 后字符量微增（+0.76%）——A2 下推让 `spec/state.yaml §m4_kernel.xxx` 引用比裸数字更长
- H8 门 1 原"目标 -15%~-25%"假设所有 M_X 等价，实测假设错误
- 修订：门 1 改为"行动度量优先（A1≥1 + A2 全覆盖）+ 文件类型分级 token 参考值"。契约密集型 -5%~+5%，平衡型 -8%~-18%，实现密集型 -15%~-25%
- 价值：暴露规约缺陷正是 Pilot 的目的，符合 H9 协议

## 2026-05-16（规范体系初始化）

**规范规则新增**：
- `00-Constitution.md` | 新增 R7（可读性硬上限：函数≤60行/文件≤400行/嵌套≤3/圈复杂度≤15）
- `00-Constitution.md` | 新增 R8（参考实现强制引用：写新代码前必须 Read canonical 标杆）
- `04-Module-Boundary.md` | 新增 B5（契约版本化与破坏性变更协议）
- `05-Coding-Workflow.md` | W2 前置 Stage 0（上下文锚定），新增 W6（PR 纪律：原子变更/契约分离/PR 描述模板/对抗审查）
- `06-Review.md` | 新增 C8（参考实现对齐）、C9（PR 体积检查）

**参考实现体系建立**：
- `07-Reference-Implementation.md` | 新增标杆代码索引，全部 `pkg/` 的 canonical 文件确认（见表）
- `pkg/*/AGENTS.md` | 6 份模块级 AI 上下文文件（substrate/cognition/action/swarm/governance/edge）

**支撑体系建立**：
- `../arch/00-Global-Dictionary.md` | 新增 §13 标识符↔概念映射表（命名一致性 SSoT）
- `../arch/decisions/` | 新建 ADR 目录，初始化 ADR-0001~0014（依赖选型回填 + R1.3/R1.4/lint/对抗审查决策）
- `../arch/spec/state.yaml` | 补 `s_interrupt` 状态（spec_consistency_test 发现 Go↔yaml 漂移）
- `.golangci.yml` | 启用 4 个规范 linter（depguard/errorlint/nestif/gocyclo）
- `.github/workflows/constitutional-review.yml` | PR 触发对抗审查 GitHub Action

**ADR 执行状态**（代码已落地，记录于各 ADR 修订记录）：
- ADR-0002：skill.go 本地接口/类型全部删除，统一 protocol.SkillMeta（-~200行死代码）
- ADR-0011：cedar_ffi.go + surreal_store.go 完成 cgo→purego 迁移，ABI 1.0 协议
- ADR-0012：spec_consistency_test.go 落地，4 项 Tier 1 SSoT 守护
- ADR-0013：.golangci.yml 启用 4 linter，CI fail-closed
- ADR-0014：constitutional-review.yml + scripts/constitutional_review.sh 落地
