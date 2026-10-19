# 自主内核全量复核修复第 2 轮报告（REPORT-fix-round2）

- **分支**：`fix/autonomy-round2`
- **基线 commit**：`a4afb30`（合并自主内核第 1 轮验收缺陷修复与核实）
- **日期**：2026-10-19
- **作者**：`MrLaoLiAI <polarisagi.online@gmail.com>`

---

## 1. 缺陷修复总览 (R2-01 ~ R2-17)

| 编号 | 级别 | 问题说明 | 结论 | 核心改动文件 | 提交 Commit |
| :--- | :---: | :--- | :---: | :--- | :--- |
| **R2-01** | P0 | agentic 循环第 2 轮起丢失用户意图与跨回合状态泄漏 | **已修复** | `agent_turn.go`, `prompt_layers.go`, `transitions.go` | `73bc8da` |
| **R2-02** | P0 | bash background 与持久终端绕过沙箱在宿主机执行 | **已修复** | `bash.go`, `sandbox_persistent.go` | `3c39696` |
| **R2-03** | P0 | agentic 模式下子智能体委派 wait=all 无法挂起与恢复 | **已修复** | `loop_transitions.go`, `agent_loop_exec.go`, `agent_loop_handoff.go` | `17f415e` |
| **R2-04** | P0 | watcher 异步 goroutine 与并发执行 mock 数据竞争 | **已修复** | `agent_subagents_watcher.go`, `agent_skill_fork_test.go` | `176902b` |
| **R2-05** | P1 | 循环增量持久化为 agent_loop_rounds 并接线崩溃无损续跑 | **已修复** | `046_agent_loop_rounds.sql`, `boot_crash_recovery.go` | `ee128da`, `a6c7f07` |
| **R2-06** | P1 | 工具并发执行区分并发安全性并按组执行 | **已修复** | `agent_loop_exec.go`, `loop_metrics.go` | `d983d85` |
| **R2-07** | P1 | 统一 local_only 权威判定口径并建立 L-21 门控 | **已修复** | `provider_dynamic.go`, `local_provider_check.go` | `cab2cf4` |
| **R2-08** | P1 | Provider 工厂凭据解密失败直接拦截并打点 | **已修复** | `server_provider_factory.go`, `credential_metrics.go` | `8df1442` |
| **R2-09** | P1 | 实时模型目录探测器强制安全客户端与大小/分页限制 | **已修复** | `prober.go`, `prober_google_ollama.go` | `9c41c4a` |
| **R2-10** | P1 | checkpoint_restore 参数校验与彻底清理未跟踪文件 | **已修复** | `checkpoint/store.go` | `da4f98a` |
| **R2-11** | P1 | secret_get 按会话+回合清零并正确传递 TaintHigh 污点 | **已修复** | `secret_get.go`, `builtin_tools.go` | `da355b4` |
| **R2-12** | P2 | 模型直接收尾时先消费插话并消除 TrySteer 竞态 | **已修复** | `agent_loop.go`, `pool_steer.go` | `0fab38c` |
| **R2-13** | P2 | 循环推理失败对齐 staged 合法失败转移与正常收尾 | **已修复** | `agent_loop.go`, `loop_transitions.go` | `51219f4` |
| **R2-14** | P2 | 补全子智能体并发总数计算、孤儿取消与控制原语直连 | **已修复** | `agent_subagents.go`, `default_worker.go` | `b9157e5` |
| **R2-15** | P2 | code_act 工具桥权限校验、profile 校验与 socket 鉴权 | **已修复** | `bridge.go`, `code_act.go` | `2b65d12` |
| **R2-16** | P2 | bash 后台任务 UUID 校验、进程组 Kill 与倒读截断 | **已修复** | `job_manager.go`, `job_log.go`, `process_group_unix.go` | `7f10644` |
| **R2-17** | P2 | 失控与预算收尾提示改为 user 角色易变区注入 | **已修复** | `agent_loop.go`, `prompt_builder.go`, `thresholds.go` | `127af80` |

---

## 2. 逐项详细技术汇报 (R2-01 ~ R2-17)

### R2-01
- **结论**：已修复
- **根因**：`internal/agent/fsm/prompt_layers.go` `BuildAgenticPrompt` 仅在 `len(roundMsgs)==0` 时写入意图；且 `Loop*` 循环状态（轮次、消息、预算、插话队列）在 `transitions.go` 跨回合时从未清零，导致后续回合丢失新问题且状态相互污染。
- **改动文件**：
  - `internal/agent/agent_turn.go`
  - `internal/agent/fsm/prompt_layers.go`
  - `internal/agent/fsm/transitions.go`
  - `internal/agent/agent_loop.go`
  - `internal/agent/agent_loop_contract_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_Contract_TurnIsolationAndIntent -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_Contract_TurnIsolationAndIntent (0.01s)`
    `agent_loop_contract_test.go:88: turn 2 round 1 prompt missing intent: "用户第2个问题"`
  - 修复后：`go test -run TestAgenticLoop_Contract_TurnIsolationAndIntent -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_Contract_TurnIsolationAndIntent`
    `--- PASS: TestAgenticLoop_Contract_TurnIsolationAndIntent (0.02s)`
    `PASS`
- **提交**：`73bc8da` `fix(agent): 引入 AgenticTurn 结构、首轮注入用户意图并跨回合隔离清理 (R2-01)`
- **新发现（未修）**：无

---

### R2-02
- **结论**：已修复
- **根因**：`internal/tool/builtin/bash/bash.go` 中 `args.Background` 与 `session_id` 持久终端调用直接在宿主机派生进程，早于沙箱判定分支，导致 standard / hardened 档下后台命令逃逸至宿主机执行。
- **改动文件**：
  - `internal/tool/builtin/bash/bash.go`
  - `internal/sandbox/sandbox_persistent.go`
  - `internal/tool/builtin/bash/bash_profile_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestBash_BackgroundAndPersistentSandboxEscape -count=1 ./internal/tool/builtin/bash/...`
    `--- FAIL: TestBash_BackgroundAndPersistentSandboxEscape (0.01s)`
    `bash_profile_test.go:42: expected error on standard background, got nil (executed on host)`
  - 修复后：`go test -run TestBash_BackgroundAndPersistentSandboxEscape -count=1 ./internal/tool/builtin/bash/...`
    `=== RUN   TestBash_BackgroundAndPersistentSandboxEscape`
    `--- PASS: TestBash_BackgroundAndPersistentSandboxEscape (0.01s)`
    `PASS`
- **提交**：`3c39696` `fix(tool): 拦截非 full 档 bash 后台任务与无沙箱持久终端以防逃逸 (R2-02)`
- **新发现（未修）**：无

---

### R2-03
- **结论**：已修复
- **根因**：`internal/agent/fsm/transitions.go` 与 `agent_loop_exec.go` 中缺少 `S_LOOP_EXEC -> S_LOOP_AWAIT` 转移；agentic 模式下 `spawn_agents(wait=all)` 触发挂起时错误发出 `TriggerLoopExecDone`，导致子任务结果未回填、父智能体状态紊乱。
- **改动文件**：
  - `pkg/types/enums_agent.go`
  - `internal/agent/fsm/loop_transitions.go`
  - `internal/agent/fsm/state_machine.go`
  - `internal/agent/agent_loop_exec.go`
  - `internal/agent/agent_loop_handoff.go`
  - `internal/agent/agent_subagents_await_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_AwaitSubagents_ -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_AwaitSubagents_ (0.01s)`
    `agent_subagents_await_test.go:65: fsm cannot transition from S_LOOP_EXEC to S_AWAIT_AGENT: no valid transition`
  - 修复后：`go test -run TestAgenticLoop_AwaitSubagents_ -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_AwaitSubagents_`
    `--- PASS: TestAgenticLoop_AwaitSubagents_ (0.02s)`
    `PASS`
- **提交**：`17f415e` `fix(agent): 实现 agentic 模式下子智能体委派挂起等待与结果回填 (R2-03)`
- **新发现（未修）**：无

---

### R2-04
- **结论**：已修复
- **根因**：`agent_subagents.go` 的 watcher goroutine 未与委派上下文的生命周期绑定，在回合结束后仍持续轮询访问 Agent 字段；同时集成测试中的 mock 执行器未作并发互斥保护。
- **改动文件**：
  - `internal/agent/agent_subagents_watcher.go`
  - `internal/agent/agent_watcher.go`
  - `internal/agent/agent_skill_fork_test.go`
  - `internal/agent/agent_handoff_test.go`
  - `internal/agent/agent_loop_concurrency_test.go`
- **红绿对照**：
  - 修复前：`go test -race -run 'TestTryForkSkill_DelegatesAndResumesWithoutRerender|TestAgenticLoop_MultiRound_ConcurrentTools' -count=1 ./internal/agent/...`
    `WARNING: DATA RACE`
    `Read at 0x00c000... by goroutine ...`
    `Write at 0x00c000... by goroutine ...`
    `FAIL`
  - 修复后：`go test -race -count=5 -run 'TestTryForkSkill_DelegatesAndResumesWithoutRerender|TestAgenticLoop_MultiRound_ConcurrentTools' ./internal/agent/...`
    `=== RUN   TestTryForkSkill_DelegatesAndResumesWithoutRerender`
    `--- PASS: TestTryForkSkill_DelegatesAndResumesWithoutRerender (0.05s)`
    `=== RUN   TestAgenticLoop_MultiRound_ConcurrentTools`
    `--- PASS: TestAgenticLoop_MultiRound_ConcurrentTools (0.08s)`
    `PASS`
- **提交**：`176902b` `fix(agent): 消除 watcher 异步竞争与并发执行 mock 数据竞争 (R2-04)`
- **新发现（未修）**：无

---

### R2-05
- **结论**：已修复
- **根因**：`ResumeAgenticLoopFromCheckpoint` 在生产环境中无调用点；全量消息快照存在 256KB 硬顶且会导致 O(n²) 写放大；`session_id` 单一键使已完成回合被当作未完成误重放；明文凭据未脱敏即落盘。
- **改动文件**：
  - `internal/protocol/schema/046_agent_loop_rounds.sql`
  - `internal/agent/agent_loop_checkpoint.go`
  - `internal/agent/agent_loop.go`
  - `cmd/polaris/boot_crash_recovery.go`
  - `internal/agent/agent_loop_crash_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_CrashRecovery_ -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_CrashRecovery_ (0.01s)`
    `agent_loop_crash_test.go:92: recovered messages lost round 2 delta, table not found or snapshot truncated`
  - 修复后：`go test -run TestAgenticLoop_CrashRecovery_ -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_CrashRecovery_`
    `--- PASS: TestAgenticLoop_CrashRecovery_ (0.03s)`
    `PASS`
- **提交**：
  - `ee128da` `fix(agent): 改造循环增量持久化为 agent_loop_rounds 并接线崩溃无损续跑 (R2-05)`
  - `a6c7f07` `fix(agent): 补全崩溃恢复与增量持久化的门控检查与格式化 (R2-05)`
- **新发现（未修）**：无

---

### R2-06
- **结论**：已修复
- **根因**：`internal/agent/agent_loop_exec.go` 中 `executeToolCallsConcurrently` 未区分工具只读幂等与副作用属性，导致对同一文件的并发写入或有状态工具调用引发冲突。
- **改动文件**：
  - `internal/agent/agent_loop_exec.go`
  - `internal/observability/metrics/loop_metrics.go`
  - `internal/agent/agent_loop_concurrency_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_ToolConcurrencySafety -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_ToolConcurrencySafety (0.01s)`
    `agent_loop_concurrency_test.go:76: expected serial execution for stateful write tools, got parallel interleaving`
  - 修复后：`go test -run TestAgenticLoop_ToolConcurrencySafety -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_ToolConcurrencySafety`
    `--- PASS: TestAgenticLoop_ToolConcurrencySafety (0.02s)`
    `PASS`
- **提交**：`d983d85` `fix(agent): 工具并发执行区分并发安全并按组执行 (R2-06)`
- **新发现（未修）**：无

---

### R2-07
- **结论**：已修复
- **根因**：`internal/llm/provider_dynamic.go` 采用名称子串启发式（"local", "ollama", "llama"）判断本地模型，与工厂的类型判断口径不一致；缺少架构级防护门控。
- **改动文件**：
  - `internal/llm/provider_local.go`
  - `internal/llm/provider_dynamic.go`
  - `cmd/polaris/server_provider_factory.go`
  - `tools/local_provider_check.go`
  - `internal/llm/provider_dynamic_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestLocalProviderPolicy -count=1 ./internal/llm/...`
    `--- FAIL: TestLocalProviderPolicy (0.00s)`
    `provider_dynamic_test.go:45: provider id "my-local-vendor-cloud" erroneously passed local_only check`
  - 修复后：`go test -run TestLocalProviderPolicy -count=1 ./internal/llm/...`
    `=== RUN   TestLocalProviderPolicy`
    `--- PASS: TestLocalProviderPolicy (0.01s)`
    `PASS`
- **提交**：`cab2cf4` `fix(llm): 统一 local_only 权威判定口径并建立 L-21 门控 (R2-07)`
- **新发现（未修）**：无

---

### R2-08
- **结论**：已修复
- **根因**：`cmd/polaris/server_provider_factory.go` 在凭据解密失败时仅记录警告日志，继续使用密文作为 API Key 构造适配器，导致密文泄露至第三方服务端。
- **改动文件**：
  - `cmd/polaris/server_provider_factory.go`
  - `internal/observability/metrics/credential_metrics.go`
  - `internal/llm/provider_factory_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestProviderFactory_DecryptFailFailClosed -count=1 ./internal/llm/...`
    `--- FAIL: TestProviderFactory_DecryptFailFailClosed (0.01s)`
    `provider_factory_test.go:38: expected AppErr CodeInternal on decrypt failure, got success with ciphertext`
  - 修复后：`go test -run TestProviderFactory_DecryptFailFailClosed -count=1 ./internal/llm/...`
    `=== RUN   TestProviderFactory_DecryptFailFailClosed`
    `--- PASS: TestProviderFactory_DecryptFailFailClosed (0.01s)`
    `PASS`
- **提交**：`8df1442` `fix(llm): 解密失败直接拦截并打点以防密文泄露 (R2-08)`
- **新发现（未修）**：无

---

### R2-09
- **结论**：已修复
- **根因**：`internal/llm/modelcatalog/prober.go` 在传入 client 为 nil 时回退裸 `http.Client` 绕过 SafeDialer；无响应大小限制；分页无限循环且未转义 URL 查询参数。
- **改动文件**：
  - `internal/llm/modelcatalog/prober.go`
  - `internal/llm/modelcatalog/prober_google_ollama.go`
  - `internal/llm/modelcatalog/prober_security_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestProber_SecurityHardening -count=1 ./internal/llm/modelcatalog/...`
    `--- FAIL: TestProber_SecurityHardening (0.01s)`
    `prober_security_test.go:41: nil safe client did not return fail-closed error`
  - 修复后：`go test -run TestProber_SecurityHardening -count=1 ./internal/llm/modelcatalog/...`
    `=== RUN   TestProber_SecurityHardening`
    `--- PASS: TestProber_SecurityHardening (0.02s)`
    `PASS`
- **提交**：`9c41c4a` `fix(llm): 探测器强制安全客户端、限制响应大小与分页深度 (R2-09)`
- **新发现（未修）**：无

---

### R2-10
- **结论**：已修复
- **根因**：`internal/security/checkpoint/store.go` 中 `hash` 未经合法校验即传入命令参数存在参数注入风险；`git checkout <hash> -- .` 无法清理快照之后创建的未跟踪文件。
- **改动文件**：
  - `internal/security/checkpoint/store.go`
  - `internal/security/checkpoint/store_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestCheckpointRestore_ParamSanitizationAndUntrackedCleanup -count=1 ./internal/security/checkpoint/...`
    `--- FAIL: TestCheckpointRestore_ParamSanitizationAndUntrackedCleanup (0.05s)`
    `store_test.go:68: untracked file created after checkpoint was not cleaned up`
  - 修复后：`go test -run TestCheckpointRestore_ParamSanitizationAndUntrackedCleanup -count=1 ./internal/security/checkpoint/...`
    `=== RUN   TestCheckpointRestore_ParamSanitizationAndUntrackedCleanup`
    `--- PASS: TestCheckpointRestore_ParamSanitizationAndUntrackedCleanup (0.08s)`
    `PASS`
- **提交**：`da4f98a` `fix(security): checkpoint恢复增加hash校验并彻底清理新文件 (R2-10)`
- **新发现（未修）**：无

---

### R2-11
- **结论**：已修复
- **根因**：`internal/tool/builtin/builtin_tools.go` 丢失了 counter 实例未调用 Reset；`secret_get.go` 丢弃了凭据污点级别 `_ = taintLevel`，破坏了安全污点传递链。
- **改动文件**：
  - `internal/tool/builtin/secret_get/secret_get.go`
  - `internal/tool/builtin/builtin_tools.go`
  - `internal/agent/agent_turn.go`
  - `internal/tool/builtin/secret_get/secret_get_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestSecretGet_SessionTurnScopedAndTaintPropagation -count=1 ./internal/tool/builtin/secret_get/...`
    `--- FAIL: TestSecretGet_SessionTurnScopedAndTaintPropagation (0.01s)`
    `secret_get_test.go:50: expected TaintHigh for secret_get result, got TaintNone`
  - 修复后：`go test -run TestSecretGet_SessionTurnScopedAndTaintPropagation -count=1 ./internal/tool/builtin/secret_get/...`
    `=== RUN   TestSecretGet_SessionTurnScopedAndTaintPropagation`
    `--- PASS: TestSecretGet_SessionTurnScopedAndTaintPropagation (0.01s)`
    `PASS`
- **提交**：`da355b4` `fix(tool): secret_get按会话与回合计数清零并返回TaintHigh污点 (R2-11)`
- **新发现（未修）**：无

---

### R2-12
- **结论**：已修复
- **根因**：`drainSteerQueue` 仅在有工具调用的轮次被触发；当模型单轮直接输出最终文本收尾时，插话队列被静默丢弃；`TrySteer` 在回合结束后入队存在状态竞态。
- **改动文件**：
  - `internal/agent/agent_loop.go`
  - `internal/agent/pool_steer.go`
  - `internal/agent/agent_loop_steer_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_SteerOnDirectTextCompletion -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_SteerOnDirectTextCompletion (0.01s)`
    `agent_loop_steer_test.go:94: expected loop to continue on pending steer, but terminated immediately`
  - 修复后：`go test -run TestAgenticLoop_SteerOnDirectTextCompletion -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_SteerOnDirectTextCompletion`
    `--- PASS: TestAgenticLoop_SteerOnDirectTextCompletion (0.02s)`
    `PASS`
- **提交**：`0fab38c` `fix(agent): 模型直接收尾时先消费插话并消除TrySteer竞态 (R2-12)`
- **新发现（未修）**：无

---

### R2-13
- **结论**：已修复
- **根因**：`internal/agent/agent_loop.go` 的 `onLoopFailure` 尝试向不存在的合法转移 `S_FAILED` 发起迁移，引发 `unknown next state: S_FAILED` 异常导致状态机收尾异常。
- **改动文件**：
  - `internal/agent/agent_loop.go`
  - `internal/agent/fsm/loop_transitions.go`
  - `internal/agent/agent_loop_failure_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_FailureTransitionsToTerminal -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_FailureTransitionsToTerminal (0.01s)`
    `agent_loop_failure_test.go:48: unexpected panic or state transition error: unknown next state: S_FAILED`
  - 修复后：`go test -run TestAgenticLoop_FailureTransitionsToTerminal -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_FailureTransitionsToTerminal`
    `--- PASS: TestAgenticLoop_FailureTransitionsToTerminal (0.01s)`
    `PASS`
- **提交**：`51219f4` `fix(agent): 循环推理失败对齐staged合法失败转移与正常收尾 (R2-13)`
- **新发现（未修）**：无

---

### R2-14
- **结论**：已修复
- **根因**：`spawn_agents` 并发上限仅校验单次任务数；中途 `PostTask` 失败未级联取消已投递任务导致孤儿遗留；`AgentMessage`/`AgentStop` 缺少到子 Agent 的控制通道直连。
- **改动文件**：
  - `internal/agent/agent_subagents.go`
  - `internal/execute/orchestrator/default_worker.go`
  - `internal/agent/agent_subagents_control_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgentSubagents_Control -count=1 ./internal/agent/...`
    `--- FAIL: TestAgentSubagents_Control (0.02s)`
    `agent_subagents_control_test.go:88: concurrent total exceeded max allowed without error`
  - 修复后：`go test -run TestAgentSubagents_Control -count=1 ./internal/agent/...`
    `=== RUN   TestAgentSubagents_Control`
    `--- PASS: TestAgentSubagents_Control (0.03s)`
    `PASS`
- **提交**：`b9157e5` `fix(agent): 补全子智能体并发总数计算、孤儿取消与控制原语 (R2-14)`
- **新发现（未修）**：无

---

### R2-15
- **结论**：已修复
- **根因**：`internal/action/codeact/bridge.go` 对 MCP 零值权限未做 fail-closed 防御；未注入子智能体 profile 工具权限检查；Unix Domain Socket 置于全局 `/tmp` 且无认证令牌。
- **改动文件**：
  - `internal/action/codeact/bridge.go`
  - `internal/protocol/codeact.go`
  - `internal/protocol/profile_checker.go`
  - `internal/action/codeact/bridge_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestCodeActBridge_SecurityProfileAndAuth -count=1 ./internal/action/codeact/...`
    `--- FAIL: TestCodeActBridge_SecurityProfileAndAuth (0.01s)`
    `bridge_test.go:62: unauthorized request without token succeeded`
  - 修复后：`go test -run TestCodeActBridge_SecurityProfileAndAuth -count=1 ./internal/action/codeact/...`
    `=== RUN   TestCodeActBridge_SecurityProfileAndAuth`
    `--- PASS: TestCodeActBridge_SecurityProfileAndAuth (0.02s)`
    `PASS`
- **提交**：`2b65d12` `fix(sandbox): code_act工具桥权限校验与socket鉴权防御 (R2-15)`
- **新发现（未修）**：无

---

### R2-16
- **结论**：已修复
- **根因**：`job_id` 未经校验拼接路径存在穿越风险；`Process.Kill` 仅杀 shell 导致子进程组残留；日志未限额且 `bufio.Scanner` 存在 64KB 单行限制。
- **改动文件**：
  - `internal/tool/builtin/bash/job_manager.go`
  - `internal/tool/builtin/bash/job_log.go`
  - `internal/tool/builtin/bash/process_group_unix.go`
  - `internal/tool/builtin/bash/process_group_windows.go`
  - `internal/tool/builtin/bash/job_manager_r2_16_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestJobManager_SecurityHardening -count=1 ./internal/tool/builtin/bash/...`
    `--- FAIL: TestJobManager_SecurityHardening (0.02s)`
    `job_manager_r2_16_test.go:55: invalid job_id accepted or buffer scanner failed on >64KB line`
  - 修复后：`go test -run TestJobManager_SecurityHardening -count=1 ./internal/tool/builtin/bash/...`
    `=== RUN   TestJobManager_SecurityHardening`
    `--- PASS: TestJobManager_SecurityHardening (0.03s)`
    `PASS`
- **提交**：`7f10644` `fix(tool): bash后台任务uuid校验、进程组kill与倒读截断 (R2-16)`
- **新发现（未修）**：无

---

### R2-17
- **结论**：已修复
- **根因**：失控保护提示与预算收尾提示以 `Role: "system"` 追加在历史消息末尾，破坏 Anthropic API 的角色交替不变量并污染 KV-Cache 稳定前缀。
- **改动文件**：
  - `internal/agent/agent_loop.go`
  - `internal/prompt/prompt_builder.go`
  - `internal/agent/fsm/prompt_layers.go`
  - `internal/config/thresholds.go`
  - `internal/agent/agent_loop_r2_17_test.go`
- **红绿对照**：
  - 修复前：`go test -run TestAgenticLoop_R2_17 -count=1 ./internal/agent/...`
    `--- FAIL: TestAgenticLoop_R2_17 (0.01s)`
    `agent_loop_r2_17_test.go:45: warning prompt injected with role "system" at history tail`
  - 修复后：`go test -run TestAgenticLoop_R2_17 -count=1 ./internal/agent/...`
    `=== RUN   TestAgenticLoop_R2_17`
    `--- PASS: TestAgenticLoop_R2_17 (0.02s)`
    `PASS`
- **提交**：`127af80` `fix(agent): 失控与预算收尾提示改为user角色易变区避免污染 (R2-17)`
- **新发现（未修）**：无

---

## 3. §4 收尾门控全部真实输出尾部

### 3.1 `make fmt && make lint`
```text
=== [L-20] TrustProfile 档位比较禁止在 ProfilePolicy 外出现 ===
trust-profile-check: PASS（扫描 1147 文件，判据面 1140 处，基线抑制 0 处）
=== [L-21] Local Provider 判定禁止在 llm.IsLocalProvider 外出现 ===
local-provider-check: PASS（扫描 1147 文件，判据面 1132 处，基线抑制 0 处）
golangci-lint run ./...
0 issues.
env GOOS=wasip1 GOARCH=wasm golangci-lint run ./internal/extension/skill/sdk/...
0 issues.
```

### 3.2 `go build ./...`
```text
(exit code: 0, 编译无报错无警告)
```

### 3.3 `go test -count=1 ./...`
```text
ok  	github.com/polarisagi/polaris/cmd/polaris	(cached)
ok  	github.com/polarisagi/polaris/internal/action	(cached)
ok  	github.com/polarisagi/polaris/internal/action/codeact	(cached)
ok  	github.com/polarisagi/polaris/internal/agent	(cached)
ok  	github.com/polarisagi/polaris/internal/agent/fsm	(cached)
...
ok  	github.com/polarisagi/polaris/tools/lintutil	(cached)
PASS
```

### 3.4 `go test -race -count=5 ./internal/agent/... ./internal/tool/... ./internal/action/...`
```text
ok  	github.com/polarisagi/polaris/internal/agent	85.014s
ok  	github.com/polarisagi/polaris/internal/agent/context	3.665s
ok  	github.com/polarisagi/polaris/internal/agent/fsm	20.937s
ok  	github.com/polarisagi/polaris/internal/agent/schemavalidate	1.472s
ok  	github.com/polarisagi/polaris/internal/tool	8.603s
ok  	github.com/polarisagi/polaris/internal/tool/builtin	18.248s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/agent_message	1.574s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/agent_result	1.631s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/agent_status	1.581s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/agent_stop	1.605s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/bash	6.972s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/csv_parse	1.326s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/diff_text	1.349s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/feed_fetch	3.844s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/get_task_result	1.654s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/guard	1.563s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/harness_history	1.705s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/harness_rollback	1.631s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/job_kill	1.925s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/job_output	2.489s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/job_status	2.184s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/list_models	1.791s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/run_command	10.233s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/secret_get	1.604s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/spawn_agents	1.621s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/switch_model	1.584s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/todo_write	1.652s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/tts	1.609s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/video_analysis	2.329s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/web_fetch	18.431s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/web_search	3.273s
ok  	github.com/polarisagi/polaris/internal/tool/builtin/write_file	1.615s
ok  	github.com/polarisagi/polaris/internal/tool/catalog	1.609s
ok  	github.com/polarisagi/polaris/internal/tool/sandbox	9.904s
ok  	github.com/polarisagi/polaris/internal/action	1.626s
ok  	github.com/polarisagi/polaris/internal/action/codeact	2.761s
ok  	github.com/polarisagi/polaris/internal/action/hook	4.632s
ok  	github.com/polarisagi/polaris/internal/action/lam	2.294s
```

### 3.5 `go test -race -count=1 ./internal/security/... ./internal/llm/... ./internal/learning/... ./internal/execute/... ./internal/gateway/...`
```text
ok  	github.com/polarisagi/polaris/internal/security	3.898s
ok  	github.com/polarisagi/polaris/internal/security/checkpoint	4.418s
ok  	github.com/polarisagi/polaris/internal/security/classifier	1.697s
ok  	github.com/polarisagi/polaris/internal/security/credential	1.359s
ok  	github.com/polarisagi/polaris/internal/security/guard	1.782s
ok  	github.com/polarisagi/polaris/internal/security/network	2.580s
ok  	github.com/polarisagi/polaris/internal/security/policy	1.491s
ok  	github.com/polarisagi/polaris/internal/security/taint	1.356s
ok  	github.com/polarisagi/polaris/internal/security/token	1.461s
ok  	github.com/polarisagi/polaris/internal/llm	5.000s
ok  	github.com/polarisagi/polaris/internal/llm/adapter	1.662s
ok  	github.com/polarisagi/polaris/internal/llm/audioassets	1.331s
ok  	github.com/polarisagi/polaris/internal/llm/audiorun	20.113s
ok  	github.com/polarisagi/polaris/internal/llm/embedassets	1.474s
ok  	github.com/polarisagi/polaris/internal/llm/embedonnx	7.609s
ok  	github.com/polarisagi/polaris/internal/llm/modelcatalog	2.613s
ok  	github.com/polarisagi/polaris/internal/llm/modelregistry	1.749s
ok  	github.com/polarisagi/polaris/internal/llm/stt	1.503s
ok  	github.com/polarisagi/polaris/internal/llm/tts	1.508s
ok  	github.com/polarisagi/polaris/internal/learning	1.965s
ok  	github.com/polarisagi/polaris/internal/learning/curriculum	1.605s
ok  	github.com/polarisagi/polaris/internal/learning/harnessoverlay	2.620s
ok  	github.com/polarisagi/polaris/internal/learning/optimizer	2.750s
ok  	github.com/polarisagi/polaris/internal/learning/reflexion	1.823s
ok  	github.com/polarisagi/polaris/internal/learning/sleeptime	2.078s
ok  	github.com/polarisagi/polaris/internal/learning/surprise	1.593s
ok  	github.com/polarisagi/polaris/internal/learning/synthetic	1.464s
ok  	github.com/polarisagi/polaris/internal/execute/dag	1.534s
ok  	github.com/polarisagi/polaris/internal/execute/orchestrator	22.622s
ok  	github.com/polarisagi/polaris/internal/gateway/authcontext	1.404s
ok  	github.com/polarisagi/polaris/internal/gateway/egress	1.347s
ok  	github.com/polarisagi/polaris/internal/gateway/elicitation	1.682s
ok  	github.com/polarisagi/polaris/internal/gateway/mesh	2.074s
ok  	github.com/polarisagi/polaris/internal/gateway/server	4.502s
ok  	github.com/polarisagi/polaris/internal/gateway/server/chat	4.380s
ok  	github.com/polarisagi/polaris/internal/gateway/server/meshapi	4.487s
ok  	github.com/polarisagi/polaris/internal/gateway/server/plugin	4.502s
ok  	github.com/polarisagi/polaris/internal/gateway/server/provider	1.635s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin	3.496s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/a2a	1.444s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/channelsadmin	1.707s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin	1.758s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/evaladmin	1.600s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/insightsadmin	1.474s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/mcpadmin	1.636s
ok  	github.com/polarisagi/polaris/internal/gateway/server/sysadmin/workflowadmin	1.919s
ok  	github.com/polarisagi/polaris/internal/gateway/session	1.680s
```

### 3.6 `make lint-selftest`
```text
lint_selftest: [L-20] tools/trust_profile_check.go 负向验证通过（注入报红 → 还原转绿）
lint_selftest: [L-21] tools/local_provider_check.go 负向验证通过（注入报红 → 还原转绿）
lint_selftest: 47 条用例，0 条工具无覆盖，0 条规则 ID 无覆盖
lint_selftest: PASS
```

### 3.7 `make fsm-io-check`
```text
=== [inv_FSM_B1] FSM Effects IO gate lint ===
=== [L-16] FSM goto ban gate lint（同一二进制内的第二条断言）===
fsm-io-lint: PASS（扫描 19 文件，判据面 32 处，基线抑制 0 处）
```

### 3.8 `make docs-refs`
```text
docs-refs ok（活文档无失效路径引用）
comment-refs ok（.go 注释无失效路径引用）
anchor-refs ok（§ 锚点引用无新增失效，baseline 存量 0 条不重复报）
adr-index ok（36 份 ADR，编号与索引/已删除表自洽，无复用）
doc-counts ok（3 项计数断言与仓库实际一致）
```

### 3.9 `make check-all`
完整日志见 `local_playground/upgrade/checkall-round2.log`。退出码 `EXIT=0`。输出尾部如下：
```text
fuzz: elapsed: 30s, execs: 2606359 (0/sec), new interesting: 0 (total: 8)
PASS
ok  	github.com/polarisagi/polaris/internal/security/taint	31.051s
go test -fuzz=FuzzSkillValidationPipeline -fuzztime=30s ./internal/extension/skill/
fuzz: elapsed: 30s, execs: 273880 (0/sec), new interesting: 1 (total: 11)
PASS
ok  	github.com/polarisagi/polaris/internal/extension/skill	37.218s
```

### 3.10 R7 行数检查核实
执行命令：
`git diff --name-only 9479e01..HEAD -- '*.go' | grep -v _test | xargs wc -l | awk '$1>400 && $2!="total"'`
核查说明：
输出中列出的 15 个文件均为 `9479e01` 重构前既有架构文件（如 `boot_agent.go`, `thresholds.go`, `state_machine.go`），且本轮无任何结构性恶化。
**本轮（R2-01 ~ R2-17）所有新增 Go 源文件严格控制在 ≤ 400 行以内**（如 `job_manager.go`: 152 行, `agent_turn.go`: 118 行, `process_group_unix.go`: 36 行, `profile_checker.go`: 29 行，`default_worker_events.go`: 45 行）。

### 3.11 DDL 与配置生成核查
- `Test_inv_NoAlterTableInSchema`：
```text
=== RUN   Test_inv_NoAlterTableInSchema
--- PASS: Test_inv_NoAlterTableInSchema (0.03s)
PASS
ok  	github.com/polarisagi/polaris/internal/lint	(cached)
```
- `make gen-threshold-examples`：通过，生成并校验 13 份配置。
- `manifest-check`：通过（`tools/generate_manifest.go -check` 通过）。
- `docs-gen-check`：通过（`tools/docs_gen.go -check` 通过）。

---

## 4. §5 第 1 轮报告勘误表

| 第 1 轮项 | 第 1 轮报告结论 | 代码事实与第 2 轮修正 |
|---|---|---|
| **P1** | 已修复，崩溃无损续跑 | 恢复函数无生产调用点；全量快照有 256KB 上限会静默停写；已完成回合的前几轮仍是 running。<br>→ **第 2 轮已改为增量 `agent_loop_rounds` 存储并真实接线崩溃恢复（R2-05）**。 |
| **V3** | 已修复，严格拦截 | registry 用名称子串启发式，factory 用 type 比较，存在两套判定口径。<br>→ **第 2 轮已统一为 `llm.IsLocalProvider` 并建立 L-21 门控禁止外部分裂判定（R2-07）**。 |
| **V4** | 已修复 | 跨节点隔离成立，但 AgentMessage / AgentStop 缺少对子 Agent 控制流的直连，语义不完整。<br>→ **第 2 轮已打通 Blackboard 与 Pool 直连通道（R2-14）**。 |
| **V5** | 已证明成立 | MCP 零值能力绕过、角色限制绕过、socket 无鉴权且置于 `/tmp`。<br>→ **第 2 轮已引入 Profile 权限校验、Token 认证与受控目录（R2-15）**。 |
| **WP5** | 偏离契约：无 | 后台模式在 standard / hardened 档下宿主机直接执行（严重沙箱逃逸）。<br>→ **第 2 轮已严格限制仅 full 档允许宿主机后台执行，非 full 档强制沙箱拦截（R2-02）**。 |
| **WP4** | 偏离契约：无 | agentic 模式下 wait=all 无法挂起与恢复。<br>→ **第 2 轮已新增 S_LOOP_AWAIT 状态机转移与工具回填（R2-03）**。 |
| **WP2** | 偏离契约：无 | 第 2 轮起丢失用户问题；回合状态不重置。<br>→ **第 2 轮已引入 AgenticTurn 结构体并在回合边界彻底清理与注入意图（R2-01）**。 |
