# 02 Rust FFI 规范

> Rust 仅用于性能关键路径 FFI。维持语言边界，禁止为方便而跨 FFI。

## RUST-1 purego ABI

- 所有跨语言调用使用 purego（零 CGO，纯 Go 调用 Rust 动态库）
- 不引入 CGO，不在 Rust 侧增加 cbindgen
- `lib.rs` 目前的单文件结构可以维持，除非功能增长超出合理范围（>2000 行判断）

参考：`internal/protocol/ffi-abi.md` 定义调用约定。

## RUST-2 内存安全

| 规则 | 说明 |
|------|------|
| 谁分配谁释放 | Rust 分配的内存必须由 Rust 释放，Go 传入的内存 Go 管理 |
| Panic 不可跨越 FFI | 所有 FFI 导出函数用 `std::panic::catch_unwind` 包裹 |
| 字符串编码 | Go 侧保证 NUL-terminated UTF-8，Rust 侧不信任长度标记 |
| 裸指针不可泄露 | FFI 边界只用整数句柄（handle）和拷贝的缓冲区，不用裸指针传递复杂结构 |

## RUST-3 文件组织

当前 `lib.rs` 54KB 单文件，功能边界明确时可以拆分：

```
src/
├── lib.rs          # 顶层 FFI 导出函数 + crate 文档
├── cedar.rs        # Cedar 策略引擎 FFI
└── vector.rs       # SIMD 向量运算（bytemuck + capnp）
```

拆分判定：当 `lib.rs` 中一个 `mod` 块 > 300 行时提取独立文件。

## RUST-4 Cargo.toml 约束

- `crate-type = ["staticlib", "cdylib"]` 不可移除
- 依赖以最小化原则添加——每加一个 `[dependencies]` 必须说明理由
- 当前依赖白名单：`cedar-policy`, `bytemuck`, `capnp`（新增需讨论）

## RUST-5 FFI 边界测试

- 所有 FFI 导出函数必须有 Rust 侧单元测试（通过 `lib::ffi_func_name` 直接调用）
- 测试必须覆盖：正常输入 + 空输入 + 巨大输入 + 非法 UTF-8
- 参考 `rust/substrate/src/lib.rs` 的 `#[cfg(test)]` 块
