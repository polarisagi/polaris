// surreal_store/vector.rs — HNSW 向量 FFI：surreal_vec_upsert / delete / knn / set_mode

use std::ffi::CStr;
use std::os::raw::{c_char, c_int};
use std::panic;

use super::{
    encode_scored, get_store, write_cstr, VecRow, SURREAL_ERR_LOCK, SURREAL_ERR_PANIC,
    SURREAL_ERR_QUERY, SURREAL_ERR_UTF8, SURREAL_OK,
};

// ─── surreal_vec_upsert ───────────────────────────────────────────────────────

/// 写入或更新向量嵌入。使用 type::record('vectors', $id) 确定性 RecordId 保证 UPSERT 幂等。
/// 原 `UPSERT vectors SET id = $id, embed = $embed`：SCHEMAFULL 模式下
/// `id` 不是普通字段，SET id 无效 → record ID 随机生成，UPSERT 不幂等。
///
/// # Safety
/// id 须为 NUL-terminated UTF-8；embed 须指向 dim 个 f32。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_vec_upsert(
    id: *const c_char,
    embed: *const f32,
    dim: usize,
) -> c_int {
    let id_str = match unsafe { CStr::from_ptr(id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let embed_vec = unsafe { std::slice::from_raw_parts(embed, dim) }.to_vec();
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
            // type::record('vectors', $id) → record ID = vectors:$id，幂等
            guard
                .db
                .query("UPSERT type::record('vectors', $id) SET embed = $embed")
                .bind(("id", id_str))
                .bind(("embed", embed_vec))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_vec_upsert] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_vec_delete ───────────────────────────────────────────────────────

/// 从 HNSW 索引删除向量记录（供 Forget 路径调用，与 SQLite float16 BLOB 同步清理）。
///
/// # Safety
/// id 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_vec_delete(id: *const c_char) -> c_int {
    let id_str = match unsafe { CStr::from_ptr(id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let result = panic::catch_unwind(|| {
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
                .query("DELETE type::record('vectors', $id)")
                .bind(("id", id_str))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_vec_delete] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_vec_knn ──────────────────────────────────────────────────────────

/// K 近邻向量检索，返回 JSON CString（须 surreal_free_string 释放）。
/// JSON: [{"id":"<id>","score":<f64>},...]
///
/// 使用 record::id(id) AS id 将 RecordId（"vectors:xxx"）转为纯字符串 "xxx"。
/// <|{k},COSINE|> 明确指定距离函数，与 DDL HNSW DIST COSINE 定义一致。
///
/// # Safety
/// query 须指向 dim 个 f32。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_vec_knn(
    query: *const f32,
    dim: usize,
    k: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    if query.is_null() || dim == 0 || k == 0 {
        write_cstr(out_json, "[]");
        return SURREAL_OK;
    }
    let q_vec: Vec<f32> = unsafe { std::slice::from_raw_parts(query, dim) }.to_vec();
    let result = panic::catch_unwind(|| {
        let Some(store_arc) = get_store() else {
            write_cstr(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        // record::id(id) AS id — 提取 RecordId key 部分为纯字符串
        // <|{k},COSINE|> — 明确距离函数，与 HNSW 索引定义保持一致
        let sql = format!(
            "SELECT record::id(id) AS id, vector::similarity::cosine(embed, $q) AS score \
             FROM vectors WHERE embed <|{k},COSINE|> $q ORDER BY score DESC"
        );
        // 查询出错时返回 SURREAL_ERR_QUERY，不以空结果伪装"无匹配"
        let rows_result: Result<Vec<VecRow>, surrealdb::Error> = guard.rt.block_on(async {
            let mut resp = guard.db.query(&sql).bind(("q", q_vec)).await?;
            resp.take(0)
        });
        let rows = match rows_result {
            Ok(r) => r,
            Err(e) => {
                eprintln!("[surreal_vec_knn] query error: {e}");
                return SURREAL_ERR_QUERY;
            }
        };

        write_cstr(out_json, &encode_scored(&rows));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_vec_set_mode — 兼容接口（no-op）────────────────────────────────
// SurrealDB HNSW 索引始终激活，此函数保留仅为 ABI 兼容（原 MTREE 模式切换接口）。

#[unsafe(no_mangle)]
pub extern "C" fn surreal_vec_set_mode(_mode: c_int) -> c_int {
    super::SURREAL_OK
}
