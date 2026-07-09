// surreal_store/store.rs — 初始化 FFI：surreal_set_worker_threads + surreal_open

use std::ffi::CStr;
use std::os::raw::{c_char, c_int};
use std::panic;
use std::sync::{Arc, RwLock};

use super::{STORE, SURREAL_ERR_PANIC, SURREAL_OK, SurrealStore, WORKER_THREADS};

use std::sync::atomic::Ordering;

// ─── surreal_set_worker_threads ────────────────────────────────────────────────

/// 配置 SurrealDB Tokio 运行时工作线程数，须在 surreal_open 前调用（否则使用默认值）。
/// n <= 0 = auto（min(CPU 核心数, 4)）；推荐 VPS 设 2 节省约 30-50MB 内存。
/// 此函数为扩展 API（SUBSTRATE_ABI_MINOR 1），不改变 surreal_open 签名。
#[unsafe(no_mangle)]
pub extern "C" fn surreal_set_worker_threads(n: c_int) -> c_int {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let v = if n <= 0 { 0u32 } else { n as u32 };
        WORKER_THREADS.store(v, Ordering::Relaxed);
        SURREAL_OK
    }))
    .unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_open ──────────────────────────────────────────────────────────────

/// 初始化全局 SurrealStore（幂等，多次调用安全）。
/// backend: "mem"（默认, kv-mem, 任意内存大小可用）或 "rocksdb"（持久化，推荐大内存服务器）。
/// vec_dim: HNSW 向量维度，须与实际嵌入模型一致（典型值 1536 或 768）。
///
/// # Safety
/// backend/db_path 须为有效 NUL-terminated C 字符串或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_open(
    backend: *const c_char,
    db_path: *const c_char,
    vec_dim: c_int,
) -> c_int {
    let bk = if backend.is_null() {
        "mem".to_string()
    } else {
        unsafe { CStr::from_ptr(backend) }
            .to_str()
            .unwrap_or("mem")
            .to_string()
    };
    let path = if db_path.is_null() {
        "".to_string()
    } else {
        unsafe { CStr::from_ptr(db_path) }
            .to_str()
            .unwrap_or("")
            .to_string()
    };
    let dim = (vec_dim.max(1)) as u32;

    let bk_clone = bk.clone();
    let path_clone = path.clone();

    let result = panic::catch_unwind(move || {
        let mut err_code = SURREAL_OK;

        static STORE_INIT: std::sync::Mutex<bool> = std::sync::Mutex::new(false);
        let mut initialized = STORE_INIT.lock().unwrap();

        if !*initialized {
            if STORE.get().is_none() {
                match SurrealStore::new(&bk_clone, &path_clone, dim) {
                    Ok(store) => {
                        let _ = STORE.set(Arc::new(RwLock::new(store)));
                        *initialized = true;
                    }
                    Err(e) => {
                        eprintln!("[surreal_store] fatal: failed to init surreal store: {e}");
                        err_code = SURREAL_ERR_PANIC;
                    }
                }
            } else {
                *initialized = true;
            }
        } else if STORE.get().is_none() {
            // 不变量违背：initialized=true 却无 STORE（理论不可达）。复用 PANIC 码但显式
            // 记录，避免与真正的 panic 混淆排障。
            eprintln!(
                "[surreal_store] invariant violation: init flag set but STORE empty; returning error"
            );
            err_code = SURREAL_ERR_PANIC;
        }

        err_code
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}
