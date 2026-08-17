# tools/ —— 门控与生成器

> 34 个 `//go:build ignore` 单文件 `package main`，由 `go run tools/X.go` 直接执行，
> **不进任何二进制**。用 ignore tag 是因为同一目录下不能有多个 `func main`。
> 公共骨架在 `tools/lintutil/`（普通包，有单元测试）。

## §跳读

| 章节 | 内容 |
|---|---|
| §1 四类工具 | 每个文件干什么、由谁调用 |
| §2 两套静态分析的边界 | tools/ 与 internal/lint/ 各管什么，判据归属规则 |
| §3 写一条新门控 | lintutil 骨架、退出码语义、锚点自毁断言 |
| §4 抑制表 | 基线 vs 规则输入表，放哪里 |
| §5 门控失真的形态 | 验收一条门控时按这几句问 |

## §1 四类工具

**代码门控 lint（21 条规则）** —— 进 `make lint`。规则 ID 见 Makefile 里各目标的
`=== [ID] ===` 报头；**门控条数按 ID 计，不按 make 目标计**（一个二进制可以承载
多条断言，多个目标跑同一个二进制则是虚增）。

| 工具 | 规则 |
|---|---|
| `safe_dialer_lint.go` | inv_safe_dialer_01：出站连接必须经 SafeDialer |
| `no_backdoor_lint.go` | inv_M7_01 能力令牌校验未被削弱；L-17 PolicyGate fail-closed |
| `taint_typed_fields_check.go` | F-3 跨界字段保持 TaintedString；L-03 赋值黑名单；L-15 禁裸构造 |
| `fsm_io_lint.go` | inv_FSM_B1 Effects 闭包禁同步 IO；L-16 FSM 包禁 goto |
| `task_state_lint.go` | inv_M8_03 CAS 状态转移；L-06 outbox 毒丸分支 |
| `must_check_error_lint.go` | F-6 关键调用 error 不得被 `_` 丢弃 |
| `rows_err_lint.go` | F-7 `for X.Next()` 后必须查 `X.Err()` |
| `route_coverage_check.go` | F-8a HTTP handler 与路由注册对账 |
| `ffi_symbol_check.go` | F-8b Rust 导出符号与 Go 绑定对账 |
| `todo_lint.go` | F-10 活跃 TODO 棘轮 |
| `nolint_unused_lint.go` | F-11 过期 `//nolint:unused` |
| `panic_lint.go` | F-12/E1 框架层 panic 棘轮（含 L-04 构造函数子句） |
| `chan_send_guard_lint.go` | L-05 ResultCh 发送须包在 select 内 |
| `scheduler_status_filter_check.go` | L-07 调度器 running 状态过滤 |
| `ffi_null_guard_check.go` | L-08 Rust FFI NULL 守卫 |
| `lifecycle_reset_lint.go` | L-09 select 单源关闭不得终止整个循环 |
| `bounded_cache_check.go` | L-10 Scanner 上限须来自 config 阀值 |
| `apperr_semantics_check.go` | L-11/R2.5 错误码与消息语义一致 |
| `regex_greedy_check.go` | L-12 贪婪跨行正则 |
| `wiring_reachability_check.go` | L-13 包级接线可达性 |

**元门控** —— `lint_selftest.go`（`make lint-selftest`）：逐条注入违规样例，断言每条
规则确实能报红，再还原断言转绿。清单 `tools/lint-selftest.txt`。它还校验两件事：
每个 `tools/*_lint.go` / `*_check.go` 都有用例，且 Makefile 里每个 `[ID]` 都有用例。
**未经负向验证的规则不算 landed。**

**文档门控** —— `sync_doc_toc.go`（§跳读行号）、`docs_gen.go`（生成块）、
`comment_refs.go`（.go 注释里的路径）、`anchor_refs.go`（§ 章节锚点）、
`adr_index_check.go`（ADR 编号）、`doc_counts_check.go`（计数断言）、
`comment_drift.go`（doc comment 与声明错位）。入口是 `make docs-refs` / `docs-check` /
`docs-gen-check` / `comment-drift`。

**审核流程与产物** —— `review_check.go` / `review_merge.go`（审核报告的机械可判属性）、
`generate_manifest.go`（内核完整性清单 + `-check`）、`gen_threshold_examples.go`、
`release_sign.go` / `release_signing_gate.go`。

## §2 两套静态分析的边界

本仓有两套，**不是重复建设，但判据命名空间必须互斥**：

| | `tools/` | `internal/lint/` |
|---|---|---|
| 运行 | `make lint`（秒级） | `go test ./internal/lint/`（~26s） |
| 形态 | 单文件 AST，不引 go/types | `package lint_test`，可用测试基础设施 |
| 抑制 | `tools/baselines/` 棘轮基线 | `testdata/*.json` 豁免表 |
| 负向验证 | 有（lint-selftest 强制） | 无 |

**判据只能有一个归属。** 2026-08-17 实测两份实现不会同步演进，只会各自腐烂到互相
掩盖：出站连接这条在两边各写了一遍，结果 `tools` 侧漏了 `net.DialContext` /
`http.Head` / `http.DefaultClient` 的非调用引用，`internal/lint` 侧漏了
`net.DialTimeout` / `http.PostForm` / `smtp.*` / `websocket.Dialer`，而**任何一方绿灯
都会被读成"这条不变量守住了"**。合并取并集后当场抓出 4 处两边都没看见的
`http.DefaultTransport` 引用。

发现两边查同一件事时：**归 `tools/`**（它在 `make lint` 里、有棘轮基线、有负向用例），
在 `internal/lint/` 原处留一段指向注记说明判据搬去哪了、为什么，**不要静默删除**。

**永远不要用 `GD-*` 给常驻门控编号。** GD 是审核批次内的序号、跨轮复用
（见 `review_check.go` 的 GD 编号判定处）。曾经一个 `GD-14-004` 同时标着三条互不
相干的断言，而 lint_selftest 正是按 ID 数门控条数的。常驻门控只用 `F-*` / `L-*` /
`inv_*`。

## §3 写一条新门控

```go
//go:build ignore

package main

import "github.com/polarisagi/polaris/tools/lintutil"

func main() {
    r := lintutil.NewReporter("my-check", lintutil.LoadBaseline("my-baseline.md"))
    lintutil.Walk(r, lintutil.WalkOptions{}, func(f lintutil.File) {
        // 每个进入判定的候选点都要 r.Anchor()，包括判定通过的
        // 违规用 r.Violation(f.At(node), "...")
    })
    r.RequireAnchors(1, "判据锚在 X 上；锚点归零说明规则失效而非仓库干净")
    r.Done()
}
```

**退出码语义**（由 `Reporter.Done` 统一）：

- `0` 通过；PASS 行会印出「扫描 N 文件，判据面 M 处，基线抑制 K 处」
- `1` 新增违规 —— 改代码
- `2` **门控自身失效**：锚点没了、扫描根不存在、源文件解析不了 —— 改规则

把 2 与 1 分开是刻意的。此前多数工具把「读不懂」写成静默 `continue`、把「锚点文件
打不开」写成静默 `return`，于是规则失效表现为一片绿灯。

接好后**必须**做三件事，缺一不算 landed：

1. Makefile 加目标，报头写 `=== [ID] 描述 ===`，并挂进 `lint`
2. `tools/lint-selftest.txt` 登记负向用例（注入报红 → 还原转绿）
3. `make lint-selftest` 通过

负向用例必须注入**仓库里真实存在的写法**。反例：L-10 的旧用例注入裸 `102400`，
恰好是旧判据唯一能识别的形态，于是自测常绿，而仓库里 7 个写成 `1024*1024` 的真实
调用点一个都抓不到——那条用例验证的是判据的舒适区。

扫描根默认全仓三根（`internal` / `cmd` / `pkg`）。**要收窄必须在规则头部写明理由**，
且理由要经得起一次 grep：L-05 曾以「ResultCh 是 store 包的约定字段名」收窄到
`internal/store`，而 `internal/learning/surprise` 里就有一个同语义的 ResultCh 发送。
收窄的理由若站不住，那它就不是收窄而是抄漏（ADR-0089）。

## §4 抑制表

- **抑制存量的**（棘轮基线 / 豁免白名单）→ `tools/baselines/`，判准见该目录 README
- **规则输入表**（删了它规则就没判据）→ 留在 `tools/` 与规则同放：
  `fsm-io-denylist.txt`、`must-check-error-calls.txt`、`taint-typed-fields.txt`、
  `taint-assign-denylist.txt`、`lint-selftest.txt`

基线文件是 `path:line` 行号锚定的：在违规行**上方**加一行注释就会让它失配，
表现为"凭空冒出新违规"。改文件头注释后记得重跑对应门控。

规则输入表读不到时一律 `exit 2`——名单即判据，读不到就没有判据可言。

## §5 门控失真的形态

验收一条门控时按这五句问（前四句来自 ADR-0091，第五句是 2026-08-17 新增）：

1. 它会不会把自己豁免掉？（锚点文件打不开 / 解析失败时静默放行）
2. 它是不是在冒充两条？（同一个二进制挂两个 make 目标，或一个 ID 标着多条断言）
3. 它的判据能不能被 no-op 满足？（硬编码字面量做锚点，常量化重构即失效）
4. 它会不会诱导破坏？（为了让门控转绿而补一个假绑定/空实现）
5. **它的判据看得见仓库里的真实写法吗？** 只匹配 `*ast.BasicLit` 而仓库全写
   `1024*1024`，或只查 `CallExpr` 而漏掉非调用引用——绿灯来自判据的盲区，
   与"仓库很干净"在输出上完全一样。`RequireAnchors` 与 PASS 行的判据面计数就是
   为了让这一条可被看见。
