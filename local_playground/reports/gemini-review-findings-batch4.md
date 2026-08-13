# 代码轨道 批次 4 审核发现报告

## 批次汇总表

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-4-001 | P1 | internal/action | CodeAct Python Stateful 模式在 Sbx-L3 容器内打开宿主快照路径因未捕获 `FileNotFoundError` 导致脚本退出报错 | 高 | 是 |
| GR-4-002 | P1 | internal/agent | BuildReflectContext 组装反思 Prompt 时硬编码 ExecuteResult 污点等级为 TaintMedium 导致高污点标记静默丢失 | 高 | 是 |

置信度分布声明: 本批次所有条目均已完成 §2-A 强制反证核对与逻辑链路验证，均无需假设运行时未满足条件，符合高置信客观判据。

---

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

## 已审文件清单

- `internal/action/capability_token.go`
- `internal/action/codeact/code_act.go`
- `internal/action/codeact/code_act_checker.go`
- `internal/action/codeact/code_act_script_staging.go`
- `internal/action/codeact/code_act_stateful.go`
- `internal/action/doc.go`
- `internal/action/facade.go`
- `internal/action/hook/hook.go`
- `internal/action/hook/registry.go`
- `internal/action/hook/runner.go`
- `internal/action/lam/continuous_action.go`
- `internal/action/lam/display_server_xvfb.go`
- `internal/action/lam/lam.go`
- `internal/action/lam/streaming_action_bus.go`
- `internal/action/provider.go`
- `internal/action/tool_usage_policy.go`
- `internal/agent/agent.go`
- `internal/agent/agent_context_compaction.go`
- `internal/agent/agent_execute_dag.go`
- `internal/agent/agent_execute_effect.go`
- `internal/agent/agent_execute_effect_helpers.go`
- `internal/agent/agent_execute_memory.go`
- `internal/agent/agent_execute_result.go`
- `internal/agent/agent_execute_util.go`
- `internal/agent/agent_execute_validate.go`
- `internal/agent/agent_handoff.go`
- `internal/agent/agent_handoff_resume.go`
- `internal/agent/agent_lifecycle.go`
- `internal/agent/agent_wiring.go`
- `internal/agent/agent_wiring_dag.go`
- `internal/agent/budget.go`
- `internal/agent/context/assembler.go`
- `internal/agent/context/context_pressure.go`
- `internal/agent/context/memory_context.go`
- `internal/agent/context/persona_refiner.go`
- `internal/agent/context/pii_vault.go`
- `internal/agent/context/tool_list_section.go`
- `internal/agent/context/whisper.go`
- `internal/agent/context/workspace_context.go`
- `internal/agent/fsm/budget.go`
- `internal/agent/fsm/context_builder.go`
- `internal/agent/fsm/epoch.go`
- `internal/agent/fsm/state_machine.go`
- `internal/agent/fsm/state_machine_effects.go`
- `internal/agent/fsm/state_machine_prompts.go`
- `internal/agent/fsm/transitions.go`
- `internal/agent/pool.go`
- `internal/agent/prm.go`
- `internal/agent/provider.go`
- `internal/agent/reconciler_handoff.go`
- `internal/agent/recovery.go`
- `internal/agent/schemavalidate/schemavalidate.go`
- `internal/agent/step_scorer.go`
- `internal/agent/step_scorer_prm.go`

## 明确未覆盖的范围

无

## 审了但无发现的模块

- `internal/agent/fsm`（FSM 控制流及状态迁移定义）
- `internal/agent/schemavalidate`（JSON Schema 校验组件）
- `internal/action/hook`（Hook 注册与执行引擎）
- `internal/action/lam`（GUI Large Action Model 模块）
