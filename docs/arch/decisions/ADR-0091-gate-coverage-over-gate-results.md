# ADR-0091: 质量门控治理：覆盖面优先、扫描根、负向自测

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0013/0014/0089 ｜ 模块：`.golangci.yml` / `internal/lint/` / `tools/` / `Makefile` / `.github/workflows/`

核心命题：CI 全绿的含义是"当前检查没看见"，不是"没有问题"。门控的**覆盖面**比其**结果**重要；手维护的覆盖清单是失效温床，能删就删，不能删就加机械校验。

## 决策一：golangci-lint 机械化守护宪法规则（原 ADR-0013 决策一）

- 已启用：`depguard`（B1 层依赖 + R6 隔离）、`errorlint`（R1.2）、`nestif`（R7 嵌套≤3）、`gocyclo`（R7 圈复杂度≤15）；Phase 2 的 `wrapcheck`、`gochecknoglobals` 现已启用（`.golangci.yml`）。
- `funlen` 判定不采用（2026-07-04）：与 `gocyclo` 冗余，Go 的 err 处理天然拉长行数、误报率高；复杂度治理收敛到 `gocyclo` 一家。
- 不用 baseline 锁定 golangci 既有违规（等于规则空转）：按优先级修复，或显式 `//nolint` + 关联 ADR；无 ADR 佐证的 `//nolint:depguard` 跨层 import 拒绝。

## 决策二：对抗审查 GitHub Action（原 ADR-0013 决策二 / 原 ADR-0014）

- `.github/workflows/constitutional-review.yml` + `scripts/constitutional_review.sh`：PR 触发独立 LLM 审查，覆盖 lint/golden 机械不可检的语义反模式（R1.4/R1.8/R1.9/R1.10 等）。
- 审查模型不可与开发模型同型号（防"被告判自己"）；Reviewer 只看 diff + `00-Constitution.md`；输出严格机器化（"R<编号> | 文件:行 | 说明" 或 "NONE"），禁建议/表扬/推理。
- **warning-only，不阻断 CI**：LLM 会误报，硬阻断会让团队学会绕过。拒绝同一开发者会话 self-review。
- 订正：脚本默认模型现为 `deepseek-flash`（环境变量 `REVIEWER_MODEL` 可覆盖），原文"默认 claude-sonnet-4-6"已过时。

## 决策三：lint 规则扫描根接回 internal/（原 ADR-0089 决策一）

- 事实：`pkg/*`→`internal/*` 迁移后 `internal/lint` 有 8 条规则（5 条守 HE-2：污点 `.Content()` 审计、SSRF、裸 `net.Dial`、FFI 边界）仍扫 `pkg/`，1189 个文件从未被检查；`NoRawDBExecWriteInGateway` 的根目录已不存在而静默通过；4 个豁免表共 24 键全指向旧路径。**豁免失配与规则空转互相掩盖**。
- 决策：`walkPkgGoFiles` 更名 `walkRepoGoFiles` 并遍历 `internal/`+`pkg/`（改名是决策的一部分：旧名是空转载体）；直接 `filepath.Walk` 的规则改走 `walkDirsErr(root, []string{"internal","pkg"}, ...)`；豁免表按现址重建，**豁免表为空是正确状态**。
- 接回后暴露的真实违规（FFI 边界 74 处合法入豁免表；storage 层外持 `*sql.DB` 3、Gateway 裸 DB 写 2、遗留 DEBUG 打印 1）已修复。
- 触发条件（包路径迁移后必查，除 `make docs-refs` 外）：`internal/lint` 各规则扫描根字面量；`internal/lint/testdata/*.json` 豁免键仍 `os.Stat` 得到。

## 决策四：裸 error 判定按 err 来源收窄（原 ADR-0089 决策二）

- `Test_inv_BareErrorReturnRatchet` 只在 err 来自**标准库/三方调用**时报违规。本仓被调方按 `pkg/apperr` 约定自行包装，调用点再包一层只得 apperr 套 apperr；且旧规则文本要求的 `fmt.Errorf`/`errors.Wrap` 与 CLAUDE.md "禁裸 `fmt.Errorf` 泄漏调用链"直接冲突。
- 作用域限最内层函数体；`err` 为该函数形参则跳过（`filepath.WalkFunc`、sql 事务闭包是把错误交还框架）。
- 106 条 baseline 溯源：104 条本仓调用、1 条库调用、1 条无法定位；收窄后全仓零违规，`internal/lint/testdata/bare_error_return_baseline.json` 为空（债务清零，非重新接受）。已验证非空转：注入 `os.ReadFile` + `return err` 稳定被捕获。
- 删除 `cmd/gen_bare_error_baseline`（持有并行判定副本，且"一键接受全部违规"是空 baseline 与空转规则共存的原因）；需重建档时以规则失败输出为清单。
- 触发条件：`pkg/apperr` 不再要求被调方自行包装时，本决策失效，恢复全量判定。

## 决策五：失效路径门控扩展到 .go 注释（原 ADR-0089 决策三）

- `make docs-refs` 原只覆盖 `docs/*.md`+`CLAUDE.md`，`.go` 注释攒下 139 处失效引用（docs/arch 重命名 41、`pkg/*` 残留、`interfaces.go` 拆分后遗留）。
- 新增 `tools/comment_refs.go`，由 `scripts/docs-refs.sh` 串联，与文档侧共用 `tools/baselines/docs-refs-allowlist.txt`。用 Go 而非 shell：注释路径无反引号定界，需"前一字符非 `/`/单词字符"回溯判断（BSD grep 无 `-P`）。
- 刻意不报四类：URL 片段、Go 符号点记法、白名单条目、非顶层目录开头的相对路径。白名单只收"记载历史"的合法引用；白名单膨胀是规则失准的症状，须收紧规则而非扩白名单。（门控全貌见 ADR-0081）

## 决策六：覆盖清单改全仓 / 并入 check-all（原 ADR-0091 决策一~三）

- **race 全仓**：`make test` 默认 `-race -count=1 -shuffle=on` 覆盖 `./internal/... ./cmd/... ./pkg/...`，`test-race` 为别名（specs 09 引用）。原 7 目录清单自称"并发高发区"，实测并发原语密度前三（`internal/gateway` 33、`internal/llm` 23、`internal/extension` 18）一个都不在内；"race 慢 5-10x"理由不成立（包间并行，全仓墙钟约 100s 增量）。扩面零 DATA RACE，仅暴露低并行度掩盖的脆弱死线（`TestPool_Acquire_StartsKernelRunLoop` 3s→15s）。触发条件：全仓 race 墙钟 >10 分钟成瓶颈才谈裁剪，且裁剪必须带"凭什么这么裁"的机械校验，不得退回手维护清单。
- **fuzz 入 check-all**：`fuzz-taint`、`fuzz-skill` 守 Taint 五级传播（HE-2）与 Skill 校验管线，不能靠"偶尔想起来"；现 `check-all` 含二者。
- **docs-refs 不设 `-maxdepth`**：原 `-maxdepth 3` 恰好卡在最深的 `internal/gateway/server/CLAUDE.md`，"刚好够用"与"已经失效"在 CI 输出上无法区分（`scripts/docs-refs.sh` 现已去除）。

## 决策七：门控落地必须带负向自测（CLAUDE.md §新增/修改门控规则）

- `tools/*_lint.go` / `tools/*_check.go` 须在 `tools/lint-selftest.txt` 登记负向用例并使 `make lint-selftest` 通过（注入违规→报红→还原→转绿）；未经负向验证的规则不算 landed。判定由门控做，不接受自述。
- 覆盖面盲区实证（2026-08-13）：`tools/safe_dialer_lint.go` 曾漏 `net/smtp` 全族（EmailAdapter SSRF 旁路，P0；现已覆盖 `smtp.SendMail`/`smtp.Dial`）；`tools/route_coverage_check.go` 曾未比对路由 `{param}` 与 handler `PathValue` 键集（`checkPathValueConsistency` 已补）；`tools/taint_typed_fields_check.go` 曾未覆盖赋值点写死常量（污点硬编码降级，P1）。
- 审核闭环自 2026-08-12 起由 `make review-check` 门控（审核方禁读报告、修复方必须读正文，误报下降）。

## 未改动项（复查确认健康）

- `errcheck_enforced_dirs.json`：未覆盖的 `pkg/*` 包有明确排除理由与接入触发条件，可作为覆盖清单"被认真管过"的参照。
- `deadcode` 入口 `./cmd/polaris/...`（`cmd/` 仅此一个入口）；`internal/lint/testdata/*.json` 僵尸键 0。

## 反例守护

- golangci baseline 锁定既有违规 — 规则空转 — 2026-05-16
- 重提 `funlen` — 须证明 `gocyclo` 存在其无法覆盖的真实漏检 — 2026-07-04
- reviewer 给详细 fix 建议 / fail CI on any violation — 信号稀释 / 误报致团队学会绕过 — 2026-05-16
- 恢复"一键接受当前全部违规"类 baseline 生成工具 — 与空转规则共存 — 2026-08-08
- 为"看起来在管事"保留空豁免表条目 — 虚假保证 — 2026-08-08
- 恢复手选目录的覆盖清单 — 漂移源 — 2026-08-09

## 重新评估触发条件

- 审查意见长期零响应（团队学会忽略而非绕过）才重议分级阻断；`wrapcheck`/`gochecknoglobals` 类新规则开启前须先盘点存量，防止大量存量报错淹没信号。
- 其余触发条件见各决策。

## 沿革

| 日期 | 变更 | 原编号/理由 |
|---|---|---|
| 2026-05-16 | 决策一、二 | 原 ADR-0013 / ADR-0014 |
| 2026-07-04 | `funlen` 判定不采用 | 原 ADR-0013 修订 |
| 2026-08-08 | 决策三~五 | 原 ADR-0089 |
| 2026-08-09 | 决策六（覆盖面优先） | 原 ADR-0091 |
| 2026-08-13 | 决策七覆盖盲区三例 | 原 ADR-0091 追记 |
| 2026-10-12 | 并入 0013/0089；Phase 2 linter 现状、reviewer 默认模型、白名单路径按代码订正 | 文档合并 |

## 引用代码

`.golangci.yml`、`Makefile`（`check-all`/`test`/`test-race`/`lint-selftest`/`deadcode`）、`tools/lint-selftest.txt`、`tools/safe_dialer_lint.go`、`tools/route_coverage_check.go`、`tools/taint_typed_fields_check.go`、`tools/comment_refs.go`、`tools/baselines/docs-refs-allowlist.txt`、`scripts/docs-refs.sh`、`scripts/constitutional_review.sh`、`.github/workflows/ci.yml`、`.github/workflows/constitutional-review.yml`、`internal/lint/inv_lint_test.go`、`internal/lint/testdata/bare_error_return_baseline.json`、`internal/lint/testdata/errcheck_enforced_dirs.json`
