# docs/specs/ 变更日志

> 规范本身的演进记录。AI 每次会话开头扫描最近 5 条以感知规范增量。仅保留 2026-09-25 及以后条目（每条 ≤6 行），
> 更早的见 [`CHANGELOG-archive.md`](./CHANGELOG-archive.md)（2026-10-12 续归档至 2026-09-21）。

格式：`YYYY-MM-DD | 文件 | 变更摘要`

## 2026-10-12（文档第四轮精简 — 含**规范语义裁定**）

- **[ADR]** 59 → 32 份：27 份按主题并入存活编号（对照见 `decisions/README.md`「已删除」表）；存活文件含「旧引用映射」表，代码注释旧「ADR-xxxx 决策N」照常可达。
- **[裁定] 03 AGENT-7**：Tier-0 容器沙箱请求降级为 SandboxNativeOS，仅 `CapPrivileged` 返回 `ErrTier0SandboxLimit`（对齐 ADR-0008 决策一 2026-09-20 复核）。
- **[订正]** 03/07 FSM 12 态 → 14 态（含 S_AWAIT_AGENT、S_RESPOND）；模块 CLAUDE.md 层级标签统一为 04 B1；specs/INDEX 守则 4 改用 CLAUDE.md 两类文档模型。
- **[精简]** 规则编号全部保留：已有门控的规则改为「规则句 + 门控名」，过程追记并入结论；CLAUDE.md 202 → 137 行，启动必读 ≈ 12K token。

## 2026-10-12（ADR-0111 设置页「多节点」— 含**契约变更**）

- **[契约] `config.PatchUserConfig`**：按 (section, key) 就地改写用户 `config.toml`（保留注释与顺序，`Load` 校验后原子替换）；设置页写配置的唯一入口。
- **[契约] `GET/PUT /v1/mesh/settings`**（本机令牌，对端令牌 403）：节点名 / 额外监听 / 对端策略即时生效，`a2a.enabled` 变化返回 `restart_required`；`mesh.Service.Policy/SetPolicy` 运行期可更新，额外监听器增量启停。
- **[修复]** `GET /v1/config` 未设 `POLARIS_CONFIG` 时显示实际加载的用户 `config.toml`。**[UI]** 设置 → 多节点（`web/src/js/store/mesh.js`）。

## 2026-10-12（ADR-0111 双实例实测修订 — 含**契约变更**）

- **[契约] `AgentProfileSpec.FileRoot` + `protocol.WithConfinedRoot`**：非空时内置文件/命令工具只能访问该目录（根非法 fail-closed）；对端任务固定为 `[mesh] peer_file_root`（默认 `<data_dir>/workspace/peer-shared`）。
- **[契约] 只读角色**：MCP 工具一律视为可写；只读/禁止委派角色隐藏写类工具与 MCP 委派通道，越权计划在 S_VALIDATE 前段打回（`checkPlanAgainstProfile`）。
- **[契约] `MCPManager.SetDelegationTokenMinter`**（仅为委派契约工具补发一次性能力令牌）；`orchestrator.PendingTaskLister`（`DefaultTaskWorker` 启动补扫积压任务）。
- **[修复] FSM**：S_EXECUTE↔S_AWAIT_AGENT 恢复产出 DeterministicEffect（零效应时委派 watcher 不启动，父任务永不恢复）。

## 2026-10-12（ADR-0111 跨节点委派 — 含**契约变更**）

- **[契约] `ClientTypePeer`**（`authcontext`）：对端令牌 `pmt_…` 只能访问 `/a2a/v1`、`/mcp`、Agent Card，其余路由 403。新黑板任务类型 `peer_delegation`（`WithPeerProfile` 未注入 fail-closed；记忆命名空间 `peer:<task_id>`）。
- **[契约] `a2a_delegate` 入参增 `spawn_depth`/`chain`**；`SafeDialer.SetPeerAllowlist` 只对 `mesh_peers` 登记的精确 host:port 放行私网/CGNAT。
- **[配置]** `[a2a]` 改为 Agent Card 配置（`[[a2a.skill]]`、`advertised_url`、`legacy_endpoint`）；新增 `[mesh]`；`[interface]` 增 `extra_listen`/`allow_wildcard_listen`。
- **[Schema]** 新增 `048_mesh.sql`；`PostTask` 补写 `tasks.intent_taint`。**[依赖]** `github.com/a2aproject/a2a-go/v2` v2.6.0（仅 JSON-RPC 绑定，无 CGO）。

## 2026-10-02（R7 文件行数软硬上限调整）

- **[阈值] R7**：软 500 / 硬 550 → 软 800 / 硬 850。801–850 仅告警，> 850 拦截须拆分（`00-Constitution.md §R7`，`internal/lint/naming_and_size_test.go`）；存量名单已清空。

## 2026-09-30（ADR-0105 WP10：真实请求边界门控 — 含**契约变更**）

- **[契约] `ImmutableCore.StableMessageWithContracts()` → `StableMessagesWithContracts() []types.Message`**：返回 `[契约段, 可变核心]`；L0 按稳定度排序，契约段独立首条且不计入 32KB 上限。
- **[契约] `PIITokenVault.ReleaseTask`**：回合终态只释放不清空令牌映射，同一 PII 跨回合令牌不变；`ClearTask` 留给显式销毁。
- **[修复] 前缀稳定**：PRM 候选请求在 `uniform_tools` 下携带一致 tools；热路径压缩与溢出恢复只改缓存前缀之后的内容。
- **[门控]** `internal/agent/request_prefix_gate*_test.go`：L0..L2 字节相等 / 相邻回合前缀 / tools 一致 / 无旁路召回 / PII 令牌一致（含负向验证）。

## 2026-09-30（ADR-0105 WP11：情景召回按相关度、删除召回画像 — 含**契约变更**）

- **[契约] `types.EpisodicQuery.Semantic` 删除，新增 `IDs`**（按 ID 直取并保序，仍受 ProjectID/MaxTaintLevel/SessionID/Topics 过滤）；子串匹配路径删除。
- **[契约] 新增消费端接口 `fsm.EpisodicSearcher.FTSEpisodic`**；`fsm.RecallProfile`、`TurnRecall.ProfileDone` 删除，用户画像只走 L0 `ImmutableCore.UserProfile`。
- 情景召回：FTS(BM25) → 项目隔离 → 按 ID 取正文；无 SurrealDB/FTS 失败降级为空。

## 2026-09-30（ADR-0105 WP8：召回单一管线、RRF 融合 — 含**契约变更**）

- **[契约] 删除第二条召回管线**（`injectMemoryToMsgs`/`assembleWithBudget`/`Assembler`/`AgentConfig.SurpriseHintThreshold`）；召回只在回合内、L4 一处注入。
- **[契约] `fsm.CognitiveSearcher` 去掉 `VecKNN`，`FTSSearch` 增 `ctx`**；`CogResult`/`KnowledgeResult` 增 `Score`/`Taint`；新增 `fsm.RecallReranker` 与 `Agent.SetRecallReranker`（无生产加载路径，门不生效）。
- **[配置] `m4_kernel.recall.*`** 增 `weight_{reflection,episodic,semantic,rag}`、`rerank_top_n`、`rerank_min_prob`、`rag_min_surprise`；`min_score_ratio` 默认 0.2→0。L2 语义召回接线 `cmd/polaris/adapters_recall.go`。

## 2026-09-29（ADR-0105 WP1/WP3/WP7：五层前缀账本 + 对话历史只追加 — 含**契约变更**）

- **[契约] `protocol.ImmutableCore` 增 `StableMessage()`/`VolatileContent()`**：`PrependToMessages` 改为稳定层置前、易变层插在最后一条消息之前；测试替身须补两方法。稳定层集合必须确定序（工具按名升序、SQL `ORDER BY`、map 按键排序），连接状态标记移入易变层。
- **[契约] `PromptBuilder.BuildLayered` + `protocol.Layer*`**：内核按 L0..L4 输出；`fsm.WriteSessionLayer/WritePlanHints/FinishLayered` 为公共写入器，新增阶段须经它们写 L1/L2。`StateContext.PlanHintBlocks` 取代对 `msgs[0]` 的追加。
- **[契约] 对话历史**：`fsm.RenderConversationHistory` → `fsm.WindowConversationHistory`（分块跳窗 + 确定性锚定摘要）。
- **[契约] 阶段契约并入 L0（WP7）**：`m4_kernel.prompt.phase_contracts_in_core`（默认 true）；`ImmutableCore` 可选能力 `HasPhaseContracts()`；L3 契约位经 `fsm.WritePhaseContract`。
- **[契约] 召回预算（WP3）**：`m4_kernel.recall.{item_max_chars,max_tokens,min_score,min_score_ratio}`；`StateContext.TurnRecall` 回合内复用；Perceive 不再查反思/L2/RAG，改由 Plan 补查；`fsm.HighTaintWarning` 常量取代字符串耦合。

## 2026-09-29（ADR-0103 决策八：MCP 2026-07-28 客户端 — 含**契约变更**与 **DDL 变更**）

- **[DDL]** `015_mcp_servers.sql` 增 `oauth` 列；新增 `040_mcp_oauth.sql`（`mcp_oauth_clients`/`mcp_oauth_tokens`）；`013_chat.sql` 增 `chat_app_views`、`chat_sessions.app_model_context`：开发库需删除重建。
- **[契约] mcp 包**：`RPCError`（-32020/-32021/-32022）；`MCPClient.SetInputHandler/SetTokenSource/CallToolTaintedRaw`；`MCPManager.SetElicitor/SetCredentialCipher/BeginAuthorization/CompleteAuthorization/ReadUIResource/CallToolAsApp`；`ToolRegistrar.RegisterAppOnly/ExecuteAppTool`（app-only 工具不进 `Lookup/List`）；`hook.Runner.FireElicitation*`；`AppendMessage`/`SaveMessage` 返回消息 ID。
- **[API]** `/v1/elicitations`；`/v1/mcp-servers/{id}/oauth*`、`GET /oauth/mcp/callback`、`GET /oauth/client-metadata.json`（精确鉴权豁免）；`/v1/mcp-apps/*`。**[配置]** `[interface] apps_enabled/apps_sandbox_port/apps_sandbox_origin/apps_host_origin`。
- **[前端]** MCP Apps 宿主桥 `web/src/js/mcp_apps.js` + 沙箱页 `web/src/mcp-apps/sandbox.html`（CSP 以 sha256 锁定内联脚本，改脚本须重算，`TestMCPAppsSandboxHTML_CSPHashMatchesInlineScript` 兜底）；elicitation 对话框；插件详情弹窗；插件目录窗口化渲染。
- **[修复]** 主密钥轮换改为 `repo.RekeyVaultCiphertexts` 单事务覆盖全部 Vault 密文列（新增密文列须登记）；`GET /v1/mcp-servers` 补回显 `headers`（PUT 为整列覆盖）。

## 2026-09-27（ADR-0103：市场收尾 / 标准市场同步 / 依赖检查 — 含**契约变更**）

- **[契约] `POST /v1/plugins/create`、`/v1/skills/create` 改为 `{"name","source"}`**（删 `manifest_url`/`repo_url`/`entrypoint`）；`pluginspec.ParseSourceSpec`、`CatalogInstaller.InstallFromSource/Prune`、`POST /v1/plugins/prune`；添加市场须 `plugin:manage`。
- **[删除]** `marketplace/adapter*.go`、`gateway/server/plugin/sync_parsers*.go`、`catalog.json` 私有格式及 `protocol.{PluginJSON,PluginInterface,MCPConfig,...}`；**[契约]** `marketplace.{CatalogSync,CatalogInstaller,SourceFetcher}`、`pluginspec.GetMarketplace`。
- **[依赖检查]** `lifecycle.PluginDependencies`（`Check/EnableBlocker/EnforceAll`）、`GET /v1/plugins/{id}/dependencies`，依赖不满足启用返回 409；新增依赖 `github.com/Masterminds/semver/v3`。
- **[修复]** `UpdateInstanceStatus` 成功态写 NULL 违反 `error_msg NOT NULL`。内置 MCP 市场改 `modelcontextprotocol/registry`（开发库需重建）。

## 2026-09-27（ADR-0103：Codex 应用绑定 / Claude 插件 channels — 含**契约变更**与 **DDL 变更**）

- **[DDL] `021_plugins.sql` 新增 `plugin_app_bindings`、`plugin_channels`**：开发库需删除重建。
- **[契约]** `ExtensionRepository.ListPluginAppBindings/ReplacePluginAppBindings/ListPluginChannelStates/SavePluginChannelState`；`lifecycle.ResolveAppBindings/BindApp`、`lifecycle.ChannelService`。
- **[契约]** `MCPClient.SetNotificationHandler/ServerMeta/Notify`、`MCPManager.SetNotificationSink/NotifyServer`、`hitl.GatewayImpl.SetPromptRelay`。API `GET /v1/plugins/{id}/apps`、`PUT /v1/plugins/{id}/apps/{alias}`、`GET|PUT /v1/plugins/channels`。

## 2026-09-26（ADR-0103：子 Agent、fork 技能、hooks、标准技能运行时 — 含**契约变更**与 **DDL 变更**）

- **[契约] 子 Agent**：`protocol.AgentController.SetAgentProfile`、`types.WithAgentProfile`（只收窄能力，`Agent.checkProfileTool` 硬拦截）；`orchestrator.SubagentRunner` 为唯一执行实现；`builtin.RegisterDelegationTools`、`list_agents`；删除 `orchestrator.AgentProfile` 与 `/v1/admin/profiles`，改 `GET /v1/agents`。
- **[契约] hooks.json 引擎**：`internal/action/hook` 重写（`hook.NewRunner(hook.Deps{...})`）；`sandbox.HookFirer` 改 `FirePreToolUse/FirePostToolUse`；`session.HookRunner`/`chat.HookRunner` 改标准事件方法；删除 `sysadmin.HookRunner`、`ExecRequest.Command`、`KindHookExecute`；`GET /v1/hooks`、`POST|DELETE /v1/hooks/trust`。
- **[契约] 标准技能运行时**：`types.SkillMeta` 增 `Description/DisplayName/Kind/DisableModelInvocation/DisableUserInvocation/SkillDir/Spec`，模型可见性只经 `catalog.ModelToolView`；技能入参 `{"arguments": "..."}`；`session.CommandResult.RewrittenInput`；`skill.SkillInjector`（`` !`cmd` `` 按命令集合哈希信任）、`agent.SkillForker`。
- **[DDL]** `008_skills.sql` 增 `description/display_name/kind/model_invocable/user_invocable/skill_dir/script_path/spec`；`021_plugins.sql` 增 `hook_trust`、`plugin_user_config`（敏感值只存 Vault 密文）：开发库需删除重建。
- **[契约]** `mcp.PluginVarsResolver.ResolvePluginVars(ctx, pluginID, serverID)`；`GET/PUT /v1/plugins/{id}/config`；升级重装只删子组件，卸载才删 plugins 行与配置。

## 2026-09-26（ADR-0103 第二阶段 + 双标准对齐：统一安装路径 — 含**契约变更**与 **DDL 变更**）

- **[DDL] `015_mcp_servers.sql` 新增 `headers`**：开发库需删除重建（`rm ~/.polaris/data/polaris.db`）。
- **[契约]** `internal/extension/pluginspec` 是插件/技能/MCP 配置唯一解析器，新增清单只接受 agent-plugins 1.0 / `.claude-plugin` / `.codex-plugin`，不得新增私有格式。MCP 启动只经 `MCPManager.StartFromDB(serverID)`（`mcp_servers` 行为配置权威源，不得自行拼 `MCPClientConfig` 调 `Add`）。
- **[契约]** `lifecycle.Installer.Install` 返回 `InstallResult{Dir, RuntimeID}`，`Manager.CompleteInstall` 为文件就位后的唯一绑定入口；卸载改收 `RuntimeUninstaller`；`UpsertPlugin` 改收 `types.PluginRow`。
- **[删除] `ext_type=app`**（`types.TypeApp`、`protocol.App`、`repo.AppRepository`、`/v1/apps*`、`apps`(028) 表）；新代码不得再引入 App 扩展类型；UI「MCP」改称「连接器」，底层 `ext_type` 仍为 `mcp`。

## 2026-09-25（R7 文件行数改为软/硬双阈值）

- **[阈值] R7**：≤400 硬上限 → 软 500 / 硬 550（后于 2026-10-02 调整）；存量名单只收超硬上限文件，ratchet 只减不增。

## 2026-09-25（ADR-0105 Token 经济增量（原 ADR-0102）：寒暄快路 / 直答合并 / 按失败成因升级 / 反思跳过 — 含**契约变更**）

- **行为变更**：寒暄/致谢/告别跳过 Perceive LLM 与召回（`fsm.ClassifyIntentWeight`，route=`phatic_bypass`）；简单任务（Complexity<`m4_kernel.reflect.skip_complexity`=0.4）首轮全成功跳过 Reflect（`reflect_skipped`）；S_PLAN 文本工具目录只列名称，**新增不支持原生 tools 的适配器须自行渲染 `InferOptions.Tools`**。
- **[契约] `fsm.TaskModel.Reply`/`StateContext.PreparedReply`**：`NeedsTools=false` 时 Perceive 同次产出回复，直答回合 1 次 LLM；新增 S_RESPOND 入边不得消费 `PreparedReply`。
- **[契约] `metrics.SelectPlanTier(escalation, complexity, SI)` 取代 `SelectThinkingMode`**；`fsm.RecordFailure(FailureKind)`：新增重规划来源须按成因记录（安全/瞬时/观察不升级，能力类才升级），不得直改 `sCtx.Escalation`。
- 新增 prompt 段落：稳定内容在前，前缀禁写时间戳/实时资源量/无序 map；新增 LLM 调用点须说明所用池与思考档依据。

## 2026-09-25（ADR-0105 Token 消耗治理（原 ADR-0101）— 含**契约变更**）

- **[契约] 新增后台 LLM 调用须显式指定思考档位**（DeepSeek 省略即 effort=high）：机械任务用 `types.ThinkingDisabled`；新增 `types.ThinkingLow`。`m4_kernel.thinking.{perceive,plan_initial,reflect,respond}` = low/high/low/""。
- **[契约] `ImmutableCore.PrependToMessages` 输出稳定层 + 易变层两条 system 消息**：随请求变化的内容不得拼入第一条。
- **[契约] `llm_calls` 表（039）+ `types.WithPurpose`**：每次 Provider 调用落库，新增调用点须标注用途；`ProviderRegistry.Get` 返回原始实例。
- **[阈值] `m4_kernel.model_pool.*`**：日常阶段默认 `default` 池，仅 `plan_replan=reasoning`；新增调用点不得固定走 `reasoning`。新增后台自动任务须有配置开关。

## 2026-09-25（ADR-0020 上下文溢出与执行结果 spill（原 ADR-0100）— 含**契约变更**）

- **[契约] `protocol.ErrContextOverflow`**：请求侧故障，路由不计入熔断/成功率，只 failover 到更大窗口 Provider，否则返回包装该哨兵的错误；新增 Provider 调用点记录健康度一律经 `recordAttempt`。
- **行为变更**：Agent 收到哨兵时对固定前缀外消息首尾保留修剪一次后重试；执行结果 >4KB 经 `ToolRefOffloader` 卸载（`read_tool_ref` 取回）；观察截断改首尾保留（`pkg/util.ElideMiddle`）。

## 2026-09-25（outbox 投影资源压力推迟 — 含**契约变更**）

- **[契约] `protocol.WithDeferrableBackgroundWork`/`ErrBackgroundDeferred`**：串行消费队列的后台 LLM 调用被水位线拒绝时返回推迟哨兵；outbox handler 及下游**须沿错误链上抛**（`apperr.Wrap`/`%w`），不得吞成 nil；Worker 推迟 30s 且不计 attempts（M02 §2.5）。

## 2026-09-25（审查修复：LLM 额度 / 能力令牌绑定 / 后台推理优先级 — 含**契约变更**）

- **[契约] `protocol.WithBackgroundWork`/`IsBackgroundWork`**：可降级后台工作标记；InferenceRouter 以 priority=1 调 `AdmitLLM` 且不刷新用户活跃时间；新增后台 LLM 调用方须标记，否则按用户可见推理对待。
- **[契约] JIT 能力令牌只为已通过 S_VALIDATE 的计划节点签发**（工具名 + 参数字节一致）；不经 S_VALIDATE 的新工具调用来源拿不到令牌。
- **行为变更**：`StreamInfer` 选路前获取 LLM 额度（修复跨池降级使 llmInFlight 变负）；linux 容器可用内存按 working set 计。

## 2026-09-25（ADR-0098 决策十 回合内人工审批 / 决策七追记 — 含**契约变更**）

- **[契约] `types.AgentStreamEventApproval` + `AgentStreamEvent.DeadlineNs`**：回合内阻塞式 HITL 推给对话流（`status{type:"approval_required",...}`）；Agent 发起 HITL 一律经 `promptHITLInTurn`，不得直调 `hitl.Prompt`。
- **[契约] `ExemptionVault.Lookup(agentID, content)`**：每 Agent 多枚令牌、按内容哈希匹配。
- **安全接线**：S_VALIDATE L1_taint 拦截发起 `CheckpointTaintReview`，批准后重新校验（M11 §2.5/§3）。**行为变更**：S_PLAN 空输出重试用 `ThinkingDisabled`；DeepSeek 适配器对显式 `ThinkingDisabled` 发送 `thinking.type=disabled`。

## 2026-09-25（ADR-0109 Embedding 调度（原 ADR-0099）/ ADR-0098 决策五～九 — 含**契约变更**）

- **[契约] `search.Embedder.Embed(ctx, text)`**：调用方 ctx 贯穿下游 HTTP；无上游 ctx 的调用点显式传 `context.Background()` 并注释原因；EmbeddingBatcher High/Low 独立通道（`m1_router.embed.*`）。
- **[契约] `TriggerFillRetry`/`TriggerReflectContinue`**（枚举尾部追加）：空输出自环重试；反思未达成回规划（共用 ReplanGuard）。
- **行为变更**：重规划耗尽转 S_RESPOND 说明失败（`TurnDegraded`），不再进 S_FAILED；`m4_kernel.max_steps` 10→24。
- **安全接线**：执行节点前 JIT 签发一次性能力令牌（M07 §6）；本地令牌先于 IP 鉴权冷却判定；AgentPool 交互预留槽位 `m8.agents.interactive_reserved`。改阶段契约模板须保持 `internal/agent/turn_contract_eval_test.go` 全绿（HE-4）。

## 2026-09-25（ADR-0098 回合输出通道分离与 S_RESPOND — 含**契约变更**）

- **[契约] `protocol.LLMFillEffect.Audience`**：零值 `AudienceInternal`（fail-closed），只有 `AudienceUser`（仅 S_RESPOND）的 token 以 `AgentStreamEventToken` 发布（`state.yaml par_inv_06`）；不得另开第二条用户输出通道。
- **[契约] FSM 第 14 态 `S_RESPOND`**：`AgentStateRespond`/`TriggerRespondReady`/`TriggerRespondDone` 追加在枚举尾部（状态值以 `%d` 落盘，禁中间插入）；S_COMPLETE 只能由 S_RESPOND 进入（`par_inv_07`）；S_REFLECT 失败尽力而为继续进入 S_RESPOND。
- **[契约]** `protocol.AgentController.SetConversationHistory`（`m4_kernel.conversation.history_max_messages/bytes`）；`types.AgentStreamEventPhase`/`TurnPhase`（映射 `status{type:"phase"}`）。
- 阶段契约 SSoT：`configs/prompts/kernel/{perceive,plan,reflect,respond}.md`；Perceive/Reflect 用 `json_object` 约束解码。
