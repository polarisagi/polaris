# ADR-0005: purego(零 CGO)作为 Go→Rust FFI 桥接方式

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M11 / `pkg/substrate/policy` / `rust/substrate`
- **实现详情**: [M11 §3 Cedar](../M11-Policy-Safety.md) | [internal/protocol/ffi-abi.md](../../../internal/protocol/ffi-abi.md)

## 上下文

Cedar 策略引擎是 Rust 实现,需 Go 主进程内调用:评估 <1ms 热路径,形式化验证(Lean)需保留 Rust 生态,跨平台编译。

## 决策

**采用 `purego`(零 CGO Go→动态库调用)作为 FFI 桥接方式。**

- Cedar 编译为 cdylib(`.so`/`.dylib`/`.dll`)
- Go 侧通过 purego 加载并调用 C ABI
- ABI 版本号双侧校验(major.minor 不匹配 → panic)
- ABI 变更视同 `B5.3` 跨语言破坏性变更,同步更新 `internal/protocol/ffi-abi.md`

同模式延伸至 SurrealDB-Core FFI(`surreal_store.go` 当前用 cgo 属历史遗留,P3 处置)。

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| cgo | 跨平台交叉编译复杂;需 C 工具链;破坏单二进制 |
| 纯 Go 重写 Cedar | 重复 Rust 生态投入;Lean 形式化验证难复现 |
| gRPC sidecar | 增加运行时进程;每调用 ~1ms+ 网络往返;违反 Tier-0 |
| WASI | Cedar 内部依赖(IP/时间)WASI 支持不全 |

**反例守护**:未来如有人提议"添加 Rust 库但用 cgo"—本 ADR 拒绝。purego 体系一致性优于个案便利。
