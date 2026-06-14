# Polaris — FFI ABI 规范

> **§跳读**: 0:7 总则 / 1:18 加载 / 2:40 ABI版本 / 3:55 错误 / 4:65 并发 / 5:78 命名
> 跨语言 FFI 边界 ABI 规范 (Go ↔ Rust)。**ADR-0011**: CGO 已全面迁移至 `github.com/ebitengine/purego`。

## 1. 总则

所有 Go→Rust 调用经 purego `Dlopen` / `RegisterLibFunc`，**禁用 CGO**（`import "C"` 不可出现在 FFI 路径）。Rust 侧编译为 cdylib（`.so`/`.dylib`），暴露 `extern "C"` 符号。

### 1.1 适用范围

| 方向 | 调用方式 | 模块 | 用途 |
|------|---------|------|------|
| Go→Rust | purego `RegisterLibFunc` | `rust/substrate/` | 本地推理/向量/全文/图存储 |
| Rust→Go | 不支持（禁回调函数指针） | — | 不适用 |

### 1.2 隔离原则

- purego 调用在 Go goroutine 中执行，不阻塞 Go scheduler（无 CGO 抢占问题）
- FFI 超时 <5s（`context.WithTimeout` 兜底）
- Rust 侧崩溃（SIGSEGV/SIGABRT）不可恢复 → Go 侧 `log.Fatalf` + CRITICAL 告警

## 2. 库加载

`pkg/substrate/ffi/dylib.go` 通过 `sync.Once` 加载 dylib，路径优先级：

1. `POLARIS_SUBSTRATE_LIB` 环境变量
2. 可执行文件同级 `lib/` 目录
3. dev：`rust/substrate/target/release/`（逐级向上搜索）

`RegisterLibFunc` 按函数签名将 Rust 导出符号绑定到 Go 函数变量（值类型）。字符串传参 **不经** `C.CString`，直接传 Go string（purego 内部处理 NUL 约定）。

### 2.1 字符串与字节传递

| 场景 | Go 侧 | Rust 侧 |
|------|-------|---------|
| Go→Rust 字符串 | Go `string`（purego 透传） | `*const c_char`（只读，CStr） |
| Rust→Go 字符串 | purego 接收 `uintptr`（字符串指针），Go 侧复制后调 `_free_string` | `CString::into_raw`，须配套导出 `_free_string(ptr)` |
| 结构化数据 | 序列化为 JSON/protobuf，通过 `*const u8 + len` 对传 | 接收 slice，Go 侧提供 `FfiBytes_drop` |

**purego 限制**：不支持结构体值类型跨边界（只传指针/标量）；复杂数据走 JSON 或 protobuf 编码后传指针+长度。

### 2.2 内存所有权

| 分配方 | 释放方 | 机制 |
|--------|--------|------|
| Go | Go | Go GC，不需手动释放 |
| Rust（返回值） | Rust | 须导出配套 `_free`，Go 侧调用释放 |
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

### 3.2 校验机制（`pkg/substrate/ffi/dylib.go` `verifyABI`）

- major 不匹配 → **panic**（fail-fast）
- minor：Go < Rust → warn（dylib 更新，Go 未同步）；Go > Rust → warn（dylib 过旧）

### 3.3 升级条件

- **升 major**：删导出符号、改函数签名、改错误码语义
- **升 minor**：新增导出符号、新增错误码

## 4. 错误处理

### 4.1 返回值约定

| 场景 | Go 侧 | Rust 侧 |
|------|-------|---------|
| 成功 | 检查零值/空指针 | 返有效值，errno=0 |
| 可恢复 | 读 errno + `ffi_last_error()` | 返 NULL/0，设 errno |
| 不可恢复 | `log.Fatalf` + CRITICAL | panic（`catch_unwind` 转 NULL）|

### 4.2 错误字符串

Rust 侧错误写 `thread_local! FFI_LAST_ERROR`：导出 `ffi_last_error() -> *const c_char`，Go 侧读取后调 `_free_string` 释放。

## 5. 并发安全

- **llama.cpp**：串行（global mutex）
- **LanceDB/CozoDB**：任意（内建锁）
- **Tantivy**：多读单写（IndexWriter 独占）

禁止 Rust 启动非导出长驻后台线程（Go 无法追踪）。

## 6. 命名规范

- `{engine}_{action}`：`lancedb_search`
- `{type}_new` / `{type}_free`：`engine_new` / `engine_free`（必配对）
- `{type}_drop`：`FfiBytes_drop`（纯数据）

**所有 FFI 导出符号必须统一在 `rust/substrate/src/ffi/mod.rs` 声明，禁散落。**

## 7. 符号清单（ABI 1.1）

- **通用**：`substrate_abi_version`, `ffi_last_error`, `ffi_last_error_free`
- **M11（Cedar）**：`cedar_load_policies`, `cedar_evaluate`, `cedar_policy_count`, `cedar_free_string`
- **M2（SurrealDB）**：`surreal_open`, `surreal_kv_{get,put,delete,scan}`, `surreal_vec_{upsert,knn,set_mode}`, `surreal_graph_{relate,traverse}`, `surreal_fts_{index,search}`, `surreal_free_{string,buf}`
- **内存容量约束**：单次 FFI 调用数据 <64MB；句柄泄露 CI `ffi_leak_check` 扫描未配对的 `_free`
