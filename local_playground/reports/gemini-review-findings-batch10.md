# 批次 10 代码审核报告

## 批次汇总表

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-10-001 | P1 | internal/automation | SQLiteScheduler.scanAndDispatch 零过滤 running 状态导致长耗时任务重复并发调度 | 高 | 是 |
| GR-10-002 | P0 | internal/eval | ShadowExecutor.scoreShadow 在 llmProvider 为 nil 时降级放行导致未评估候选版本自动确认上线 | 高 | 是 |
| GR-10-003 | P2 | internal/cli | internal/cli 模块包含 AgentREPL 等导出类型但零生产接线且未列入白名单 | 高 | 是 |
| GR-10-004 | P0 | internal/channel | EmailAdapter.Send 经 EmailSendMessage 直调 smtp.SendMail 绕过 SafeDialer SSRF 防护 | 高 | 是 |
| GR-10-005 | P2 | internal/automation | IdleEvolutionScheduler 在后台任务完成时未清理 cancelFuncs 导致后续空闲周期永久阻断 | 高 | 否 |

置信度分布声明: 全部 5 条发现均属于高置信度，已逐条做 §2-A 强制反证，且证据直接可见于代码特定行号。

---

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

## 已审文件清单

- internal/automation/cost_report.go
- internal/automation/cron.go
- internal/automation/facade.go
- internal/automation/hitl/gateway.go
- internal/automation/hitl/gateway_approval.go
- internal/automation/hitl/gateway_l3gate.go
- internal/automation/hitl/gateway_notify.go
- internal/automation/hitl/provider.go
- internal/automation/hitl/trust_score.go
- internal/automation/idle_evolution.go
- internal/automation/notify/dispatcher.go
- internal/automation/provider.go
- internal/automation/queue.go
- internal/automation/resource_governor.go
- internal/automation/worktree.go
- internal/channel/adapter/adapter.go
- internal/channel/adapter/dingtalk.go
- internal/channel/adapter/discord.go
- internal/channel/adapter/email.go
- internal/channel/adapter/feishu.go
- internal/channel/adapter/homeassistant.go
- internal/channel/adapter/line.go
- internal/channel/adapter/matrix.go
- internal/channel/adapter/mattermost.go
- internal/channel/adapter/message.go
- internal/channel/adapter/qqbot.go
- internal/channel/adapter/signal.go
- internal/channel/adapter/slack.go
- internal/channel/adapter/sms.go
- internal/channel/adapter/teams.go
- internal/channel/adapter/telegram.go
- internal/channel/adapter/webhook.go
- internal/channel/adapter/wecom.go
- internal/channel/adapter/whatsapp.go
- internal/channel/dispatch.go
- internal/channel/manager.go
- internal/channel/message.go
- internal/channel/provider.go
- internal/cli/cli.go
- internal/eval/analysis/incident_to_eval.go
- internal/eval/analysis/meta_eval.go
- internal/eval/analysis/sampling_monitor.go
- internal/eval/analysis/sampling_scorer.go
- internal/eval/analysis/shadow_executor.go
- internal/eval/benchmark/benchmark.go
- internal/eval/control/engine.go
- internal/eval/founding_anchor.go
- internal/eval/harness/benchmark/benchmark.go
- internal/eval/harness/benchmark/gaia.go
- internal/eval/harness/benchmark/locomo.go
- internal/eval/harness/benchmark/swebench.go
- internal/eval/harness/benchmark/taubench.go
- internal/eval/harness/benchmark/terminalbench.go
- internal/eval/harness/eval.go
- internal/eval/harness/judge_schema.go
- internal/eval/harness/runner.go
- internal/eval/harness/runner_eval.go
- internal/eval/harness/store.go
- internal/eval/harness/trajectory_judge.go
- internal/eval/provider.go
- internal/eval/red_team.go
- internal/eval/regression/detector.go
- internal/eval/synthetic_adapter.go
- internal/eval/util/jsonutil.go
- internal/sysmgr/locale/locale.go
- internal/sysmgr/osutils/script.go
- internal/sysmgr/updater/signature.go
- internal/sysmgr/updater/signer.go
- internal/sysmgr/updater/signing_state.go
- internal/sysmgr/updater/updater.go
- internal/sysmgr/updater/updater_install.go

## 明确未覆盖的范围

无

## 审了但无发现的模块

- internal/sysmgr
