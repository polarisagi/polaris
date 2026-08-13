# Polaris 统一审核报告（批次 3）

置信度分布声明: 本批次全部发现均对应确切源码/文档位置，且已完成 §2-A 反证核对。

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-3-001 | P1 | internal/bootstrap | 04-Module-Boundary.md 声明 internal/bootstrap 被 cmd/polaris 引用与代码/架构文档矛盾 | 高 | 是 |
| GR-3-002 | P1 | internal/bootstrap | Bootstrapper 四阶关停按 map 随机序遍历破坏逆序依赖优雅关停 | 高 | 是 |
| GR-3-003 | P2 | internal/bootstrap | Bootstrapper.Ignite 模块初始化中途失败缺乏已初始化模块的回滚清理 | 高 | 是 |

---

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

## 已审文件清单

- `api/proto/blackboard.proto`
- `api/proto/event.proto`
- `api/proto/mutation.proto`
- `cmd/polaris/adapters_agent.go`
- `cmd/polaris/adapters_eval.go`
- `cmd/polaris/adapters_mcp_a2a.go`
- `cmd/polaris/adapters_mcp_async.go`
- `cmd/polaris/adapters_memory.go`
- `cmd/polaris/adapters_misc.go`
- `cmd/polaris/adapters_security.go`
- `cmd/polaris/adapters_surreal.go`
- `cmd/polaris/benchmark.go`
- `cmd/polaris/boot_agent.go`
- `cmd/polaris/boot_crash_recovery.go`
- `cmd/polaris/boot_events.go`
- `cmd/polaris/boot_handoff_reconciler.go`
- `cmd/polaris/boot_knowledge.go`
- `cmd/polaris/boot_memory.go`
- `cmd/polaris/boot_server.go`
- `cmd/polaris/boot_stream_audit.go`
- `cmd/polaris/boot_substrate.go`
- `cmd/polaris/boot_tools.go`
- `cmd/polaris/cli.go`
- `cmd/polaris/cli_allowlist.go`
- `cmd/polaris/cli_eval.go`
- `cmd/polaris/cli_eval_bench.go`
- `cmd/polaris/cli_eval_cigate.go`
- `cmd/polaris/cli_fanout.go`
- `cmd/polaris/cli_i18n.go`
- `cmd/polaris/cli_release_key.go`
- `cmd/polaris/cli_skill.go`
- `cmd/polaris/cli_unseal.go`
- `cmd/polaris/cli_vault.go`
- `cmd/polaris/main.go`
- `cmd/polaris/migrate_openclaw.go`
- `cmd/polaris/migrate_openclaw_memory.go`
- `cmd/polaris/process_staging.go`
- `cmd/polaris/server_provider_loader.go`
- `cmd/polaris/server_stt_tts.go`
- `cmd/polaris/skill_loader.go`
- `cmd/polaris/version.go`
- `configs/automations/templates/builtin.yaml`
- `configs/automations/templates/index.json.example`
- `configs/defaults.toml`
- `configs/embed.go`
- `configs/extensions/automation_sources.yaml`
- `configs/extensions/marketplaces.yaml`
- `configs/extensions/registry.yaml`
- `configs/extensions/trusted-publishers.yaml`
- `configs/loader.go`
- `configs/policy/hard_constraints.cedar`
- `configs/policy/memory.cedar`
- `configs/policy/soft_constraints.cedar`
- `configs/prompts/identity.md`
- `configs/prompts/kernel/computer_use_policy.md`
- `configs/prompts/kernel/perceive.md`
- `configs/prompts/kernel/plan.md`
- `configs/prompts/kernel/reflect.md`
- `configs/prompts/operational/coding_style.md`
- `configs/prompts/operational/execution_discipline.md`
- `configs/prompts/operational/memory_hygiene.md`
- `configs/prompts/operational/output_efficiency.md`
- `configs/prompts/operational/risky_actions.md`
- `configs/prompts/operational/task_completion.md`
- `configs/prompts/operational/tool_use.md`
- `configs/prompts/platform/api.md`
- `configs/prompts/platform/cli.md`
- `configs/prompts/platform/cron.md`
- `configs/prompts/platform/telegram.md`
- `configs/prompts/platform/webui.md`
- `configs/prompts/system_prompt.md`
- `configs/prompts/tool_enforcement/deepseek.md`
- `configs/prompts/tool_enforcement/google.md`
- `configs/prompts/tool_enforcement/openai.md`
- `configs/threshold-examples/m10_knowledge.toml`
- `configs/threshold-examples/m11_policy.toml`
- `configs/threshold-examples/m12_eval.toml`
- `configs/threshold-examples/m13_interface.toml`
- `configs/threshold-examples/m1_router.toml`
- `configs/threshold-examples/m2_storage.toml`
- `configs/threshold-examples/m3_observability.toml`
- `configs/threshold-examples/m4_kernel.toml`
- `configs/threshold-examples/m5_memory.toml`
- `configs/threshold-examples/m6_skill.toml`
- `configs/threshold-examples/m7_tool.toml`
- `configs/threshold-examples/m8_orchestrator.toml`
- `configs/threshold-examples/m9_self_improve.toml`
- `internal/bootstrap/bootable.go`
- `internal/bootstrap/bootstrapper.go`
- `internal/config/config.go`
- `internal/config/config_types.go`
- `internal/config/immutable_constants.go`
- `internal/config/integrity_check.go`
- `internal/config/kernel_manifest.json`
- `internal/config/layout.go`
- `internal/config/runtime.go`
- `internal/config/thresholds.go`
- `internal/config/trusted_publishers.go`
- `internal/lint/testdata/bare_error_return_baseline.json`
- `internal/lint/testdata/errcheck_baseline.json`
- `internal/lint/testdata/errcheck_enforced_dirs.json`
- `internal/lint/testdata/errcheck_exempt.json`
- `internal/lint/testdata/ffi_boundary_exempt.json`
- `internal/lint/testdata/file_line_limit_baseline.json`
- `internal/lint/testdata/gateway_db_write_baseline.json`
- `internal/lint/testdata/global_var_baseline.json`
- `internal/lint/testdata/m13_raw_http_exempt.json`
- `internal/lint/testdata/raw_http_calls_exempt.json`
- `internal/lint/testdata/raw_net_dial_exempt.json`
- `internal/lint/testdata/sql_db_field_exempt.json`
- `internal/lint/testdata/taint_content_approved_calls.json`
- `internal/lint/testdata/turn_entry_exempt.json`
- `internal/lint/testdata/vfs_quota_exempt.json`
- `internal/lint/testdata/xr06_raw_transport_exempt.json`
- `internal/protocol/codeact.go`
- `internal/protocol/cognitive.go`
- `internal/protocol/context.go`
- `internal/protocol/dag_node.go`
- `internal/protocol/dag_validation.go`
- `internal/protocol/errors.go`
- `internal/protocol/extensions.go`
- `internal/protocol/ffi-abi.md`
- `internal/protocol/immutable_core.go`
- `internal/protocol/interfaces_agent.go`
- `internal/protocol/interfaces_automation.go`
- `internal/protocol/interfaces_channel.go`
- `internal/protocol/interfaces_eval.go`
- `internal/protocol/interfaces_event.go`
- `internal/protocol/interfaces_llm.go`
- `internal/protocol/interfaces_memory.go`
- `internal/protocol/interfaces_net.go`
- `internal/protocol/interfaces_other.go`
- `internal/protocol/interfaces_prompt.go`
- `internal/protocol/interfaces_skill.go`
- `internal/protocol/interfaces_store.go`
- `internal/protocol/interfaces_swarm.go`
- `internal/protocol/interfaces_tool.go`
- `internal/protocol/mcp.go`
- `internal/protocol/outbox.go`
- `internal/protocol/prompt_builder.go`
- `internal/protocol/replay.go`
- `internal/protocol/repo/repo_app.go`
- `internal/protocol/repo/repo_automation.go`
- `internal/protocol/repo/repo_budget.go`
- `internal/protocol/repo/repo_channel.go`
- `internal/protocol/repo/repo_event.go`
- `internal/protocol/repo/repo_mock.go`
- `internal/protocol/repo/repo_modelversion.go`
- `internal/protocol/repo/repo_system.go`
- `internal/protocol/repo/repo_workflow.go`
- `internal/protocol/saga_compensation.go`
- `internal/protocol/sandbox.go`
- `internal/protocol/schema/001_events.sql`
- `internal/protocol/schema/002_outbox.sql`
- `internal/protocol/schema/003_episodic_memory.sql`
- `internal/protocol/schema/004_semantic_memory.sql`
- `internal/protocol/schema/005_workspace_vfs.sql`
- `internal/protocol/schema/006_decision_log.sql`
- `internal/protocol/schema/007_tasks.sql`
- `internal/protocol/schema/008_skills.sql`
- `internal/protocol/schema/009_rag_chunks.sql`
- `internal/protocol/schema/010_self_improve.sql`
- `internal/protocol/schema/011_providers.sql`
- `internal/protocol/schema/012_channels.sql`
- `internal/protocol/schema/013_chat.sql`
- `internal/protocol/schema/014_cron_jobs.sql`
- `internal/protocol/schema/015_mcp_servers.sql`
- `internal/protocol/schema/016_preferences.sql`
- `internal/protocol/schema/017_automations.sql`
- `internal/protocol/schema/018_plugin_marketplaces.sql`
- `internal/protocol/schema/019_extension_catalog.sql`
- `internal/protocol/schema/020_extension_instances.sql`
- `internal/protocol/schema/021_plugins.sql`
- `internal/protocol/schema/022_provider_catalog.sql`
- `internal/protocol/schema/023_notes.sql`
- `internal/protocol/schema/024_reflection_memory.sql`
- `internal/protocol/schema/028_apps.sql`
- `internal/protocol/schema/029_workflows.sql`
- `internal/protocol/schema/030_oom_guard_log.sql`
- `internal/protocol/schema/031_planner_sessions.sql`
- `internal/protocol/schema/032_mock_response_cache.sql`
- `internal/protocol/schema/033_model_version_registry.sql`
- `internal/protocol/schema/034_core_memory.sql`
- `internal/protocol/schema/035_task_checkpoints.sql`
- `internal/protocol/schema/036_archive_checkpoints.sql`
- `internal/protocol/schema/037_world_model.sql`
- `internal/protocol/schema/038_idempotent_cache.sql`
- `internal/protocol/schema/embed.go`
- `internal/protocol/skill_compile.go`
- `internal/protocol/synthetic_case.go`
- `internal/protocol/topics.go`
- `internal/protocol/types.go`
- `internal/protocol/whisper.go`

## 明确未覆盖的范围

- `internal/protocol/pb/`（Protobuf 自动编译生成物）
- `internal/protocol/*_test.go` 与 `internal/config/*_test.go`（测试文件跳过）

## 审了但无发现的模块

- `internal/config`
- `cmd/polaris`
- `api/proto`
- `configs`
- `internal/lint`
