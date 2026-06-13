// surreal_store.rs — SurrealDB v3 统一认知存储引擎 FFI
// 后端运行时选择: kv-mem（默认，任意机器，含 2GB VPS）/ kv-rocksdb（显式，大内存服务器）
// 向量维度 vec_dim 由调用方传入（典型值 1536 或 768）。
// Tokio worker_threads 由 surreal_set_worker_threads 在 surreal_open 前配置（VPS 节省内存）。
// 架构: docs/arch/M02-Storage-Fabric.md §10，ADR-0010

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, OnceLock, RwLock};

use surrealdb::Surreal;
use surrealdb::engine::local::{Db, Mem, RocksDb};
use surrealdb::types::SurrealValue;
use tokio::runtime::Runtime;

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const SURREAL_OK: c_int = 0;
const SURREAL_NOT_FOUND: c_int = 1;
const SURREAL_ERR_UTF8: c_int = -1;
const SURREAL_ERR_LOCK: c_int = -2;
const SURREAL_ERR_PANIC: c_int = -3;
const SURREAL_ERR_QUERY: c_int = -4;

// ─── 运行时配置（VPS 优化：surreal_open 前调用 surreal_set_worker_threads）──────
// 0 = auto（min(CPU 核心数, 4)），> 0 = 显式线程数。
// kv-mem 后端 2 个线程已足够；VPS 建议 2，大内存服务器可设 0（自动）。
static WORKER_THREADS: AtomicU32 = AtomicU32::new(0);

// ─── 存储类型 ──────────────────────────────────────────────────────────────────

struct SurrealStore {
    db: Surreal<Db>,
    rt: Runtime,
}

impl SurrealStore {
    fn new(backend: &str, db_path: &str, vec_dim: u32) -> Result<Self, Box<dyn std::error::Error>> {
        // FIX: 限制 Tokio worker 线程数，避免多核服务器上 new_multi_thread() 无限创建线程浪费内存
        // VPS 设置 2 线程可节省约 30-50MB；0 = auto → min(cpu_count, 4) 作为保守上限
        let wt = WORKER_THREADS.load(Ordering::Relaxed);
        let threads = if wt == 0 {
            std::cmp::min(
                std::thread::available_parallelism()
                    .map(|n| n.get())
                    .unwrap_or(1),
                4,
            )
        } else {
            wt as usize
        };

        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(threads)
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

        // FIX: MTREE → HNSW（M=8, EFC=64），内存占用减少约 50%，适配 VPS
        // FIX: docs 表移除 doc_id 字段，改用 type::record('docs', $id) record ID + record::id() 投影
        //      避免 UNIQUE 约束缺失导致重复文档降低 BM25 精度
        let ddl = format!(
            "DEFINE TABLE IF NOT EXISTS kv SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS k ON kv TYPE string; \
             DEFINE FIELD IF NOT EXISTS v ON kv TYPE string; \
             DEFINE INDEX IF NOT EXISTS kv_k ON kv FIELDS k UNIQUE; \
             DEFINE TABLE IF NOT EXISTS vectors SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS embed ON vectors TYPE array<float>; \
             DEFINE INDEX IF NOT EXISTS hnsw_idx ON vectors FIELDS embed HNSW DIMENSION {vec_dim} DIST COSINE M 8 EFC 64; \
             DEFINE TABLE IF NOT EXISTS edges SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS from_id ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS edge_type ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS to_id ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS weight ON edges TYPE float DEFAULT 1.0; \
             DEFINE INDEX IF NOT EXISTS edge_from ON edges FIELDS from_id, edge_type; \
             DEFINE TABLE IF NOT EXISTS docs SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS body ON docs TYPE string; \
             DEFINE ANALYZER IF NOT EXISTS ascii_lower TOKENIZERS class FILTERS lowercase; \
             DEFINE INDEX IF NOT EXISTS fts_idx ON docs FIELDS body FULLTEXT ANALYZER ascii_lower BM25;"
        );

        // FIX: DDL 失败改为 eprintln 记录而非 `let _ = ...` 静默丢弃，便于问题排查
        rt.block_on(async {
            match db.query(&ddl).await {
                Ok(_) => {}
                Err(e) => eprintln!("[surreal_store] DDL error (non-fatal): {e}"),
            }
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

// FIX: id 字段由 record::id(id) AS id 投影为纯字符串，避免 RecordId 反序列化失败
// （原 SELECT id 返回 RecordId 类型如 "vectors:xxx"，String 字段无法直接接收）
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
struct ToIdWeightRow {
    to_id: String,
    weight: f64,
}

// FIX: doc_id 由 record::id(id) AS doc_id 投影，与 type::record UPSERT 结构匹配
#[derive(Debug, SurrealValue)]
struct DocScoreRow {
    doc_id: String,
    score: f64,
}

#[derive(Debug, SurrealValue)]
struct CountRow {
    count: i64,
}

// ─── 内部工具 ──────────────────────────────────────────────────────────────────

fn write_cstr(out: *mut *mut c_char, s: &str) {
    if !out.is_null()
        && let Ok(cs) = CString::new(s)
    {
        unsafe { *out = cs.into_raw() };
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

/// 为有向边计算确定性 record key（\x1f 为 ASCII 单元分隔符，防止 ID 碰撞）。
/// FIX: 避免相同三元组 (from, edge_type, to) 因 INSERT 无去重产生重复边
fn edge_record_key(from: &str, et: &str, to: &str) -> String {
    format!("{from}\x1f{et}\x1f{to}")
}

// ─── surreal_set_worker_threads ────────────────────────────────────────────────

/// 配置 SurrealDB Tokio 运行时工作线程数。必须在 surreal_open 前调用（否则使用默认值）。
/// n <= 0 表示 auto（min(CPU 核心数, 4)）；推荐 VPS 设为 2 节省内存。
/// 此函数为新增扩展 API（SUBSTRATE_ABI_MINOR 1），不改变 surreal_open 签名。
#[unsafe(no_mangle)]
pub extern "C" fn surreal_set_worker_threads(n: c_int) -> c_int {
    let v = if n <= 0 { 0u32 } else { n as u32 };
    WORKER_THREADS.store(v, Ordering::Relaxed);
    SURREAL_OK
}

// ─── surreal_open ──────────────────────────────────────────────────────────────

/// 初始化全局 SurrealStore（幂等，多次调用安全）。
/// backend: "mem"（默认，kv-mem，任意内存大小可用）或 "rocksdb"（持久化，推荐大内存服务器）。
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
#[unsafe(no_mangle)]
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

/// FIX: 使用 type::record('kv', $k) 确定性 RecordId，避免并发 UPSERT WHERE 竞态。
/// 原 `UPSERT kv SET ... WHERE k = $k`：两个协程同时写新 key 时，WHERE 均未匹配 →
/// 各自 INSERT 新行 → UNIQUE INDEX 报错但被 `let _ =` 静默丢弃，数据写入失败。
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
        // FIX: type::record('kv', $k) → record ID = kv:$k_hex，UPSERT 天然幂等无竞态
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

/// FIX: DELETE type::record('kv', $k) 直接定位 record ID，O(1)，避免全表扫描。
/// 原 `DELETE kv WHERE k = $k` 需 kv_k 索引扫描，而 record ID 直接访问更高效。
///
/// # Safety
/// key 须为 key_len 字节长的有效内存地址。
#[unsafe(no_mangle)]
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

/// 前缀扫描，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"k":"<hex>","v":"<hex>"},...]
///
/// # Safety
/// prefix 须为 prefix_len 字节长的有效内存地址。
#[unsafe(no_mangle)]
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
                    .query("SELECT k, v FROM kv WHERE string::starts_with(k, $prefix) ORDER BY k")
                    .bind(("prefix", prefix_hex))
                    .await?;
                let rows: Vec<KvRow> = match resp.take(0) {
                    Ok(r) => r,
                    Err(e) => {
                        eprintln!("[surreal_kv_scan] Error taking rows: {e}");
                        vec![]
                    }
                };
                Ok::<Vec<KvRow>, surrealdb::Error>(rows)
            })
            .unwrap_or_else(|e| {
                eprintln!("[surreal_kv_scan] query error: {e}");
                vec![]
            });

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

/// FIX: 使用 type::record('vectors', $id) 明确指定 record ID。
/// 原 `UPSERT vectors SET id = $id, embed = $embed`：在 SCHEMAFULL 模式下
/// `id` 不是普通字段，SET id = $id 无效 → record ID 变为随机生成，UPSERT 不幂等。
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
            // FIX: type::record('vectors', $id) → record ID = vectors:$id，幂等
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

/// K 近邻向量检索，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"id":"<id>","score":<f64>},...]
///
/// FIX: 使用 record::id(id) AS id 将 RecordId（"vectors:xxx"）转为纯字符串 "xxx"。
/// FIX: <|{k},COSINE|> 明确指定距离函数，与 DDL HNSW DIST COSINE 定义一致。
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
        // FIX: record::id(id) AS id — 提取 RecordId key 部分为纯字符串
        // FIX: <|{k},COSINE|> — 明确距离函数，与 HNSW 索引定义保持一致
        let sql = format!(
            "SELECT record::id(id) AS id, vector::similarity::cosine(embed, $q) AS score \
             FROM vectors WHERE embed <|{k},COSINE|> $q ORDER BY score DESC"
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

/// FIX: 使用确定性 edge_key 作为 record ID，UPSERT 保证 (from, type, to) 唯一。
/// 原 INSERT INTO edges：每次调用创建新记录，同一条边重复插入，
/// 导致蔓延激活算法能量因重复边被放大，图结构错误。
///
/// # Safety
/// 所有参数须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_graph_relate(
    from_id: *const c_char,
    edge_type: *const c_char,
    to_id: *const c_char,
    weight: f64,
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
        // FIX: type::record('edges', $ek) 确定性 ID = edges:(from\x1fet\x1fto)
        // UPSERT 保证同一条边最多一条记录，同时支持 weight 更新
        let edge_key = edge_record_key(&from, &et, &to);
        let q_res = guard.rt.block_on(async {
            guard
                .db
                .query(
                    "UPSERT type::record('edges', $ek) \
                     SET from_id = $from, edge_type = $et, to_id = $to, weight = $weight",
                )
                .bind(("ek", edge_key))
                .bind(("from", from))
                .bind(("et", et))
                .bind(("to", to))
                .bind(("weight", weight))
                .await
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_graph_relate] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_graph_delete_edges ───────────────────────────────────────────────

/// 删除指定 from_id 的出边（供 Forget 路径清理关联图结构）。
/// edge_type 为空串表示删除该节点所有出边；否则仅删除指定类型的出边。
///
/// # Safety
/// from_id/edge_type 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_graph_delete_edges(
    from_id: *const c_char,
    edge_type: *const c_char,
) -> c_int {
    let from = match unsafe { CStr::from_ptr(from_id) }.to_str() {
        Ok(s) => s.to_string(),
        Err(_) => return SURREAL_ERR_UTF8,
    };
    let et = match unsafe { CStr::from_ptr(edge_type) }.to_str() {
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
            if et.is_empty() {
                guard
                    .db
                    .query("DELETE edges WHERE from_id = $from")
                    .bind(("from", from))
                    .await
            } else {
                guard
                    .db
                    .query("DELETE edges WHERE from_id = $from AND edge_type = $et")
                    .bind(("from", from))
                    .bind(("et", et))
                    .await
            }
        });
        if let Err(e) = q_res {
            eprintln!("[surreal_graph_delete_edges] Query error: {e}");
            return SURREAL_ERR_QUERY;
        }
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_graph_spreading_activation ───────────────────────────────────────

/// 蔓延激活图算法（Spreading Activation）。
/// 返回 JSON CString: [{"id":"<node>","score":<energy>},...]
///
/// # Safety
/// start_ids_json 须为有效 JSON string 数组（如 `["A", "B"]`）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_graph_spreading_activation(
    start_ids_json: *const c_char,
    max_depth: usize,
    energy_decay: f64,
    dormancy_threshold: f64,
    fan_out_limit: usize,
    out_json: *mut *mut c_char,
) -> c_int {
    let ids_str = match unsafe { CStr::from_ptr(start_ids_json) }.to_str() {
        Ok(s) => s,
        Err(_) => {
            write_cstr(out_json, "[]");
            return SURREAL_ERR_UTF8;
        }
    };

    let start_ids: Vec<String> = match serde_json::from_str(ids_str) {
        Ok(v) => v,
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

        let mut node_energy: std::collections::HashMap<String, f64> =
            std::collections::HashMap::new();
        let mut frontier: Vec<(String, f64)> = Vec::new();

        for id in &start_ids {
            node_energy.insert(id.clone(), 1.0);
            frontier.push((id.clone(), 1.0));
        }

        for _ in 0..max_depth {
            if frontier.is_empty() {
                break;
            }

            let mut next_frontier: std::collections::HashMap<String, f64> =
                std::collections::HashMap::new();

            for (curr_node, curr_energy) in frontier {
                if curr_energy < dormancy_threshold {
                    continue;
                }

                let sql = format!(
                    "SELECT to_id, weight FROM edges WHERE from_id = $curr \
                     ORDER BY weight DESC LIMIT {fan_out_limit}"
                );

                let neighbors: Vec<ToIdWeightRow> = guard
                    .rt
                    .block_on(async {
                        let mut resp = guard
                            .db
                            .query(&sql)
                            .bind(("curr", curr_node.clone()))
                            .await?;
                        resp.take(0)
                    })
                    .unwrap_or_default();

                for edge in neighbors {
                    let transferred_energy = curr_energy * edge.weight * energy_decay;
                    if transferred_energy >= dormancy_threshold {
                        *next_frontier.entry(edge.to_id.clone()).or_insert(0.0) +=
                            transferred_energy;
                        *node_energy.entry(edge.to_id.clone()).or_insert(0.0) += transferred_energy;
                    }
                }
            }

            frontier = next_frontier.into_iter().collect();
        }

        let mut results: Vec<VecRow> = node_energy
            .into_iter()
            .filter(|(id, _)| !start_ids.contains(id))
            .map(|(id, score)| VecRow { id, score })
            .collect();

        results.sort_by(|a, b| {
            b.score
                .partial_cmp(&a.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        results.truncate(50);

        write_cstr(out_json, &encode_scored(&results));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_graph_traverse ───────────────────────────────────────────────────

/// BFS 图遍历，返回 JSON CString，须 surreal_free_string 释放。
/// edge_type 为空串表示匹配所有边类型。不包含起点自身。
/// FIX: 结果排序保证确定性（原 HashSet 顺序不确定，影响测试可复现性）。
/// JSON: ["id1","id2",...]
///
/// # Safety
/// start_id/edge_type 须为有效 NUL-terminated UTF-8 C 字符串。
#[unsafe(no_mangle)]
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

        // FIX: 排序保证结果确定性
        let mut result_ids: Vec<String> = visited.into_iter().filter(|id| id != &start).collect();
        result_ids.sort();
        write_cstr(out_json, &encode_ids(&result_ids));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}

// ─── surreal_fts_index ────────────────────────────────────────────────────────

/// FIX: 使用 type::record('docs', $id) UPSERT，原子操作代替非原子 DELETE + INSERT。
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
            // FIX: 原子 UPSERT，record ID = docs:$id，天然唯一
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

/// BM25 全文检索，返回 JSON CString，须 surreal_free_string 释放。
/// JSON: [{"id":"<doc_id>","score":<f64>},...]
///
/// FIX: 使用 record::id(id) AS doc_id，与 type::record UPSERT 结构匹配。
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
        // FIX: record::id(id) AS doc_id — 与 type::record 存储的 record ID 结构一致
        let sql = format!(
            "SELECT record::id(id) AS doc_id, search::score(0) AS score FROM docs \
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
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        unsafe { drop(CString::from_raw(ptr)) };
    }
}

/// 释放 surreal_kv_get 分配的二进制 buffer。
///
/// # Safety
/// ptr 须为 surreal_kv_get 分配的指针，len 须与 out_len 一致，或 ptr 为 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn surreal_free_buf(ptr: *mut u8, len: usize) {
    if !ptr.is_null() && len > 0 {
        unsafe { drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len))) };
    }
}

// ─── surreal_vec_set_mode — 兼容接口（no-op）──────────────────────────────────
// SurrealDB HNSW 索引始终激活，此函数保留仅为 ABI 兼容（原 MTREE 模式切换接口）。

#[unsafe(no_mangle)]
pub extern "C" fn surreal_vec_set_mode(_mode: c_int) -> c_int {
    SURREAL_OK
}

// ─── surreal_stats ─────────────────────────────────────────────────────────────

/// 返回当前后端状态 JSON，须 surreal_free_string 释放。
/// JSON: {"backend":"surreal","ready":true,"kv_count":N,"vec_count":N,"doc_count":N,"edge_count":N}
/// 未初始化时：{"backend":"none","ready":false}
#[unsafe(no_mangle)]
pub extern "C" fn surreal_stats(out_json: *mut *mut c_char) -> c_int {
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
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::{CStr, CString};

    unsafe fn read_out_json(out: *mut c_char) -> String {
        unsafe {
            let cstr = CStr::from_ptr(out);
            let s = cstr.to_string_lossy().into_owned();
            surreal_free_string(out);
            s
        }
    }

    #[test]
    fn test_surreal_store_all_features() {
        unsafe {
            let rc = surreal_open(std::ptr::null(), std::ptr::null(), 3);
            assert_eq!(rc, SURREAL_OK);

            // 1. KV
            let key = b"test_key";
            let val = b"test_val";
            assert_eq!(
                surreal_kv_put(key.as_ptr(), key.len(), val.as_ptr(), val.len()),
                SURREAL_OK
            );

            let mut out_val: *mut u8 = std::ptr::null_mut();
            let mut out_len: usize = 0;
            assert_eq!(
                surreal_kv_get(key.as_ptr(), key.len(), &mut out_val, &mut out_len),
                SURREAL_OK
            );
            assert_eq!(std::slice::from_raw_parts(out_val, out_len), b"test_val");
            surreal_free_buf(out_val, out_len);

            let mut out_json: *mut c_char = std::ptr::null_mut();
            assert_eq!(
                surreal_kv_scan(b"test_".as_ptr(), 5, &mut out_json),
                SURREAL_OK
            );
            let scan_json = read_out_json(out_json);
            assert!(
                scan_json.contains(&bytes_to_hex(b"test_key")),
                "scan_json was {}",
                scan_json
            );

            assert_eq!(surreal_kv_delete(key.as_ptr(), key.len()), SURREAL_OK);

            // 2. Vector
            let id1 = CString::new("vec1").unwrap();
            let embed1 = [1.0, 0.0, 0.0];
            assert_eq!(
                surreal_vec_upsert(id1.as_ptr(), embed1.as_ptr(), 3),
                SURREAL_OK
            );

            let id2 = CString::new("vec2").unwrap();
            let embed2 = [0.0, 1.0, 0.0];
            assert_eq!(
                surreal_vec_upsert(id2.as_ptr(), embed2.as_ptr(), 3),
                SURREAL_OK
            );

            let query = [1.0, 0.1, 0.0];
            assert_eq!(
                surreal_vec_knn(query.as_ptr(), 3, 2, &mut out_json),
                SURREAL_OK
            );
            let knn_json = read_out_json(out_json);
            assert!(knn_json.contains("vec1"));

            assert_eq!(surreal_vec_delete(id1.as_ptr()), SURREAL_OK);

            // 3. Graph
            let from = CString::new("nodeA").unwrap();
            let et = CString::new("knows").unwrap();
            let to = CString::new("nodeB").unwrap();
            assert_eq!(
                surreal_graph_relate(from.as_ptr(), et.as_ptr(), to.as_ptr(), 1.0),
                SURREAL_OK
            );

            let empty = CString::new("").unwrap();
            assert_eq!(
                surreal_graph_traverse(from.as_ptr(), empty.as_ptr(), 2, &mut out_json),
                SURREAL_OK
            );
            let traverse_json = read_out_json(out_json);
            assert!(traverse_json.contains("nodeB"));

            let start_ids = CString::new("[\"nodeA\"]").unwrap();
            assert_eq!(
                surreal_graph_spreading_activation(
                    start_ids.as_ptr(),
                    2,
                    0.8,
                    0.1,
                    10,
                    &mut out_json
                ),
                SURREAL_OK
            );
            let sa_json = read_out_json(out_json);
            assert!(sa_json.contains("nodeB"));

            assert_eq!(
                surreal_graph_delete_edges(from.as_ptr(), et.as_ptr()),
                SURREAL_OK
            );

            // 4. FTS
            let doc_id = CString::new("doc1").unwrap();
            let text = CString::new("Hello world surreal").unwrap();
            assert_eq!(
                surreal_fts_index(doc_id.as_ptr(), text.as_ptr()),
                SURREAL_OK
            );

            let q = CString::new("world").unwrap();
            assert_eq!(
                surreal_fts_search(q.as_ptr(), 10, &mut out_json),
                SURREAL_OK
            );
            let search_json = read_out_json(out_json);
            assert!(search_json.contains("doc1"));

            assert_eq!(surreal_fts_delete(doc_id.as_ptr()), SURREAL_OK);

            // 5. Stats
            assert_eq!(surreal_stats(&mut out_json), SURREAL_OK);
            let stats_json = read_out_json(out_json);
            assert!(stats_json.contains("\"ready\":true"));
        }
    }
}
