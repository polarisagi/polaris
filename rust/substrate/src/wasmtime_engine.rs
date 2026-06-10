use std::ffi::CString;
use std::os::raw::{c_char, c_int};
use std::panic;
use std::sync::{Arc, Mutex};
use wasmtime::*;

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const WASMTIME_OK: c_int = 0;
const WASMTIME_ERR_INTERNAL: c_int = -1;
#[allow(dead_code)]
const WASMTIME_ERR_COMPILE: c_int = -2;
#[allow(dead_code)]
const WASMTIME_ERR_EXECUTE: c_int = -3;
#[allow(dead_code)]
const WASMTIME_ERR_UTF8: c_int = -4;

pub struct EngineState {
    pub engine: Engine,
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

#[no_mangle]
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
#[no_mangle]
pub unsafe extern "C" fn wasmtime_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        unsafe { drop(CString::from_raw(ptr)) };
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
#[no_mangle]
pub extern "C" fn wasmtime_ping() -> c_int {
    42
}

#[no_mangle]
pub unsafe extern "C" fn wasmtime_execute(
    wasm_bytes: *const u8,
    wasm_len: usize,
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        let global = GLOBAL_ENGINE.lock().unwrap();
        let engine_state = match global.as_ref() {
            Some(s) => s,
            None => {
                write_err(out_err, "Engine not initialized");
                return WASMTIME_ERR_INTERNAL;
            }
        };

        let _input_str = if input_json.is_null() {
            ""
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
        
        // MVP: We'll implement execution by mapping WASI pipes later, 
        // for now let's just make sure it compiles with WasiCtxBuilder.
        // Wait, to safely test we can just compile the module.
        let _module = match Module::from_binary(&engine_state.engine, bytes) {
            Ok(m) => m,
            Err(e) => {
                write_err(out_err, &format!("Compile error: {}", e));
                return WASMTIME_ERR_COMPILE;
            }
        };

        write_err(out_json, "{\"status\": \"mock_success\"}");
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

