// surreal_store.rs — SurrealDB v3 统一认知存储引擎 FFI
// 后端运行时选择: kv-mem（默认，任意机器）/ kv-rocksdb（显式，≥16GB）
// 向量维度 vec_dim 由调用方传入，修复 v2 硬编码 DIMENSION 4 的 bug。
// 架构: docs/arch/M02-Storage-Fabric.md §10，ADR-0010

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;
use std::sync::{Arc, OnceLock, RwLock};

use surrealdb::engine::local::{Db, Mem, RocksDb};
use surrealdb::types::SurrealValue;
use surrealdb::Surreal;
use tokio::runtime::Runtime;

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const SURREAL_OK: c_int = 0;
const SURREAL_NOT_FOUND: c_int = 1;
const SURREAL_ERR_UTF8: c_int = -1;
const SURREAL_ERR_LOCK: c_int = -2;
const SURREAL_ERR_PANIC: c_int = -3;

// ─── 存储类型 ──────────────────────────────────────────────────────────────────

struct SurrealStore {
    db: Surreal<Db>,
    rt: Runtime,
}

impl SurrealStore {
    fn new(backend: &str, db_path: &str, vec_dim: u32) -> Result<Self, Box<dyn std::error::Error>> {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()?;

        let db = rt.block_on(async {
            if backend == "rocksdb" && !db_path.is_empty() {
                Surreal::new::<RocksDb>(db_path).await
            } else {
                Surreal::new::<Mem>(()).await
            }
        })?;
        rt.block_on(async { db.use_ns("polaris").use_db("cognition").await })?;

        let ddl = format!(
            "DEFINE TABLE IF NOT EXISTS kv SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS k ON kv TYPE string; \
             DEFINE FIELD IF NOT EXISTS v ON kv TYPE string; \
             DEFINE INDEX IF NOT EXISTS kv_k ON kv FIELDS k UNIQUE; \
             DEFINE TABLE IF NOT EXISTS vectors SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS embed ON vectors TYPE array<float>; \
             DEFINE INDEX IF NOT EXISTS hnsw_idx ON vectors FIELDS embed MTREE DIMENSION {vec_dim} DISTANCE COSINE; \
             DEFINE TABLE IF NOT EXISTS edges SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS from_id ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS edge_type ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS to_id ON edges TYPE string; \
             DEFINE INDEX IF NOT EXISTS edge_from ON edges FIELDS from_id, edge_type; \
             DEFINE TABLE IF NOT EXISTS docs SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS doc_id ON docs TYPE string; \
             DEFINE FIELD IF NOT EXISTS body ON docs TYPE string; \
             DEFINE ANALYZER IF NOT EXISTS ascii_lower TOKENIZERS class FILTERS lowercase; \
             DEFINE INDEX IF NOT EXISTS fts_idx ON docs FIELDS body SEARCH ANALYZER ascii_lower BM25;"
        );
        rt.block_on(async {
            let _ = db.query(&ddl).await;
        });

        Ok(SurrealStore { db, rt })
    }
}

static STORE: OnceLock<Arc<RwLock<SurrealStore>>> = OnceLock::new();

// ─── 查询结果结构（SurrealDB v3 required derive SurrealValue）──────────────────

#[derive(Debug, SurrealValue)]
struct KvRow {
    k: String,
    v: String,
}

#[derive(Debug, SurrealValue)]
struct VRow {
    v: String,
}

#[derive(Debug, SurrealValue)]
struct VecRow {
    id: String,
    score: f64,
}

#[derive(Debug, SurrealValue)]
struct ToIdRow {
    to_id: String,
}

#[derive(Debug, SurrealValue)]
struct DocScoreRow {
    doc_id: String,
    score: f64,
}

// ─── 内部工具 ──────────────────────────────────────────────────────────────────

fn write_cstr(out: *mut *mut c_char, s: &str) {
    if !out.is_null() {
        if let Ok(cs) = CString::new(s) {
            unsafe { *out = cs.into_raw() };
        }
    }
}

fn bytes_to_hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

fn hex_to_bytes(s: &str) -> Vec<u8> {
    (0..s.len())
        .step_by(2)
        .filter_map(|i| u8::from_str_radix(&s[i..i + 2], 16).ok())
        .collect()
}

fn encode_scored(results: &[VecRow]) -> String {
    let mut out = String::from("[");
    for (i, r) in results.iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        let id = r.id.replace('"', "\\\"");
        out.push_str(&format!("{{\"id\":\"{id}\",\"score\":{}}}", r.score));
    }
    out.push(']');
    out
}

fn encode_ids(ids: &[String]) -> String {
    let mut out = String::from("[");
    for (i, id) in ids.iter().enumerate() {
        if i > 0 {
            out.push(',');
        }
        let escaped = id.replace('"', "\\\"");
        out.push_str(&format!("\"{escaped}\""));
    }
    out.push(']');
    out
}

fn get_store() -> Option<Arc<RwLock<SurrealStore>>> {
    STORE.get().cloned()
}

// ─── surreal_open ──────────────────────────────────────────────────────────────

/// 初始化全局 SurrealStore（幂等，多次调用安全）。
/// backend: "mem"（默认）或 "rocksdb"（要求 ≥16GB，db_path 不可为空）。
/// vec_dim: HNSW 向量维度，需与实际嵌入模型一致（典型值 1536）。
///
/// # Safety
/// backend/db_path 须为有效 NUL-terminated C 字符串或 null。
#[no_mangle]
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

    let result = panic::catch_unwind(move || {
        STORE.get_or_init(|| {
            Arc::new(RwLock::new(
                SurrealStore::new(&bk, &path, dim).expect("failed to init surreal store"),
            ))
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_get ───────────────────────────────────────────────────────────

/// 读取 KV 值。out_val/out_len 指向 Rust 分配 buffer，须 surreal_free_buf 释放。
/// 返回 0=找到, 1=不存在, 负数=错误。
///
/// # Safety
/// key 须为 key_len 字节长的有效内存地址。
#[no_mangle]
pub unsafe extern "C" fn surreal_kv_get(
    key: *const u8,
    key_len: usize,
    out_val: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let key_owned = unsafe { std::slice::from_raw_parts(key, key_len) }.to_vec();
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
        let res: Option<VRow> = guard
            .rt
            .block_on(async {
                let mut resp = guard
                    .db
                    .query("SELECT v FROM kv WHERE k = $k LIMIT 1")
                    .bind(("k", key_hex))
                    .await?;
                let rows: Vec<VRow> = resp.take(0)?;
                Ok::<Option<VRow>, surrealdb::Error>(rows.into_iter().next())
            })
            .unwrap_or(None);

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

/// # Safety
/// key/val 须为对应 len 字节长的有效内存地址。
#[no_mangle]
pub unsafe extern "C" fn surreal_kv_put(
    key: *const u8,
    key_len: usize,
    val: *const u8,
    val_len: usize,
) -> c_int {
    let k = bytes_to_hex(unsafe { std::slice::from_raw_parts(key, key_len) });
    let v = bytes_to_hex(unsafe { std::slice::from_raw_parts(val, val_len) });
    let result = panic::catch_unwind(|| {
        let store_arc = match get_store() {
            Some(s) => s,
            None => return SURREAL_ERR_LOCK,
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query("UPSERT kv SET k = $k, v = $v WHERE k = $k")
                .bind(("k", k))
                .bind(("v", v))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_delete ────────────────────────────────────────────────────────

/// # Safety
/// key 须为 key_len 字节长的有效内存地址。
#[no_mangle]
pub unsafe extern "C" fn surreal_kv_delete(key: *const u8, key_len: usize) -> c_int {
    let k = bytes_to_hex(unsafe { std::slice::from_raw_parts(key, key_len) });
    let result = panic::catch_unwind(|| {
        let store_arc = match get_store() {
            Some(s) => s,
            None => return SURREAL_ERR_LOCK,
        };
        let guard = match store_arc.read() {
            Ok(g) => g,
            Err(_) => return SURREAL_ERR_LOCK,
        };
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query("DELETE kv WHERE k = $k")
                .bind(("k", k))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_kv_scan ──────────────────────────────────────────────────────────

/// 前缀扫描，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"k":"<hex>","v":"<hex>"},...]
///
/// # Safety
/// prefix 须为 prefix_len 字节长的有效内存地址。
#[no_mangle]
pub unsafe extern "C" fn surreal_kv_scan(
    prefix: *const u8,
    prefix_len: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let prefix_owned = if prefix_len == 0 {
        vec![]
    } else {
        unsafe { std::slice::from_raw_parts(prefix, prefix_len) }.to_vec()
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
        let rows: Vec<KvRow> = guard
            .rt
            .block_on(async {
                let mut resp = guard
                    .db
                    .query("SELECT k, v FROM kv WHERE string::startsWith(k, $prefix) ORDER BY k")
                    .bind(("prefix", prefix_hex))
                    .await?;
                resp.take(0)
            })
            .unwrap_or_default();

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

// ─── surreal_vec_upsert ───────────────────────────────────────────────────────

/// # Safety
/// id 须为 NUL-terminated UTF-8；embed 须指向 dim 个 f32。
#[no_mangle]
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
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query("UPSERT vectors SET id = $id, embed = $embed")
                .bind(("id", id_str))
                .bind(("embed", embed_vec))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_vec_knn ──────────────────────────────────────────────────────────

/// K 近邻向量检索，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"id":"<id>","score":<f64>},...]
///
/// # Safety
/// query 须指向 dim 个 f32。
#[no_mangle]
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
        // k 须为字面量嵌入查询字符串
        let sql = format!(
            "SELECT id, vector::similarity::cosine(embed, $q) AS score \
             FROM vectors WHERE embed <|{k}|> $q ORDER BY score DESC"
        );
        let rows: Vec<VecRow> = guard
            .rt
            .block_on(async {
                let mut resp = guard.db.query(&sql).bind(("q", q_vec)).await?;
                resp.take(0)
            })
            .unwrap_or_default();

        write_cstr(out_json, &encode_scored(&rows));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_graph_relate ─────────────────────────────────────────────────────

/// 写入一条有向图边 from -[edge_type]-> to。
///
/// # Safety
/// 所有参数须为有效 NUL-terminated UTF-8 C 字符串。
#[no_mangle]
pub unsafe extern "C" fn surreal_graph_relate(
    from_id: *const c_char,
    edge_type: *const c_char,
    to_id: *const c_char,
) -> c_int {
    let from = match unsafe { CStr::from_ptr(from_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let et = match unsafe { CStr::from_ptr(edge_type) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let to = match unsafe { CStr::from_ptr(to_id) }.to_str() {
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
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query("INSERT INTO edges (from_id, edge_type, to_id) VALUES ($from, $et, $to)")
                .bind(("from", from))
                .bind(("et", et))
                .bind(("to", to))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_graph_traverse ───────────────────────────────────────────────────

/// BFS 图遍历，返回 JSON CString，须 surreal_free_string 释放。
/// edge_type 为空串表示匹配所有边类型。不包含起点自身。
/// JSON: ["id1","id2",...]
///
/// # Safety
/// start_id/edge_type 须为有效 NUL-terminated UTF-8 C 字符串。
#[no_mangle]
pub unsafe extern "C" fn surreal_graph_traverse(
    start_id: *const c_char,
    edge_type: *const c_char,
    max_depth: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let start = match unsafe { CStr::from_ptr(start_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => {
            write_cstr(out_json, "[]");
            return SURREAL_ERR_UTF8;
        }
    };
    let et = match unsafe { CStr::from_ptr(edge_type) }.to_str() {
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

        let mut visited: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut frontier = vec![start.clone()];
        visited.insert(start.clone());

        for _ in 0..max_depth {
            if frontier.is_empty() {
                break;
            }
            let sql = if et.is_empty() {
                "SELECT to_id FROM edges WHERE from_id IN $frontier".to_string()
            } else {
                "SELECT to_id FROM edges WHERE from_id IN $frontier AND edge_type = $et".to_string()
            };
            let next: Vec<String> = guard
                .rt
                .block_on(async {
                    let mut resp = guard
                        .db
                        .query(&sql)
                        .bind(("frontier", frontier.clone()))
                        .bind(("et", et.clone()))
                        .await?;
                    let rows: Vec<ToIdRow> = resp.take(0)?;
                    Ok::<Vec<String>, surrealdb::Error>(rows.into_iter().map(|r| r.to_id).collect())
                })
                .unwrap_or_default();

            frontier = next
                .into_iter()
                .filter(|id| visited.insert(id.clone()))
                .collect();
        }

        // 排除起点自身
        let result_ids: Vec<String> = visited.into_iter().filter(|id| id != &start).collect();
        write_cstr(out_json, &encode_ids(&result_ids));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_fts_index ────────────────────────────────────────────────────────

/// 将文档写入全文检索索引（upsert 语义）。
///
/// # Safety
/// doc_id/text 须为有效 NUL-terminated UTF-8 C 字符串。
#[no_mangle]
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
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query(
                    "DELETE docs WHERE doc_id = $id; \
                     INSERT INTO docs (doc_id, body) VALUES ($id, $body)",
                )
                .bind(("id", id))
                .bind(("body", body))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_fts_search ───────────────────────────────────────────────────────

/// BM25 全文检索，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"id":"<doc_id>","score":<f64>},...]
///
/// # Safety
/// query 须为有效 NUL-terminated UTF-8 C 字符串。
#[no_mangle]
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
        let sql = format!(
            "SELECT doc_id, search::score(0) AS score FROM docs \
             WHERE body @0@ $q ORDER BY score DESC LIMIT {k}"
        );
        let rows: Vec<DocScoreRow> = guard
            .rt
            .block_on(async {
                let mut resp = guard.db.query(&sql).bind(("q", q)).await?;
                resp.take(0)
            })
            .unwrap_or_default();

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

// ─── 内存管理 FFI ─────────────────────────────────────────────────────────────

/// 释放 surreal_kv_scan / surreal_vec_knn / surreal_graph_traverse / surreal_fts_search
/// 返回的 JSON CString。
///
/// # Safety
/// ptr 须为上述函数分配的指针，或 null。
#[no_mangle]
pub unsafe extern "C" fn surreal_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        unsafe { drop(CString::from_raw(ptr)) };
    }
}

/// 释放 surreal_kv_get 分配的二进制 buffer。
///
/// # Safety
/// ptr 须为 surreal_kv_get 分配的指针，len 须与 out_len 一致，或 ptr 为 null。
#[no_mangle]
pub unsafe extern "C" fn surreal_free_buf(ptr: *mut u8, len: usize) {
    if !ptr.is_null() && len > 0 {
        unsafe { drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len))) };
    }
}

// ─── surreal_vec_set_mode — 兼容接口（no-op）──────────────────────────────────
// SurrealDB MTREE 索引始终激活，此函数保留仅为 ABI 兼容。

#[no_mangle]
pub extern "C" fn surreal_vec_set_mode(_mode: c_int) -> c_int {
    SURREAL_OK
}

// ─── surreal_stats ─────────────────────────────────────────────────────────────

/// 返回当前后端状态 JSON，须 surreal_free_string 释放。
/// JSON: {"backend":"mem"|"rocksdb","ready":true|false}
#[no_mangle]
pub extern "C" fn surreal_stats(out_json: *mut *mut c_char) -> c_int {
    let ready = STORE.get().is_some();
    let json = format!("{{\"backend\":\"surreal\",\"ready\":{ready}}}");
    write_cstr(out_json, &json);
    SURREAL_OK
}
