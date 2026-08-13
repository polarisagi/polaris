# Fix Summary — 2026-08-13 升级实施（WP-1 ~ WP-14）

## 状态统计

| 状态 | 数量 |
|---|---|
| fixed | 42 |
| partial | 1 (GR-5-002，ConfigureWorkingMemBudget 因 config 无对应阈值暂缓) |
| rejected | 1 (GR-8-004，误报，见 00-核验结论.md §1.1) |
| deferred | 1 (WP-11 GD-13-003，单路硬超时因无实测数据暂缓，登记 99-遗留线索.md) |

**Rejected 率**：1/44 = **2.3%**（符合预期）

---

## 各 WP Commit 哈希

| WP | 描述 | Commit |
|---|---|---|
| WP-1 + L-01~L-14 | fail-closed 修复 + Lint 规则全套落地 | `3cef8d5` |
| WP-2 | Taint & Stream Security | `005e5ce` |
| WP-3 | Egress Hardening (SMTP SSRF) | `f0641d0` |
| WP-4 | Storage Layer (Outbox 毒丸 + MutationBus) | `fd09cd1` |
| WP-5 | Lifecycle & Background Workers | `92039ea` |
| WP-6 | SQLite 调度幂等 + StateGraph 死锁防护 | `164be51` |
| WP-7 | CircuitBreaker HalfOpen 并发泄漏 | `75701d9` |
| WP-8 | Rust FFI NULL 守卫 | `f9f86bd` |
| WP-9 | ClientType 枚举 SSoT + 插件路由修复 | `4cf0aa4` |
| WP-10.1 | 接线断裂统一处置 | `03bd9c6` |
| WP-10.2 | 硬编码与错误码清理 | `cb7ca5d` |
| WP-11 | 混合检索降级 (暂缓，登记遗留线索) | — |
| WP-12 | Docs↔Code 漂移 8 条 | `14847e7` |
| WP-13 + WP-14 | ROADMAP 止环 + ADR 追记归档 | `d55804a` |

---

## 本轮落地的 Lint 规则清单 (L-01~L-14)

| 规则 | 文件 | lint-selftest.txt 用例行（近似） |
|---|---|---|
| L-01 | tools/safe_dialer_lint.go | 第 48 行 |
| L-02 | tools/route_coverage_check.go | 第 49 行 |
| L-03 | tools/taint_typed_fields_check.go | 第 50 行 |
| L-04 | tools/panic_lint.go | 已有负向用例 |
| L-05 | tools/chan_send_guard_lint.go | 第 51 行 |
| L-06 | tools/task_state_lint.go | 第 52 行 |
| L-07 | tools/scheduler_status_filter_check.go | 第 53 行 |
| L-08 | tools/ffi_null_guard_check.go | 第 54 行 |
| L-09 | tools/lifecycle_reset_lint.go | 第 55 行 |
| L-10 | tools/bounded_cache_check.go | 第 56 行 |
| L-11 | tools/apperr_semantics_check.go | 第 57 行 |
| L-12 | tools/regex_greedy_check.go | 第 58 行 |
| L-13 | tools/wiring_reachability_check.go | 注释说明（依赖外部命令）|
| L-14 | tools/docs_gen.go + doc_counts_check.go | — |

---

## Rejected 逐条说明

- **GR-8-004**：`makeMCPToolAsyncFn` 的 TaintLevel 未标注属误报，见 `00-核验结论.md §1.1`。实际返回值已通过调用链继承污点级别，无需显式重标。

---

## 遗留线索（登记 99-遗留线索.md）

- **WP-11 GD-13-003**：`hybrid_retriever.go` 单路独立硬超时的 `RecallPathTimeoutMs` 阈值因无实测 P99 延迟数据，无法按 HE-4 要求写明依据，暂缓实施。下轮增量审核时补充实测数据后再推进。
- **GR-5-002 partial**：`ConfigureWorkingMemBudget` 因 `internal/config` 无对应的 `WorkingMem`/`TokenBudget` 阈值字段，不自行生造，待架构决策后跟进。
