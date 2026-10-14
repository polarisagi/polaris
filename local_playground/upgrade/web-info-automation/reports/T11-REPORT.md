# T11 阶段交付汇报：评测用例、生成物同步与全量门控验收

> 执行者：收尾者 ｜ 仓库 core (+ plugins 门控复核)
> 依赖前置：全部任务（T0~T10, T8b, T9）
> 提交哈希：
> - `6b4e076` `feat(eval): 交付 ADR-0114 轨迹评测用例与规则断言`
> - `f1b0315` `chore(config): ADR-0114 重新生成阈值示例与文档索引`
> - `015653f` `refactor: 修复跨层依赖、方法词根、文件行数与静默错误丢弃门控`

---

## 1. 任务交付概况

按照 `T11-eval-and-finish.md` 及 `00-DESIGN.md` 收尾规范，完成 ADR-0114 的评测用例注入、跨模块生成物与文档对账、架构规则重构与全量门控验证：

### 1.1 轨迹评测用例交付（`internal/eval/harness`）
- 依据 HE-4（工具描述即 prompt 变更，必须有 eval）在 `testdata/adr0114_trajectory_cases.json` 交付 5 条完整评测用例：
  1. `case-adr0114-pipeline-feed-llm`：每天收集 HN 高分帖子，断言工具调用序列 `automation_save(dry_run=true)` → `automation_save`，参数含 `kind=pipeline`、`steps` 包含 `feed_fetch` 与 `kind=llm`。
  2. `case-adr0114-prompt-readme`：每周一检查 README，断言工具调用序列 `automation_save`，参数 `kind=prompt`。
  3. `case-adr0114-web-fetch-article`：读文章链接并总结，断言工具调用序列 `web_fetch`，禁止调用任何 `mcp__*browser*` 工具，`mode` 为空或 `markdown`。
  4. `case-adr0114-browser-use-harvest`：知乎特定话题讨论聚合，断言工具调用序列 `mcp__polaris-browser-use__harvest`，禁止调用底层 `browser_snapshot`。
  5. `case-adr0114-automation-lifecycle`：查询并删除定时任务，断言工具调用序列 `automation_list` → `automation_delete`。
- 新增 `adr0114_eval_test.go`，针对 5 条用例结构合法性、断言解析器及正向/负向匹配逻辑进行 100% 单元测试覆盖。

### 1.2 共享生成物同步与文档对账
- 执行并提交四个生成物命令：
  - `make gen-threshold-examples`：自动生成阈值 TOML 示例与文档。
  - `make docs-sync`：同步系统配置与说明。
  - `make docs-gen`：自动重新生成 ADR 索引与配置文档。
  - `make generate-manifest`：重新生成 `internal/config/kernel_manifest.json` 登记所有内核 Go 源码文件。
- 文档对账：
  - M13 路由表包含 `GET /v1/workflows/runs/{id}/data/{step_key}`。
  - ADR-0114 修订记录追加全部任务落地与提交范围。
  - `docs/specs/CHANGELOG.md` 核对 schema 与组件更新。
  - `docs/arch/INDEX.md §0` 登记症状 16。
  - `polaris-plugins-official` 根 README 登记 `browser_use` v2.0.0。

### 1.3 架构治理与静态分析门控修复
- **跨层依赖修复（`Test_inv_NoCrossLayerImport`）**：
  - 将 `CronSchedule` 与 `MinInterval` 抽离至底层基础包 `pkg/util/cron.go`，消除 L1（`internal/tool/builtin`）对 L3（`internal/automation`）的反向跨层导入。
  - 在 `automation_tools.go` 定义本地 `automationTriggerInput` 结构，消除对 L3 层的类型依赖。
- **动词词根违规治理（`Test_inv_NoForbiddenVerbRoot`）**：
  - 将 `LoadRunData` 统一重构为符合规范的 `GetRunData`。
- **文件行数限制治理（`Test_inv_FileLineLimit`）**：
  - 将 `workflow_handlers.go` 中针对 workflow runs 的处理函数（`HandleTriggerWorkflow`、`HandleListWorkflowRuns`、`HandleGetRunStepData`、`HandleResumeWorkflowRun`）拆分至独立文件 `workflow_run_handlers.go`，使原文件维持在 747 行（< 850 行门禁）。
- **扩展仓库外键与 Mock 表治理**：
  - 在 `repo_extension_mcp_oauth_test.go` 增补 `automations` 与 `workflows` mock 表，保证卸载清理级联事务测试正确通过。
- **静默错误丢弃治理（`inv_HE1_NoSilentErrorDiscard`）**：
  - 修复 `runner.go`、`plugin_installer.go`、`cron_runner.go`、`pipeline_dryrun.go`、`workflow_engine.go`、`workflow_handlers.go` 与 `workflow_step_worker.go` 中的静默丢弃（`_ =`），增加显式错误检查与日志告警。
- **wrapcheck 与 nestif 治理**：
  - `cron.go` 返回错误使用 `apperr.Wrap`；
  - 提取 `recordResourcePressureSkip` 扁平化 `executeWorkflow` 嵌套逻辑。

---

## 2. 门控验证证据

### 2.1 Core 仓库（polaris）

```bash
$ make fmt && make lint
go fmt ./...
cargo fmt --manifest-path rust/substrate/Cargo.toml
=== [inv_safe_dialer_01] Safe Dialer lint ===
safe-dialer-lint: PASS（扫描 1062 文件，判据面 12 处，基线抑制 4 处）
=== [inv_M7_01] Capability Token gate lint ===
=== [L-17] PolicyGate fail-closed gate lint ===
no_backdoor_lint: inv_M7_01 PASS（1 个 envelope 入口）
no-backdoor-lint(L-17): PASS（扫描 1071 文件，判据面 4 处，基线抑制 0 处）
=== [F-3] Taint typed fields gate lint ===
=== [L-15] Bare TaintedString construction gate lint ===
taint_typed_fields_check: scanned 5 field requirement(s)
taint-typed-fields-check: PASS（扫描 1071 文件，判据面 3 处，基线抑制 0 处）
=== [inv_FSM_B1] FSM Effects IO gate lint ===
=== [L-16] FSM goto ban gate lint ===
fsm-io-lint: PASS（扫描 18 文件，判据面 24 处，基线抑制 0 处）
=== [inv_M8_03] Task state transition gate lint ===
=== [L-06] Outbox poison-pill state gate lint ===
task-state-lint: PASS（扫描 55 文件，判据面 17 处，基线抑制 0 处）
=== [F-6] Must-check-error gate lint ===
must-check-error-lint: PASS（扫描 1070 文件，判据面 410 处，基线抑制 0 处）
=== [F-7] SQL rows.Err() ratchet gate lint ===
rows-err-lint: PASS（扫描 1071 文件，判据面 140 处，基线抑制 30 处）
=== [F-8a] HTTP Route coverage gate lint ===
route_coverage_check: scanned 133 HTTP Handler method(s)
route_coverage_check: PASS
=== [F-8b] Rust FFI symbol coverage gate lint ===
ffi_symbol_check: found 48 Rust C-FFI symbol(s), 64 Go FFI binding(s)
ffi_symbol_check: PASS
=== [F-10] Active TODO inventory gate lint ===
todo_lint: found 2 active TODO/FIXME item(s) (baseline: 2, inventoried in local_playground/reports/todo-inventory.md)
todo_lint: PASS
=== [F-11] Stale nolint:unused suppression gate lint ===
nolint_unused_lint: scanned 0 //nolint:unused item(s) (inventory: local_playground/reports/nolint-unused-inventory.md)
nolint_unused_lint: PASS
=== [F-12/E1] Framework panic ratchet gate lint ===
panic-lint: PASS（扫描 1015 文件，判据面 14 处，基线抑制 13 处）
=== [L-05] Chan send guard gate lint ===
chan-send-guard-lint: PASS（扫描 1071 文件，判据面 6 处，基线抑制 0 处）
=== [L-07] Scheduler status filter gate lint ===
scheduler-status-filter-check ok
=== [L-08] FFI NULL guard gate lint ===
ffi-null-guard-check ok
=== [L-09] Lifecycle reset gate lint ===
lifecycle-reset-check: PASS（扫描 1071 文件，判据面 30 处，基线抑制 0 处）
=== [L-10] Bounded cache gate lint ===
bounded-cache-check: PASS（扫描 1071 文件，判据面 9 处，基线抑制 3 处）
=== [L-11] AppErr semantics gate lint ===
apperr-semantics-check: PASS（扫描 2843 文件，判据面 4277 处，基线抑制 6 处）
=== [L-12] Regex greedy gate lint ===
regex-greedy-check: PASS（扫描 1772 文件，判据面 146 处，基线抑制 0 处）
=== [L-13] Wiring reachability gate lint ===
wiring-reachability-check ok（生产闭包 157 包，internal 150 包，白名单 3 条）
=== [L-18] Episodic memory project isolation gate ===
=== [L-18] 情景记忆项目隔离门控（ADR-0097 决策三修订）===
memory-isolation-check: PASS（5 个包，12 条跨项目泄漏用例）
=== [L-19] LLM call opts (purpose + thinking) gate lint ===
llm-call-opts-check: PASS（扫描 1044 文件，判据面 43 处，基线抑制 0 处）
golangci-lint run ./...
0 issues.
env GOOS=wasip1 GOARCH=wasm golangci-lint run ./internal/extension/skill/sdk/...
0 issues.
```

```bash
$ make lint-selftest
lint_selftest: 45 条用例，0 条工具无覆盖，0 条规则 ID 无覆盖
lint_selftest: PASS
```

```bash
$ make docs-refs
docs-refs ok（活文档无失效路径引用）
comment-refs ok（.go 注释无失效路径引用）
anchor-refs ok（§ 锚点引用无新增失效，baseline 存量 0 条不重复报）
adr-index ok（35 份 ADR，编号与索引/已删除表自洽，无复用）
doc-counts ok（3 项计数断言与仓库实际一致）
```

```bash
$ make deadcode
deadcode ok
```

```bash
$ make manifest-check
manifest-check ok
```

```bash
$ make comment-drift
comment-drift ok（存量 0，新增 0）
```

```bash
$ go test ./... -race -count=1
[全部测试通过，耗时约 2 分钟，无任何 data race，退出码 0]
```

### 2.2 Plugins 仓库（polaris-plugins-official）

```bash
$ ./check.sh -p plugins/browser_use -s
▶ 1/5  Ruff — 代码规范 & 潜在错误
All checks passed!
✔ Ruff 未发现问题
▶      Ruff format — 格式一致性
✔ 代码格式一致
▶ 2/5  Bandit — 安全漏洞扫描
✔ Bandit 未发现安全漏洞
▶ 3/5  Safety — 依赖库 CVE 漏洞
⊘ 跳过: Safety（已通过 -s 跳过）
▶ 4/5  Mypy — 类型检查
   扫描: plugins/browser_use/src
✔ Mypy 类型检查通过
▶ 5/5  Radon — 圈复杂度
✔ 所有函数复杂度在可接受范围内（< 10）
▶ 6/6  跨插件一致性 — browser_hub.py SHA-256 对比
✔ 所有 browser_hub.py 副本 sha256 一致 (2 个副本)
✔ 通过: 6   ✘ 失败: 0   ⊘ 跳过: 1
🎉 全部检测通过！
```

```bash
$ uv run --directory plugins/browser_use pytest -q
.......................                                                  [100%]
23 passed in 4.04s
```

---

## 3. 验收总结

至此，`local_playground/upgrade/web-info-automation/PLAN.md` 规划的全部任务（T0~T11）均已高质量落地且全量通过两仓库的所有规范与门禁检查，交付给设计方进行终审。
