# tools/baselines/ —— 门控抑制表

本目录收**抑制存量违规**的文件：棘轮基线（baseline）与豁免白名单（allowlist/denylist 中
"允许"语义的那一类）。判断标准只有一条：

> 删掉这个文件，某条门控会因为存量而报红 → 它属于本目录。
> 删掉这个文件，某条门控会失去判据、无从下手 → 它是**规则输入**，留在 `tools/` 下与
> 规则本体同放（如 `fsm-io-denylist.txt`、`must-check-error-calls.txt`、
> `taint-typed-fields.txt`、`lint-selftest.txt`）。

## 为什么集中

2026-08-17 之前抑制表散在三处：`tools/`、`tools/baselines/`、`scripts/`。同一条规则的
两个抑制文件甚至分居两地——`regex_greedy_check` 的 baseline 在 `tools/baselines/*.md`，
allowlist 在 `scripts/*.txt`。散落的直接后果是没人能一眼答出"这条门控现在放过了多少
存量"，而这恰恰是 ADR-0091 认定产出最高的那个问题（看门控在看哪里 > 看门控报了什么）。

本轮从 `scripts/` 迁入 6 个、从 `tools/` 迁入 1 个：

| 文件 | 归属门控 |
|---|---|
| `deadcode-allowlist.txt` | `make deadcode` |
| `docs-refs-allowlist.txt` | `make docs-refs`（文档侧 + .go 注释侧共用） |
| `anchor-refs-baseline.txt` | `tools/anchor_refs.go`（§ 章节锚点） |
| `wiring-allowlist.txt` | `[L-13]` `tools/wiring_reachability_check.go` |
| `review-check-baseline.txt` | `make review-check` |
| `regex-greedy-allowlist.txt` | `[L-12]` `tools/regex_greedy_check.go` |
| `route-coverage-allowlist.txt` | `[F-8a]` `tools/route_coverage_check.go` |

`docs/arch/decisions/` 下的 ADR 正文仍写着旧路径，**刻意不改**：ADR 记录的是写作当时的
事实，改它等于篡改历史（同 ADR-0081 里"docs-refs 不扫 decisions/"的裁定）。活文档与
`.go` 注释里的引用已同步更新。

## 加条目的纪律

沿用 ADR-0089 与「白名单要审计不要清空」的结论：**先验证规则本身，再谈还债**。
往这里加一条之前，先确认该条判定成立（不是判据过宽造成的误报）；加进来的每一条都要
写明理由，且理由必须是"为什么这里合法"，而不是"改起来麻烦"。
