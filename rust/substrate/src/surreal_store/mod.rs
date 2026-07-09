// surreal_store — SurrealDB v3 统一认知存储引擎 FFI
//
// 后端运行时选择: kv-mem（默认，任意机器，含 2GB VPS）/ kv-rocksdb（显式，大内存服务器）
// 向量维度 vec_dim 由调用方传入（典型值 1536 或 768）。
// 架构: docs/arch/M02-Storage-Fabric.md §10，ADR-0010
//
// 模块结构：
//   mod.rs  — 共享类型、常量、全局 STORE、辅助函数
//   store   — surreal_open / surreal_set_worker_threads
//   kv      — surreal_kv_get/put/delete/scan
//   vector  — surreal_vec_upsert/delete/knn/set_mode
//   graph   — surreal_graph_relate/delete_edges/spreading_activation/traverse
//   fts     — surreal_fts_index/delete/search / surreal_stats / 内存管理 FFI

use std::ffi::CString;
use std::os::raw::{c_char, c_int};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, OnceLock, RwLock};

use surrealdb::Surreal;
use surrealdb::engine::local::{Db, Mem, RocksDb};
use surrealdb::types::SurrealValue;
use tokio::runtime::Runtime;

mod fts;
mod graph;
mod kv;
mod store;
mod vector;

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
pub(super) const SURREAL_OK: c_int = 0;
pub(super) const SURREAL_NOT_FOUND: c_int = 1;
pub(super) const SURREAL_ERR_UTF8: c_int = -1;
pub(super) const SURREAL_ERR_LOCK: c_int = -2;
pub(super) const SURREAL_ERR_PANIC: c_int = -3;
pub(super) const SURREAL_ERR_QUERY: c_int = -4;

// ─── 运行时配置（VPS 优化）──────────────────────────────────────────────────────
// 0 = auto（min(CPU 核心数, 4)），> 0 = 显式线程数。
// kv-mem 后端 2 个线程已足够；VPS 建议 2，大内存服务器可设 0（自动）。
pub(super) static WORKER_THREADS: AtomicU32 = AtomicU32::new(0);

// ─── 存储类型 ──────────────────────────────────────────────────────────────────

pub(super) struct SurrealStore {
    pub(super) db: Surreal<Db>,
    pub(super) rt: Runtime,
}

impl SurrealStore {
    pub(super) fn new(
        backend: &str,
        db_path: &str,
        vec_dim: u32,
    ) -> Result<Self, Box<dyn std::error::Error>> {
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

        rt.block_on(async {
            match db.query(&ddl).await {
                Ok(_) => Ok(()),
                Err(e) => {
                    eprintln!("[surreal_store] DDL error (fatal): {e}");
                    Err(Box::new(e) as Box<dyn std::error::Error>)
                }
            }
        })?;

        Ok(SurrealStore { db, rt })
    }
}

pub(super) static STORE: OnceLock<Arc<RwLock<SurrealStore>>> = OnceLock::new();

// ─── 查询结果结构 ──────────────────────────────────────────────────────────────

#[derive(Debug, SurrealValue)]
pub(super) struct KvRow {
    pub(super) k: String,
    pub(super) v: String,
}

#[derive(Debug, SurrealValue)]
pub(super) struct VRow {
    pub(super) v: String,
}

#[derive(Debug, SurrealValue, serde::Serialize)]
pub(super) struct VecRow {
    pub(super) id: String,
    pub(super) score: f64,
}

#[derive(Debug, SurrealValue)]
pub(super) struct ToIdRow {
    pub(super) to_id: String,
}

#[derive(Debug, SurrealValue)]
pub(super) struct ToIdWeightRow {
    pub(super) to_id: String,
    pub(super) weight: f64,
}

#[derive(Debug, SurrealValue, serde::Serialize)]
pub(super) struct DocScoreRow {
    #[serde(rename = "id")]
    pub(super) doc_id: String,
    pub(super) score: f64,
}

#[derive(Debug, SurrealValue)]
pub(super) struct CountRow {
    pub(super) count: i64,
}

// ─── 内部工具 ──────────────────────────────────────────────────────────────────

pub(super) fn write_cstr(out: *mut *mut c_char, s: &str) {
    if !out.is_null()
        && let Ok(cs) = CString::new(s)
    {
        unsafe { *out = cs.into_raw() };
    }
}

pub(super) fn bytes_to_hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

/// hex 解码。奇数长度或含非 hex 字符时返回 None（不静默丢弃产生半损坏字节序列）。
/// 按字节处理，非 ASCII 输入 `as char` 落在 latin-1 区、to_digit(16) 稳返 None，不会 panic。
pub(super) fn hex_to_bytes(s: &str) -> Option<Vec<u8>> {
    let b = s.as_bytes();
    if !b.len().is_multiple_of(2) {
        return None;
    }
    (0..b.len())
        .step_by(2)
        .map(|i| {
            let hi = (b[i] as char).to_digit(16)?;
            let lo = (b[i + 1] as char).to_digit(16)?;
            Some((hi * 16 + lo) as u8)
        })
        .collect()
}

pub(super) fn encode_scored(results: &[VecRow]) -> String {
    let sanitized: Vec<VecRow> = results
        .iter()
        .map(|r| {
            let score = if r.score.is_nan() || r.score.is_infinite() {
                eprintln!(
                    "surreal_store: invalid score {} for id {}, setting to 0.0",
                    r.score, r.id
                );
                0.0
            } else {
                r.score
            };
            VecRow {
                id: r.id.clone(),
                score,
            }
        })
        .collect();
    serde_json::to_string(&sanitized).unwrap_or_else(|_| "[]".to_string())
}

pub(super) fn encode_ids(ids: &[String]) -> String {
    serde_json::to_string(ids).unwrap_or_else(|_| "[]".to_string())
}

pub(super) fn get_store() -> Option<Arc<RwLock<SurrealStore>>> {
    STORE.get().cloned()
}

/// 为有向边计算确定性 record key（\x1f 为 ASCII 单元分隔符，防止 ID 碰撞）。
pub(super) fn edge_record_key(from: &str, et: &str, to: &str) -> String {
    format!("{from}\x1f{et}\x1f{to}")
}

// ─── 集成测试 ──────────────────────────────────────────────────────────────────
// 覆盖全部 5 条 API 轴：KV / Vector / Graph / FTS / Stats
// 直接调用各子模块 pub extern "C" 符号，无需跨私有路径引用。

#[cfg(test)]
mod tests {
    use std::ffi::{CStr, CString};
    use std::os::raw::c_char;

    use super::fts::{
        surreal_free_buf, surreal_free_string, surreal_fts_delete, surreal_fts_index,
        surreal_fts_search, surreal_stats,
    };
    use super::graph::{
        surreal_graph_delete_edges, surreal_graph_relate, surreal_graph_spreading_activation,
        surreal_graph_traverse,
    };
    use super::kv::{surreal_kv_delete, surreal_kv_get, surreal_kv_put, surreal_kv_scan};
    use super::store::surreal_open;
    use super::vector::{surreal_vec_delete, surreal_vec_knn, surreal_vec_upsert};
    use super::{SURREAL_OK, bytes_to_hex};

    // D2 回归：hex 解码必须严格——奇数长度/非 hex 返回 None，不静默产生半损坏字节。
    #[test]
    fn test_hex_to_bytes_strict() {
        assert_eq!(
            super::hex_to_bytes(&bytes_to_hex(b"abc\x00\xff")),
            Some(b"abc\x00\xff".to_vec())
        );
        assert_eq!(super::hex_to_bytes(""), Some(vec![]));
        assert_eq!(super::hex_to_bytes("abc"), None); // 奇数长度
        assert_eq!(super::hex_to_bytes("zz"), None); // 非 hex 字符
        assert_eq!(super::hex_to_bytes("züx"), None); // 非 ASCII 不 panic
    }

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
                "scan_json was {scan_json}"
            );
            assert_eq!(surreal_kv_delete(key.as_ptr(), key.len()), SURREAL_OK);

            // 2. Vector
            let id1 = CString::new("vec1").unwrap();
            let embed1 = [1.0_f32, 0.0, 0.0];
            assert_eq!(
                surreal_vec_upsert(id1.as_ptr(), embed1.as_ptr(), 3),
                SURREAL_OK
            );
            let id2 = CString::new("vec2").unwrap();
            let embed2 = [0.0_f32, 1.0, 0.0];
            assert_eq!(
                surreal_vec_upsert(id2.as_ptr(), embed2.as_ptr(), 3),
                SURREAL_OK
            );
            let query = [1.0_f32, 0.1, 0.0];
            assert_eq!(
                surreal_vec_knn(query.as_ptr(), 3, 2, &mut out_json),
                SURREAL_OK
            );
            let knn_json = read_out_json(out_json);
            assert!(knn_json.contains("vec1"), "knn_json was {knn_json}");
            assert_eq!(surreal_vec_delete(id1.as_ptr()), SURREAL_OK);

            // 3. Graph
            let from = CString::new("nodeA").unwrap();
            let et = CString::new("knows").unwrap();
            let to = CString::new("nodeB").unwrap();
            assert_eq!(
                surreal_graph_relate(
                    from.as_bytes().as_ptr(),
                    from.as_bytes().len(),
                    et.as_bytes().as_ptr(),
                    et.as_bytes().len(),
                    to.as_bytes().as_ptr(),
                    to.as_bytes().len(),
                    1.0_f64.to_bits(),
                ),
                SURREAL_OK
            );
            let empty = CString::new("").unwrap();
            assert_eq!(
                surreal_graph_traverse(
                    from.as_bytes().as_ptr(),
                    from.as_bytes().len(),
                    empty.as_bytes().as_ptr(),
                    empty.as_bytes().len(),
                    2,
                    &mut out_json,
                ),
                SURREAL_OK
            );
            let traverse_json = read_out_json(out_json);
            assert!(
                traverse_json.contains("nodeB"),
                "traverse_json was {traverse_json}"
            );

            let start_ids = CString::new("[\"nodeA\"]").unwrap();
            assert_eq!(
                surreal_graph_spreading_activation(
                    start_ids.as_bytes().as_ptr(),
                    start_ids.as_bytes().len(),
                    2,
                    0.8_f64.to_bits(),
                    0.1_f64.to_bits(),
                    10,
                    &mut out_json,
                ),
                SURREAL_OK
            );
            let sa_json = read_out_json(out_json);
            assert!(sa_json.contains("nodeB"), "sa_json was {sa_json}");
            assert_eq!(
                surreal_graph_delete_edges(
                    from.as_bytes().as_ptr(),
                    from.as_bytes().len(),
                    et.as_bytes().as_ptr(),
                    et.as_bytes().len(),
                ),
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
            assert!(
                search_json.contains("doc1"),
                "search_json was {search_json}"
            );
            assert_eq!(surreal_fts_delete(doc_id.as_ptr()), SURREAL_OK);

            // 5. Stats
            assert_eq!(surreal_stats(&mut out_json), SURREAL_OK);
            let stats_json = read_out_json(out_json);
            assert!(
                stats_json.contains("\"ready\":true"),
                "stats_json was {stats_json}"
            );
        }
    }
}
