# 02 Rust FFI 规范

> Rust 仅用于性能关键路径 FFI。维持语言边界，禁止为方便而跨 FFI。

## RUST-1 purego ABI

- 所有跨语言调用使用 purego（零 CGO，纯 Go 调用 Rust 动态库）
- 不引入 CGO，不在 Rust 侧增加 cbindgen

参考：`internal/protocol/ffi-abi.md` 定义调用约定。

## RUST-2 内存安全

| 规则 | 说明 |
|------|------|
| 谁分配谁释放 | Rust 分配的内存必须由 Rust 释放，Go 传入的内存 Go 管理 |
| Panic 不可跨越 FFI | 所有 FFI 导出函数用 `std::panic::catch_unwind` 包裹 |
| 字符串编码 | Go 侧保证 NUL-terminated UTF-8，Rust 侧不信任长度标记 |
| 裸指针不可泄露 | FFI 边界只用整数句柄（handle）和拷贝的缓冲区，不用裸指针传递复杂结构 |

## RUST-3 文件组织

当前 `rust/substrate/src/` 已按功能拆分：

```
src/
├── lib.rs              # 顶层 FFI 导出函数（含 Cedar）+ crate 文档
├── surreal_store/      # SurrealDB 认知检索轴 FFI（mod/store/kv/fts/vector/graph；见 ADR-0003/ADR-0011）
├── wasmtime_engine.rs  # Wasmtime Wasm 执行引擎 FFI
├── native_sandbox/     # 原生进程沙箱 FFI（mod/dispatch/engine/env/types + seatbelt/bwrap/fallback 平台后端）
├── llama_infer/        # 本地推理 FFI（tier1 feature 门控，P3-1）
└── check_wasi.rs       # WASI 可用性探测
```

> 2026-09-20 追记（GR-11-002）：原树为单文件 `surreal_store.rs` / `native_sandbox.rs`，二者已按"子模块 > 300 行提取"规则下沉为目录，并新增 `llama_infer/`；依赖白名单同步订正如下。

拆分判定：新增子模块 > 300 行时提取独立文件，禁止无 ADR 静默扩大 lib.rs。

## RUST-4 Cargo.toml 约束

- `crate-type = ["staticlib", "cdylib"]` 不可移除
- 依赖以最小化原则添加——每加一个 `[dependencies]` 必须说明理由
- 当前依赖白名单：`cedar-policy`（Cedar 策略引擎）、`bytemuck`（安全字节转换）、`capnp`（序列化）、`surrealdb`（认知检索轴，见 ADR-0003）、`wasmtime`+`wasmtime-wasi`（**Rust 侧原生依赖**，L2 沙箱执行引擎，通过 FFI 暴露给 Go；不是 Go 侧 wazero，两者完全分离）、`tokio`（异步运行时）、`serde`+`serde_json`（序列化）、`anyhow`（错误传播）、`bytes`+`once_cell`（工具；原列 `lazy_static` 已由 `once_cell` 取代）、`llama-cpp-2`+`encoding_rs`（optional，仅 tier1 本地推理 feature 启用；`encoding_rs` 为 token 解码器类型所需的直接依赖）、`libc = "0.2"`（Unix 平台依赖，`[target.'cfg(unix)'.dependencies]`，用于 `native_sandbox/` 的 `pre_exec` 等 Unix 系统调用）
- 新增依赖必须经过讨论并记录 ADR，禁止静默引入

## RUST-5 FFI 边界测试

- 所有 FFI 导出函数必须有 Rust 侧单元测试（通过 `lib::ffi_func_name` 直接调用）
- 测试必须覆盖：正常输入 + 空输入 + 巨大输入 + 非法 UTF-8
- 参考 `rust/substrate/src/lib.rs` 的 `#[cfg(test)]` 块
