# 审核报告（批次 11）

| ID | 严重级 | 模块 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-11-001 | P1 | rust/substrate | surreal_store FFI 导出函数缺乏 NULL 指针前置校验导致潜在空指针解引用未定义行为 | 高 | 是 |

### [GR-11-001] surreal_store FFI 导出函数缺乏 NULL 指针前置校验导致潜在空指针解引用未定义行为
- 严重级: P1
- 模块: rust/substrate（层: rust）
- 位置: rust/substrate/src/surreal_store/fts.rs:25
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: grep -rn 'CStr::from_ptr(' rust/substrate/src/ | grep -v 'is_null()'）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/ffi/ 与 internal/store/repo/。Purego FFI 在 Go 侧传递空/nil 字符串时，unsafe.StringData("") 或 unsafe.Pointer(nil) 会传入 NULL (0x0) 指针。surreal_fts_delete (fts.rs:67) 与 surreal_fts_search (fts.rs:116) 已显式加入 if doc_id.is_null() 前置检查并附注 // 入参 null 判断在 catch_unwind 外前置检查，避免 null 解引用 UB（GR-11-001），但 surreal_fts_index (fts.rs:25)、surreal_vec_upsert (vector.rs:28) 与 surreal_vec_delete (vector.rs:69) 漏掉 null 校验，直接在 catch_unwind 闭包内调用 CStr::from_ptr(doc_id) / CStr::from_ptr(id)。当 Go 侧传入 nil/NULL 指针时会触发空指针解引用未定义行为（UB）与段错误崩溃。
- 问题: surreal_fts_index、surreal_vec_upsert 与 surreal_vec_delete 导出函数在跨 FFI 边界解引用 C 字符串指针前未校验指针非空，违反 HE-2 物理防线与可验证执行原则中 FFI 边界安全要求。
- 证据: 如下 Rust 代码所示：
  ```rust
  // rust/substrate/src/surreal_store/fts.rs:25
  pub unsafe extern "C" fn surreal_fts_index(doc_id: *const c_char, text: *const c_char) -> c_int {
      let result = panic::catch_unwind(move || {
          let id = match unsafe { CStr::from_ptr(doc_id) }.to_str() {
  ```
- 修复方向提示: 在 catch_unwind 外部/顶部统一添加 if doc_id.is_null() || text.is_null() { return SURREAL_ERR_UTF8; } 等 NULL 指针防护。

## 已审文件清单
- rust/substrate/src/check_wasi.rs
- rust/substrate/src/lib.rs
- rust/substrate/src/llama_infer/dispatch.rs
- rust/substrate/src/llama_infer/mod.rs
- rust/substrate/src/native_sandbox/bwrap.rs
- rust/substrate/src/native_sandbox/dispatch.rs
- rust/substrate/src/native_sandbox/engine.rs
- rust/substrate/src/native_sandbox/env.rs
- rust/substrate/src/native_sandbox/fallback.rs
- rust/substrate/src/native_sandbox/mod.rs
- rust/substrate/src/native_sandbox/seatbelt.rs
- rust/substrate/src/native_sandbox/types.rs
- rust/substrate/src/surreal_store/fts.rs
- rust/substrate/src/surreal_store/graph.rs
- rust/substrate/src/surreal_store/kv.rs
- rust/substrate/src/surreal_store/mod.rs
- rust/substrate/src/surreal_store/store.rs
- rust/substrate/src/surreal_store/vector.rs
- rust/substrate/src/wasmtime_engine.rs
- tools/adr_index_check.go
- tools/anchor_refs.go
- tools/comment_drift.go
- tools/comment_refs.go
- tools/doc_counts_check.go
- tools/docs_gen.go
- tools/ffi_symbol_check.go
- tools/fsm_io_lint.go
- tools/gen_threshold_examples.go
- tools/generate_manifest.go
- tools/lint_selftest.go
- tools/must_check_error_lint.go
- tools/no_backdoor_lint.go
- tools/nolint_unused_lint.go
- tools/panic_lint.go
- tools/release_sign.go
- tools/release_signing_gate.go
- tools/review_check.go
- tools/review_merge.go
- tools/route_coverage_check.go
- tools/rows_err_lint.go
- tools/safe_dialer_lint.go
- tools/sync_doc_toc.go
- tools/taint_typed_fields_check.go
- tools/task_state_lint.go
- tools/todo_lint.go
- Makefile
- .github/workflows/benchmark.yml
- .github/workflows/ci.yml
- .github/workflows/constitutional-review.yml
- .github/workflows/release.yml
- .github/workflows/rust-build.yml
- scripts/ci_test.sh
- scripts/constitutional_review.sh
- scripts/docs-refs.sh
- scripts/install.ps1
- scripts/install.sh
- scripts/release-signing.sh
- scripts/restart.sh
- scripts/review-batch-scope.txt
- scripts/review-prescan.sh
- scripts/uninstall.ps1
- scripts/uninstall.sh

## 明确未覆盖的范围
- 无

## 审了但无发现的模块
- tools/ （门控脚本、Linter 校验逻辑、单据生成与发布检查工具集未见死锁、泄漏或反模式）
- .github/workflows/ 与 scripts/ （CI/CD 工作流定义与辅助 Shell 脚本未见路径断链或漏洞）
- rust/substrate/src/native_sandbox/ 与 wasmtime_engine.rs （沙箱隔离逻辑与 WASM 执行引擎架构符合 HE-2 可验证执行约束，包含 timeout 与 epoch 中断机制）
