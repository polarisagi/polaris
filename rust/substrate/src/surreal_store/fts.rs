// surreal_store/fts.rs — BM25 全文检索 FFI：
//   surreal_fts_index / delete / search / surreal_stats / 内存管理 FFI

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;

use super::{
    CountRow, DocScoreRow, SURREAL_ERR_LOCK, SURREAL_ERR_PANIC, SURREAL_ERR_QUERY,
    SURREAL_ERR_UTF8, SURREAL_OK, get_store, write_cstr,
};

// ─── surreal_fts_index ────────────────────────────────────────────────────────

/// 索引或更新文档。使用 type::record('docs', $id) UPSERT，原子操作代替非原子 DELETE + INSERT。
/// 原实现：DELETE docs WHERE doc_id=$id; INSERT INTO docs ... 不是原子操作，
/// 且 doc_id 是普通字段无 UNIQUE 约束 → 多次写入产生重复文档降低 BM25 精度。
///
/// # Safety
/// doc_id/text 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_fts_index(doc_id: *const c_char, text: *const c_char) -> c_int {
    let id = match unsafe { CStr::from_ptr(doc_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let body = match unsafe { CStr::from_ptr(text) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = get_store() else {
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let q_res = guard.rt.block_on(async {
            // 原子 UPSERT，record ID = docs:$id，天然唯一
            guard
                .db
                .query("UPSERT type::record('docs', $id) SET body = $body")
                .bind(("id", id))
                .bind(("body", body))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_fts_index] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_fts_delete ───────────────────────────────────────────────────────

/// 从 FTS 索引删除文档（供 Forget 路径调用，与 episodic_memory 表同步清理）。
///
/// # Safety
/// doc_id 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_fts_delete(doc_id: *const c_char) -> c_int {
    let id = match unsafe { CStr::from_ptr(doc_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = get_store() else {
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        let q_res = guard.rt.block_on(async {
            guard
                .db
                .query("DELETE type::record('docs', $id)")
                .bind(("id", id))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_fts_delete] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_fts_search ───────────────────────────────────────────────────────

/// BM25 全文检索，返回 JSON CString（须 surreal_free_string 释放）。
/// JSON: [{"id":"<doc_id>","score":<f64>},...]
///
/// 使用 record::id(id) AS doc_id，与 type::record UPSERT 结构匹配。
/// 原 `SELECT doc_id` 对应表字段，已从 DDL 移除。
///
/// # Safety
/// query 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_fts_search(
    query: *const c_char,
    k: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let q = match unsafe { CStr::from_ptr(query) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => {
            write_cstr(out_json, "[]");
            return SURREAL_ERR_UTF8;
        }
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = get_store() else {
            write_cstr(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        // record::id(id) AS doc_id — 与 type::record 存储的 record ID 结构一致
        let sql = format!(
            "SELECT record::id(id) AS doc_id, search::score(0) AS score FROM docs \
             WHERE body @0@ $q ORDER BY score DESC LIMIT {k}"
        );
        // 查询出错时返回 SURREAL_ERR_QUERY，不以空结果伪装"无匹配"
        let rows_result: Result<Vec<DocScoreRow>, surrealdb::Error> = guard.rt.block_on(async {
            let mut resp = guard.db.query(&sql).bind(("q", q)).await?;
            resp.take(0)
        });
        let rows = match rows_result {
            Ok(r) => r,
            Err(e) => {
                eprintln!("[surreal_fts_search] query error: {e}");
                return SURREAL_ERR_QUERY;
            }
        };

        let mut json = String::from("[");
        let mut first = true;
        for r in &rows {
            if !first {
                json.push(',');
            }
            let id = r.doc_id.replace('"', "\\\"");
            json.push_str(&format!("{{\"id\":\"{id}\",\"score\":{}}}", r.score));
            first = false;
        }
        json.push(']');
        write_cstr(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_stats ─────────────────────────────────────────────────────────────

/// 返回当前后端状态 JSON（须 surreal_free_string 释放）。
/// JSON: {"backend":"surreal","ready":true,"kv_count":N,"vec_count":N,"doc_count":N,"edge_count":N}
/// 未初始化时：{"backend":"none","ready":false}
#[unsafe(no_mangle)]
pub extern "C" fn surreal_stats(out_json: *mut *mut c_char) -> c_int {
    let result = panic::catch_unwind(|| {
        let Some(store_arc) = get_store() else {
            write_cstr(out_json, r#"{"backend":"none","ready":false}"#);
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => {
                write_cstr(out_json, r#"{"backend":"error","ready":false}"#);
                return SURREAL_ERR_LOCK;
            }
        };

        let (kv_count, vec_count, doc_count, edge_count) = guard.rt.block_on(async {
            let kv: i64 = guard
                .db
                .query("SELECT count() AS count FROM kv GROUP ALL")
                .await
                .ok()
                .and_then(|mut r| r.take::<Vec<CountRow>>(0).ok())
                .and_then(|v| v.into_iter().next())
                .map(|r| r.count)
                .unwrap_or(0);
            let vec: i64 = guard
                .db
                .query("SELECT count() AS count FROM vectors GROUP ALL")
                .await
                .ok()
                .and_then(|mut r| r.take::<Vec<CountRow>>(0).ok())
                .and_then(|v| v.into_iter().next())
                .map(|r| r.count)
                .unwrap_or(0);
            let doc: i64 = guard
                .db
                .query("SELECT count() AS count FROM docs GROUP ALL")
                .await
                .ok()
                .and_then(|mut r| r.take::<Vec<CountRow>>(0).ok())
                .and_then(|v| v.into_iter().next())
                .map(|r| r.count)
                .unwrap_or(0);
            let edge: i64 = guard
                .db
                .query("SELECT count() AS count FROM edges GROUP ALL")
                .await
                .ok()
                .and_then(|mut r| r.take::<Vec<CountRow>>(0).ok())
                .and_then(|v| v.into_iter().next())
                .map(|r| r.count)
                .unwrap_or(0);
            (kv, vec, doc, edge)
        });

        let json = format!(
            r#"{{"backend":"surreal","ready":true,"kv_count":{kv_count},"vec_count":{vec_count},"doc_count":{doc_count},"edge_count":{edge_count}}}"#
        );
        write_cstr(out_json, &json);
        SURREAL_OK
    });

    match result {
        Ok(ret) => ret,
        Err(_) => {
            write_cstr(
                out_json,
                r#"{"backend":"surreal","ready":false,"error":"panic"}"#,
            );
            SURREAL_ERR_PANIC
        }
    }
}

// ─── 内存管理 FFI ─────────────────────────────────────────────────────────────

/// 释放 surreal_kv_scan / surreal_vec_knn / surreal_graph_traverse / surreal_fts_search
/// 返回的 JSON CString。
///
/// # Safety
/// ptr 须为上述函数分配的指针，或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_free_string(ptr: *mut c_char) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        if !ptr.is_null() {
            unsafe { drop(CString::from_raw(ptr)) };
        }
    }));
}

/// 释放 surreal_kv_get 分配的二进制 buffer。
///
/// # Safety
/// ptr 须为 surreal_kv_get 分配的指针，len 须与 out_len 一致，或 ptr 为 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_free_buf(ptr: *mut u8, len: usize) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        if !ptr.is_null() && len > 0 {
            unsafe { drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len))) };
        }
    }));
}

/// KV-mem 后端无法显式 purge，no-op 安全；kv-rocksdb 未来可调用 compact_range。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_purge() {}
