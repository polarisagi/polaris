// surreal_store/kv.rs — KV CRUD FFI：surreal_kv_get / put / delete / scan

use std::os::raw::{c_char, c_int};
use std::panic;
use std::slice;

use super::{
    bytes_to_hex, get_store, hex_to_bytes, write_cstr, KvRow, VRow, SURREAL_ERR_LOCK,
    SURREAL_ERR_PANIC, SURREAL_ERR_QUERY, SURREAL_NOT_FOUND, SURREAL_OK,
};

// ─── surreal_kv_get ───────────────────────────────────────────────────────────

/// 读取 KV 值。out_val/out_len 指向 Rust 分配 buffer，须 surreal_free_buf 释放。
/// 返回 0=找到, 1=不存在, 负数=错误。
///
/// # Safety
/// key 须为 key_len 字节长的有效内存地址。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_kv_get(
    key: *const u8,
    key_len: usize,
    out_val: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let key_owned = unsafe { slice::from_raw_parts(key, key_len) }.to_vec();
    let key_hex = bytes_to_hex(&key_owned);
    let result = panic::catch_unwind(|| {
        let store_arc = match get_store() {
            Some(s) => s,
            None => return SURREAL_ERR_LOCK,
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let res_result = guard.rt.block_on(async {
            let mut resp = guard
                .db
                .query("SELECT v FROM kv WHERE k = $k LIMIT 1")
                .bind(("k", key_hex))
                .await?;
            let rows: Vec<VRow> = resp.take(0)?;
            Ok::<Option<VRow>, surrealdb::Error>(rows.into_iter().next())
        });

        let res = match res_result {
            Ok(opt) => opt,
            Err(e) => {
                eprintln!("[surreal_kv_get] Query error: {e}");
                return SURREAL_ERR_QUERY;
            }
        };

        match res {
            None => SURREAL_NOT_FOUND,
            Some(row) => {
                let val_bytes = hex_to_bytes(&row.v);
                let mut boxed = val_bytes.into_boxed_slice();
                let ptr = boxed.as_mut_ptr();
                let len = boxed.len();
                std::mem::forget(boxed);
                unsafe {
                    *out_val = ptr;
                    *out_len = len;
                }
                SURREAL_OK
            }
        }
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_put ───────────────────────────────────────────────────────────

/// 写入 KV。使用 type::record('kv', $k) 确定性 RecordId 保证 UPSERT 幂等，
/// 避免并发 WHERE 竞态导致 UNIQUE INDEX 冲突且数据静默丢失。
///
/// # Safety
/// key/val 须为对应 len 字节长的有效内存地址。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_kv_put(
    key: *const u8,
    key_len: usize,
    val: *const u8,
    val_len: usize,
) -> c_int {
    let k = bytes_to_hex(unsafe { slice::from_raw_parts(key, key_len) });
    let v = bytes_to_hex(unsafe { slice::from_raw_parts(val, val_len) });
    let result = panic::catch_unwind(|| {
        let store_arc = match get_store() {
            Some(s) => s,
            None => return SURREAL_ERR_LOCK,
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let q_res = guard.rt.block_on(async {
            guard
                .db
                .query("UPSERT type::record('kv', $k) SET k = $k, v = $v")
                .bind(("k", k))
                .bind(("v", v))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_kv_put] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_delete ────────────────────────────────────────────────────────

/// 删除 KV 记录。使用 DELETE type::record('kv', $k) 直接定位 record ID，O(1)。
///
/// # Safety
/// key 须为 key_len 字节长的有效内存地址。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_kv_delete(key: *const u8, key_len: usize) -> c_int {
    let k = bytes_to_hex(unsafe { slice::from_raw_parts(key, key_len) });
    let result = panic::catch_unwind(|| {
        let store_arc = match get_store() {
            Some(s) => s,
            None => return SURREAL_ERR_LOCK,
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let q_res = guard.rt.block_on(async {
            guard
                .db
                .query("DELETE type::record('kv', $k)")
                .bind(("k", k))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_kv_delete] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_scan ──────────────────────────────────────────────────────────

/// 前缀扫描，返回 JSON CString（须 surreal_free_string 释放）。
/// JSON: [{"k":"<hex>","v":"<hex>"},...]
/// 查询出错时返回 SURREAL_ERR_QUERY，不以空结果伪装"无匹配"。
///
/// # Safety
/// prefix 须为 prefix_len 字节长的有效内存地址（prefix_len=0 返回全表）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_kv_scan(
    prefix: *const u8,
    prefix_len: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let prefix_owned = if prefix_len == 0 {
        vec![]
    } else {
        unsafe { slice::from_raw_parts(prefix, prefix_len) }.to_vec()
    };
    let prefix_hex = bytes_to_hex(&prefix_owned);
    let result = panic::catch_unwind(|| {
        let Some(store_arc) = get_store() else {
            write_cstr(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let rows_result: Result<Vec<KvRow>, surrealdb::Error> = guard.rt.block_on(async {
            let mut resp = guard
                .db
                .query("SELECT k, v FROM kv WHERE string::starts_with(k, $prefix) ORDER BY k")
                .bind(("prefix", prefix_hex))
                .await?;
            let rows: Vec<KvRow> = resp.take(0)?;
            Ok(rows)
        });
        let rows = match rows_result {
            Ok(r) => r,
            Err(e) => {
                eprintln!("[surreal_kv_scan] query error: {e}");
                return SURREAL_ERR_QUERY;
            }
        };

        let mut json = String::from("[");
        let mut first = true;
        for row in &rows {
            if !first {
                json.push(',');
            }
            json.push_str(&format!("{{\"k\":\"{}\",\"v\":\"{}\"}}", row.k, row.v));
            first = false;
        }
        json.push(']');
        write_cstr(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}
