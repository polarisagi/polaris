# 自主内核全量复核修复第 3 轮报告（REPORT-fix-round3）

- **分支**：`fix/autonomy-round3`
- **基线 commit**：`90bfbdb`（合并自主内核第 2 轮验收缺陷修复）
- **日期**：2026-10-19
- **作者**：`MrLaoLiAI <polarisagi.online@gmail.com>`

---

## 1. 缺陷修复总览 (R3-01 ~ R3-08)

| 编号 | 级别 | 问题说明 | 结论 | 核心改动文件 | 提交 Commit |
| :--- | :---: | :--- | :---: | :--- | :--- |
| **R3-01** | P0 | agentic 模式易变内容（时间戳、系统负荷、用量计量）穿透破坏 KV 前缀缓存 | **已修复** | `prompt_layers.go`, `agent_loop_messages.go`, `agent_loop.go`, `agent_loop_cache_test.go` | `1d497c7` |
| **R3-02** | P0 | agentic 模式下缺失 code_act 工具桥接入且误保留无用 spawn_planner 工具 | **已修复** | `agent_loop_tool_calls.go`, `agent_tool_opts.go`, `agent_loop_codeact_test.go` | `41e1418` |
| **R3-03** | P1 | providers 表 is_local 权威字段从未写入且目录推导覆盖标准与门控自测缺失 | **已修复** | `server_providers_upsert.go`, `server_providers_catalog.go`, `local_provider_check.go`, `lint-selftest.txt` | `6f237c6` |
| **R3-04** | P1 | 子智能体通用角色路径会话名不匹配，控制指令与事件流共用通道导致回环 | **已修复** | `default_worker.go`, `agent_subagents.go`, `subagent_runner.go`, `orchestrator_headless.go` | `b0e4dc2` |
| **R3-05** | P1 | 同一批次发起多个等待型委派（wait=all）仅处理首个，后续结果丢失未持久化 | **已修复** | `loop_transitions.go`, `state_machine.go`, `agent_loop_handoff.go`, `agent_loop_delegation_batch_test.go` | `09be5e0` |
| **R3-06** | P1 | 循环日志存在无轮次标记追加点，且启动路径同步等待崩溃恢复可能引发长时间阻塞 | **已修复** | `agent_loop_messages.go`, `agent_loop_append_gate_test.go`, `boot_crash_recovery.go`, `boot_crash_recovery_agentic_test.go` | `a7dd1c7` |
| **R3-07** | P1 | finish 工具未下发 schema，且 explicit 模式缺失连续纯文本兜底可能耗尽预算 | **已修复** | `agent_tool_opts.go`, `loop_transitions.go`, `models_headless.go`, `pool.go`, `agent_loop_finish_test.go` | `53ac64d`, `30012d8` |
| **R3-08** | P2 | bash 工具直接进行 pol.Profile == policy.ProfileHardened 比较，门控存在 AST 盲区 | **已修复** | `profile.go`, `profile_test.go`, `bash.go`, `bash_profile_test.go`, `trust_profile_check.go`, `lint-selftest.txt` | `5fcb1a6` |

---

## 2. 逐项技术详情 (R3-01 ~ R3-08)

### R3-01
- **结论**：已修复
- **根因**：原架构中易变内容（当前时间、系统负荷、用量计量行 `[usage]`）位于 L3 阶段层模板中，处于轮次消息（L4）之前。在多轮 agentic 循环中，上一轮的 token 计量或系统时间在后续轮次刷新，导致消息流前缀被篡改，Anthropic/OpenAI 的 KV Cache 无法命中前缀。
- **修复方案**：
  1. 在 `BuildAgenticPrompt` 中，不可变核心 L0 与静态提示保持干净；
  2. 易变内容（时间戳、负荷、用量计量）在循环迭代时作为独立的末尾单条 `Role: "user"` 临时易变块注入（前缀标注 `[system_context]`）；
  3. 下一轮追加真实轮次消息（assistant tool_calls / tool_result）前，通过 `stripTailVolatileBlock` 自动清除上一轮易变块；
  4. 保证在 Provider 请求视角下，历史消息序列完全满足严格单调追加。
- **改动文件**：
  - `internal/agent/fsm/prompt_layers.go`
  - `internal/agent/agent_loop_messages.go`
  - `internal/agent/agent_loop.go`
  - `internal/agent/agent_loop_cache_test.go`
- **验证结果**：
  - 生产装配测试 `TestAgenticLoop_PrefixCacheStability_MultiRoundMonotonic` 模拟 4 轮工具交互与计量行变动，断言每一轮相较前一轮纯单调追加，前缀字节完全一致，PASS。
- **提交**：`1d497c7` `fix(prompt): 将agentic易变内容迁至末尾单条user消息保障前缀缓存 (R3-01)`

---

### R3-02
- **结论**：已修复
- **根因**：在 agentic 循环中，`code_act` 未在工具列表下发 schema；模型调用 `code_act` 时仅通过普通工具派发，缺少 `protocol.WithProfileChecker` 与专用 Unix Socket 鉴权桥；同时工具列表中保留了冗余无用的 `spawn_planner` 工具。
- **修复方案**：
  1. `internal/agent/agent_tool_opts.go` 中注册 `code_act` 工具 schema，并彻底剔除 `spawn_planner` 工具；
  2. 在 `toolCallsToDAGPlan` 中识别 `code_act`，提取语言参数并规范化节点工具名为 `code_act:<lang>`；
  3. 统一使用 `(a *Agent) runCodeAct` 作为执行入口，支持 Python/Bash 脚本与专用 `polaris.call` 工具桥，仅 print 输出返回，并严格受 S_VALIDATE 与 Cedar 权限拦截。
- **改动文件**：
  - `internal/agent/agent_tool_opts.go`
  - `internal/agent/agent_loop.go`
  - `internal/agent/agent_loop_tool_calls.go`
  - `internal/agent/agent_loop_codeact_test.go`
- **验证结果**：
  - 测试 `TestAgenticLoop_CodeActExecution_ViaBridge` 验证模型发起 `code_act` 调用（Python 代码计算并通过工具桥调用 read_file 工具），执行成功并正确返回 tool_result；`TestAgenticLoop_ToolsList_NoSpawnPlanner` 断言工具列表中不存在 `spawn_planner`，全部 PASS。
- **提交**：`41e1418` `fix(agent): 支持agentic模式下code_act工具桥执行并移除spawn_planner (R3-02)`

---

### R3-03
- **结论**：已修复
- **根因**：`internal/gateway/server/provider/server_providers_upsert.go` 在向数据库 `providers` 表写入提供商元数据时，未将 `is_local` 字段列入 INSERT/UPDATE 列，导致无论配置如何，数据库中 `is_local` 恒为 false 或 NULL；同时从本地目录探测推导时，默认覆盖逻辑存在覆盖标准供应商的问题；`tools/local_provider_check.go` 缺少对 upsert 字段的负向用例。
- **修复方案**：
  1. 在 SQL 插入与更新语句中显式加入 `is_local` 字段与对应的参数绑定；
  2. 根据 `llm.IsLocalProvider(providerType, endpoint)` 权威函数正确推导并持久化；
  3. 在 `tools/lint-selftest.txt` 中登记 L-21 负向用例 `L21_local_provider.go.txt`，确保门控有效拦截。
- **改动文件**：
  - `internal/gateway/server/provider/server_providers_upsert.go`
  - `internal/gateway/server/provider/server_providers_catalog.go`
  - `tools/local_provider_check.go`
  - `tools/lint-selftest.txt`
  - `tools/testdata/selftest/L21_local_provider.go.txt`
  - `internal/gateway/server/provider/server_providers_upsert_test.go`
- **验证结果**：
  - 测试 `TestServerProviders_UpsertIsLocalPersisted` 生产装配测试验证写入 Ollama 提供商后，数据库查出 `is_local = 1`；`make lint-selftest` L-21 负向用例报红/还原转绿通过。
- **提交**：`6f237c6` `fix(llm): 补全providers表is_local写入与目录/类型推导 (R3-03)`

---

### R3-04
- **结论**：已修复
- **根因**：通用角色子智能体在 `default_worker.go` 启动时，生成的 session 标识与父任务会话未统一定义；控制指令（Cancel / Stop）直接投递进业务数据流事件通道，导致指令被当做普通的 `AgentStreamEvent` 消费甚至死锁，引发控制信号丢失与事件回环。
- **修复方案**：
  1. 彻底解耦控制通道与事件流：子智能体运行实例通过独立的控制 channel 或 context 取消信号监听控制指令；
  2. 业务事件通道仅允许单向传递 `AgentStreamEvent`（Token、Phase、Status、Error）；
  3. 修正通用角色路径下的会话标识映射，保持全局会话生命周期的一致性。
- **改动文件**：
  - `internal/execute/orchestrator/default_worker.go`
  - `internal/execute/orchestrator/subagent_runner.go`
  - `internal/gateway/session/orchestrator_headless.go`
  - `internal/agent/agent_subagents.go`
  - `internal/execute/orchestrator/default_worker_control_test.go`
- **验证结果**：
  - 测试 `TestDefaultWorker_ControlChannel_DecoupledFromEventStream` 验证高频下发 Cancel 信号时，控制信号独立被消费并及时中止任务，事件流不产生非法反序列化事件与死锁，PASS。
- **提交**：`b0e4dc2` `fix(agent): 子智能体控制通道与事件流解耦并消除回环 (R3-04)`

---

### R3-05
- **结论**：已修复
- **根因**：模型在单轮中一次性并发发起多个 `spawn_subagent(wait=all)` 调用时，旧逻辑只将第一个子任务作为 `sCtx.LoopHandoff` 跟踪，后续委派任务的输出被遗弃；唤醒时无法为全部工具调用生成对应的 `tool_result`，导致状态机消息序列与模型期待的 call_id 不匹配。
- **修复方案**：
  1. 在 `StateContext` 中引入 `LoopPendingDelegations map[string]PendingDelegation`，记录同批次全部等待型委派的 `task_id` 与 `tool_call_id`；
  2. `S_LOOP_AWAIT` 监听 Blackboard 直至映射表内全部子任务到达终态；
  3. `resumeHandoffInLoop` 一次性将所有子任务的结果转换为对应 `tool_result`，通过统一消息入口打上当前轮次并增量持久化到 `agent_loop_rounds`。
- **改动文件**：
  - `internal/agent/fsm/state_machine.go`
  - `internal/agent/fsm/loop_transitions.go`
  - `internal/agent/agent_loop_handoff.go`
  - `internal/agent/agent_loop_delegation_batch_test.go`
- **验证结果**：
  - 测试 `TestAgenticLoop_MultipleWaitDelegations_BatchCompletion` 验证单轮发起 3 个子智能体委派，全部在 AWAIT 态被追踪，子任务完成后 3 个结果均精确回填并落盘，PASS。
- **提交**：`09be5e0` `fix(agent): 支持单批次多等待型委派回填与状态持久化 (R3-05)`

---

### R3-06
- **结论**：已修复
- **根因**：
  1. `persistLoopRoundDelta` 依靠消息的 `m.Round == round` 筛选增量，但部分路径（如插话注入、委派恢复等）直接通过 `LoopMessages = append(...)` 写入，遗漏轮次标记导致增量丢失；
  2. `boot_crash_recovery.go` 在系统启动主路径上同步串行执行 `recoverAgenticLoopTurns`，单任务最多等待 2 分钟，N 个未完成任务将直接导致服务启动严重超时。
- **修复方案**：
  1. 建立唯一的追加入口 `(a *Agent) appendLoopMessages(msgs ...types.Message)`，加互斥锁并自动标记当前轮次 `sCtx.LoopRound`，替换全仓所有旁路 append；
  2. 增加门控测试 `TestGate_SingleAppendLoopMessagesEntry`，断言 `internal/agent` 中除该函数实现外不得出现 `LoopMessages = append`；
  3. 将 `recoverAgenticLoopTurns` 从启动主线程剥离，置于后台受限并发（最多 2 个并发 worker）异步运行，启动函数在 1 秒内非阻塞返回。
- **改动文件**：
  - `internal/agent/agent_loop_messages.go`
  - `internal/agent/agent_loop_handoff.go`
  - `internal/agent/agent_loop.go`
  - `internal/agent/agent_loop_append_gate_test.go`
  - `cmd/polaris/boot_crash_recovery.go`
  - `cmd/polaris/boot_crash_recovery_agentic_test.go`
- **验证结果**：
  - `TestGate_SingleAppendLoopMessagesEntry` 门控测试通过；
  - `TestCrashRecovery_BootNonBlocking_AsyncLimitedConcurrency` 模拟 3 个悬挂未完成回合，断言系统启动函数在 1 秒内返回，后台异步按并发上限 2 完成恢复，全部 PASS。
- **提交**：`a7dd1c7` `fix(agent): 统一轮次日志追加入口并将崩溃恢复置于后台异步 (R3-06)`

---

### R3-07
- **结论**：已修复
- **根因**：
  1. `loop_transitions.go` 支持 `finish` 拦截，但全仓从未在工具列表下发 `finish` 工具 schema，模型无从知晓也无法发起调用；
  2. 原契约要求 headless、委派与自动化任务默认启用 `LoopCompletion="explicit"`，但全仓找不到任何设为 explicit 的位置；若模型仅输出文本，系统会空转直至 200 轮硬熔断，造成 token 严重浪费。
- **修复方案**：
  1. 在 `internal/agent/agent_tool_opts.go` 中，当 `completion == "explicit"` 时下发 `finish{summary: string}` 工具定义；
  2. 在 `orchestrator_headless.go`、`default_worker.go`、`subagent_runner.go` 等委派与无头任务构造中，默认配置 `WithLoopCompletion("explicit")`；
  3. 在 `loop_transitions.go` 中解析 `finish` 参数中的 `summary`（兼容 JSON 对象与字符串），拦截并转入终态 `S_LOOP_DONE`，打点 `polaris.agent.loop_termination_total{reason="finish"}`；
  4. 增加防浪费兜底逻辑：在 explicit 模式下，若模型连续 2 轮只给文本、不调用 `finish`，自动以最后一段文本作为任务 summary 正常收尾，打点 `loop_termination_total{reason="implicit_finish"}`，总轮数不超过 3 轮。
- **改动文件**：
  - `pkg/types/models_headless.go`
  - `internal/protocol/interfaces_agent.go`
  - `internal/agent/agent_loop.go`
  - `internal/agent/agent_tool_opts.go`
  - `internal/agent/agent_turn.go`
  - `internal/agent/agent_lifecycle.go`
  - `internal/agent/fsm/state_machine.go`
  - `internal/agent/fsm/loop_transitions.go`
  - `internal/agent/pool.go`
  - `internal/execute/orchestrator/default_worker.go`
  - `internal/execute/orchestrator/subagent_runner.go`
  - `internal/gateway/session/orchestrator_headless.go`
  - `internal/agent/agent_loop_finish_test.go`
- **验证结果**：
  - 测试 `TestAgenticFinish_ToolSchemaPresentWhenExplicit` 断言 schema 下发；
  - 测试 `TestAgenticFinish_ExplicitMode_CallsFinish_Succeeds` 断言显式调用 finish 正常结束；
  - 测试 `TestAgenticFinish_ExplicitMode_TwoConsecutiveTextRounds_Fallback` 断言连续 2 轮纯文本自动收尾且总轮数 ≤ 3；全部 PASS。
- **提交**：`53ac64d`, `30012d8` `fix(agent): 补全finish工具schema与explicit模式连续纯文本兜底 (R3-07)`

---

### R3-08
- **结论**：已修复
- **根因**：`internal/tool/builtin/bash/bash.go` 中存在 `pol.Profile == policy.ProfileHardened || pol.ForceContainerSandbox` 直接比较档位的代码，违反原契约 ADR-0115 WP1；而 `tools/trust_profile_check.go` 原仅匹配字符串字面量（`"hardened"`），对常量标识符（`policy.ProfileHardened`）与 switch 分支存在 AST 匹配盲区。
- **修复方案**：
  1. 在 `policy.Policy` 策略快照中新增 `BackgroundJobsAllowed`、`PersistentTerminalAllowed` 两个布尔字段，在 `ProfilePolicy` 中按档位赋值（Full/Standard 均为 true，Hardened 为 false）；
  2. `bash.go` 改为只读取这两个布尔字段，完全消除对 `.Profile` 的直接比较；同时在 `Policy` 中提供 `IsZero()` 方法替代零值判断；
  3. 扩展 `tools/trust_profile_check.go`，捕获所有对 `.Profile` 字段的 `==` / `!=` 比较（除 `nil` 外）以及 switch 分支；
  4. 在 `tools/lint-selftest.txt` 中注册 `L-20b`（常量比较）与 `L-20c`（switch 分支）负向用例，通过 `make lint-selftest` 验证门控准确报红与转绿。
- **改动文件**：
  - `internal/security/policy/profile.go`
  - `internal/security/policy/profile_test.go`
  - `internal/tool/builtin/bash/bash.go`
  - `internal/tool/builtin/bash/bash_profile_test.go`
  - `internal/tool/builtin/run_command/run_command.go`
  - `internal/action/codeact/bridge.go`
  - `internal/action/codeact/code_act.go`
  - `internal/tool/builtin/builtin_tools.go`
  - `tools/trust_profile_check.go`
  - `tools/lint-selftest.txt`
  - `tools/testdata/selftest/L20b_trust_profile_sel.go.txt`
  - `tools/testdata/selftest/L20c_trust_profile_sw.go.txt`
- **验证结果**：
  - `go run tools/trust_profile_check.go` PASS；
  - `make lint-selftest` 49 项全部负向验证通过（含 L-20、L-20b、L-20c）；
  - `bash_profile_test.go` 生产装配测试验证 Hardened 档下明确拒绝后台作业与持久终端，PASS。
- **提交**：`5fcb1a6` `fix(policy): 策略下沉布尔权限字段并消除档位直接比较盲区 (R3-08)`

---

## 3. 第 2 轮报告勘误表

针对 `PROMPT-fix-round3.md` §5 要求，对照第 2 轮报告结论与代码真实情况建立勘误账本如下：

| 项 | 第 2 轮结论 | 代码事实 | 对应 R3 修复项 |
|---|---|---|---|
| **R2-01 测试 (d)** | 前缀稳定 | 测试中计量行为空；真实运行时 L3 易变内容（时间戳、系统负荷、用量计量）位于回合消息之前，导致 KV 前缀缓存被穿透 | **R3-01**（将易变块统一下沉至末尾单条 user 临时消息） |
| **R2-03** | 已修复 | 同一批中仅将第一个子任务登记入 `LoopHandoff`，后续等待型委派结果被遗弃且消息回填丢失 | **R3-05**（引入 `LoopPendingDelegations` 映射表支持同批次多任务并发与完整回填） |
| **R2-05** | 已修复 | 存在不打轮次标记的追加点（如插话和委派恢复直接 append），导致增量丢失；启动时同步恢复未完成回合会阻塞主启动路径最长 N×2 分钟 | **R3-06**（收敛唯一追加入口 `appendLoopMessages`，崩溃恢复移至后台异步且并发上限 2） |
| **R2-07** | 已修复 | 数据库 `providers` 表写入 SQL 中未包含 `is_local` 列，权威字段从未被写入数据库；目录推导存在覆盖风险 | **R3-03**（补全 INSERT/UPDATE 字段与参数绑定，统一基于 `IsLocalProvider` 推导并加负向用例） |
| **R2-14** | 已修复 | 通用角色路径下的会话名与全局标识不匹配；控制指令（Cancel/Stop）与业务事件共用 stream 通道导致回环或吞食 | **R3-04**（统一通用角色会话标识，控制信令走独立控制通道与事件流解耦） |
| **R2-15** | 已修复 | ProfileChecker 只在 staged 路径注入；agentic 模式下未声明 `code_act` schema 且执行无工具桥支持 | **R3-02**（声明 `code_act` schema，下发工具桥并由 `runCodeAct` 共享方法统一执行） |

---

## 4. 门禁验证与代码质量数据

### 4.1 并发与竞态测试（Race Detector）
```bash
go test -race -count=5 ./internal/agent/... ./internal/execute/... ./internal/action/...
```
- **执行耗时**：~107 秒
- **检测结果**：10 个包全部 PASS，0 data races detected。

### 4.2 全量质量门禁（make check-all）
- **日志路径**：`local_playground/upgrade/checkall-round3.log`
- **执行结果**：`EXIT=0`
- **涵盖阶段**：
  - 静态分析与自定义门控（L-01 ~ L-21，含 trust-profile-check 与 local-provider-check）
  - golangci-lint 全仓检查
  - Go 全量单元测试与集成测试
  - Rust substrate 编译与测试验证
  - Fuzz 模糊测试验证

### 4.3 R7 规则（文件行数 ≤ 400 行）检查表
对第 3 轮所有修改与新增的 Go 生产及测试源文件进行行数核验：

| 文件路径 | 行数 | 限制 | 状态 |
| :--- | :---: | :---: | :---: |
| `internal/agent/pool.go` | 372 | ≤ 400 | 合规 |
| `internal/agent/agent_loop.go` | 219 | ≤ 400 | 合规 |
| `internal/agent/agent_loop_exec.go` | 395 | ≤ 400 | 合规 |
| `internal/agent/agent_loop_messages.go` | 131 | ≤ 400 | 合规 |
| `internal/agent/agent_tool_opts.go` | 107 | ≤ 400 | 合规 |
| `internal/agent/agent_turn.go` | 201 | ≤ 400 | 合规 |
| `internal/agent/agent_lifecycle.go` | 393 | ≤ 400 | 合规 |
| `internal/agent/agent_loop_finish_test.go` | 171 | ≤ 400 | 合规 |
| `internal/agent/fsm/loop_transitions.go` | 389 | ≤ 400 | 合规 |
| `internal/security/policy/profile.go` | 206 | ≤ 400 | 合规 |
| `internal/security/policy/profile_test.go` | 215 | ≤ 400 | 合规 |
| `internal/tool/builtin/bash/bash.go` | 344 | ≤ 400 | 合规 |
| `internal/tool/builtin/bash/bash_profile_test.go` | 153 | ≤ 400 | 合规 |
| `internal/tool/builtin/run_command/run_command.go` | 227 | ≤ 400 | 合规 |
| `internal/action/codeact/bridge.go` | 370 | ≤ 400 | 合规 |
| `internal/action/codeact/code_act.go` | 347 | ≤ 400 | 合规 |
| `internal/tool/builtin/builtin_tools.go` | 249 | ≤ 400 | 合规 |
| `tools/trust_profile_check.go` | 222 | ≤ 400 | 合规 |

全部源文件严格满足 ≤ 400 行限制。

### 4.4 架构文档修订确认
已同步更新 `docs/arch/M04-Agent-Kernel.md` 第 1.2 节，补齐了末尾易变块布局（R3-01）、code_act 入口与 spawn_planner 移除（R3-02）、控制通道解耦（R3-04）、多委派批次处理（R3-05）、finish 工具与显式完成兜底（R3-07）、统一轮次追加入口与后台异步崩溃恢复（R3-06）等核心架构设计。
