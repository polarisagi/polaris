# Polaris — FFI ABI 规范

> **§跳读**: 0:7 总则 / 1:19 内存 / 2:54 错误 / 3:68 并发 / 4:80 容量 / 5:85 命名 / 6:92 ABI协议
> 跨语言 FFI 边界 ABI 规范 (Go ↔ Rust)。

## 1. 总则

所有跨语言调用必经 C ABI (cdylib/CGO), 禁其他桥接。

### 1.1 适用范围
| 方向 | 调用方式 | 模块 | 用途 |
|------|---------|------|------|
| Go→Rust | `import "C"` + cdylib | `rust/substrate/` | 本地推理/向量/全文/图存储 |
| Rust→Go | Go 回调函数指针 | — | 预留 (暂未使用) |

### 1.2 隔离原则
- Rust cdylib 独立线程执行, 不阻塞 Go scheduler
- FFI 超时 <5s (Go `context.WithTimeout` 兜底)
- Rust 侧崩溃(SIGSEGV/SIGABRT)不可恢复 → Go 侧 `log.Fatalf` + CRITICAL 告警

## 2. 内存生命周期

### 2.1 所有权规则
| 分配方 | 释放方 | 场景 | 机制 |
|------|-------|------|------|
| Go (C.malloc) | Go (C.free) | 入参 | CGO 分配 C 内存 |
| Rust (Box) | Rust | 返回值 | Rust `extern "C"` 须提供对应 `_free` |
| Rust (String/Vec) | Rust | 跨调用持久态 | `Box::into_raw/from_raw` 传递 opaque ptr |

### 2.2 字符串传递
边界统一用 `*const c_char` (NUL-terminated):
- Go→Rust: `C.CString` → Rust `CStr` (只读)
- Rust→Go: `CString::into_raw` → 暴露 `_free_string(ptr)` 供 Go 释放
- 禁 Rust 返回 `*const u8` 切片 (防无 NUL 越界)

### 2.3 字节传递
结构化数据 (proto/JSON/向量) 用 FfiBytes:
```c
typedef struct { const uint8_t *data; size_t len; } FfiBytes;
void FfiBytes_drop(FfiBytes b); // Rust 侧释放
```

### 2.4 Opaque 句柄
长生命周期对象经 `*mut c_void` opaque ptr:
- 创建: `Box::into_raw` 返回 `*mut c_void`
- 消费: `Box::from_raw(handle as *mut Engine)`
- 异常: 统一返 NULL, Go 侧取 errno

## 3. 错误处理

### 3.1 返回值约定
| 场景 | Go 侧 | Rust 侧 |
|------|-------|---------|
| 成功 | 检查 NULL/nil | 返有效指针/0, errno=0 |
| 可恢复 | 读 errno + CGO 最后错误 | 返 NULL/错误码, 设置 errno |
| 不可恢复 | log.Fatalf + CRITICAL | 返 NULL, internal panic |

### 3.2 错误字符串
Rust 侧错误写入 `thread_local! FFI_LAST_ERROR`:
- 写: `set_last_error(msg)`
- 读: 导出 `ffi_last_error() -> *const c_char` 供 Go 获取

## 4. 并发安全

### 4.1 线程模型
- llama.cpp: 串行 (global mutex)
- LanceDB/CozoDB: 任意 (内建锁)
- Tantivy: 多读单写 (IndexWriter 独占)

### 4.2 禁止模式
- 禁 Rust 启动非导出长驻后台线程 (Go 无法追踪)
- 禁 Rust 回调 Go 函数指针 (跨 goroutine 会 panic)
- 禁 Rust 持有 Go 内存跨越边界 (防 GC 移动)

## 5. 内存容量约束
- 单次 FFI 调用 <64MB (超出触发 M11 KillSwitch Throttle)
- 句柄泄露: CI `ffi_leak_check` 扫描未成对的 `_free`
- Rust 侧 panic: 边界加 `catch_unwind`, panic 转换至 `log.Fatal`

## 6. 命名规范
- `{engine}_{action}`: `lancedb_search`
- `{type}_new` / `{type}_free`: `engine_new` / `engine_free` (必配对)
- `{type}_drop`: `FfiBytes_drop` (纯数据)
**所有 FFI 导出符号必须统一在 `rust/substrate/src/ffi/mod.rs` 声明, 禁散落.**

## 7. ABI 版本协议 (ADR-0011)

### 7.1 协议定义
导出 `substrate_abi_version() -> u32` (高16位=major, 低16位=minor)。
当前版本: **1.0** (`0x00010000`)。

### 7.2 校验机制
Go 侧 `purego.Dlopen` 后立即校验:
- major 不匹配 → **panic** (fail-fast)
- minor: 旧<新 允许; 旧>新 警告 (dylib 比 Go 旧)

### 7.3 升级条件
- **升 major**: 删导出、改签名、改错误码语义
- **升 minor**: 增导出、增错误码

### 7.4 符号清单 (ABI 1.0)
- **通用**: `substrate_abi_version`
- **M11**: `cedar_load_policies`, `cedar_evaluate`, `cedar_policy_count`, `cedar_free_string`
- **M2**: `surreal_open`, `surreal_kv_{get,put,delete,scan}`, `surreal_vec_{upsert,knn,set_mode}`, `surreal_graph_{relate,traverse}`, `surreal_fts_{index,search}`, `surreal_free_{string,buf}`
- **加载器**: 查 `POLARIS_SUBSTRATE_LIB` → 同级 `lib/` → dev 各级 `target/release/`