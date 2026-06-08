use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;
use std::sync::{Arc, OnceLock, RwLock};

use surrealdb::engine::local::{Db, Mem, RocksDb};
use surrealdb::Surreal;
use tokio::runtime::Runtime;

// ─── Surreal FFI 错误码 ────────────────────────────────────────────────────────
const SURREAL_OK: c_int = 0;
const SURREAL_NOT_FOUND: c_int = 1;

const SURREAL_ERR_PANIC: c_int = -3;

pub struct SurrealTier1Store {
    pub db: Surreal<Db>,
    pub rt: Runtime,
    pub use_hnsw: bool,
}

impl SurrealTier1Store {
    pub fn new(tier: i32, db_path: &str) -> Result<Self, Box<dyn std::error::Error>> {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()?;
        let db = rt.block_on(async {
            if tier >= 1 && !db_path.is_empty() {
                Surreal::new::<RocksDb>(db_path).await
            } else {
                Surreal::new::<Mem>(()).await
            }
        })?;
        rt.block_on(async { db.use_ns("polaris").use_db("cognition").await })?;

        rt.block_on(async {
            let _ = db.query(
                "DEFINE TABLE IF NOT EXISTS vectors SCHEMAFULL; \
                 DEFINE FIELD IF NOT EXISTS embed ON vectors TYPE array<float>; \
                 DEFINE INDEX IF NOT EXISTS hnsw_idx ON vectors FIELDS embed MTREE DIMENSION 4 DISTANCE COSINE; \
                 DEFINE TABLE IF NOT EXISTS edges SCHEMAFULL; \
                 DEFINE FIELD IF NOT EXISTS from_id ON edges TYPE string; \
                 DEFINE FIELD IF NOT EXISTS edge_type ON edges TYPE string; \
                 DEFINE FIELD IF NOT EXISTS to_id ON edges TYPE string; \
                 DEFINE INDEX IF NOT EXISTS edge_from ON edges FIELDS from_id, edge_type; \
                 DEFINE TABLE IF NOT EXISTS docs SCHEMAFULL; \
                 DEFINE FIELD IF NOT EXISTS doc_id ON docs TYPE string; \
                 DEFINE FIELD IF NOT EXISTS body ON docs TYPE string; \
                 DEFINE ANALYZER IF NOT EXISTS ascii_lower TOKENIZERS class FILTERS lowercase; \
                 DEFINE INDEX IF NOT EXISTS fts_idx ON docs FIELDS body SEARCH ANALYZER ascii_lower BM25;",
            )
            .await;
        });

        Ok(SurrealTier1Store {
            db,
            rt,
            use_hnsw: false,
        })
    }
}

static STORE_TIER1: OnceLock<Arc<RwLock<SurrealTier1Store>>> = OnceLock::new();

fn write_err(out_json: *mut *mut c_char, s: &str) {
    if !out_json.is_null() {
        unsafe { *out_json = CString::new(s).unwrap().into_raw() };
    }
}

fn bytes_to_hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

fn hex_to_bytes(s: &str) -> Result<Vec<u8>, std::num::ParseIntError> {
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16))
        .collect()
}

#[no_mangle]
pub unsafe extern "C" fn surreal_open(tier: c_int, db_path: *const c_char) -> c_int {
    let path = if db_path.is_null() {
        "".to_string()
    } else {
        unsafe { CStr::from_ptr(db_path) }
            .to_str()
            .unwrap_or("")
            .to_string()
    };

    let result = panic::catch_unwind(|| {
        STORE_TIER1.get_or_init(|| {
            Arc::new(RwLock::new(
                SurrealTier1Store::new(tier, &path).expect("failed to init tier1 db"),
            ))
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

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
        let store_arc = STORE_TIER1.get().unwrap();
        let guard = store_arc.read().unwrap();

        // Query SurrealDB
        let res: Option<String> = guard.rt.block_on(async {
            let mut response = guard
                .db
                .query("SELECT v FROM kv WHERE k = $k")
                .bind(("k", key_hex))
                .await
                .ok()?;
            let vals: Vec<surrealdb::sql::Value> = response.take(0).ok()?;
            if vals.is_empty() {
                return None;
            }
            if let surrealdb::sql::Value::Object(obj) = &vals[0] {
                if let Some(surrealdb::sql::Value::Strand(s)) = obj.get("v") {
                    return Some(s.clone().as_string());
                }
            }
            None
        });

        match res {
            None => SURREAL_NOT_FOUND,
            Some(hex_str) => {
                let val_bytes = hex_to_bytes(&hex_str).unwrap_or_default();
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
        let store_arc = STORE_TIER1.get().unwrap();
        let guard = store_arc.read().unwrap();
        guard.rt.block_on(async {
            let _ = guard
                .db
                .query("UPSERT kv SET k = $k, v = $v")
                .bind(("k", k))
                .bind(("v", v))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_kv_delete(key: *const u8, key_len: usize) -> c_int {
    let k = bytes_to_hex(unsafe { std::slice::from_raw_parts(key, key_len) });
    let result = panic::catch_unwind(|| {
        let store_arc = STORE_TIER1.get().unwrap();
        let guard = store_arc.read().unwrap();
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
        let Some(store_arc) = STORE_TIER1.get() else {
            write_err(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();
        let rows: Vec<surrealdb::sql::Value> = guard
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
            if let surrealdb::sql::Value::Object(obj) = row {
                let k = match obj.get("k") {
                    Some(surrealdb::sql::Value::Strand(s)) => s.0.clone(),
                    _ => continue,
                };
                let v = match obj.get("v") {
                    Some(surrealdb::sql::Value::Strand(s)) => s.0.clone(),
                    _ => continue,
                };
                if !first {
                    json.push(',');
                }
                json.push_str(&format!("{{\"k\":\"{k}\",\"v\":\"{v}\"}}"));
                first = false;
            }
        }
        json.push(']');
        write_err(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_vec_upsert(
    id: *const c_char,
    embed: *const f32,
    dim: usize,
) -> c_int {
    let id_str = unsafe { CStr::from_ptr(id) }.to_str().unwrap().to_string();
    let embed_vec = unsafe { std::slice::from_raw_parts(embed, dim) }.to_vec();
    let result = panic::catch_unwind(|| {
        let store_arc = STORE_TIER1.get().unwrap();
        let guard = store_arc.read().unwrap();
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

#[no_mangle]
pub unsafe extern "C" fn surreal_vec_knn(
    query: *const f32,
    dim: usize,
    k: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    if query.is_null() || dim == 0 || k == 0 {
        write_err(out_json, "[]");
        return SURREAL_OK;
    }
    let q_vec: Vec<f32> = unsafe { std::slice::from_raw_parts(query, dim) }.to_vec();
    let result = panic::catch_unwind(|| {
        let Some(store_arc) = STORE_TIER1.get() else {
            write_err(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();
        // k must be a literal in the ANN operator — embed it in the query string.
        let sql = format!(
            "SELECT id, vector::similarity::cosine(embed, $q) AS score \
             FROM vectors WHERE embed <|{k}|> $q ORDER BY score DESC"
        );
        let rows: Vec<surrealdb::sql::Value> = guard
            .rt
            .block_on(async {
                let mut resp = guard.db.query(&sql).bind(("q", q_vec)).await?;
                resp.take(0)
            })
            .unwrap_or_default();

        let mut json = String::from("[");
        let mut first = true;
        for row in &rows {
            if let surrealdb::sql::Value::Object(obj) = row {
                let id_str = match obj.get("id") {
                    Some(surrealdb::sql::Value::Thing(t)) => t.id.to_string(),
                    _ => continue,
                };
                let score = match obj.get("score") {
                    Some(surrealdb::sql::Value::Number(n)) => n.as_float(),
                    _ => 0.0_f64,
                };
                if !first {
                    json.push(',');
                }
                json.push_str(&format!("{{\"id\":\"{id_str}\",\"score\":{score}}}"));
                first = false;
            }
        }
        json.push(']');
        write_err(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_graph_relate(
    from_id: *const c_char,
    edge_type: *const c_char,
    to_id: *const c_char,
) -> c_int {
    let from = match unsafe { CStr::from_ptr(from_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_PANIC,
    };
    let et = match unsafe { CStr::from_ptr(edge_type) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_PANIC,
    };
    let to = match unsafe { CStr::from_ptr(to_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_PANIC,
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = STORE_TIER1.get() else {
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();
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
            write_err(out_json, "[]");
            return SURREAL_ERR_PANIC;
        }
    };
    let et = match unsafe { CStr::from_ptr(edge_type) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => {
            write_err(out_json, "[]");
            return SURREAL_ERR_PANIC;
        }
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = STORE_TIER1.get() else {
            write_err(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();

        // BFS 迭代：每一层查询 frontier 节点的出边
        let mut visited: std::collections::HashSet<String> = std::collections::HashSet::new();
        let mut frontier = vec![start.clone()];
        visited.insert(start);

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
                    let rows: Vec<surrealdb::sql::Value> = resp.take(0)?;
                    let mut ids = Vec::new();
                    for row in &rows {
                        if let surrealdb::sql::Value::Object(obj) = row {
                            if let Some(surrealdb::sql::Value::Strand(s)) = obj.get("to_id") {
                                ids.push(s.0.clone());
                            }
                        }
                    }
                    Ok::<Vec<String>, surrealdb::Error>(ids)
                })
                .unwrap_or_default();

            frontier = next
                .into_iter()
                .filter(|id| visited.insert(id.clone()))
                .collect();
        }

        // 排除起点自身
        let result_ids: Vec<&String> = visited.iter().collect();
        let mut json = String::from("[");
        let mut first = true;
        for id in result_ids {
            if !first {
                json.push(',');
            }
            json.push_str(&format!("\"{id}\""));
            first = false;
        }
        json.push(']');
        write_err(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_fts_index(doc_id: *const c_char, text: *const c_char) -> c_int {
    let id = match unsafe { CStr::from_ptr(doc_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_PANIC,
    };
    let body = match unsafe { CStr::from_ptr(text) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_PANIC,
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = STORE_TIER1.get() else {
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();
        guard.rt.block_on(async {
            // 先删旧记录，再插入——实现 upsert 语义（doc_id 作为唯一键）
            let _ = guard
                .db
                .query("DELETE docs WHERE doc_id = $id; INSERT INTO docs (doc_id, body) VALUES ($id, $body)")
                .bind(("id", id))
                .bind(("body", body))
                .await;
        });
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_fts_search(
    query: *const c_char,
    k: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let q = match unsafe { CStr::from_ptr(query) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => {
            write_err(out_json, "[]");
            return SURREAL_ERR_PANIC;
        }
    };
    let result = panic::catch_unwind(move || {
        let Some(store_arc) = STORE_TIER1.get() else {
            write_err(out_json, "[]");
            return SURREAL_OK;
        };
        let guard = store_arc.read().unwrap();
        // k 须为字面量嵌入查询字符串
        let sql = format!(
            "SELECT doc_id, search::score(0) AS score FROM docs \
             WHERE body @0@ $q ORDER BY score DESC LIMIT {k}"
        );
        let rows: Vec<surrealdb::sql::Value> = guard
            .rt
            .block_on(async {
                let mut resp = guard.db.query(&sql).bind(("q", q)).await?;
                resp.take(0)
            })
            .unwrap_or_default();

        let mut json = String::from("[");
        let mut first = true;
        for row in &rows {
            if let surrealdb::sql::Value::Object(obj) = row {
                let id_str = match obj.get("doc_id") {
                    Some(surrealdb::sql::Value::Strand(s)) => s.0.clone(),
                    _ => continue,
                };
                let score = match obj.get("score") {
                    Some(surrealdb::sql::Value::Number(n)) => n.as_float(),
                    _ => 0.0_f64,
                };
                if !first {
                    json.push(',');
                }
                json.push_str(&format!("{{\"id\":\"{id_str}\",\"score\":{score}}}"));
                first = false;
            }
        }
        json.push(']');
        write_err(out_json, &json);
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

#[no_mangle]
pub unsafe extern "C" fn surreal_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        unsafe { drop(CString::from_raw(ptr)) };
    }
}

#[no_mangle]
pub unsafe extern "C" fn surreal_free_buf(ptr: *mut u8, len: usize) {
    if !ptr.is_null() && len > 0 {
        unsafe { drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len))) };
    }
}

#[no_mangle]
pub extern "C" fn surreal_vec_set_mode(mode: c_int) -> c_int {
    let result = panic::catch_unwind(|| {
        let store_arc = STORE_TIER1.get().unwrap();
        let mut guard = store_arc.write().unwrap();
        guard.use_hnsw = mode == 1;
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}
