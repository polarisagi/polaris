<!-- 由 make review-merge 生成，勿手改；条目正文逐字取自批次报告 -->
# 审核发现汇总（GR/DR）

## 全局汇总表
| ID | 严重级 | 模块 | 一句话标题 | 置信度 | 可机械化 | 类别 | 来源 |
|---|---|---|---|---|---|---|---|
| GR-10-002 | P0 | internal/eval（层: L3） | ShadowExecutor.scoreShadow 在 llmProvider 为 nil 时降级放行导致未评估候选版本自动确认上线 | 高 | 是（建议规则: AST 检查 scoreShadow 头部 if e.llmProvider == nil 返回 true, nil） | HE 不变量违反（B） | gemini-review-findings-batch10.md |
| GR-10-004 | P0 | internal/channel（层: L3） | EmailAdapter.Send 经 EmailSendMessage 直调 smtp.SendMail 绕过 SafeDialer SSRF 防护 | 高 | 是（建议规则: grep "smtp.SendMail("） | HE 不变量违反（B） | gemini-review-findings-batch10.md |
| GR-6-001 | P0 | internal/vfs（层: L1） | StageEphemeralFile 缺失 filename 路径穿越校验导致任意文件写突破沙箱隔离 | 高 | 是（建议规则: StageEphemeralFile 中传入 filename 必须经过 filepath.Base / resolveWithinRoot 校验，禁止直拼 filepath.Join） | HE 不变量违反（B） | gemini-review-findings-batch6.md |
| GR-7-001 | P0 | internal/knowledge（层: L2） | KnowledgeBase.Search 缺失 TaintMax 校验导致高污染数据穿越安全隔离区 | 高 | 是（建议规则: KnowledgeBase.Search 结果组合前必须校验 chunk.TaintLevel <= req.TaintMax，超出必须过滤） | Taint 传播断点（D） | gemini-review-findings-batch7.md |
| GR-9-002 | P0 | internal/gateway/authcontext（层: L3） | ContextRefExpander.resolveFile 未校验 workDir 隔离边界导致任意文件读取 | 高 | 是（建议规则: 扫描 AST 中 os.ReadFile 前的 filepath 校验逻辑，确保存在 strings.HasPrefix(abs, cleanWorkDir) 越界断言） | HE 不变量违反（B） | gemini-review-findings-batch9.md |
| GR-1-001 | P1 | internal/store（层: L0） | MutationBus 单写者在超时与版本冲突路径上直接向 ResultCh 写通道引发永久死锁 | 高 | 是（建议规则: ast 检查 dw.ResultCh <- expr 未包裹在 select-default 块中） | 并发与资源（C） | gemini-review-findings-batch1.md |
| GR-1-002 | P1 | internal/store（层: L0） | OutboxWorker 处理毒丸记录时误返回 nil 导致死字记录被更新为 done 状态 | 高 | 是（建议规则: 检查 CrashRecoveryCount >= 3 分支返回值是否为非 nil 错误） | 幂等重放与一致性（M） | gemini-review-findings-batch1.md |
| GR-10-001 | P1 | internal/automation（层: L3） | SQLiteScheduler.scanAndDispatch 零过滤 running 状态导致长耗时任务重复并发调度 | 高 | 是（建议规则: grep "st.Status == \"completed\" || st.Status == \"failed\"" 检查包含 running 状态过滤） | 幂等重放与一致性（M） | gemini-review-findings-batch10.md |
| GR-11-001 | P1 | rust/substrate（层: rust） | surreal_store FFI 导出函数缺乏 NULL 指针前置校验导致潜在空指针解引用未定义行为 | 高 | 是（建议规则: grep -rn 'CStr::from_ptr(' rust/substrate/src/ | grep -v 'is_null()'） | HE 不变量违反（B） | gemini-review-findings-batch11.md |
| GR-2-001 | P1 | internal/llm（层: L0） | circuitBreaker 在 HalfOpen 探测失败时未恢复到 circuitOpen 状态，导致熔断器探活失败后卡死在全量放行状态 | 高 | 否 | LLM/Agent 生产陷阱（H） | gemini-review-findings-batch2.md |
| GR-3-001 | P1 | internal/bootstrap（层: 契约） | 04-Module-Boundary.md 声明 internal/bootstrap 被 cmd/polaris 引用与代码/架构文档矛盾 | 高 | 是（建议规则: 检查规范文档中模块依赖声明与 cmd/polaris/ 实际 import 导入集的求差校验） | docs↔code 漂移（A/K） | gemini-review-findings-batch3.md |
| GR-3-002 | P1 | internal/bootstrap（层: 契约） | Bootstrapper 四阶关停按 map 随机序遍历破坏逆序依赖优雅关停 | 高 | 是（建议规则: AST 检查 Bootstrapper 关停 Phase 循环中对 b.modules map 的直接 range 遍历） | 生命周期与关停（L） | gemini-review-findings-batch3.md |
| GR-4-001 | P1 | internal/action（层: L1） | CodeAct Python Stateful 模式在 Sbx-L3 容器内打开宿主快照路径因未捕获 `FileNotFoundError` 导致脚本退出报错 | 高 | 是（建议规则: grep `"with open(__CA_STATE_FILE__, \"wb\")"` 检索并在 AST/正则上校验是否位于 try-except 外部） | LLM/Agent 生产陷阱（H） | gemini-review-findings-batch4.md |
| GR-4-002 | P1 | internal/agent（层: L1） | BuildReflectContext 组装反思 Prompt 时硬编码 ExecuteResult 污点等级为 TaintMedium 导致高污点标记静默丢失 | 高 | 是（建议规则: grep `WriteUserData\(taint\.NewTaintedString\(.*ExecuteResult.*TaintMedium` 断言此处错误硬编码） | Taint 传播断点（D） | gemini-review-findings-batch4.md |
| GR-5-004 | P1 | internal/tool（层: L1） | run_command 与 bash 内置工具对 RiskHITL 风险指令静默放行，绕过 HITL 审批 | 高 | 是（建议规则: AST check for switch cases on classifier.RiskHITL missing HITL prompt or return） | 错误处理与边界（E） | gemini-review-findings-batch5.md |
| GR-6-002 | P1 | internal/execute/orchestrator（层: L1） | StateGraphExecutor 无条件多节点循环边被误入 AND-Join 硬依赖集合导致状态图死锁 | 高 | 是（建议规则: 状态图构建 requiredPreds 时须剔除可达环路中的回边，不能仅过滤自环 From != To） | 并发与资源（C） | gemini-review-findings-batch6.md |
| GR-7-002 | P1 | internal/learning（层: L2） | Engine.Start 对 taskEvents/versionEvents 通道关闭误用 return nil 导致后台自进化引擎提前异常关停 | 高 | 是（建议规则: Engine.Start 中 select-case 读取通道时，!ok 应对通道置 nil 继续循环，禁止直接 return nil） | 生命周期与关停（L） | gemini-review-findings-batch7.md |
| GR-8-001 | P1 | internal/extension/marketplace（层: L2） | NewMCPMarketplaceClient 中的 SafeHTTPClient 校验缺乏 nil 空值防护导致 panic 崩溃 | 高 | 是（建议规则: `grep -n "!.*\.IsSafe()" internal/extension/marketplace/marketplace.go`） | 错误处理与边界（E） | gemini-review-findings-batch8.md |
| GR-8-002 | P1 | internal/extension/skill（层: L2） | skill_creator 与 plugin_creator 的 extractJSON 贪婪正则 (?s)\{.*\} 导致多括号 LLM 响应解析失败 | 高 | 是（建议规则: `grep -n "regexp.MustCompile.*(?s)" internal/extension/`） | LLM/Agent 生产陷阱（H） | gemini-review-findings-batch8.md |
| GR-9-001 | P1 | internal/gateway/server（层: L3） | handleAgentInterrupt 未将 WebUI 默认客户端类型纳入 whitelist 导致中断请求遭 403 拒绝 | 高 | 是（建议规则: 检查 handleAgentInterrupt 中 authCtx.ClientType 的判断，比对 middleware_auth.go 注入的 WebUI 客户端类型 webui） | HE 不变量违反（B） | gemini-review-findings-batch9.md |
| GR-1-003 | P2 | internal/observability（层: L0） | TierParameters 仍保留已废弃的 GraphRAGLLMDailyBudget 字段而缺失 GraphRAGConcurrentWorkers | 高 | 是（建议规则: AST 检查 probe.TierParameters 结构体中是否存在废弃字段 GraphRAGLLMDailyBudget） | LLM/Agent 生产陷阱（H） | gemini-review-findings-batch1.md |
| GR-1-004 | P2 | internal/downloader（层: L0） | downloadResume 多源降级时误把不支持 Range 的截断 partial 文件尺寸当作后续 Candidate 偏移量导致数据损坏 | 高 | 是（建议规则: 检查 downloadChunk 中 os.O_TRUNC 标志设置时是否清空 offset 或删除旧 part 文件） | 其他 | gemini-review-findings-batch1.md |
| GR-10-003 | P2 | internal/cli（层: L3） | internal/cli 模块包含 AgentREPL 等导出类型但零生产接线且未列入白名单 | 高 | 是（建议规则: 导出符号生产调用方可达性扫描，测试调用不计） | 接线断裂（G-bis） | gemini-review-findings-batch10.md |
| GR-10-005 | P2 | internal/automation（层: L3） | IdleEvolutionScheduler 在后台任务完成时未清理 cancelFuncs 导致后续空闲周期永久阻断 | 高 | 否 | 其他 | gemini-review-findings-batch10.md |
| GR-12-001 | P2 | docs/arch（层: L0） | M02 §16 DDL 全量文件数与末尾编号与 schema 现状漂移 | 高 | 是 (建议规则: grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限) | Schema/配置漂移（F） | gemini-review-findings-batch12.md |
| GR-12-002 | P2 | docs/arch（层: L1） | M04 §1 状态枚举声明列表缺漏且权威源文件名标注错误 | 高 | 是 (建议规则: grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件) | docs↔code 漂移（A/K） | gemini-review-findings-batch12.md |
| GR-12-003 | P2 | docs/arch（层: 契约） | INDEX §1 场景加载预算中多份核心文档 est_tok 估算严重低估 | 高 | 是 (建议规则: 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移) | docs↔code 漂移（A/K） | gemini-review-findings-batch12.md |
| GR-12-004 | P2 | docs/specs（层: 契约） | 宪法 R2.5 错误码字典缺失 CodeStorageUnavailable 权威定义 | 高 | 是 (建议规则: grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集) | docs↔code 漂移（A/K） | gemini-review-findings-batch12.md |
| GR-12-005 | P2 | docs/arch（层: L0） | 全局字典 §12 追溯表中各模块 DDL 文件范围陈旧 | 高 | 是 (建议规则: grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围) | Schema/配置漂移（F） | gemini-review-findings-batch12.md |
| GR-12-006 | P2 | docs/arch（层: L0） | M02 §2 tasks 表结构列说明遗漏 spawn_depth 字段 | 高 | 是 (建议规则: 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集) | docs↔code 漂移（A/K） | gemini-review-findings-batch12.md |
| GR-12-007 | P2 | docs/arch（层: L3） | M13-bis §2.2 表格中 origin 枚举值与 DDL/类型定义自相矛盾 | 高 | 是 (建议规则: grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性) | Schema/配置漂移（F） | gemini-review-findings-batch12.md |
| GR-2-002 | P2 | internal/security（层: L0） | SafeDialer.dnsCache 缺乏容量上限与淘汰机制，长时运行下存在无界内存泄露风险 | 高 | 否 | LLM/Agent 生产陷阱（H） | gemini-review-findings-batch2.md |
| GR-2-003 | P2 | internal/security（层: L0） | security/provider.go 声明的 AuditRepo / KillSwitchMetrics / GuardProvider 接口全仓零消费方 | 高 | 是（建议规则: grep -rn "security\.AuditRepo" internal/ 且测试不计入） | 接线断裂（G-bis） | gemini-review-findings-batch2.md |
| GR-3-003 | P2 | internal/bootstrap（层: 契约） | Bootstrapper.Ignite 模块初始化中途失败缺乏已初始化模块的回滚清理 | 高 | 是（建议规则: AST 检查 Bootstrapper.Ignite 循环中 mod.Init 失败处理缺失逆序 Cleanup 逻辑） | 其他 | gemini-review-findings-batch3.md |
| GR-5-001 | P2 | internal/memory（层: L1） | MemImpl.InjectRelevantMemory 接口与实现未接线，零生产调用方 | 高 | 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计） | 接线断裂（G-bis） | gemini-review-findings-batch5.md |
| GR-5-002 | P2 | internal/memory（层: L1） | MemImpl.ConfigureWorkingMemBudget 与 InjectSkillRegistry 未接线 | 高 | 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计） | 接线断裂（G-bis） | gemini-review-findings-batch5.md |
| GR-5-003 | P2 | internal/memory（层: L1） | MemorySystemImpl/MemoryFacadeImpl 的 Forget 方法无生产调用方 | 高 | 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计） | 接线断裂（G-bis） | gemini-review-findings-batch5.md |
| GR-5-005 | P2 | internal/tool（层: L1） | CompositeCatalog.CleanupSession 未在会话终态接入，存在 activeSessions 内存泄漏风险 | 高 | 是（建议规则: 导出函数生产调用方可达性扫描，测试调用不计） | 接线断裂（G-bis） | gemini-review-findings-batch5.md |
| GR-6-003 | P2 | internal/vfs（层: L1） | provider.go 存留 2 行无符号声明空文件 | 高 | 是（建议规则: 仅含 package 声明且无任何导出的源文件应清除） | 其他 | gemini-review-findings-batch6.md |
| GR-7-003 | P2 | internal/swarm（层: L2） | PlannerPool.workerEngineA 派生超时 context 后未校验 parent context 导致取消信号下仍然运行高消耗沙箱构建 | 高 | 是（建议规则: 在 context.WithTimeout 派生后执行沙箱/子进程操作前必须检查 parent ctx.Err()） | HE 不变量违反（B） | gemini-review-findings-batch7.md |
| GR-8-003 | P2 | internal/extension/skill（层: L2） | ScriptSkillExecutor 限流触发时错误码误用 CodeInternal 替代 CodeResourceExhausted | 高 | 是（建议规则: `grep -n "rate limit exceeded" internal/extension/skill/skill_executor.go`） | 其他 | gemini-review-findings-batch8.md |
| GR-8-004 | P2 | internal/extension/mcp（层: L2） | makeMCPToolAsyncFn 异步变体工具返回结果未标注 TaintLevel 退化为 TaintNone | 高 | 是（建议规则: `grep -n "makeMCPToolAsyncFn" internal/extension/mcp/mcp_manager_tools.go`） | Taint 传播断点（D） | gemini-review-findings-batch8.md |
| GR-8-005 | P2 | internal/extension/mcp（层: L2） | stdio readLoop 的 bufio.Scanner 硬编码 1MB 缓冲区上限导致大载荷 MCP 响应断连 | 高 | 是（建议规则: `grep -n "scanner.Buffer" internal/extension/mcp/mcp_client_stdio.go`） | 其他 | gemini-review-findings-batch8.md |
| GR-9-003 | P2 | internal/gateway/server（层: L3） | HandleTogglePluginMCP 路由与 handler 路径参数不匹配导致永远 400 | 高 | 是（建议规则: 提取 mux.HandleFunc 模式中的路径参数与 Handler 中 r.PathValue 调用的 key 集合，校验子集包含关系） | HE 不变量违反（B） | gemini-review-findings-batch9.md |

## 类别收敛统计
> 机械统计（映射梯见 tools/review_merge.go categoryLadder）。判读规则：某类别连续两轮不降 → 该类门控缺失，优先落地对应 lint 规则，而不是下一轮更用力地扫它。

| 缺陷类别 | 本轮 | 上一轮 | 趋势 |
|---|---|---|---|
| 接线断裂（G-bis） | 6 | — | ↑ |
| 注释漂移（G） | 0 | — | — |
| Taint 传播断点（D） | 3 | — | ↑ |
| 幂等重放与一致性（M） | 2 | — | ↑ |
| 生命周期与关停（L） | 2 | — | ↑ |
| 并发与资源（C） | 2 | — | ↑ |
| 错误处理与边界（E） | 2 | — | ↑ |
| Schema/配置漂移（F） | 3 | — | ↑ |
| docs↔code 漂移（A/K） | 5 | — | ↑ |
| LLM/Agent 生产陷阱（H） | 5 | — | ↑ |
| HE 不变量违反（B） | 8 | — | ↑ |
| 其他 | 6 | — | ↑ |

## 疑似同根因（同文件同行号，人工判定是否合并）
- 无

## 详细发现清单

### [GR-10-002] ShadowExecutor.scoreShadow 在 llmProvider 为 nil 时降级放行导致未评估候选版本自动确认上线
- 严重级: P0
- 模块: internal/eval（层: L3）
- 位置: internal/eval/analysis/shadow_executor.go:281
- 违反规则: HE-2 | HE-7
- 置信度: 高
- 可机械化: 是（建议规则: AST 检查 scoreShadow 头部 if e.llmProvider == nil 返回 true, nil）
- 反证: 已查 cmd/polaris/boot_*.go 与 internal/bootstrap/，ShadowExecutor 的构造函数 NewShadowExecutor 允许 provider 传入 nil。在 RunReplayBatch 流程中，processSingleSample 调用 scoreShadow 评判样本。当 e.llmProvider 为 nil 时，scoreShadow 直接 return true, nil，导致所有样本均判定为 Passed (passRate = 1.0)。该通过率 100% 超过 ShadowPassRateThreshold 阈值，进而直接触发 e.staging.ConfirmShadow(ctx, candidateVersion)，将未经任何 LLM Judge 实际评估的 Staging 候选版本上线，违反了 fail-closed 安全门法则。
- 问题: ShadowExecutor 的对比评判函数 scoreShadow 在 e.llmProvider 为 nil 时，硬编码返回了 `return true, nil` 降级放行。当系统未注入 LLM 评判 Provider 时，所有的影子回放样本都会被误判为通过，使得整体 pass_rate 达到 100%，自动触发 ConfirmShadow 确认将候选版本上线，构成了严重的 nil 旁路放行漏洞。
- 证据: internal/eval/analysis/shadow_executor.go:280-283
  ```go
  func (e *ShadowExecutor) scoreShadow(ctx context.Context, req *types.InferRequest, baseline *types.InferResponse, shadow *types.ProviderResponse) (bool, error) {
  	if e.llmProvider == nil {
  		return true, nil // 降级放行
  	}
  ```
- 修复方向提示: 当 e.llmProvider == nil 时应按 fail-closed 原则返回 false 或返回 error，不得默认放行通过。

### [GR-10-004] EmailAdapter.Send 经 EmailSendMessage 直调 smtp.SendMail 绕过 SafeDialer SSRF 防护
- 严重级: P0
- 模块: internal/channel（层: L3）
- 位置: internal/channel/adapter/email.go:105
- 违反规则: HE-7 | R1.13
- 置信度: 高
- 可机械化: 是（建议规则: grep "smtp.SendMail("）
- 反证: 已查外部入口到该行的完整调用链：`EmailAdapter.Send` (email.go:284) -> `EmailSendMessage` (email.go:95) -> `smtp.SendMail` (email.go:105)。已查 cmd/polaris/boot_*.go 及 internal/bootstrap/，`EmailSendMessage` 被 `EmailAdapter.Send` 直接调用。Go 标准库 `smtp.SendMail` 底层使用裸 `net.Dial("tcp", addr)` 发起 TCP 连接，完全没有传入或使用 `host.SafeDialer()`。当 channel 配置中的 `smtp_host` 被设置为内网 IP 或云厂商元数据服务地址（如 169.254.169.254）时，将绕过 SafeDialer 产生内网探测与 SSRF 风险。
- 问题: `EmailAdapter.Send` 在发送邮件时调用了 `EmailSendMessage`，而 `EmailSendMessage` 直接使用 Go 标准库的 `smtp.SendMail` 发起 SMTP 连接。`smtp.SendMail` 底层使用裸 `net.Dial`，未接入 `SafeDialer` SSRF 防护机制，违反了 XR-06 和 HE-7 中关于所有出站网络连接必须经 `SafeDialer` 物理过滤的要求。
- 证据: internal/channel/adapter/email.go:105-107
  ```go
  	if err := smtp.SendMail(smtpHost+":"+smtpPort, auth, address, []string{to}, msg); err != nil {
  		return apperr.Wrap(apperr.CodeNetworkUnavailable, "email: SendMail 失败", err)
  	}
  ```
- 修复方向提示: 使用基于 `host.SafeDialer().DialContext` 的自定义 SMTP Client 代替标准库 `smtp.SendMail`。

### [GR-6-001] StageEphemeralFile 缺失 filename 路径穿越校验导致任意文件写突破沙箱隔离
- 严重级: P0
- 模块: internal/vfs（层: L1）
- 位置: internal/vfs/workspace_manager_ephemeral.go:53
- 违反规则: HE-2 | HE-7
- 置信度: 高
- 可机械化: 是（建议规则: StageEphemeralFile 中传入 filename 必须经过 filepath.Base / resolveWithinRoot 校验，禁止直拼 filepath.Join）
- 反证: 已查 internal/action/codeact/code_act_script_staging.go:55/74、internal/swarm/planner/pool.go:162 及 workspace_manager_ephemeral.go:46-70 四处。code_act_script_staging.go:55 注释显式声明 "已在 vfs.WorkspaceManager.StageEphemeralFile 内部做路径穿越净化"，但 StageEphemeralFile 仅对 namespace 执行 filepath.Base 净化，对 filename 参数未经 filepath.Base 或 resolveWithinRoot 校验即直拼 filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename) 并在 L68 执行 os.WriteFile(full, data, 0700)。攻击者或 untrusted LLM 传入包含 ../.. 的 filename 可在宿主文件系统任意可写路径写入 0700 可执行文件，且该文件无法被 SweepEphemeralOrphans 巡检归还，构成物理隔离绕过。
- 问题: StageEphemeralFile 针对 namespace 做了 filepath.Base(filepath.Clean(namespace)) 过滤，但对 filename 未做任何路径穿越判定或 resolveWithinRoot 校验。直接拼接到 filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename) 会被 .. 相对路径解析击穿，允许在 wm.rootDir 甚至 ephemeralScriptsSubdir 之外的宿主任意目录创建可执行文件（0700）。上游 code_act_script_staging.go 误以为 VFS 内部已做防穿越而直接透传 filename，形成严重安全漏洞。
- 证据: 关键代码摘录（internal/vfs/workspace_manager_ephemeral.go:53）
  ```go
  func (wm *WorkspaceManager) StageEphemeralFile(namespace, filename string, data []byte) (absPath string, cleanup func(), err error) {
  	safeNS := filepath.Base(filepath.Clean(namespace))
  	...
  	full := filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename)
  ```
- 修复方向提示: 对 filename 统一应用 filepath.Base(filepath.Clean(filename)) 净化，或通过 resolveWithinRoot 进行范围边界校验。

### [GR-7-001] KnowledgeBase.Search 缺失 TaintMax 校验导致高污染数据穿越安全隔离区
- 严重级: P0
- 模块: internal/knowledge（层: L2）
- 位置: internal/knowledge/rag_retrieval.go:340
- 违反规则: HE-2 | HE-7
- 置信度: 高
- 可机械化: 是（建议规则: KnowledgeBase.Search 结果组合前必须校验 chunk.TaintLevel <= req.TaintMax，超出必须过滤）
- 反证: 已查 cmd/polaris/boot_knowledge.go、internal/knowledge/rag_retrieval.go:311-370、DefaultHybridRetriever.Search(rag_retrieval.go:76)、HybridRetrieverImpl.Search(retriever.go:103) 及 ContextExpander.Expand(rag_retrieval.go:167) 四处。KnowledgeBaseSearchRequest 明确定义了 TaintMax 门控字段，但在 KnowledgeBase.Search 中，从 kb.retriever.Search 返回的 chunks 未经任何 TaintLevel <= req.TaintMax 的校验即直接 append 入 allChunks 并由 ContextExpander 扩展返回。DefaultHybridRetriever/HybridRetrieverImpl 的 RetrievalConfig 均不包含 TaintMax 过滤逻辑。全查 boot_knowledge.go 确认 KnowledgeBase.Search 为外部唯一检索入口，TaintMax 过滤在整个调用链中物理缺失，导致高污染数据可绕过安全门控渗入 Prompt。
- 问题: KnowledgeBaseSearchRequest 在接口契约中定义了 TaintMax 字段作为控制最大允许污点级别的安全门控，但在 KnowledgeBase.Search 的实现中，检索得到的 chunks 未经 TaintLevel <= req.TaintMax 的断言与过滤直接汇总并送入 ContextExpander 扩展。攻击者注入的高污点（TaintHigh/TaintCritical）外部知识块可绕过 TaintMax 门控直接渗入 Prompt。
- 证据: internal/knowledge/rag_retrieval.go:322-331
  ```go
				chunk := Chunk{
					ID:          c.Source,
					DocID:       c.Source,
					Content:     c.Content,
					TaintLevel:  int(c.TaintLevel),
					TaintSource: c.Metadata["taint_source"],
					SourceURI:   c.Source,
				}
				allChunks = append(allChunks, chunk)
  ```
- 修复方向提示: 在 KnowledgeBase.Search 遍历 chunks 时，追加 if req.TaintMax > 0 && int(c.TaintLevel) > req.TaintMax { continue } 条件过滤。

### [GR-9-002] ContextRefExpander.resolveFile 未校验 workDir 隔离边界导致任意文件读取
- 严重级: P0
- 模块: internal/gateway/authcontext（层: L3）
- 位置: internal/gateway/authcontext/contextref.go:239-251
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 扫描 AST 中 os.ReadFile 前的 filepath 校验逻辑，确保存在 strings.HasPrefix(abs, cleanWorkDir) 越界断言）
- 反证: 已核对 server_lifecycle.go:160（通过 WithWorkDir(s.dataDir) 初始化 ContextRefExpander）与 authcontext/contextref.go:239-251。当用户输入中包含 `@file:"/etc/passwd"` 或带 `..` 相对路径（如 `@file:"../../../../etc/passwd"`）时，resolveFile 的 `!filepath.IsAbs(path)` 判断直接跳过 workDir 拼接；后续仅使用 `isSensitivePath(abs)` 检查黑名单（仅拦截 `.ssh` / `.aws` 等硬编码路径），完全没有检查 `abs` 是否位于 `e.workDir` 目录之内。攻击者或恶意 prompt 只要构造绝对路径，即可绕过工作区限制读取宿主机任意文件（在 1MB 上限内）并拼接入 LLM 上下文。
- 问题: `ContextRefExpander.resolveFile` 负责解析用户消息中的 `@file` 标签并读取文件内容追加到对话上下文。然而在解析绝对路径或包含 `..` 的路径时，未校验解析出的绝对路径 `abs` 是否属于授权的 `workDir` 根目录。`isSensitivePath` 仅维护了一个有限的黑名单（如 `.ssh`, `.aws`），未包含 `/etc/passwd`、系统配置文件、Polaris 数据库等大量敏感目标。这使得攻击者可以通过发送包含 `@file` 绝对路径的消息，在未授权情况下读取服务器任意文件。
- 证据: 关键代码摘录如下
  ```go
  // authcontext/contextref.go:239-251
  if !filepath.IsAbs(path) && e.workDir != "" {
      path = filepath.Join(e.workDir, path)
  }

  abs, err := filepath.Abs(path)
  if err != nil {
      return "", 0, apperr.Wrap(apperr.CodeInternal, "resolve path", err)
  }
  if isSensitivePath(abs) {
      return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("blocked: sensitive path %q", abs))
  }

  data, err := os.ReadFile(abs)
  ```
- 修复方向提示: 在 `resolveFile` 中校验 `e.workDir` 不为空时，要求 `abs` 必须以 `filepath.Clean(e.workDir) + string(filepath.Separator)` 为前缀，阻止越界读取 `workDir` 之外的文件。

### [GR-1-001] MutationBus 单写者在超时与版本冲突路径上直接向 ResultCh 写通道引发永久死锁
- 严重级: P1
- 模块: internal/store（层: L0）
- 位置: internal/store/mutation_bus_execute.go:177, internal/store/mutation_bus_execute.go:258, internal/store/mutation_bus.go:264, internal/store/mutation_bus.go:280
- 违反规则: HE-6 | R1.16
- 置信度: 高
- 可机械化: 是（建议规则: ast 检查 dw.ResultCh <- expr 未包裹在 select-default 块中）
- 反证: 已查 cmd/polaris/boot_server.go、internal/bootstrap/ 拓扑及 DatabaseWriter 全量调用链。AppendEvent/AppendDecision 均在与 ctx.Done() 的 select 中等待 ResultCh。若调用 context 先超时退出，ResultCh 将无人接收。flushBatch/drainCh/drainPriorityCh 统一执行 r.intent.ResultCh <- r.err，无 select/default 防护，将在 0 容量 channel 写入时永久卡死 DatabaseWriter 单写者协程，导致全局 events 与 decision_log 无法写入。已与 failAll (355行) 的 select-default 结构对比确认差异。
- 问题: DatabaseWriter 在处理批次提交成功 (line 258)、Lease 校验失败 (line 177) 以及 priorityCh/ch 排空 (lines 264, 280) 路径上，直接使用无缓冲 channel 发送操作 `r.intent.ResultCh <- r.err`。当 Submit 调用方（如 AppendEvent/AppendDecision）传入 0 容量 channel 且由于 context 超时/取消而提前退出 select 接收块时，DatabaseWriter 作为全局唯一的 SQLite 写入协程将被永久阻塞在 channel 发送操作上，卡死后续所有 events/decision_log/tasks 等写入操作。
- 证据: internal/store/mutation_bus_execute.go:257-260
  ```go
  for _, r := range results {
  	if r.intent.ResultCh != nil {
  		r.intent.ResultCh <- r.err
  	}
  }
  ```
- 修复方向提示: 在 ResultCh 发送点使用 `select { case r.intent.ResultCh <- r.err: default: }` 结构（对齐 failAll 方法中的安全写法），防止因调用方超时退场卡死单写者协程。

### [GR-1-002] OutboxWorker 处理毒丸记录时误返回 nil 导致死字记录被更新为 done 状态
- 严重级: P1
- 模块: internal/store（层: L0）
- 位置: internal/store/outbox_worker.go:362
- 违反规则: HE-6
- 置信度: 高
- 可机械化: 是（建议规则: 检查 CrashRecoveryCount >= 3 分支返回值是否为非 nil 错误）
- 反证: 已查 cmd/polaris/boot_server.go、internal/bootstrap/、002_outbox.sql 及 spec/state.yaml §outbox 状态机约束。state.yaml outbox_inv_03 明确规定 crash_recovery_count >= 3 时必须直接转移至 dead 状态。OutboxWorker.Process() 在 lines 362-366 检测到 record.CrashRecoveryCount >= 3 时返回 nil，上层 processAndMark (line 264) 接收到 err==nil 后误以为处理成功，执行 UPDATE outbox SET status='done'，导致原本崩溃 3 次的毒丸记录在数据库中被错误标为 done 而非 dead。
- 问题: OutboxWorker.Process 在检测到 CrashRecoveryCount >= 3 (毒丸记录) 时，虽然记录了 dead_letter 指标与日志，但函数末尾直接返回了 nil。上层 processAndMark 方法在 err == nil 时会将 SQL 状态更新为 'done' (`UPDATE outbox SET status='done'`)。这导致达到最大崩溃重试次数的异常 Outbox 任务在数据库台账中被伪造为"成功完成"，破坏了 state.yaml §outbox outbox_inv_03 ("crash_recovery_count >= 3 -> directly to dead") 不变量。
- 证据: internal/store/outbox_worker.go:362-366
  ```go
  if record.CrashRecoveryCount >= 3 {
  	metrics.GlobalOutboxDeadLetterTotal.Add(1)
  	slog.Error("outbox message dead (poison pill)", "id", record.ID, "target", record.TargetEngine)
  	return nil
  }
  ```
- 修复方向提示: 在 Process 中检测到毒丸记录时返回 ErrUnknownTargetEngine 或专用 ErrPoisonPill 哨兵错误，使 processAndMark 正确更新 status='dead'。

### [GR-10-001] SQLiteScheduler.scanAndDispatch 零过滤 running 状态导致长耗时任务重复并发调度
- 严重级: P1
- 模块: internal/automation（层: L3）
- 位置: internal/automation/queue.go:168
- 违反规则: HE-6 | 维度M-幂等重放
- 置信度: 高
- 可机械化: 是（建议规则: grep "st.Status == \"completed\" || st.Status == \"failed\"" 检查包含 running 状态过滤）
- 反证: 已查 cmd/polaris/boot_*.go 与 internal/bootstrap/，SQLiteScheduler 由 boot_agent.go 实例化并由 Start() 拉起 5s 周期 scanAndDispatch。在 queue.go:168 循环中仅跳过 completed 与 failed 状态，未检查或过滤 running 状态，亦无内存中 in-flight task_id 避重集合。运行耗时超过 5s 的任务在下一次 5s tick 中将被再次读出、Attempts 自增并再次由 SafeGo 发起并发调度，直到 Attempts 达到 MaxAttempts 被误判为 failed。
- 问题: SQLiteScheduler 在 5 秒周期扫描 pending 任务时，状态过滤条件仅写为 `if st.Status == "completed" || st.Status == "failed" { continue }`，未能跳过已经处于 `running` 状态的任务。当某个后台任务执行时间超过 5 秒时，下一次扫描仍会将该任务读出，导致 `Attempts` 计数虚高累加，并并发触发多次 `dispatchFn` 重复调度，破坏任务执行幂等性，最终在 3 次扫描后提前将仍在执行中的任务强制标记为 `failed`。
- 证据: internal/automation/queue.go:168-170
  ```go
  // 跳过已完成 / 已超出重试次数
  if st.Status == "completed" || st.Status == "failed" {
  	continue
  }
  ```
- 修复方向提示: 在 scanAndDispatch 过滤逻辑中增加 `st.Status == "running"` 判断，或在内存中维护当前正在执行中的 task_id 集合。

### [GR-11-001] surreal_store FFI 导出函数缺乏 NULL 指针前置校验导致潜在空指针解引用未定义行为
- 严重级: P1
- 模块: rust/substrate（层: rust）
- 位置: rust/substrate/src/surreal_store/fts.rs:25
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: grep -rn 'CStr::from_ptr(' rust/substrate/src/ | grep -v 'is_null()'）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/ffi/ 与 internal/store/repo/。Purego FFI 在 Go 侧传递空/nil 字符串时，unsafe.StringData("") 或 unsafe.Pointer(nil) 会传入 NULL (0x0) 指针。surreal_fts_delete (fts.rs:67) 与 surreal_fts_search (fts.rs:116) 已显式加入 if doc_id.is_null() 前置检查并附注 // 入参 null 判断在 catch_unwind 外前置检查，避免 null 解引用 UB（GR-11-001），但 surreal_fts_index (fts.rs:25)、surreal_vec_upsert (vector.rs:28) 与 surreal_vec_delete (vector.rs:69) 漏掉 null 校验，直接在 catch_unwind 闭包内调用 CStr::from_ptr(doc_id) / CStr::from_ptr(id)。当 Go 侧传入 nil/NULL 指针时会触发空指针解引用未定义行为（UB）与段错误崩溃。
- 问题: surreal_fts_index、surreal_vec_upsert 与 surreal_vec_delete 导出函数在跨 FFI 边界解引用 C 字符串指针前未校验指针非空，违反 HE-2 物理防线与可验证执行原则中 FFI 边界安全要求。
- 证据: 如下 Rust 代码所示：
  ```rust
  // rust/substrate/src/surreal_store/fts.rs:25
  pub unsafe extern "C" fn surreal_fts_index(doc_id: *const c_char, text: *const c_char) -> c_int {
      let result = panic::catch_unwind(move || {
          let id = match unsafe { CStr::from_ptr(doc_id) }.to_str() {
  ```
- 修复方向提示: 在 catch_unwind 外部/顶部统一添加 if doc_id.is_null() || text.is_null() { return SURREAL_ERR_UTF8; } 等 NULL 指针防护。

### [GR-2-001] circuitBreaker 在 HalfOpen 探测失败时未恢复到 circuitOpen 状态，导致熔断器探活失败后卡死在全量放行状态
- 严重级: P1
- 模块: internal/llm（层: L0）
- 位置: internal/llm/circuit_breaker.go:70
- 违反规则: A-10
- 置信度: 高
- 可机械化: 否
- 反证: 已查 internal/llm/circuit_breaker.go (45-77 行) 与 provider_registry.go (180-220 行)，circuitBreaker 的 RecordFailure 为唯一的失败记录入口。当 state 为 circuitHalfOpen 时，RecordFailure() 调用 cb.failures.Add(1)，但 n >= maxFailures (默认为 5) 校验失败，导致 state 保持在 circuitHalfOpen，未重新置为 circuitOpen 且未更新 openUntil 冷却到期时间。已查 cmd/polaris/ 与 internal/llm/ 均无其他机制修正 HalfOpen 状态下的失败探测。
- 问题: circuitBreaker.RecordFailure() 在熔断器处于 circuitHalfOpen 探测状态下如果探活请求失败，未立即重新切回 circuitOpen 并刷新 openUntil 冷却期，而是仅递增 failures (从 0 变 1)。因为 1 < maxFailures (5)，state 保持在 circuitHalfOpen。这导致 Allow() 对后续所有并发请求均在 case circuitHalfOpen: 分支直接返回 true，使熔断器在探活失败后反而陷入放行全量请求的卡死状态，违反了 docs/specs/09-LLM-Agent-Production.md A-10 的熔断器状态机防护规则。
- 证据: internal/llm/circuit_breaker.go:70-76
  ```go
  func (cb *circuitBreaker) RecordFailure() {
  	n := cb.failures.Add(1)
  	if n >= cb.maxFailures {
  		cb.state.Store(int32(circuitOpen))
  		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
  		cb.failures.Store(0)
  	}
  }
  ```
- 修复方向提示: 在 RecordFailure() 中判断若当前状态为 circuitHalfOpen，无条件重置 state 为 circuitOpen 并更新 openUntil 冷却到期时间。

### [GR-3-001] 04-Module-Boundary.md 声明 internal/bootstrap 被 cmd/polaris 引用与代码/架构文档矛盾
- 严重级: P1
- 模块: internal/bootstrap（层: 契约）
- 位置: docs/specs/04-Module-Boundary.md:46
- 违反规则: A
- 置信度: 高
- 可机械化: 是（建议规则: 检查规范文档中模块依赖声明与 cmd/polaris/ 实际 import 导入集的求差校验）
- 反证: 已核对 cmd/polaris/boot_*.go、cmd/polaris/main.go、internal/bootstrap/ 及全仓包引用，确认零生产/测试代码 import internal/bootstrap。ARCHITECTURE.md §8.2、ADR-0081 及 ADR-0088 均明确登记 internal/bootstrap 处于未接线状态，04-Module-Boundary.md:46 属于过期失真陈述。
- 问题: docs/specs/04-Module-Boundary.md:46 标注 internal/bootstrap/ 为「仅被 cmd/polaris/ 引用」，与代码现状（cmd/polaris/ 下 0 处 import internal/bootstrap）及权威架构文档 ARCHITECTURE.md §8.2 / ADR-0081 / ADR-0088 记录的「生产启动走 cmd/polaris/boot_*.go 手工装配链，internal/bootstrap 零处 import」存在直接矛盾。这会导致开发与评审人员误以为 cmd/polaris 在使用 Bootstrapper。
- 证据: docs/specs/04-Module-Boundary.md:46
  ```text
  > **注意**：`internal/bootstrap/` 为跨层初始化编排器（Bootable + DependencyMap + Kahn 拓扑排序，四阶优雅关停）。仅被 `cmd/polaris/` 引用，不属于 L0~L3 业务层
  ```
- 修复方向提示: 修改 docs/specs/04-Module-Boundary.md:46 的描述，更正为「已定义未接线的自动编排契约（参见 ARCHITECTURE.md §8.2 / ADR-0088），生产装配物理落点为 cmd/polaris/boot_*.go」。

### [GR-3-002] Bootstrapper 四阶关停按 map 随机序遍历破坏逆序依赖优雅关停
- 严重级: P1
- 模块: internal/bootstrap（层: 契约）
- 位置: internal/bootstrap/bootstrapper.go:134
- 违反规则: L
- 置信度: 高
- 可机械化: 是（建议规则: AST 检查 Bootstrapper 关停 Phase 循环中对 b.modules map 的直接 range 遍历）
- 反证: 已查 cmd/polaris/boot_*.go 与 internal/bootstrap/ 全包，确认 Bootstrapper.Ignite() 计算出的拓扑顺序 order 仅为局部变量，未在 Bootstrapper 结构体中留存。gracefulShutdown 内 Phase 1~4 均直接 for name, mod := range b.modules 随机迭代 map。
- 问题: Go 语言中 map 迭代顺序是随机的。Bootstrapper.gracefulShutdown 在 Phase 1~Phase 4 各关停阶段中均直接遍历 b.modules 字典。由于 Ignite() 计算出的拓扑排序 order 未在 Bootstrapper 中保存，导致各模块在各关停阶段的执行顺序在每次运行期都是随机的，违反了「关停需按拓扑逆序/确定顺序执行」的生命周期约束，极易在关停时引发资源先于依赖方被关闭的竞态条件。
- 证据: internal/bootstrap/bootstrapper.go:134-137
  ```go
	// Phase 1：停流——熔断外部感知，停止接收新请求
	for name, mod := range b.modules {
		if s, ok := mod.(Stage1Stopper); ok {
			if err := s.StopIngress(ctx); err != nil {
  ```
- 修复方向提示: 在 Bootstrapper 结构体中保存 order []string（拓扑排序结果），在 gracefulShutdown 各 Phase 内按照 order 逆序遍历执行关停。

### [GR-4-001] CodeAct Python Stateful 模式在 Sbx-L3 容器内打开宿主快照路径因未捕获 `FileNotFoundError` 导致脚本退出报错
- 严重级: P1
- 模块: internal/action（层: L1）
- 位置: internal/action/codeact/code_act_stateful.go:97
- 违反规则: HE-2 | A-01
- 置信度: 高
- 可机械化: 是（建议规则: grep `"with open(__CA_STATE_FILE__, \"wb\")"` 检索并在 AST/正则上校验是否位于 try-except 外部）
- 反证: 已查 1) `sessionStateFile`（`code_act_stateful.go:65-79`）拼接的为宿主机绝对/相对路径 `ca.stateDir`（如 `~/.polaris/codeact_repl_state`）；2) `ca.Execute()` 构造 `sandbox.ExecRequest` 时（`code_act.go:305-320`）仅设置 `ScriptPath: tmpFile`，并未将宿主 `ca.stateDir` 目录 bind-mount 挂载至 Sbx-L3 容器内部；3) `wrapPythonStateful` 导出的 `__ca_save_state__` 函数中，`with open(__CA_STATE_FILE__, "wb") as __ca_f:` 未被包裹在 `try...except` 块中。当脚本在隔离容器沙箱中运行时，容器文件系统中不存在该宿主目录结构，抛出 `FileNotFoundError: [Errno 2] No such file or directory`，导致成功执行的用户代码在退出保存状态时被强行置为 ExitCode 1 失败。已查 `cmd/polaris/boot_*.go`、`internal/bootstrap/`、工具注册表及反射四处均符合此行为。
- 问题: 在 CodeAct 执行 Python 代码启用 `StatefulSession=true` 且未开启 L4 长驻进程时，`wrapPythonStateful` 会在脚本末尾注入 `__ca_save_state__` 函数。该函数在保存状态时直接 open 打开宿主机路径 `__CA_STATE_FILE__`，而未将其置于 `try...except` 保护块内。当脚本运行在 Sbx-L3 隔离沙箱容器中时，宿主机路径无法在容器内部创建，引发未捕获的 `FileNotFoundError` 异常，导致用户业务代码虽然成功执行，但整段脚本在退出时抛出异常并返回 ExitCode 1 失败。引用文档 `docs/specs/09-LLM-Agent-Production.md §A-01` 与 `docs/arch/M07-Tool-Action-Layer.md §7.4`。
- 证据: internal/action/codeact/code_act_stateful.go:97-110
  ```go
	with open(__CA_STATE_FILE__, "wb") as __ca_f:
		__ca_pickle.dump(__ca_state, __ca_f)
  ```
- 修复方向提示: 将 `open(__CA_STATE_FILE__, "wb")` 的打开与写入过程包裹在 `try...except Exception:` 块内，当处于无法写入宿主快照的容器沙箱环境时静默跳过状态保存。

### [GR-4-002] BuildReflectContext 组装反思 Prompt 时硬编码 ExecuteResult 污点等级为 TaintMedium 导致高污点标记静默丢失
- 严重级: P1
- 模块: internal/agent（层: L1）
- 位置: internal/agent/context/memory_context.go:361
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: grep `WriteUserData\(taint\.NewTaintedString\(.*ExecuteResult.*TaintMedium` 断言此处错误硬编码）
- 反证: 已查 1) `agent_execute_dag.go:566-568` 中当 DAG 节点包含高污点输出（`TaintHigh`）时，系统会正确将 `a.sCtx.GlobalTaintLevel` 抬升至 `TaintHigh`；2) `memory_context.go:361` 的 `BuildReflectContext` 函数在组装 `ExecuteResult` UserData 文本时，显式硬编码 `OriginTaintLevel: types.TaintMedium`，忽略了 `sCtx.GlobalTaintLevel` 的实际高污点状态；3) 高污点工具输出（如包含 Untrusted 外部网络/文件文本）在此处被强行打标为 Medium，导致下游 `PromptBuilder` 构建的 Taint 标记失真，违反了 HE-2 不变量与污点只升不降（PropagateTaint）原则。已查 `cmd/polaris/boot_*.go`、`internal/bootstrap/`、注册表及反射四处，确认此处的污点传递无其他补偿纠正机制。
- 问题: `BuildReflectContext` 函数在将 `sCtx.ExecuteResult` 字符串注入 `PromptBuilder` 时，硬编码 `taint.TaintSource{OriginTaintLevel: types.TaintMedium}`。然而，当工具执行包含外部未安全脱敏的 High Taint 数据时，`sCtx.GlobalTaintLevel` 已经被抬升为 `TaintHigh`。此处硬编码为 `TaintMedium` 造成了污点标签的静默降级与丢失，破坏了 `HE-2 可验证执行` 中关于污点传播“只升不降”的不变量要求。引用文档 `docs/specs/00-Constitution.md §R3-HE2` 与 `docs/arch/00-Global-Dictionary.md §7`。
- 证据: internal/agent/context/memory_context.go:358-363
  ```go
	if len(sCtx.ExecuteResult) > 0 {
		b.WriteUserData(taint.NewTaintedString(
			"Execution Result Summary:\n"+string(sCtx.ExecuteResult)+"\n\n",
			taint.TaintSource{OriginTaintLevel: types.TaintMedium},
			"execute_result"))
	}
  ```
- 修复方向提示: 改为 `OriginTaintLevel: sCtx.GlobalTaintLevel` 或使用 `types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)` 动态决定污点等级。

---

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

### [GR-6-002] StateGraphExecutor 无条件多节点循环边被误入 AND-Join 硬依赖集合导致状态图死锁
- 严重级: P1
- 模块: internal/execute/orchestrator（层: L1）
- 位置: internal/execute/orchestrator/pattern_state_graph.go:300
- 违反规则: HE-5
- 置信度: 高
- 可机械化: 是（建议规则: 状态图构建 requiredPreds 时须剔除可达环路中的回边，不能仅过滤自环 From != To）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/execute/orchestrator/pattern_state_graph.go:249-316。initializeStateGraph 行 300 处构建 requiredPreds 时，仅通过 e.From != e.To 过滤了单节点自环，未过滤多节点环路（如 B -> C -> B）中的无条件反馈边。导致 requiredPreds["B"] 误记入 {"A": true, "C": true}。入口节点 A 执行完毕后，arriveJoin("B", "A") 判定已到达前驱 1/2 < 2，返回 false 拒绝触发 B；而 C 依赖 B 产出才能执行，造成 B 与 C 相互等待的运行时死锁。
- 问题: StateGraphExecutor 引入 AND-Join 语义以支持多前驱汇聚，但在提取无条件硬依赖前驱集合（requiredPreds）时，仅剔除了 e.From == e.To 的单节点自环，未排除多节点状态循环中的反馈回边（如 B -> C -> B 中的 C -> B）。导致节点在首次调度时误将未执行的下游反馈节点算作必须等待的前驱，引发永久死锁（死锁 — len(inFlight) == 0）。
- 证据: 关键代码摘录（internal/execute/orchestrator/pattern_state_graph.go:300）
  ```go
  if e.Condition == nil && e.From != e.To {
  	if requiredPreds[e.To] == nil {
  		requiredPreds[e.To] = make(map[string]bool, 2)
  	}
  	requiredPreds[e.To][e.From] = true
  }
  ```
- 修复方向提示: 在 initializeStateGraph 构建 requiredPreds 时，结合拓扑分析剔除从 To 到 From 可达的反馈回边，仅保留无环主干上的前驱依赖。

### [GR-7-002] Engine.Start 对 taskEvents/versionEvents 通道关闭误用 return nil 导致后台自进化引擎提前异常关停
- 严重级: P1
- 模块: internal/learning（层: L2）
- 位置: internal/learning/engine.go:234
- 违反规则: HE-5 | HE-6 | 维度L-生命周期
- 置信度: 高
- 可机械化: 是（建议规则: Engine.Start 中 select-case 读取通道时，!ok 应对通道置 nil 继续循环，禁止直接 return nil）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/learning/engine.go:228-336 四处。Engine.Start 是 M9 自进化引擎的主后台工作协程，在一个 select 循环中同时承载 taskEvents 消费、heuristicEvents 消费、midTicker 课程生成 (每2min)、l3Ticker 策略漂移检测 (每10min)、l4TriggerCh、versionEvents 及 evalEvents 7 条并发管道。核对 heuristicEvents (:249)、l4TriggerCh (:296)、evalEvents (:323) 的处理，在 !ok 时均采用 e.xxx = nil; continue 保持后台主循环继续运行。唯独 taskEvents (:234) 与 versionEvents (:302) 在 !ok 时直接 return nil，导致上游任一事件源关闭即中断整个 Engine.Start 协程，使课程生成与漂移检测静默挂起。
- 问题: Engine.Start 管理着包含内环、中环、外环及 L3/L4 检测的复合生命周期。但在处理 taskEvents 和 versionEvents 通道关闭 (!ok) 时，使用了 return nil 而不是 e.taskEvents = nil; continue。一旦生产环境中的某个上游事件生产者关闭了通道，会导致整个 Engine.Start 协程静默退出，中断所有定时课程生成与策略漂移检测。
- 证据: internal/learning/engine.go:234-237
  ```go
		case ev, ok := <-e.taskEvents:
			if !ok {
				return nil
			}
  ```
- 修复方向提示: 将 taskEvents 和 versionEvents 的 !ok 分支修改为 e.taskEvents = nil; continue 与 e.versionEvents = nil; continue。

### [GR-8-001] NewMCPMarketplaceClient 中的 SafeHTTPClient 校验缺乏 nil 空值防护导致 panic 崩溃
- 严重级: P1
- 模块: internal/extension/marketplace（层: L2）
- 位置: internal/extension/marketplace/marketplace.go:40
- 违反规则: R1.14 | HE-2 | 维度D
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "!.*\.IsSafe()" internal/extension/marketplace/marketplace.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。boot_tools.go 中生产装配路径传入了 SafeHTTP 实例；但 NewMCPMarketplaceClient 的文档注释明文声称「传 nil 时降级为裸 http.Client（仅测试场景允许）」，而实现第 40 行无条件调用 `!httpClient.IsSafe()`，当未注入 httpClient 传入 nil 时会触发 Go `nil pointer dereference` 运行时 panic 崩溃，未能按 R1.14 要求做到安全的 fail-closed 判断。
- 问题: NewMCPMarketplaceClient 构造函数在文档注释中声明支持传入 nil httpClient 作为降级路径，但第 40 行代码未做 nil 解引用判空直接调用 `!httpClient.IsSafe()`。当外部测试或可选调用方传入 nil 时程序直接 panic 崩溃。
- 证据:
  ```go
  	if !httpClient.IsSafe() {
  		panic("marketplace: httpClient must be a valid network.SafeHTTPClient")
  	}
  ```
- 修复方向提示: 在调用 IsSafe 前增加 nil 判定，若 nil 则返回 fail-closed error 或安全初始化 SafeHTTPClient。

### [GR-8-002] skill_creator 与 plugin_creator 的 extractJSON 贪婪正则 (?s)\{.*\} 导致多括号 LLM 响应解析失败
- 严重级: P1
- 模块: internal/extension/skill（层: L2）
- 位置: internal/extension/skill/skill_creator.go:234
- 违反规则: A-01 | P-2 | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "regexp.MustCompile.*(?s)" internal/extension/`）
- 反证: 已查 boot_*.go / bootstrap / 注册表 / 反射，生成 Skill/Plugin 的请求经由 sysadmin HTTP Handler 调用 `GenerateSkill`/`GeneratePlugin`。当 LLM 响应包含 Markdown 提示词或正文前后含有 `{}`（如 `Here is option {1}: \n{...}\n Note: use {option 2}`）时，`(?s)\{.*\}` 的贪婪匹配从最外层第一个 `{` 截断至最后一个 `}`，使得截取的 JSON 文本包含非 JSON 前后缀，导致 `json.Unmarshal` 100% 失败并耗尽重试。
- 问题: `skill_creator.go` 与 `plugin_creator.go` 中的 `extractJSON` 函数均使用了 `(?s)\{.*\}` 贪婪匹配正则表达式。当 LLM 输出包含多个 JSON 块或在 JSON 前后文本中包含花括号 `{}` 时，正则表达式会跨非 JSON 文本大范围贪婪截取，导致得到的并非合法的 JSON 对象，破坏了 A-01 / P-2 的 LLM 响应格式兜底机制。
- 证据:
  ```go
  var jsonExtractRegex = regexp.MustCompile(`(?s)\{.*\}`)

  func extractJSON(input string) string {
  	match := jsonExtractRegex.FindString(input)
  ```
- 修复方向提示: 改用非贪婪匹配或基于括号计数 / json.RawMessage 边界提取的非贪婪 JSON 对象截取算法。

### [GR-9-001] handleAgentInterrupt 未将 WebUI 默认客户端类型纳入 whitelist 导致中断请求遭 403 拒绝
- 严重级: P1
- 模块: internal/gateway/server（层: L3）
- 位置: internal/gateway/server/server_handlers_hitl.go:131
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 检查 handleAgentInterrupt 中 authCtx.ClientType 的判断，比对 middleware_auth.go 注入的 WebUI 客户端类型 webui）
- 反证: 已核对 cmd/polaris/boot_server.go、internal/bootstrap/、middleware_auth.go 与 server_handlers_hitl.go。在未配置 POLARIS_API_KEY 的开发/本地部署场景下，middleware_auth.go:68 为 loopback 请求注入的 ClientType 为 "webui" 且 Authenticated 为 false。然而 handleAgentInterrupt (server_handlers_hitl.go:131) 在检查权限时仅判断 authCtx.ClientType == "local_webui" || authCtx.ClientType == "local"，并不匹配 "webui"。因 Authenticated 为 false 且 ClientType 不匹配，isLocalWebUI 为 false，所有来自本地 WebUI 的任务中断请求 (POST /v1/agent/{taskID}/interrupt) 均必定被 403 拒绝。
- 问题: 当用户在 WebUI 中点击“中止/恢复/重定向”Agent 任务时，`POST /v1/agent/{taskID}/interrupt` 端点会被调用。`middleware_auth.go` 在零认证配置下将本机请求的 `ClientType` 设为 `"webui"`，但 `handleAgentInterrupt` 中的硬编码判断只检查了 `"local_webui"` 和 `"local"`，未能包含 `"webui"`，导致用户合法的中断操作必定触发 `403 Forbidden: unauthorized user`，无法中断运行中的 Agent。
- 证据: 关键代码摘录如下
  ```go
  // middleware_auth.go:68
  return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{UserID: "anonymous", ClientType: "webui", TraceID: traceID, Authenticated: false}), true

  // server_handlers_hitl.go:130-135
  isAdmin := authCtx.Authenticated && (authCtx.UserID == "admin" || authCtx.UserID == "system")
  isLocalWebUI := authCtx.ClientType == "local_webui" || authCtx.ClientType == "local"
  if !isAdmin && !isLocalWebUI {
      http.Error(w, "forbidden: unauthorized user", http.StatusForbidden)
      return
  }
  ```
- 修复方向提示: 在 `server_handlers_hitl.go:131` 的 `isLocalWebUI` 判断条件中增加 `authCtx.ClientType == "webui"`。

### [GR-1-003] TierParameters 仍保留已废弃的 GraphRAGLLMDailyBudget 字段而缺失 GraphRAGConcurrentWorkers
- 严重级: P2
- 模块: internal/observability（层: L0）
- 位置: internal/observability/probe/tier_parameters.go:35, internal/observability/auto_config_tiers.go:30
- 违反规则: SSoT-L0
- 置信度: 高
- 可机械化: 是（建议规则: AST 检查 probe.TierParameters 结构体中是否存在废弃字段 GraphRAGLLMDailyBudget）
- 反证: 已查 docs/specs/CHANGELOG.md (2026-06-09 记录) 与 docs/arch/M03-Observability.md §5.3。规范变更明确记载: "TierParameterTable 中 GraphRAGLLMDailyBudget 参数与 M10 inv_M10_05 '已取消财务日预算限制' 直接冲突，改为 GraphRAGConcurrentWorkers（资源维度并发数）"。实测 internal/observability/probe/tier_parameters.go:35 与 auto_config_tiers.go (lines 30, 57, 84, 111) 仍定义并填充了 GraphRAGLLMDailyBudget，未按照架构裁决订正为 GraphRAGConcurrentWorkers。
- 问题: TierParameters 结构体及其自动配置映射逻辑与 SSoT 规范脱节。CHANGELOG.md (2026-06-09) 已裁决取消 GraphRAGLLMDailyBudget 财务限制并替换为资源维度的 GraphRAGConcurrentWorkers。但 probe/tier_parameters.go:35 与 auto_config_tiers.go 仍保留并硬编码填充旧字段，导致硬件层级的并发调优无法传递给 GraphRAG 模块。
- 证据: internal/observability/probe/tier_parameters.go:35
  ```go
  GraphRAGLLMDailyBudget int `json:"graphrag_llm_daily_budget"`
  ```
- 修复方向提示: 将 TierParameters 中的 GraphRAGLLMDailyBudget 替换为 GraphRAGConcurrentWorkers，并在 auto_config_tiers.go 中配置各 Tier 的并发数。

### [GR-1-004] downloadResume 多源降级时误把不支持 Range 的截断 partial 文件尺寸当作后续 Candidate 偏移量导致数据损坏
- 严重级: P2
- 模块: internal/downloader（层: L0）
- 位置: internal/downloader/http.go:42, internal/downloader/http.go:84
- 违反规则: E
- 置信度: 高
- 可机械化: 是（建议规则: 检查 downloadChunk 中 os.O_TRUNC 标志设置时是否清空 offset 或删除旧 part 文件）
- 反证: 已查 cmd/polaris/boot_tools.go、internal/bootstrap/ 及 sysmgr/updater 全量调用链。downloadResume 为资源/模型/插件下载的核心底层。在 CandidateURLs 循环中（line 80），若首个 Candidate 不支持 Range 响应 200 OK，downloadChunk 将使用 os.O_TRUNC (line 42) 重新从头写入 partPath。若下载中途网络中断或出错，partPath 保留了部分写入的字节数。后续循环尝试 Candidate 2 时，downloadResume 重新读取 fi.Size() (line 85) 作为 offset 并向 Candidate 2 发起 Range: bytes=offset- 请求。若 Candidate 2 支持 Range，将在已截断/损坏的 offset 处追加数据，造成最终重命名文件的数据损坏。已查 boot_tools.go/sysmgr 无上层校验层。
- 问题: downloadResume 在遍历 CandidateURLs 时，若某个候选源返回 HTTP 200 OK（不支持 HTTP Range 请求），downloadChunk 会使用 os.O_TRUNC 覆盖打开 partPath 重新写入。若本次下载中途中断返回错误，downloadResume 在 lines 84-86 重新通过 os.Stat(partPath) 更新 offset 为当前已写入字节数。在接下来的 Candidate 循环中，后续支持 Range 的候选源将从该中途截断的 offset 处发起 Range 请求并追加，导致前段内容丢失或数据错位位移，破坏下载文件完整性。
- 证据: internal/downloader/http.go:80-87
  ```go
  for _, url := range candidates {
  	if err := downloadChunk(ctx, client, url, partPath, offset); err != nil {
  		slog.Warn("downloader: source failed, trying fallback", "url", url, "err", err)
  		if fi, statErr := os.Stat(partPath); statErr == nil {
  			offset = fi.Size()
  		}
  ```
- 修复方向提示: 在 downloadChunk 中若服务端返回 200 OK（不支持 Range），重置写入偏移量并在失败时将 offset 归零或清理不完整的 .part 文件。

---

### [GR-10-003] internal/cli 模块包含 AgentREPL 等导出类型但零生产接线且未列入白名单
- 严重级: P2
- 模块: internal/cli（层: L3）
- 位置: internal/cli/cli.go:20
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: 导出符号生产调用方可达性扫描，测试调用不计）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、注册表/工厂注册点与 scripts/deadcode-allowlist.txt 四处。全仓库没有任何生产代码 import `github.com/polarisagi/polaris/internal/cli`，其导出的 `AgentREPL`、`RateLimiterMiddleware`、`WebSocketHub` 仅在 `cli_test.go` 中有调用，属于写了但从未接线且未在 deadcode 白名单备案的存量代码。
- 问题: `internal/cli/cli.go` 中定义并导出了 `AgentREPL`、`RateLimiterMiddleware`、`WebSocketHub` 等交互与限流类型，但在主入口 `cmd/polaris/` 以及所有生产模块中均零引用、零接线。由于 `cli_test.go` 存在测试调用，使其在常规死代码扫描中逃逸，形成了典型写了没接线的悬空代码。
- 证据: internal/cli/cli.go:20-24
  ```go
  type AgentREPL struct {
  	history []REPLEntry
  	session *Session
  	InferFn func(ctx context.Context, input string) (<-chan types.StreamEvent, error)
  }
  ```
- 修复方向提示: 将 CLI 接入生产引导入口或命令体系，或清理未接线代码并同步删除/备案白名单。

### [GR-10-005] IdleEvolutionScheduler 在后台任务完成时未清理 cancelFuncs 导致后续空闲周期永久阻断
- 严重级: P2
- 模块: internal/automation（层: L3）
- 位置: internal/automation/idle_evolution.go:115
- 违反规则: 维度L-生命周期
- 置信度: 高
- 可机械化: 否
- 反证: 已查 boot_agent.go:913，`IdleEvolutionScheduler` 被创建并运行。在 `idle_evolution.go:113` 的 `tryRunIdleTasks` 中，首次检测到空闲时创建 `taskCtx, cancel` 并将 `cancel` append 入 `s.cancelFuncs`。但在提交异步 Goroutine 运行 `consolidateFn`/`forgettingFn`/`graphPruneFn` 后，任务自然完成时没有任何 defer 或 callback 逻辑将 `s.cancelFuncs` 清空。若系统持续保持空闲状态且无新 activity 触发 `cancelAll()`，后续每 30s 的 `tryRunIdleTasks` 均因 `len(s.cancelFuncs) > 0` 直接 return，使得后续所有空闲演化周期被永久阻断。
- 问题: `IdleEvolutionScheduler.tryRunIdleTasks` 在拉起空闲演化任务时，会将 `cancel` 函数加入 `s.cancelFuncs` 切片。然而当异步任务正常执行完毕后，`s.cancelFuncs` 并没有被清空。当系统在后续 30 秒 Ticker 周期依然处于空闲状态时，`tryRunIdleTasks` 会因为 `len(s.cancelFuncs) > 0` 误认为任务尚在运行中而直接返回，导致空闲演化任务在首次运行完成后再也无法自动触发。
- 证据: internal/automation/idle_evolution.go:113-119
  ```go
  func (s *IdleEvolutionScheduler) tryRunIdleTasks(ctx context.Context) {
  	s.mu.Lock()
  	if len(s.cancelFuncs) > 0 {
  		// 已经在运行中
  		s.mu.Unlock()
  		return
  	}
  ```
- 修复方向提示: 在异步空闲任务全部完成后，加锁将 `s.cancelFuncs` 重置为 `nil`，或使用 `sync.WaitGroup` 跟踪运行状态。

---

### [GR-12-001] M02 §16 DDL 全量文件数与末尾编号与 schema 现状漂移
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/M02-Storage-Fabric.md:433
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限)
- 反证: 已核对 internal/protocol/schema/ 物理文件，确认文件数确实为 35 份，028~038 段包含 029_workflows.sql 至 038_idempotent_cache.sql 10 个新增建表文件；根 CLAUDE.md 已标注 35 份，但 M02-Storage-Fabric.md 仍遗留历史 25 份快照，未及时更新。
- 问题: M02-Storage-Fabric.md §16 表中声明 "全部 DDL（001_events 至 028_apps，共 25 份，权威目录 internal/protocol/schema/）"。而实际上 internal/protocol/schema/ 目录中共有 35 个 SQL 文件（001~024，028~038，其中 025~027 为刻意预留跳号），末尾文件已延伸至 038_idempotent_cache.sql。文档在 DDL 文件总量（25 vs 35）及末尾落点（028_apps vs 038_idempotent_cache）上与代码 SSoT 均严重脱节。
- 证据: 如下
  ```text
  文档: docs/arch/M02-Storage-Fabric.md:433
  | DDL | 全部 DDL（001_events 至 028_apps，共 25 份，权威目录 `internal/protocol/schema/`） | internal/protocol/schema/ |
  代码: ls internal/protocol/schema/*.sql -> 001_events.sql ... 038_idempotent_cache.sql (共 35 份)
  ```
- 修复方向提示: 将 M02-Storage-Fabric.md:433 的 "001_events 至 028_apps，共 25 份" 更新为 "001_events 至 038_idempotent_cache，共 35 份（025~027 保留）"。

### [GR-12-002] M04 §1 状态枚举声明列表缺漏且权威源文件名标注错误
- 严重级: P2
- 模块: docs/arch（层: L1）
- 位置: docs/arch/M04-Agent-Kernel.md:46
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件)
- 反证: 已核对 pkg/types/enums_agent.go 与 internal/protocol/types.go，确认 AgentState 类型及其 13 个常数（含 AgentStateSuspended 和 AgentStateAwaitAgent）定位于 pkg/types/enums_agent.go，internal/protocol/types.go 中仅有结构体字段引用该类型；M04 本身第 62 行也已按 13 态阐述，证明第 46 行属早期遗留未同步的矛盾陈述。
- 问题: M04-Agent-Kernel.md §1 第 46 行标注 "状态枚举权威定义见 internal/protocol/types.go (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)"。此段存在两处漂移：① 状态列表仅列出 11 态，漏掉了 Suspended 与 AwaitAgent，与同一文件第 62 行明确声明的 "共 13 态" 以及 state.yaml:32 矛盾；② 将全系统唯一权威定义文件标注为 internal/protocol/types.go，而实际代码规范 SSoT 文件已收敛至 pkg/types/enums_agent.go:14。
- 证据: 如下
  ```text
  文档: docs/arch/M04-Agent-Kernel.md:46
  状态枚举权威定义见 `internal/protocol/types.go` (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)。
  代码: pkg/types/enums_agent.go:14-30 (AgentStateIdle ... AgentStateAwaitAgent 共 13 个常数)
  ```
- 修复方向提示: 将第 46 行中的 internal/protocol/types.go 改为 pkg/types/enums_agent.go，并将括号内枚举补齐为 13 态全集。

### [GR-12-003] INDEX §1 场景加载预算中多份核心文档 est_tok 估算严重低估
- 严重级: P2
- 模块: docs/arch（层: 契约）
- 位置: docs/arch/INDEX.md:101
- 违反规则: SSoT-L1
- 置信度: 高
- 可机械化: 是 (建议规则: 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移)
- 反证: 已核对 wc -c docs/arch/*.md 的物理字节大小，确认 M13-bis、00-Dict、M13、M05、M07 等文档在经历多轮架构重构与功能回写后体积已大幅膨胀，但 INDEX.md §1 的 est_tok 仍保留了初期的快照估算值，未随文档演进同步刷新。
- 问题: docs/arch/INDEX.md §1 "文档清单" 表中给出的文档 token 体量估算（est_tok）与实际文件体积发生严重偏差。例如：M13-bis-Extension-Registry.md 标注为 5K，实测文件 33.3KB（约 25K~30K token，低估达 6.6 倍）；00-Global-Dictionary.md 标注为 23K，实测 51.4KB（约 45K~50K token）；M13-Interface-Scheduler.md 标注为 28K，实测 76.5KB（约 65K~70K token）。偏差导致 AI 编程在依据 §2 评估场景加载预算时严重误判上下文开销，引发 token 溢出或装载超限。
- 证据: 如下
  ```text
  文档: docs/arch/INDEX.md:101-107
  | `M13-Interface-Scheduler.md` | L3 接口 | 28K |
  | `M13-bis-Extension-Registry.md` | L3 扩展 | 5K |
  | `00-Global-Dictionary.md` | 字典 | 23K |
  实际文件大小: docs/arch/M13-bis-Extension-Registry.md (33,351 bytes), docs/arch/00-Global-Dictionary.md (51,428 bytes)
  ```
- 修复方向提示: 重新计算并更新 docs/arch/INDEX.md §1 表中各文档的 est_tok 估算值。

### [GR-12-004] 宪法 R2.5 错误码字典缺失 CodeStorageUnavailable 权威定义
- 严重级: P2
- 模块: docs/specs（层: 契约）
- 位置: docs/specs/00-Constitution.md:101
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集)
- 反证: 已核对 pkg/apperr/apperr.go 第 50 行，确认 CodeStorageUnavailable 已存在且在 internal/agent/agent_execute_util.go 中被生产代码引用；而宪法 §R2.5 的表格中未收录此码。
- 问题: docs/specs/00-Constitution.md §R2.5 声明 "权威源：pkg/apperr/apperr.go（唯一定义处，禁止在其他包新增 Code 常量）"，但在其列出的错误码表中，缺失了 pkg/apperr/apperr.go:50 定义的 CodeStorageUnavailable（持久化存储层故障熔断专用错误码，由 GD-13-003 / ADR-0046 引入）。规范文档未覆盖完整错误码枚举。
- 证据: 如下
  ```text
  文档: docs/specs/00-Constitution.md:101-109
  | 资源 | `CodeNotFound`, `CodeAlreadyExists`, `CodeConflict`, `CodeResourceExhausted` |
  代码: pkg/apperr/apperr.go:50
  CodeStorageUnavailable Code = "STORAGE_UNAVAILABLE"
  ```
- 修复方向提示: 在 docs/specs/00-Constitution.md §R2.5 的表格中补齐 CodeStorageUnavailable 项。

### [GR-12-005] 全局字典 §12 追溯表中各模块 DDL 文件范围陈旧
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/00-Global-Dictionary.md:628
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围)
- 反证: 已查 internal/protocol/schema/ 目录，确认架构 DDL 远不止 001-006，全仓 35 份 SQL 文件均包含架构定义与字段注释；此处 "001-006_*.sql" 属早期字典初稿留下的陈旧范围，未同步扩展。
- 问题: docs/arch/00-Global-Dictionary.md §12 [标签→实现文件追溯] 表中第 628 行标注 "各模块 DDL: internal/protocol/schema/001-006_*.sql (架构 DDL 含中文注释)"。此处将全仓 DDL 范围严重限制在历史前 6 份文件（001-006），未能体现现行 35 份 DDL Schema 文件（001_events 至 038_idempotent_cache）的现状，存在明显文档陈旧漂移。
- 证据: 如下
  ```text
  文档: docs/arch/00-Global-Dictionary.md:628
  | 各模块 DDL | `internal/protocol/schema/001-006_*.sql` (架构 DDL 含中文注释) |
  代码: ls internal/protocol/schema/*.sql -> 001_events.sql ... 038_idempotent_cache.sql (共 35 份)
  ```
- 修复方向提示: 将 00-Global-Dictionary.md:628 中的 001-006_*.sql 更新为 001-038_*.sql。

### [GR-12-006] M02 §2 tasks 表结构列说明遗漏 spawn_depth 字段
- 严重级: P2
- 模块: docs/arch（层: L0）
- 位置: docs/arch/M02-Storage-Fabric.md:178
- 违反规则: docs↔code 漂移
- 置信度: 高
- 可机械化: 是 (建议规则: 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集)
- 反证: 已查 internal/protocol/schema/007_tasks.sql 第 70 行与 internal/execute/orchestrator/sqlite_blackboard.go，确认 spawn_depth 已实际落盘与读取；M02 §2 声明覆盖所有历史迁移后的最终列，却未收录该列。
- 问题: docs/arch/M02-Storage-Fabric.md §2 给出 tasks 表的关键列说明（第 178 行声明 "以下为文档层声明，覆盖所有历史迁移后的最终列集合"）。但在随后的表格（第 180-208 行）中，漏掉了 internal/protocol/schema/007_tasks.sql:70 新增的 spawn_depth 列（TaskEntry.SpawnDepth 持久化落点，由 ADR-0084 引入，用于 transfer_to_agent 委派深度限制）。
- 证据: 如下
  ```text
  文档: docs/arch/M02-Storage-Fabric.md:178-208
  (表格列出 task_id, session_id ... cost_usd, created_at, updated_at，缺失 spawn_depth)
  代码: internal/protocol/schema/007_tasks.sql:70
  spawn_depth INTEGER NOT NULL DEFAULT 0,
  ```
- 修复方向提示: 在 M02-Storage-Fabric.md §2 tasks 表结构说明表格中补充 spawn_depth 列的定义与语义说明。

### [GR-12-007] M13-bis §2.2 表格中 origin 枚举值与 DDL/类型定义自相矛盾
- 严重级: P2
- 模块: docs/arch（层: L3）
- 位置: docs/arch/M13-bis-Extension-Registry.md:72
- 违反规则: SSoT-L1
- 置信度: 高
- 可机械化: 是 (建议规则: grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性)
- 反证: 已查 internal/protocol/schema/020_extension_instances.sql 与 internal/extension/ 模块相关代码，确认代码中 origin 仅处理 builtin / marketplace / user / learned，不存在 official 字符串枚举值；官方市场扩展在代码中均设为 origin = "marketplace"。§2.2 表格中的 official 属于概念混淆误写。
- 问题: docs/arch/M13-bis-Extension-Registry.md §2.2 表格第 72 行将 official 列为 origin 枚举值之一（"official: 官方市场推荐包, trust_tier 默认 3"）。然而：① 同一文件第 26 行与 020_extension_instances.sql:14-18 明确指明 origin 枚举仅有 4 个合法值（'builtin' | 'marketplace' | 'user' | 'learned'）；② 官方包在架构中由 origin='marketplace' + trust_tier=3（或 catalog_id 非空）表达，并非独立的 origin='official' 枚举。§2.2 的表格描述破坏了文档内部及 docs↔schema 的概念一致性。
- 证据: 如下
  ```text
  文档内部矛盾:
  M13-bis §1 (line 26): origin 枚举: builtin | marketplace | user | learned
  M13-bis §2.2 表格 (line 72): | official | 官方市场推荐包 | 3 TrustOfficial |
  DDL: internal/protocol/schema/020_extension_instances.sql:14
  -- origin 枚举: builtin | marketplace | user | learned
  ```
- 修复方向提示: 修正 M13-bis-Extension-Registry.md §2.2 表格中的 official 行，澄清官方包的实际表示为 origin='marketplace'（trust_tier=3）。

---

### [GR-2-002] SafeDialer.dnsCache 缺乏容量上限与淘汰机制，长时运行下存在无界内存泄露风险
- 严重级: P2
- 模块: internal/security（层: L0）
- 位置: internal/security/network/safe_dialer.go:353
- 违反规则: A-06
- 置信度: 高
- 可机械化: 否
- 反证: 已查 internal/security/network/safe_dialer.go 第 50-54 行与 327-360 行、cmd/polaris/boot_substrate.go 及 internal/bootstrap/。SafeDialer 在 boot 时作为全局单例创建，其 dnsCache 与 dnsCacheTs map 仅在 resolveDNSBypass 中向 map 写入解析结果，全仓没有任何针对 dnsCache 的 delete、清理逻辑或 FIFO/LRU 容量上限控制。
- 问题: SafeDialer 中的 dnsCache 与 dnsCacheTs map 存储域名解析结果，虽然在 resolveDNS 中判断了 TTL 超时，但仅在未超时时复用缓存，超时后再次调用 resolveDNSBypass 重新写入 map。全仓没有任何清理旧 key 或容量限制的代码。当系统长期运行并请求不同域名时，dnsCache 中的 key 数量无上限增长，违反了 docs/specs/09-LLM-Agent-Production.md P-5 / A-06 缓存必须有容量上限与过期淘汰的要求。
- 证据: internal/security/network/safe_dialer.go:353-357
  ```go
  	// 写回缓存（更新时间戳）
  	sd.dnsCacheMu.Lock()
  	sd.dnsCache[host] = result
  	sd.dnsCacheTs[host] = time.Now()
  	sd.dnsCacheMu.Unlock()
  ```
- 修复方向提示: 为 SafeDialer.dnsCache 引入容量上限（如 LRU/FIFO map）或在写回时清理过期条目。

### [GR-2-003] security/provider.go 声明的 AuditRepo / KillSwitchMetrics / GuardProvider 接口全仓零消费方
- 严重级: P2
- 模块: internal/security（层: L0）
- 位置: internal/security/provider.go:27
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: grep -rn "security\.AuditRepo" internal/ 且测试不计入）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/security/ 及全仓 internal/ 代码。AuditRepo、KillSwitchMetrics、GuardProvider 这三个导出接口仅在 internal/security/provider.go 中定义，在全仓生产代码及测试中均没有任何类型实现或变量引用。audit_trail.go 实际使用了 protocol.AuditRepository，killswitch.go 实际使用了 StateChangeCallback。
- 问题: internal/security/provider.go 声明了 AuditRepo、KillSwitchMetrics、GuardProvider 三个消费端/生产端接口，但在仓库实际实现中，audit_trail.go 直接消费了 protocol.AuditRepository，killswitch.go 消费了回调函数，导致这三个接口成为无任何生产及测试调用的死代码，违反了 docs/specs/00-Constitution.md R1.4 关于接口不许无真实消费方的要求及维度 G-bis 接线可达性原则。
- 证据: internal/security/provider.go:27-52
  ```go
  type AuditRepo interface {
  	Insert(ctx context.Context, record *AuditRecord) error
  	LoadSince(ctx context.Context, afterTimestampMicro int64) ([]*AuditRecord, error)
  }
  ```
- 修复方向提示: 清理 internal/security/provider.go 中无调用的冗余接口或将真实消费点收口迁移至这些接口。

---

### [GR-3-003] Bootstrapper.Ignite 模块初始化中途失败缺乏已初始化模块的回滚清理
- 严重级: P2
- 模块: internal/bootstrap（层: 契约）
- 位置: internal/bootstrap/bootstrapper.go:93
- 违反规则: L
- 置信度: 高
- 可机械化: 是（建议规则: AST 检查 Bootstrapper.Ignite 循环中 mod.Init 失败处理缺失逆序 Cleanup 逻辑）
- 反证: 已核对 internal/bootstrap/bootstrapper.go:88-101，Ignite 循环在 mod.Init 返回 err 或 !mod.Ready() 时，直接 return apperr.Wrap/New，未对已成功 Init 的 order[0..i-1] 模块调用任何 Close 或 Rollback。
- 问题: 在 Bootstrapper.Ignite 拓扑初始化循环中，若某个模块在 mod.Init(b.deps) 失败或 !mod.Ready() 校验不通过，Ignite 会直接返回 error 中断启动。但此时先前已初始化成功的模块（order[0..i-1]）没有得到任何关停或清理处理（如已分配的 Goroutine、句柄或 DB 连接未被释放），导致进程启动失败时留存悬空资源。
- 证据: internal/bootstrap/bootstrapper.go:93-95
  ```go
		if err := mod.Init(b.deps); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("bootstrap: module %s init failed", name), err)
		}
  ```
- 修复方向提示: 在 Ignite 初始化失败时，对已初始化的模块按照已完成顺序的逆序触发 gracefulShutdown 或清理操作。

---

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

### [GR-6-003] provider.go 存留 2 行无符号声明空文件
- 严重级: P2
- 模块: internal/vfs（层: L1）
- 位置: internal/vfs/provider.go:1
- 违反规则: 维度G-死代码
- 置信度: 高
- 可机械化: 是（建议规则: 仅含 package 声明且无任何导出的源文件应清除）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/vfs/ 全包。provider.go 仅包含 2 行（package vfs 与空行），无任何类型、接口或函数声明，且全仓无任何对该文件的编译/构建依赖，属于清理遗留的空文件死代码。
- 问题: internal/vfs/provider.go 文件仅存 package vfs 声明，缺乏任何实质性代码或接口定义。作为历史重构遗留物，增加了模块目录的维护噪音。
- 证据: 源码全部内容（internal/vfs/provider.go:1）
  ```go
  package vfs

  ```
- 修复方向提示: 删除 internal/vfs/provider.go 文件。

### [GR-7-003] PlannerPool.workerEngineA 派生超时 context 后未校验 parent context 导致取消信号下仍然运行高消耗沙箱构建
- 严重级: P2
- 模块: internal/swarm（层: L2）
- 位置: internal/swarm/planner/pool.go:175
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: 在 context.WithTimeout 派生后执行沙箱/子进程操作前必须检查 parent ctx.Err()）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/swarm/planner/pool.go:175-193 四处。workerEngineA 在 L175 使用 buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second) 派生超时 context，但在 L181 发起 p.sandbox.Execute(buildCtx, "go", []string{"build", tmpDir}, wd, 30*time.Second) 之前未检查父 context (ctx.Err())。沙箱内部会重新根据传入的 timeout 参数构造独立超时，导致在父 ctx 已被取消（如 HTTP 请求超时或用户取消）时，系统仍会无意义地启动外部 go build / go test 编译测试子进程，造成 CPU 资源浪费。
- 问题: PlannerPool.workerEngineA 在派生 buildCtx 和 testCtx 之后，直接发起沙箱编译与测试，未检查父 context (ctx.Err()) 是否已经取消。在并发任务被外部取消时，仍会继续触发消耗极高 CPU 资源的 go build 和 go test 子进程。
- 证据: internal/swarm/planner/pool.go:175-180
  ```go
	buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel1()

	var compileScore = 0.0

	if p.sandbox != nil {
		_, buildErr := p.sandbox.Execute(buildCtx, "go", []string{"build", tmpDir}, wd, 30*time.Second)
  ```
- 修复方向提示: 在调用 sandbox.Execute 前增加 if ctx.Err() != nil { return } 检查。

### [GR-8-003] ScriptSkillExecutor 限流触发时错误码误用 CodeInternal 替代 CodeResourceExhausted
- 严重级: P2
- 模块: internal/extension/skill（层: L2）
- 位置: internal/extension/skill/skill_executor.go:85
- 违反规则: P-9 | R2.5 | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "rate limit exceeded" internal/extension/skill/skill_executor.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。`ScriptSkillExecutor.ExecuteSkill` 被 Agent 执行引擎在 S_EXECUTE 阶段调用。限流触发时返回的错误码被指定为 `apperr.CodeInternal`（对应 HTTP 500），而上层重试与熔断逻辑依赖 `apperr.CodeOf(err)` 识别限流/资源耗尽（`CodeResourceExhausted` 429）以进行退避重试，误用 CodeInternal 会导致上层误判为系统崩溃事故。
- 问题: `ScriptSkillExecutor` 在技能执行超过限流阈值 (20 QPS) 时，使用 `apperr.CodeInternal` 构造错误并抛出。违反了 P-9 全链路错误语义化与 `pkg/apperr` 错误码规范映射要求，阻断了上层对 429 资源限流错误的精准感知。
- 证据:
  ```go
  	// P1-8 限流：Skill 执行速率上限 20 QPS
  	if !e.skillLimiter.Allow() {
  		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("skill_executor: rate limit exceeded for skill %s", skillID))
  	}
  ```
- 修复方向提示: 将 `apperr.CodeInternal` 替换为 `apperr.CodeResourceExhausted`。

### [GR-8-004] makeMCPToolAsyncFn 异步变体工具返回结果未标注 TaintLevel 退化为 TaintNone
- 严重级: P2
- 模块: internal/extension/mcp（层: L2）
- 位置: internal/extension/mcp/mcp_manager_tools.go:211
- 违反规则: HE-2 | 维度D | 维度M
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "makeMCPToolAsyncFn" internal/extension/mcp/mcp_manager_tools.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。MCP 异步工具变体在注册时由 `makeMCPToolAsyncFn` 构造 ToolResult，返回 `&types.ToolResult{Success: true, Output: out}`，未设置 `TaintLevel` 字段，默认值为 Go 零值 0 (`TaintNone`)。尽管该返回结果为 `task_id` 包装载荷，但外部 MCP Server 工具的交互结果应继承该 Server 对应的静态污点策略（`TaintMedium` 或 `TaintHigh`），直接返回 `TaintNone` 导致后续 DAG 执行节点的污点追踪被截断。
- 问题: `makeMCPToolAsyncFn` 函数中构造异步变体响应 `ToolResult` 时，漏掉了 `TaintLevel` 字段赋值。这使得从外部/未受信 MCP 工具派生的异步任务 Initial ToolResult 在进入系统后被误标为 `TaintNone` (系统级无污点)，违反了 HE-2 可验证执行中污点只升不降的传播规范。
- 证据:
  ```go
  		taskID := m.runAsyncCall(ctx, client, mcpName, args)
  		out, _ := json.Marshal(map[string]string{"task_id": taskID, "status": string(AsyncTaskPending)}) //nolint:errchkjson // 固定字段结构体，Marshal 不会失败
  		return &types.ToolResult{Success: true, Output: out}, nil
  ```
- 修复方向提示: 构造 ToolResult 时显式传入该 MCP 工具注册时的静态 `taint` 等级。

### [GR-8-005] stdio readLoop 的 bufio.Scanner 硬编码 1MB 缓冲区上限导致大载荷 MCP 响应断连
- 严重级: P2
- 模块: internal/extension/mcp（层: L2）
- 位置: internal/extension/mcp/mcp_client_stdio.go:72
- 违反规则: Tier-0-Limit | 维度D | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "scanner.Buffer" internal/extension/mcp/mcp_client_stdio.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。stdio 模式下 MCP 客户端建立子进程后由 `readLoop` 循环按行读取 stdout 的 JSON-RPC 消息。`scanner.Buffer` 的最大容量被写死为 `1024*1024` (1MB)。当 MCP 工具返回大型 Base64 图片数据、大型文件或长上下文知识块时，单行 JSON 超过 1MB 会导致 `scanner.Scan()` 触发 `bufio.ErrTooLong` 并退出循环、自动调用 `c.Close()` 导致 MCP 连接意外断开崩溃。
- 问题: stdio 传输层中的 `readLoop` 为 `bufio.Scanner` 设置了固定 1MB 的 `maxBuffer` 上限。在包含多模态图片 (image content block) 或大图谱数据的场景中，单次 JSON-RPC 消息体很容易超过 1MB，引发 `token too long` 扫描错误，强制关停底层进程管道。
- 证据: internal/extension/mcp/mcp_client_stdio.go:93-98
  ```go
  func (c *MCPClient) readLoop(r io.Reader) {
  	scanner := bufio.NewScanner(r)
  	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
  	for scanner.Scan() {
  ```
- 修复方向提示: 增大 maxBuffer 缓冲区上限（例如 16MB 或 64MB），或改用 json.Decoder 块读取流式解析。

### [GR-9-003] HandleTogglePluginMCP 路由与 handler 路径参数不匹配导致永远 400
- 严重级: P2
- 模块: internal/gateway/server（层: L3）
- 位置: internal/gateway/server/server_routes.go:203
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 提取 mux.HandleFunc 模式中的路径参数与 Handler 中 r.PathValue 调用的 key 集合，校验子集包含关系）
- 反证: 已核对 server_routes.go:203 与 internal/gateway/server/plugin/manage.go:279-282。`server_routes.go:203` 将 `POST /v1/plugins/{id}/toggle` 绑定到 `HandleTogglePluginMCP`。但 `HandleTogglePluginMCP` 内部试图通过 `r.PathValue("serverName")` 读取路径参数 `serverName`。由于注册的路由中根本不包含 `{serverName}` 占位符，`r.PathValue("serverName")` 必定返回 `""`，从而触发 line 280 的 `if pluginID == "" || serverName == ""` 校验，强制返回 `400 Bad Request: id and serverName required`。此 HTTP 端点在生产环境中 100% 无法正常工作。
- 问题: 在 `server_routes.go` 中注册插件子 MCP 切换路由时，填写的路由模式为 `POST /v1/plugins/{id}/toggle`；而在 `HandleTogglePluginMCP` 的实现中，要求从 URL 路径参数中读取 `id` 与 `serverName`。由于路由定义遗漏了 `{serverName}` 占位符（正确格式应为 `POST /v1/plugins/{id}/mcp/{serverName}/toggle` 或在 request body 中传递 `serverName`），handler 提取出的 `serverName` 始终为空字符串，导致该 API 任何请求均直接触发 400 校验错误。
- 证据: 关键代码摘录如下
  ```go
  // server_routes.go:203
  mux.HandleFunc("POST /v1/plugins/{id}/toggle", s.pluginHandler.HandleTogglePluginMCP)

  // plugin/manage.go:278-283
  pluginID := r.PathValue("id")
  serverName := r.PathValue("serverName")
  if pluginID == "" || serverName == "" {
      http.Error(w, "id and serverName required", http.StatusBadRequest)
      return
  }
  ```
- 修复方向提示: 将 `server_routes.go:203` 的路由修改为 `POST /v1/plugins/{id}/mcp/{serverName}/toggle`（或 `PATCH /v1/plugins/{id}/mcp/{serverName}`）以匹配 handler 的参数提取模式。

---

