// llama_infer/dispatch.rs — 公开 FFI 函数
// 约定与 native_sandbox/dispatch.rs 一致: input_json: *const c_char，
// out_json/out_err: *mut *mut c_char（out_* 可为 null），panic 经
// catch_unwind 捕获转为错误码，字符串统一走 llama_infer_free_string 释放。

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;

use super::{
    EmbedRequest, GenerateRequest, LoadModelRequest, RerankRequest, embed, evict_kv_cache,
    generate, load_model, rerank, status, unload_model,
};

pub const LI_OK: c_int = 0;
pub const LI_ERR_UTF8: c_int = -1;
pub const LI_ERR_PARSE: c_int = -2;
pub const LI_ERR_INTERNAL: c_int = -3;

fn li_write_cstr(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    let s = msg.replace('\0', "?");
    if let Ok(cs) = CString::new(s) {
        unsafe { *out = cs.into_raw() };
    }
}

unsafe fn li_read_cstr<'a>(ptr: *const c_char) -> Result<&'a str, ()> {
    unsafe {
        if ptr.is_null() {
            return Ok("");
        }
        CStr::from_ptr(ptr).to_str().map_err(|_| ())
    }
}

/// llama_infer_load — 加载/热切换本地模型（单槽位，覆盖式替换）。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_load(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match li_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    li_write_cstr(out_err, "invalid UTF-8 in input_json");
                    return LI_ERR_UTF8;
                }
            };
            let req: LoadModelRequest = match serde_json::from_str(json_str) {
                Ok(r) => r,
                Err(e) => {
                    li_write_cstr(out_err, &format!("JSON parse error: {e}"));
                    return LI_ERR_PARSE;
                }
            };
            match load_model(req) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(j) => {
                        li_write_cstr(out_json, &j);
                        LI_OK
                    }
                    Err(e) => {
                        li_write_cstr(out_err, &format!("serialize error: {e}"));
                        LI_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    li_write_cstr(out_err, &e);
                    LI_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                li_write_cstr(out_err, "panic in llama_infer_load");
                LI_ERR_INTERNAL
            }
        }
    }
}

/// llama_infer_unload — 卸载当前模型，释放所有资源。
///
/// # Safety
/// out_err 须为有效指针或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_unload(out_err: *mut *mut c_char) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        match unload_model() {
            Ok(()) => LI_OK,
            Err(e) => {
                li_write_cstr(out_err, &e);
                LI_ERR_INTERNAL
            }
        }
    });
    match result {
        Ok(code) => code,
        Err(_) => {
            li_write_cstr(out_err, "panic in llama_infer_unload");
            LI_ERR_INTERNAL
        }
    }
}

/// llama_infer_generate — 对话生成（chat template + sampler chain + 可选 GBNF grammar）。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_generate(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match li_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    li_write_cstr(out_err, "invalid UTF-8 in input_json");
                    return LI_ERR_UTF8;
                }
            };
            let req: GenerateRequest = match serde_json::from_str(json_str) {
                Ok(r) => r,
                Err(e) => {
                    li_write_cstr(out_err, &format!("JSON parse error: {e}"));
                    return LI_ERR_PARSE;
                }
            };
            match generate(req) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(j) => {
                        li_write_cstr(out_json, &j);
                        LI_OK
                    }
                    Err(e) => {
                        li_write_cstr(out_err, &format!("serialize error: {e}"));
                        LI_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    li_write_cstr(out_err, &e);
                    LI_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                li_write_cstr(out_err, "panic in llama_infer_generate");
                LI_ERR_INTERNAL
            }
        }
    }
}

/// llama_infer_embed — 批量文本嵌入（Mean pooling）。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_embed(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match li_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    li_write_cstr(out_err, "invalid UTF-8 in input_json");
                    return LI_ERR_UTF8;
                }
            };
            let req: EmbedRequest = match serde_json::from_str(json_str) {
                Ok(r) => r,
                Err(e) => {
                    li_write_cstr(out_err, &format!("JSON parse error: {e}"));
                    return LI_ERR_PARSE;
                }
            };
            match embed(req) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(j) => {
                        li_write_cstr(out_json, &j);
                        LI_OK
                    }
                    Err(e) => {
                        li_write_cstr(out_err, &format!("serialize error: {e}"));
                        LI_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    li_write_cstr(out_err, &e);
                    LI_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                li_write_cstr(out_err, "panic in llama_infer_embed");
                LI_ERR_INTERNAL
            }
        }
    }
}

/// llama_infer_rerank — 查询-文档相关性打分（双塔嵌入余弦相似度）。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_rerank(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match li_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    li_write_cstr(out_err, "invalid UTF-8 in input_json");
                    return LI_ERR_UTF8;
                }
            };
            let req: RerankRequest = match serde_json::from_str(json_str) {
                Ok(r) => r,
                Err(e) => {
                    li_write_cstr(out_err, &format!("JSON parse error: {e}"));
                    return LI_ERR_PARSE;
                }
            };
            match rerank(req) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(j) => {
                        li_write_cstr(out_json, &j);
                        LI_OK
                    }
                    Err(e) => {
                        li_write_cstr(out_err, &format!("serialize error: {e}"));
                        LI_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    li_write_cstr(out_err, &e);
                    LI_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                li_write_cstr(out_err, "panic in llama_infer_rerank");
                LI_ERR_INTERNAL
            }
        }
    }
}

/// llama_infer_evict_kv_cache — 清空当前常驻生成上下文的 KV cache。
///
/// # Safety
/// out_err 须为有效指针或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_evict_kv_cache(out_err: *mut *mut c_char) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        match evict_kv_cache() {
            Ok(()) => LI_OK,
            Err(e) => {
                li_write_cstr(out_err, &e);
                LI_ERR_INTERNAL
            }
        }
    });
    match result {
        Ok(code) => code,
        Err(_) => {
            li_write_cstr(out_err, "panic in llama_infer_evict_kv_cache");
            LI_ERR_INTERNAL
        }
    }
}

/// llama_infer_status — 查询当前加载状态（loaded/path/n_ctx 等）。
///
/// # Safety
/// out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_status(
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        match status() {
            Ok(resp) => match serde_json::to_string(&resp) {
                Ok(j) => {
                    li_write_cstr(out_json, &j);
                    LI_OK
                }
                Err(e) => {
                    li_write_cstr(out_err, &format!("serialize error: {e}"));
                    LI_ERR_INTERNAL
                }
            },
            Err(e) => {
                li_write_cstr(out_err, &e);
                LI_ERR_INTERNAL
            }
        }
    });
    match result {
        Ok(code) => code,
        Err(_) => {
            li_write_cstr(out_err, "panic in llama_infer_status");
            LI_ERR_INTERNAL
        }
    }
}

/// llama_infer_free_string — 释放由 llama_infer_* 分配的 C 字符串。
///
/// # Safety
/// ptr 须为 llama_infer_* 分配的指针，或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn llama_infer_free_string(ptr: *mut c_char) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
        if !ptr.is_null() {
            drop(CString::from_raw(ptr));
        }
    }));
}
