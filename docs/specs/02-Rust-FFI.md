# 02 Rust FFI 规范

> Rust 仅用于性能关键路径 FFI。维持语言边界，禁止为方便而跨 FFI。

## RUST-1 purego ABI

- 跨语言调用一律 purego（零 CGO，纯 Go 调用 Rust 动态库）；不引入 CGO，不在 Rust 侧加 cbindgen
- 调用约定见 `internal/protocol/ffi-abi.md`；ABI 版本常量同步由 `Test_inv_FFIABIConstantsInSync` 校验

## RUST-2 内存安全

| 规则 | 说明 |
|------|------|
| 谁分配谁释放 | Rust 分配的内存由 Rust 释放，Go 传入的由 Go 管理 |
| Panic 不可跨越 FFI | 所有导出函数用 `std::panic::catch_unwind` 包裹 |
| 字符串编码 | Go 侧保证 NUL-terminated UTF-8，Rust 侧不信任长度标记 |
| 裸指针不可泄露 | 边界只用整数句柄与拷贝缓冲区，不用裸指针传复杂结构 |

## RUST-3 文件组织

`rust/substrate/src/`：`lib.rs`（顶层 FFI 导出，含 Cedar）、`surreal_store/`（SurrealDB 检索轴，ADR-0003/0011）、`wasmtime_engine.rs`、`native_sandbox/`（原生进程沙箱，seatbelt/bwrap/fallback 平台后端）、`llama_infer/`（本地推理，tier1 feature 门控）、`check_wasi.rs`。

拆分判定：子模块 > 300 行提取独立文件；禁止无 ADR 静默扩大 `lib.rs`。

## RUST-4 Cargo.toml 约束

- `crate-type = ["staticlib", "cdylib"]` 不可移除
- 依赖最小化，每加一个 `[dependencies]` 须说明理由并记 ADR，禁止静默引入
- 依赖白名单：`cedar-policy`（策略引擎）、`bytemuck`、`capnp`、`surrealdb`（ADR-0003）、`wasmtime`+`wasmtime-wasi`（Rust 侧 L2 沙箱引擎，经 FFI 暴露；与 Go 侧 wazero 完全分离）、`tokio`、`serde`+`serde_json`、`anyhow`、`bytes`+`once_cell`、`llama-cpp-2`+`encoding_rs`（optional，仅 tier1 本地推理）、`libc`（`cfg(unix)`，`native_sandbox/` 的 `pre_exec` 等系统调用）

## RUST-5 FFI 边界测试

- 每个 FFI 导出函数须有 Rust 侧单测（经 `lib::ffi_func_name` 直调），覆盖：正常、空、巨大、非法 UTF-8 输入
- 参考 `rust/substrate/src/lib.rs` 的 `#[cfg(test)]` 块
