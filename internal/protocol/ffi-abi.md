# Polaris — FFI ABI 规范

> **§跳读**: 0:7 总则 / 1:18 加载 / 2:40 ABI版本 / 3:55 错误 / 4:65 并发 / 5:78 命名
> 跨语言 FFI 边界 ABI 规范 (Go ↔ Rust)。**ADR-0011**: CGO 已全面迁移至 `github.com/ebitengine/purego`。

## 1. 总则

所有 Go→Rust 调用经 purego `Dlopen` / `RegisterLibFunc`，**禁用 CGO**（`import "C"` 不可出现在 FFI 路径）。Rust 侧编译为 cdylib（`.so`/`.dylib`），暴露 `extern "C"` 符号。

### 1.1 适用范围

| 方向 | 调用方式 | 模块 | 用途 |
|------|---------|------|------|
| Go→Rust | purego `RegisterLibFunc` | `rust/substrate/` | Cedar 策略引擎 / SurrealDB 认知存储 / 原生进程沙箱 |
| Rust→Go | 不支持（禁回调函数指针） | — | 不适用 |

**Go 侧调用方**：
- `internal/security/policy/cedar_ffi.go`（Cedar 4 函数）
- `internal/store/surreal_store.go`（SurrealDB 21 函数）
- `internal/tool/sandbox/rust_native_sandbox.go`（native_sandbox 3 函数）

### 1.2 隔离原则

- purego 调用在 Go goroutine 中执行，不阻塞 Go scheduler（无 CGO 抢占问题）
- FFI 超时 <5s（`context.WithTimeout` 兜底）
- Rust 侧崩溃（SIGSEGV/SIGABRT）不可恢复 → Go 侧 `log.Fatalf` + CRITICAL 告警

## 2. 库加载

`internal/ffi/dylib.go` 通过 `sync.Once` 加载 dylib，路径优先级：

1. `POLARIS_SUBSTRATE_LIB` 环境变量
2. 可执行文件同级 `lib/` 目录
3. dev：`rust/substrate/target/release/`（逐级向上搜索）

`RegisterLibFunc` 按函数签名将 Rust 导出符号绑定到 Go 函数变量（值类型）。

### 2.1 字符串与字节传递

> **实际约定（ptr+len）**：FFI 边界全面采用 `*const u8 + usize` 风格，**不使用 NUL 终止字符串**（无 `*const c_char` / `CStr`）。

| 场景 | Go 侧 | Rust 侧 |
|------|-------|---------|
| Go→Rust 字符串输入 | Go `string`（purego 透传为指针） | `(*const u8, usize)` → `slice::from_raw_parts` → `std::str::from_utf8` |
| Rust→Go 错误/结果字符串 | 读 `(*const u8, usize)` 后复制，调配套 `_free_bytes` 释放 | `Box<[u8]>::into_raw` + `write_bytes` 写 `(out_ptr, out_len)` |
| 结构化数据 | 序列化为 JSON，传 `(*const u8, usize)` | `slice::from_raw_parts` 解析 JSON，响应同样走 `(out_ptr, out_len)` |

**purego 限制**：不支持结构体值类型跨边界（只传指针/标量）；复杂数据走 JSON 编码后传指针+长度。

### 2.2 内存所有权

| 分配方 | 释放方 | 机制 |
|--------|--------|------|
| Go | Go | Go GC，不需手动释放 |
| Rust（返回字节/字符串） | Rust | 须调用配套 `_free_bytes` / `_free_string`，Go 侧收到后立即复制 |
| Rust（opaque 句柄） | Rust | `Box::into_raw` / `Box::from_raw` |

**禁止**：Rust 持有 Go 内存跨 FFI 边界（purego 无 pinning，GC 可移动）。

## 3. ABI 版本协议

### 3.1 版本号定义

Rust 导出 `substrate_abi_version() -> u32`（高16位=major, 低16位=minor）。

| 侧 | 当前值 |
|----|--------|
| Go `ExpectedABIMajor` | 1 |
| Go `ExpectedABIMinor` | 1 |
| Rust `SUBSTRATE_ABI_MAJOR` | 1 |
| Rust `SUBSTRATE_ABI_MINOR` | 1 |

ABI 1.1 新增：`surreal_set_worker_threads` / `surreal_vec_delete` / `surreal_fts_delete` / `surreal_graph_delete_edges`；`surreal_stats` 扩展四路计数字段；HNSW 替换 MTREE。

### 3.2 校验机制（`internal/ffi/dylib.go` `verifyABI`）

- major 不匹配 → **panic**（fail-fast，防 silent ABI drift）
- minor：Go < Rust → warn（dylib 更新，Go 未同步）；Go > Rust → warn（dylib 过旧）

### 3.3 升级条件

- **升 major**：删导出符号、改函数签名、改错误码语义
- **升 minor**：新增导出符号、新增错误码

## 4. 错误处理

### 4.1 返回值约定

| 场景 | Go 侧 | Rust 侧 |
|------|-------|---------|
| 成功 | 检查零值/空指针 | 返有效值，返回码 0 |
| 可恢复 | 读返回码 + `out_err` 字符串 | 返 NULL/0，写 `(out_err_ptr, out_err_len)` |
| 不可恢复 | `log.Fatalf` + CRITICAL | `panic::catch_unwind` 转错误码 -3（`CEDAR_ERR_INTERNAL`）|

### 4.2 错误字符串约定

**无全局 `ffi_last_error()` 函数**。每个 FFI 函数内联输出错误：

```
fn foo(..., out_err_ptr: *mut *const u8, out_err_len: *mut usize) -> c_int
```

Rust 侧通过 `write_bytes(out_err_ptr, out_err_len, "message")` 写入，Go 侧读取后调配套 `_free_bytes` / `_free_string` 释放，**不得跨调用缓存错误指针**。

## 5. 并发安全

| 模块 | 并发模型 |
|------|---------|
| **Cedar**（`lib.rs`） | `Arc<RwLock<PolicySet>>` — 多读单写；`OnceLock` 确保全局初始化一次 |
| **SurrealDB**（`surreal_store.rs`） | SurrealDB 内建并发控制（kv-mem 引擎线程安全）；写路径串行化由上层 MutationBus 保证 |
| **native_sandbox**（`native_sandbox.rs`） | 无共享状态，每次调用独立子进程，并发安全 |

禁止 Rust 启动非导出长驻后台线程（Go 无法追踪）。

## 6. 命名规范

- `{engine}_{action}`：`cedar_evaluate`、`surreal_kv_get`、`native_sandbox_exec`
- `{type}_free_bytes` / `{type}_free_string`：必须与分配函数配对导出
- `{type}_drop`：纯数据释放（`surreal_free_buf`）

**所有 FFI 导出符号声明位置**：
- Cedar → `rust/substrate/src/lib.rs`
- SurrealDB → `rust/substrate/src/surreal_store.rs`
- native_sandbox → `rust/substrate/src/native_sandbox.rs`

禁止在其他文件散落 `#[no_mangle] pub extern "C"` 符号。

## 7. 符号清单（ABI 1.1）

### 通用
- `substrate_abi_version`

### Cedar（`lib.rs`，4 函数）
- `cedar_load_policies` — 加载/替换全局 PolicySet
- `cedar_evaluate` — 单次授权评估（返回 ALLOW=0 / DENY=1 / 错误<0）
- `cedar_policy_count` — 当前 PolicySet 策略数（健康检查用）
- `cedar_free_bytes` — 释放 Rust 分配的 `(*mut u8, usize)` 字节块

### SurrealDB（`surreal_store.rs`，21 函数）
- **生命周期**：`surreal_set_worker_threads`、`surreal_open`、`surreal_purge`
- **KV**：`surreal_kv_get`、`surreal_kv_put`、`surreal_kv_delete`、`surreal_kv_scan`
- **向量（HNSW）**：`surreal_vec_upsert`、`surreal_vec_delete`、`surreal_vec_knn`、`surreal_vec_set_mode`
- **图**：`surreal_graph_relate`、`surreal_graph_delete_edges`、`surreal_graph_traverse`、`surreal_graph_spreading_activation`
- **全文（BM25）**：`surreal_fts_index`、`surreal_fts_delete`、`surreal_fts_search`
- **内存管理**：`surreal_free_string`、`surreal_free_buf`
- **诊断**：`surreal_stats`（JSON 四路计数：kv/vec/graph/fts）

### native_sandbox（`native_sandbox.rs`，3 函数）
- `native_sandbox_exec` — 在原生沙箱中执行命令（JSON 请求/响应）
- `native_sandbox_probe_tools` — 探测平台沙箱可用工具
- `native_sandbox_free_string` — 释放 Rust 分配的字符串

**内存容量约束**：单次 FFI 调用数据 <64MB；句柄泄露 CI `ffi_leak_check` 扫描未配对的 `_free`。
