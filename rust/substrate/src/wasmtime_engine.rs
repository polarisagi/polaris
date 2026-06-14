use std::ffi::CString;
use std::os::raw::{c_char, c_int};
use std::panic;
use std::path::Path;
use std::sync::{Arc, Mutex};
use wasmtime::*;
use wasmtime_wasi::{DirPerms, FilePerms, WasiCtxBuilder};

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const WASMTIME_OK: c_int = 0;
const WASMTIME_ERR_INTERNAL: c_int = -1;
const WASMTIME_ERR_COMPILE: c_int = -2;
const WASMTIME_ERR_EXECUTE: c_int = -3;
const WASMTIME_ERR_UTF8: c_int = -4;

pub struct EngineState {
    pub engine: Engine,
}

struct SandboxState {
    wasi: wasmtime_wasi::p1::WasiP1Ctx,
    max_pages: usize,
}

impl wasmtime::ResourceLimiter for SandboxState {
    fn memory_growing(
        &mut self,
        _current: usize,
        desired: usize,
        _maximum: Option<usize>,
    ) -> Result<bool> {
        // desired is in bytes. max_pages is in 64KiB pages.
        Ok(desired <= self.max_pages * 65536)
    }

    fn table_growing(
        &mut self,
        _current: usize,
        _desired: usize,
        _maximum: Option<usize>,
    ) -> Result<bool> {
        Ok(true)
    }
}

impl EngineState {
    pub fn new() -> Result<Self, anyhow::Error> {
        let mut config = Config::new();
        config.wasm_component_model(true); // 开启 Component Model
        config.consume_fuel(true); // 开启燃料计费，用于大模型代码防死循环

        let engine = Engine::new(&config)?;
        Ok(Self { engine })
    }
}

// 全局 Engine 单例，避免重复创建（开销较大）
lazy_static::lazy_static! {
    static ref GLOBAL_ENGINE: Mutex<Option<Arc<EngineState>>> = Mutex::new(None);
}

#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_pool_init(_n: c_int) -> c_int {
    WASMTIME_OK
}

#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_init(out_err: *mut *mut c_char) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        let mut global = GLOBAL_ENGINE.lock().unwrap();
        if global.is_none() {
            match EngineState::new() {
                Ok(state) => {
                    *global = Some(Arc::new(state));
                    write_err(out_err, "");
                    WASMTIME_OK
                }
                Err(e) => {
                    write_err(out_err, &format!("Engine init failed: {}", e));
                    WASMTIME_ERR_INTERNAL
                }
            }
        } else {
            WASMTIME_OK
        }
    });

    match result {
        Ok(code) => code,
        Err(_) => {
            write_err(out_err, "Panic in wasmtime_init");
            WASMTIME_ERR_INTERNAL
        }
    }
}

/// 释放由 wasmtime 侧分配的字符串
#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        unsafe { drop(CString::from_raw(ptr)) };
    }
}

/// 释放由 wasmtime 侧分配的字节切片
#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_free_bytes(ptr: *mut u8, len: usize) {
    if !ptr.is_null() {
        unsafe { drop(Vec::from_raw_parts(ptr, len, len)) };
    }
}

/// 写入 out 指针处的错误字符串（调用方须用 wasmtime_free_string 释放）。
fn write_err(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    match CString::new(msg) {
        Ok(cs) => unsafe { *out = cs.into_raw() },
        Err(_) => {
            let safe = msg.replace('\0', "?");
            if let Ok(cs) = CString::new(safe) {
                unsafe { *out = cs.into_raw() }
            }
        }
    }
}

// MVP: Provide a ping function to verify FFI linkage
#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_ping() -> c_int {
    42
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_execute(
    wasm_bytes: *const u8,
    wasm_len: usize,
    input_json: *const c_char,
    workspace_dir: *const c_char,
    max_pages: c_int,
    max_fuel: u64,
    network_allowed: c_int,
    max_output_bytes: c_int,
    out_json: *mut *mut u8,
    out_json_len: *mut usize,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            // 先取出 Arc clone，立即释放 Mutex。
            let engine_arc = {
                let global = GLOBAL_ENGINE.lock().unwrap();
                match global.as_ref() {
                    Some(s) => Arc::clone(s),
                    None => {
                        write_err(out_err, "Engine not initialized; call wasmtime_init first");
                        return WASMTIME_ERR_INTERNAL;
                    }
                }
            };

            let input_str = if input_json.is_null() {
                "{}"
            } else {
                match std::ffi::CStr::from_ptr(input_json).to_str() {
                    Ok(s) => s,
                    Err(_) => {
                        write_err(out_err, "Invalid UTF-8 in input_json");
                        return WASMTIME_ERR_UTF8;
                    }
                }
            };

            let bytes = std::slice::from_raw_parts(wasm_bytes, wasm_len);

            // 编译验证（在锁外执行，Engine 本身线程安全）
            let module = match Module::from_binary(&engine_arc.engine, bytes) {
                Ok(m) => m,
                Err(e) => {
                    write_err(out_err, &format!("Compile error: {}", e));
                    return WASMTIME_ERR_COMPILE;
                }
            };

            let mut linker: Linker<SandboxState> = Linker::new(&engine_arc.engine);
            if let Err(e) = wasmtime_wasi::p1::add_to_linker_sync(&mut linker, |cx| &mut cx.wasi) {
                write_err(out_err, &format!("Failed to add wasi to linker: {}", e));
                return WASMTIME_ERR_INTERNAL;
            }

            let stdin = wasmtime_wasi::p2::pipe::MemoryInputPipe::new(bytes::Bytes::from(
                input_str.to_owned(),
            ));

            let max_out = if max_output_bytes > 0 {
                max_output_bytes as usize
            } else {
                10 * 1024 * 1024
            };
            let stdout = wasmtime_wasi::p2::pipe::MemoryOutputPipe::new(max_out);

            let mut builder = WasiCtxBuilder::new();
            builder.stdin(stdin.clone()).stdout(stdout.clone());

            if network_allowed == 1 {
                builder.inherit_network();
                builder.allow_ip_name_lookup(true);
            }

            // 如果传入了工作目录，则挂载为 /workspace
            if !workspace_dir.is_null()
                && let Ok(host_path_str) = std::ffi::CStr::from_ptr(workspace_dir).to_str()
                && !host_path_str.is_empty()
            {
                let host_path = Path::new(host_path_str);
                if let Err(e) = builder.preopened_dir(
                    host_path,
                    "/workspace",
                    DirPerms::all(),
                    FilePerms::all(),
                ) {
                    write_err(out_err, &format!("Failed to preopen directory: {}", e));
                    return WASMTIME_ERR_INTERNAL;
                }
            }

            let wasi_ctx = builder.build_p1();

            // 最大内存页数（默认 256 页 = 16MB）
            let limit_pages = if max_pages > 0 {
                max_pages as usize
            } else {
                256
            };

            let state = SandboxState {
                wasi: wasi_ctx,
                max_pages: limit_pages,
            };
            let mut store = Store::new(&engine_arc.engine, state);
            store.limiter(|s| s as &mut dyn wasmtime::ResourceLimiter);

            // 燃料设定
            if let Err(e) = store.set_fuel(if max_fuel > 0 { max_fuel } else { 100_000_000 }) {
                write_err(out_err, &format!("Failed to set fuel: {}", e));
                return WASMTIME_ERR_INTERNAL;
            }

            let instance = match linker.instantiate(&mut store, &module) {
                Ok(i) => i,
                Err(e) => {
                    write_err(out_err, &format!("Instantiate error: {}", e));
                    return WASMTIME_ERR_EXECUTE;
                }
            };

            let start = match instance.get_typed_func::<(), ()>(&mut store, "_start") {
                Ok(f) => f,
                Err(_) => {
                    write_err(out_err, "Module does not export '_start' function");
                    return WASMTIME_ERR_EXECUTE;
                }
            };

            let exec_result = start.call(&mut store, ());

            if let Err(e) = exec_result {
                // 在整条错误因果链中查找 I32Exit。WASI proc_exit 经 p1 适配器抛出时，
                // I32Exit 可能位于 std::error::Error 的 source 链中，而 anyhow 顶层
                // downcast_ref 只查 anyhow 上下文层、不遍历 source 链 → 会漏判，把正常
                // exit(0) 误当作执行错误。改用 e.chain() 遍历全链以稳健识别退出码。
                let exit_code = e
                    .chain()
                    .find_map(|cause| cause.downcast_ref::<wasmtime_wasi::I32Exit>().map(|x| x.0));
                match exit_code {
                    // exit(0) 为正常退出，视为成功
                    Some(0) => {}
                    Some(code) => {
                        write_err(out_err, &format!("Execution error: exit status {}", code));
                        return WASMTIME_ERR_EXECUTE;
                    }
                    None => {
                        write_err(out_err, &format!("Execution error: {}", e));
                        return WASMTIME_ERR_EXECUTE;
                    }
                }
            }

            let output_bytes = stdout.contents();
            // into_boxed_slice() 保证底层分配 capacity == len，与 wasmtime_free_bytes 的
            // Vec::from_raw_parts(ptr, len, len) 严格匹配。原 shrink_to_fit() 仅尽力收缩、
            // 不保证 capacity == len，按 len 重建 Vec 释放会因容量不符触发 UB。
            let mut boxed = output_bytes.to_vec().into_boxed_slice();
            let len = boxed.len();
            let ptr = boxed.as_mut_ptr();
            std::mem::forget(boxed);
            if !out_json.is_null() {
                *out_json = ptr;
            }
            if !out_json_len.is_null() {
                *out_json_len = len;
            }
            WASMTIME_OK
        });

        match result {
            Ok(code) => code,
            Err(_) => {
                write_err(out_err, "Panic in wasmtime_execute");
                WASMTIME_ERR_INTERNAL
            }
        }
    }
}
