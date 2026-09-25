# docs/specs/ 变更日志

> 规范本身的演进记录。AI 每次会话开头扫描最近 5 条以感知规范增量。只保留最近 20 条，
> 更早的见 [`CHANGELOG-archive.md`](./CHANGELOG-archive.md)（2026-08-09 归档，
> P3，`local_playground/prompt/docs-optimization-plan.md`）。

格式：`YYYY-MM-DD | 文件 | 变更摘要`

## 2026-09-25（ADR-0101 Token 经济：寒暄快路 / 规划级联 / 缓存稳定前缀 — 含**契约变更**）

- **[契约] `metrics.SelectThinkingMode(replanCount, complexity, surpriseIndex)`**：第二参数由 `maxTaint` 改为 `TaskModel.Complexity`，污点不再驱动思考深度；新增 `metrics.SelectPlanModelPool`，规划池 general→reasoning 级联。新阈值 `m4_kernel.plan.reasoning_complexity`（0.7）。
- **行为变更**：寒暄/致谢/告别跳过 Perceive LLM 与记忆召回（`fsm.ClassifyIntentWeight`，route=`phatic_bypass`）；短确认仍走 Perceive 但不召回长期记忆；Anthropic system 按消息分 block、首末块各一缓存断点；`SysEnvSnapshot` 去掉已用内存/磁盘剩余。
- 新增 prompt 段落时：稳定内容在前、易变内容在后，禁止在前缀写入时间戳/实时资源量/无序 map；新增 LLM 调用点须说明所用池与思考档的依据（ADR-0101 反例守护）。

## 2026-09-25（ADR-0100 DeepSeek Harness 评审：上下文溢出与执行结果 spill — 含**契约变更**）

- **[契约] `protocol.ErrContextOverflow`**：上下文超限 / payload 过大是请求侧故障。InferenceRouter 不计入熔断与成功率，只 failover 到窗口更大的 Provider，否则返回包装该哨兵的错误；调用方须缩减请求后再试，不得当作 Provider 耗尽处理。新增 Provider 调用点记录健康度一律经 `recordAttempt`。
- **行为变更**：Agent 收到该哨兵时对固定前缀外的消息首尾保留修剪一次后重试（不走 LLM 摘要）；执行结果 >4KB 经 `ToolRefOffloader` 卸载，预览首行给出 `read_tool_ref` 取回提示，`logs/exec_results/` 不再写入；观察截断改为首尾保留（`pkg/util.ElideMiddle`）。
- 新增截断逻辑时：用 `util.ElideMiddle` 保留首尾，并给出可取回全文的引用或如实声明未保留。

## 2026-09-25（outbox 投影资源压力推迟 — 含**契约变更**）

- **[契约] `protocol.WithDeferrableBackgroundWork` / `ErrBackgroundDeferred`**：串行消费队列（当前为 OutboxWorker）的后台 LLM 调用被水位线拒绝时，路由立即返回推迟哨兵而不挂起。outbox handler 及其下游**须沿错误链上抛**该哨兵（`apperr.Wrap` / `%w`），不得降级落库或吞成 nil；Worker 据此推迟 30s 且不计入 attempts / crash_recovery_count（M02 §2.5）。整合管线已接：抽取不再因推迟退回规则抽取，`Run` 不因推迟另投 OOM 重试。

## 2026-09-25（审查修复：LLM 额度 / 能力令牌绑定 / 后台推理优先级 — 含**契约变更**）

- **[契约] `protocol.WithBackgroundWork` / `IsBackgroundWork`**：可降级后台工作标记。空闲自进化任务 ctx 与 `AcquireHeadless` 获取的 Agent 的 effect ctx 自动带标记；InferenceRouter 据此以 priority=1 调 `AdmitLLM`（受水位线约束，被拒时 2s 间隔挂起至 ctx 到期），且不刷新用户活跃时间。新增后台 LLM 调用方须标记，否则按用户可见推理对待。
- **[契约] JIT 能力令牌只为已通过 S_VALIDATE 的计划节点签发**（工具名 + 参数字节一致；ADR-0098 决策五复核）。执行路径上新增的工具调用来源若不经 S_VALIDATE，将拿不到令牌，写类与 trust<3 工具会被执行闸门拒绝。
- **行为变更**：`StreamInfer` 在选路前获取 LLM 额度（修复跨池降级"没借就还"使 llmInFlight 变负）；linux 容器可用内存按 working set（扣除 inactive_file）计。

## 2026-09-25（ADR-0098 决策十 回合内人工审批 / 决策七追记 — 含**契约变更**）

- **[契约] `types.AgentStreamEventApproval` + `AgentStreamEvent.DeadlineNs`**：回合内阻塞式 HITL 推给对话流，session 映射 `status{type:"approval_required", id, tool, input, deadline_ns}`；Agent 发起 HITL 一律经 `promptHITLInTurn`，不得直调 `hitl.Prompt` 使对话用户不可见。
- **[契约] `ExemptionVault.Lookup(agentID, content)`**：每 Agent 多枚令牌、按内容哈希匹配（原 `Lookup(agentID)` 单枚覆盖写移除）。
- **安全接线**：S_VALIDATE L1_taint 拦截发起 `CheckpointTaintReview`，批准后重新校验；TaintMedium write_network 同样查询复核豁免（M11 §2.5/§3）。
- **行为变更**：S_PLAN 空输出重试改用 `ThinkingDisabled`；DeepSeek 适配器对显式 `ThinkingDisabled` 发送 `thinking.type=disabled`（此前未生效，服务端默认开启思考）。

## 2026-09-25（ADR-0099 Embedding 调度 / ADR-0098 决策五～九 — 含**契约变更**）

- **[契约] `search.Embedder.Embed(ctx, text)`**（ADR-0099）：调用方 ctx 贯穿到下游 HTTP；新增实现须遵守调用方截止时间，无上游 ctx 的调用点显式传 `context.Background()` 并注释原因。EmbeddingBatcher 改为 High/Low 独立通道，阈值 `m1_router.embed.*`。
- **[契约] `TriggerFillRetry` / `TriggerReflectContinue`**（ADR-0098 决策七/八，枚举尾部追加）：空输出自环重试；反思判定未达成时回到规划（共用 ReplanGuard）。
- **行为变更**：重规划耗尽不再进入 S_FAILED，而是转 S_RESPOND 说明失败（`TurnDegraded`，指标按失败计，决策九）；`m4_kernel.max_steps` 10 → 24。
- **安全接线**：Agent 执行节点前 JIT 签发一次性能力令牌（M07 §6）；S_VALIDATE L1 与执行闸门同问 `(agent, tool_execute, <tool>)`；本地令牌先于 IP 鉴权冷却判定（ADR-0096 决策五追记）；AgentPool 交互预留槽位 `m8.agents.interactive_reserved`（ADR-0025 §E 追记）。
- 编写新的内核阶段或改阶段契约模板时：须保持 `internal/agent/turn_contract_eval_test.go` 全绿（回合契约评测，HE-4）。

## 2026-09-25（ADR-0098 回合输出通道分离与 S_RESPOND — 含**契约变更**）

`docs/arch/decisions/ADR-0098-turn-output-channel-and-respond-state.md` 新增：

- **[契约] `protocol.LLMFillEffect.Audience`**：零值 `AudienceInternal`（fail-closed）。只有 `AudienceUser`（当前仅 S_RESPOND）的 token 以 `AgentStreamEventToken` 发布；新增 LLM 阶段不声明即不外泄。`state.yaml par_inv_06`。
- **[契约] FSM 第 14 态 `S_RESPOND`**：`AgentStateRespond` / `TriggerRespondReady` / `TriggerRespondDone` 追加在枚举尾部（状态值以 `%d` 落盘，禁止中间插入）。S_COMPLETE 只能由 S_RESPOND 进入（`par_inv_07`）；S_REFLECT 失败改为尽力而为继续进入 S_RESPOND。
- **[契约] `protocol.AgentController.SetConversationHistory`**：session 每轮注入本轮之前的历史；阈值 `m4_kernel.conversation.history_max_messages/bytes`。
- **[契约] `types.AgentStreamEventPhase` + `types.TurnPhase`**：阶段进度结构化事件，session 映射为 `status{type:"phase"}`。
- 阶段契约 SSoT 收敛到 `configs/prompts/kernel/{perceive,plan,reflect,respond}.md`，记忆路径不再内联一句话指令；Perceive/Reflect 请求 `json_object` 约束解码；Perceive 输出 `NeedsTools` 由 Go 解析后路由直答。
- 编写新 LLM 阶段时：默认内部受众；需要面向用户输出的只有 S_RESPOND，不得另开第二条用户输出通道。

## 2026-09-21（ADR-0096 桌面版/命令行版形态 — 含**破坏性变更**）

`docs/arch/decisions/ADR-0096-desktop-shell-and-daemon-client-split.md` 新增（八条决策）：

- 守护进程 + 薄客户端（Web/桌面/CLI 共用 `HTTP /v1 + SSE` 唯一业务通道）；桌面外壳选 Tauri v2 + 独立 sidecar 进程，驳回 Wails（CGO 破坏交叉编译矩阵 / 绑定绕过网关中间件 / 生命周期绑窗口）。
- 无平台签名证书下的分发：命令行安装为主渠道，信任锚点全部落到 cosign（ADR-0095）；守护进程与 dylib 装在 app bundle 之外，故 `sysmgr/updater` 的就地替换在两条渠道行为一致。
- **[破坏性] 取消「未配置 API Key 时回环即凭证」**：本机客户端改用 `run/polaris.token`（CLI/桌面壳走请求头，Web UI 走 HttpOnly + SameSite=Strict Cookie，前端零改动）。迁移期逃生阀 `POLARIS_ALLOW_ANONYMOUS_LOOPBACK=1`（默认关），供本机裸调 `/v1` 的第三方 OpenAI 兼容客户端过渡；静态外壳与 `/healthz` 等健康端点不受影响。
- `internal/cli`（在册未接线包）删除：`AgentREPL` 依赖 `InferFn` 直连内核，与唯一业务通道的约束冲突。
- 新增 `internal/runtimeinfo`（run/ 状态读写）、`polaris serve`、`polaris service install|uninstall|status`；单实例锁用 flock/LockFileEx，不用 PID 探活。
- 新增 `desktop/`（Tauri v2 外壳）：启动判定状态机（宿主/附着/四态故障页）、托盘、关窗收托盘；`make desktop-build|test|lint|bundle|smoke`。
- 服务注册归并为单一实现：`scripts/install.sh` / `install.ps1` 不再自写 launchd plist 与计划任务，改调 `polaris service install`（此前两份实现标签不同，装上即产生两个注册项）。
- `/healthz` 响应增加 `version`：外壳与守护进程分开安装、各自更新，需在附着前比对版本。
- `polaris service status` 判活改以单实例锁为准（新增 `probeInstanceLock`），`kill -9` 残留的 run/ 文件报 `stale`；守护进程取锁改为约 1 秒短暂重试。

## 2026-08-09（新增 ADR-0094 Fail-Closed 安全判定与生命周期锚定 Lint 门控）

`docs/arch/decisions/ADR-0094-fail-closed-and-lifecycle-lint-gates.md` 新增：

- 明确 8 条规范性决策（Fail-Closed 三态、身份单源、生命周期锚定、状态落盘不得静默吞错、FFI 指针保活判据、结构化载体禁直拼、stdlib 包装透传与模型池枚举规范）。
- 对应在 `internal/lint/` 中补齐 10 条 CI 机械化门控规则。

## 2026-08-01（阶段01~05 新增 4 条防退化 lint 不变量）


`internal/lint/inv_lint_test.go` 新增：

- `Test_inv_M13_06_ChannelNoRawHTTPClient`：`internal/channel/` 下禁止裸 HTTP Client（三个渠道 Poller 收敛至 `host.HTTPClient` 恢复 SSRF 防护）
- `Test_inv_VFS_QuotaMustUseWithQuota`：VFS 配额预占必须经 `WithQuota` 闭包收敛，禁止手动 `CheckQuota`/`ReleaseQuota` 配对（防止阶段03 GR-6-001 一类遗漏路径重新出现）
- `Test_inv_M13_SessionPkgNoHTTP`：`internal/gateway/session/` 下禁止任何 `net/http` 依赖（ADR-0085，SessionOrchestrator 零 HTTP 依赖约束）
- `Test_inv_M13_SingleTurnEntry`：`SetTaskIntent`+`SendIntent(types.TriggerIntentReceived)` 驱动 FSM 轮次的调用只允许出现在三处固定位置（ADR-0085，防"第三方会话编排重复实现"重新出现）

## 2026-07-12（新建 internal/execute 模块，收敛单/多 Agent 执行引擎，ADR-0046）

响应"是否该重新引入 Executor 模块"的提问，调研 2026 年 Planner-Executor 分离行业实践后判断：不采纳"规划一并并入"，但采纳"单/多 Agent 执行引擎物理归拢"。

- `internal/agent/dag/` → `internal/execute/dag/`（git mv，package 名不变）| FSM 核心（`fsm/state_machine.go`、`fsm/transitions.go`、`agent_execute_*.go`）此前直接 import 同目录子包，迁出后按 `agent/provider.go` 既有消费端接口模式改造：新增 `DAGRunner`/`DAGValidator` 接口 + `DAGToolExecutorFn`/`DAGLeaseRenewFn` 桥接函数类型（刻意用匿名函数类型避免与 execute/dag 相互 import）；`execute/dag/runner.go` 新增无状态 `Runner`/`Validator` 适配器；`agent.go` 的 `NewAgentWithDefaults` 保留对 execute/dag 的直接 import 作为测试默认值例外，生产路径由 `cmd/polaris/boot_agent.go` 的 `buildAgent` 显式注入（覆盖 agent-0 与 AgentPool 派生的所有实例）
- `internal/swarm/orchestrator/` → `internal/execute/orchestrator/`（git mv，仅 6 处组装根引用，纯路径重命名，无接口改造）
- `internal/protocol/dag_node.go` + 新增 `dag_validation.go` | 迁移中发现并修复遗留死代码：`DAGPlan`/`ExecEdge`/`EdgePolarity` 此前已在 protocol 定义却从未被 `agent/dag` 引用（`agent/dag` 保留了独立重复定义）；本次改为正确的类型别名，`NodeResult`/`DAGValidationContext`/`DAGValidationError` 一并上移
- `internal/config/immutable_constants.go` | `ImmutableKernelPackages()` 注释与实际返回值长期不一致的问题一并修正；新增 `internal/execute/dag/` 进白名单（承接 L1 TaintGate/PolicyGate 校验，随迁移保留 L4 保护，不扩大到未受保护过的 `internal/execute/orchestrator/`）
- `internal/execute/CLAUDE.md`（新增）+ `internal/swarm/CLAUDE.md` + `internal/agent/CLAUDE.md` | 模块权力边界随物理迁移同步更新
- `docs/arch/decisions/ADR-0046-execute-module.md`（新增）+ `M04-Agent-Kernel.md` + `M08-Multi-Agent-Orchestrator.md` + 根 `CLAUDE.md` 项目结构树 + `docs/arch/INDEX.md` §0 症状9 | 记录决策依据与现状同步

**附带核实**：`go build ./...`、`go test ./internal/agent/... ./internal/execute/... ./internal/protocol/... ./cmd/polaris/...`、`go test ./internal/config/...` 均通过；全量 `make lint`/`go test ./...` 见下一条提交前核实记录。

## 2026-07-12（StateGraphExecutor 首个生产接入：workflow DAG 并行 + 失败重试）

响应"分析 StateGraphExecutor 该接入哪个模块并实际接入，系统最优/功能强大/性能最优"的要求，选定 WebUI 工作流自动化功能作为接入点（此前是不经 Blackboard 的朴素顺序 for 循环）。实现过程中额外发现并修复两个此前未暴露的真实缺陷（因全仓库此前没有真实生产 Worker 消费任一 `Pattern*Executor` 投递的任务）：

- `internal/swarm/orchestrator/pattern_state_graph.go` | 补齐扇入 AND-Join 记账：原实现对无条件边是"任一前驱完成即触发"OR 语义，多前驱 DAG 场景会误触发；新增 `requiredPreds`/`arrivedPreds` + `arriveJoin`，仅对无条件非自环边生效，条件边/自环边 OR 语义不变
- `internal/swarm/orchestrator/sqlite_blackboard.go` + `007_tasks.sql` | 补齐 `PostTask`/`PostBatch` 从未持久化 `TaskEntry.Intent` 的缺陷（新增 `intent` 列）；`types.TaskSnapshot` 新增 `Intent`/`Type` 字段，`PeekTask` 一并读回
- `internal/gateway/server/sysadmin/workflowadmin/` | 新增 `workflow_graph.go`（`buildGraphSpec`：`depends_on` 为 seq 索引字符串，非步骤 id——id 每次保存都重新生成，不稳定）+ `workflow_step_worker.go`（自订阅 Blackboard 认领执行，业务失败走 `CompleteTask` 而非 `FailTask` 把重试交给声明式自环条件边）；`029_workflows.sql` 新增 `max_retries` 列 + `workflows.type` 列首次被消费
- `internal/gateway/server/server_lifecycle.go` | Worker `ListenLoop` 随 `Start()`/`Shutdown()` 生命周期管理
- `web/src/pages/automation.html` + `web/src/js/store/workflow.js` + `i18n.js` | 工作流编辑器新增执行模式（chain/dag）选择器 + 每步骤依赖勾选（仅 dag 模式）+ 失败重试次数输入
- `docs/arch/M08-Multi-Agent-Orchestrator.md` §3-quinquies-a/b/c | 新增三段分别说明 AND-Join 修复、Blackboard Intent 持久化修复、workflow 接入设计

**附带核实**：`go build ./...`、`golangci-lint run ./...`（含 wasip1 子集）、`go test ./...`（0 FAIL）、`npm run build`（web/ 构建通过）均已验证。

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

> 更早的历史条目（2026-06-11 docs/arch/decisions + AGENTS.md + 02-Rust-FFI 全量修订 及更早，共 10 条）已归档至 [`CHANGELOG-archive.md`](./CHANGELOG-archive.md)。
