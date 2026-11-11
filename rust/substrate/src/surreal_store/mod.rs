// surreal_store — SurrealDB v4 统一认知存储引擎 FFI
//
// 后端运行时选择: kv-mem（任意机器，含 2GB VPS）/ kv-surrealkv（持久化）
// 向量维度 vec_dim 由调用方传入（典型值 1536 或 768）。
// 架构: docs/arch/M02-Storage-Fabric.md §10，ADR-0003
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
use surrealdb::engine::local::{Db, Mem, SurrealKv};
use surrealdb::types::SurrealValue;
use tokio::runtime::Runtime;

mod disk_guard;
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

// ─── 栈保护 ────────────────────────────────────────────────────────────────────
// block_on 在调用方线程上轮询 future，FFI 调用方是 Go 线程，其栈大小取决于平台
// （macOS/Linux glibc ≈8MiB，Windows amd64 2MiB，musl 仅 128KiB），不受本库控制。
// 实测 SurrealDB v4 单次调用栈峰值：release ≈384KiB，debug 超 2MiB（未优化的
// async 状态机）。剩余栈低于 RED_ZONE 时由 stacker 切到新分配的 STACK 段执行，
// 使栈安全不再依赖调用方线程；正常 8MiB 线程上不触发分配，热路径零开销。
#[cfg(not(debug_assertions))]
const BLOCK_ON_RED_ZONE: usize = 1024 * 1024;
#[cfg(not(debug_assertions))]
const BLOCK_ON_STACK_SIZE: usize = 8 * 1024 * 1024;
#[cfg(debug_assertions)]
const BLOCK_ON_RED_ZONE: usize = 8 * 1024 * 1024;
#[cfg(debug_assertions)]
const BLOCK_ON_STACK_SIZE: usize = 32 * 1024 * 1024;

// tokio 默认工作线程栈 2MiB；SurrealDB 后台任务（索引构建、surrealkv 压缩）跑在
// 工作线程上，放大到 8MiB 留足余量。仅为虚拟地址预留，按需提交，不增加常驻内存。
const WORKER_STACK_SIZE: usize = 8 * 1024 * 1024;

/// 所有同步桥接必须经由此函数，禁止直接调用 `Runtime::block_on`。
pub(super) fn block_on<F: std::future::Future>(rt: &Runtime, fut: F) -> F::Output {
    stacker::maybe_grow(BLOCK_ON_RED_ZONE, BLOCK_ON_STACK_SIZE, || rt.block_on(fut))
}

// ─── 存储类型 ──────────────────────────────────────────────────────────────────

pub(super) struct SurrealStore {
    pub(super) db: Surreal<Db>,
    pub(super) rt: Runtime,
    /// HNSW 向量维度（surreal_open 传入），供 surreal_stats 估算索引内存占用
    /// 使用，DDL 建索引后无法从 surrealdb 反查，故在此保留一份。
    pub(super) vec_dim: u32,
}

impl SurrealStore {
    pub(super) fn block_on<F: std::future::Future>(&self, fut: F) -> F::Output {
        block_on(&self.rt, fut)
    }

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
            .thread_stack_size(WORKER_STACK_SIZE)
            .enable_all()
            .build()?;

        let db = match backend {
            "" | "mem" => block_on(&rt, async { Surreal::new::<Mem>(()).await })?,
            "surrealkv" => {
                disk_guard::prepare_v4_directory(db_path)?;
                block_on(&rt, async { Surreal::new::<SurrealKv>(db_path).await })?
            }
            other => return Err(format!("unsupported SurrealDB backend: {other}").into()),
        };
        block_on(&rt, async {
            db.use_ns("polaris").use_db("cognition").await
        })?;

        let base_ddl = "DEFINE TABLE IF NOT EXISTS kv SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS k ON kv TYPE string; \
             DEFINE FIELD IF NOT EXISTS v ON kv TYPE string; \
             DEFINE INDEX IF NOT EXISTS kv_k ON kv FIELDS k UNIQUE; \
             DEFINE TABLE IF NOT EXISTS meta SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS dim ON meta TYPE int; \
             DEFINE TABLE IF NOT EXISTS vectors SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS embed ON vectors TYPE array<float>; \
             DEFINE TABLE IF NOT EXISTS edges SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS from_id ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS edge_type ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS to_id ON edges TYPE string; \
             DEFINE FIELD IF NOT EXISTS weight ON edges TYPE float DEFAULT 1.0; \
             DEFINE INDEX IF NOT EXISTS edge_from ON edges FIELDS from_id, edge_type; \
             DEFINE TABLE IF NOT EXISTS docs SCHEMAFULL; \
             DEFINE FIELD IF NOT EXISTS body ON docs TYPE string; \
             DEFINE ANALYZER IF NOT EXISTS ascii_lower TOKENIZERS class FILTERS lowercase; \
             DEFINE INDEX IF NOT EXISTS fts_idx ON docs FIELDS body FULLTEXT ANALYZER ascii_lower BM25;";

        block_on(&rt, async {
            let base_result = db.query(base_ddl).await;
            if let Err(e) = base_result.and_then(|response| response.check()) {
                eprintln!("[surreal_store] Base DDL error (fatal): {e}");
                return Err(Box::new(e) as Box<dyn std::error::Error>);
            }

            // 检查 meta:vector_index 是否存在已落库维度（ADR-0109 P4）
            let meta_row: Option<MetaRow> = match db
                .query("SELECT dim FROM type::record('meta', 'vector_index')")
                .await
            {
                Ok(mut resp) => {
                    let rows: Vec<MetaRow> = resp.take(0).unwrap_or_default();
                    rows.into_iter().next()
                }
                Err(_) => None,
            };

            let need_rebuild = match meta_row {
                Some(row) => {
                    let stored_dim = row.dim as u32;
                    if stored_dim != vec_dim {
                        eprintln!(
                            "[surreal_store] HNSW dimension changed from {stored_dim} to {vec_dim}. Rebuilding index..."
                        );
                        true
                    } else {
                        false
                    }
                }
                None => {
                    // 无元数据（老库或全新库）：检查现有 vectors 表任一条推断维度
                    let vec_len: Option<u32> = match db
                        .query("SELECT array::len(embed) AS len FROM vectors LIMIT 1")
                        .await
                    {
                        Ok(mut resp) => {
                            let rows: Vec<VectorLenRow> = resp.take(0).unwrap_or_default();
                            rows.into_iter()
                                .next()
                                .and_then(|r| r.len.map(|l| l as u32))
                        }
                        Err(_) => None,
                    };

                    if let Some(inferred) = vec_len
                        && inferred > 0
                    {
                        if inferred != vec_dim {
                            eprintln!(
                                "[surreal_store] Legacy HNSW dimension mismatch: inferred={inferred}, target={vec_dim}. Rebuilding index..."
                            );
                            true
                        } else {
                            false
                        }
                    } else {
                        // 表空或新库
                        false
                    }
                }
            };

            if need_rebuild {
                // 删除失败必须致命：否则下方 DEFINE INDEX IF NOT EXISTS 会保留旧维度索引，
                // 之后所有新维度向量写入静默失败（正是 ADR-0109 R5 要修的缺陷）。
                let rebuild = db
                    .query("REMOVE INDEX IF EXISTS hnsw_idx ON vectors; DELETE vectors;")
                    .await
                    .and_then(|resp| resp.check());
                if let Err(e) = rebuild {
                    eprintln!("[surreal_store] HNSW rebuild (remove old index) error (fatal): {e}");
                    return Err(Box::new(e) as Box<dyn std::error::Error>);
                }
            }

            let define_idx = format!(
                "DEFINE INDEX IF NOT EXISTS hnsw_idx ON vectors FIELDS embed HNSW DIMENSION {vec_dim} DIST COSINE M 8 EFC 64;"
            );
            let define_idx_result = db.query(&define_idx).await;
            if let Err(e) = define_idx_result.and_then(|response| response.check()) {
                eprintln!("[surreal_store] Define HNSW index error (fatal): {e}");
                return Err(Box::new(e) as Box<dyn std::error::Error>);
            }

            // 写入或更新 meta:vector_index
            let meta_result = db
                .query("UPSERT type::record('meta', 'vector_index') SET dim = $dim")
                .bind(("dim", vec_dim as i64))
                .await;
            if let Err(e) = meta_result.and_then(|response| response.check()) {
                eprintln!("[surreal_store] Save HNSW dimension error (fatal): {e}");
                return Err(Box::new(e) as Box<dyn std::error::Error>);
            }

            Ok(())
        })?;

        Ok(SurrealStore { db, rt, vec_dim })
    }
}

pub(super) static STORE: OnceLock<Arc<RwLock<SurrealStore>>> = OnceLock::new();

// ─── 查询结果结构 ──────────────────────────────────────────────────────────────

#[derive(Debug, SurrealValue)]
pub(super) struct MetaRow {
    pub(super) dim: i64,
}

#[derive(Debug, SurrealValue)]
pub(super) struct VectorLenRow {
    pub(super) len: Option<i64>,
}

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
    use super::store::{surreal_open, surreal_storage_version};
    use super::vector::{surreal_vec_delete, surreal_vec_knn, surreal_vec_upsert};
    use super::{SURREAL_ERR_QUERY, SURREAL_ERR_UTF8, SURREAL_OK, bytes_to_hex};

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
            assert_eq!(surreal_storage_version(), 4);
            assert!(super::STORE.get().is_none());

            let rocksdb = CString::new("rocksdb").unwrap();
            assert_eq!(
                surreal_open(rocksdb.as_ptr(), std::ptr::null(), 3),
                SURREAL_ERR_QUERY
            );
            let invalid_backend = [0xff_u8 as c_char, 0];
            assert_eq!(
                surreal_open(invalid_backend.as_ptr(), std::ptr::null(), 3),
                SURREAL_ERR_UTF8
            );

            let rc = surreal_open(std::ptr::null(), std::ptr::null(), 3);
            assert_eq!(rc, SURREAL_OK);
            assert_eq!(
                surreal_open(rocksdb.as_ptr(), std::ptr::null(), 3),
                SURREAL_ERR_QUERY
            );

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

    #[test]
    fn backend_validation_and_mem_leave_disk_path_untouched() {
        let temp_dir = std::env::temp_dir().join(format!(
            "polaris_test_surreal_mem_guard_{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&temp_dir).unwrap();
        let sentinel = temp_dir.join("legacy.sst");
        std::fs::write(&sentinel, b"legacy data").unwrap();
        let before_names = std::fs::read_dir(&temp_dir)
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .collect::<Vec<_>>();

        assert!(super::SurrealStore::new("rocksdb", temp_dir.to_str().unwrap(), 3).is_err());
        let store = super::SurrealStore::new("mem", temp_dir.to_str().unwrap(), 3)
            .expect("memory backend opens independently of disk path");
        drop(store);

        assert_eq!(std::fs::read(&sentinel).unwrap(), b"legacy data");
        let after_names = std::fs::read_dir(&temp_dir)
            .unwrap()
            .map(|entry| entry.unwrap().file_name())
            .collect::<Vec<_>>();
        assert_eq!(after_names, before_names);
        std::fs::remove_dir_all(temp_dir).unwrap();
    }

    #[test]
    fn test_hnsw_dimension_migration() {
        let temp_dir = std::env::temp_dir().join(format!(
            "polaris_test_surreal_mig_{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let path = temp_dir.to_str().unwrap();

        // 1. 模拟旧库：以 2560 维打开并写入 2560 维向量
        {
            let store1 = super::SurrealStore::new("surrealkv", path, 2560)
                .expect("init store with 2560 dim");
            let v2560 = vec![0.1f32; 2560];
            let q_res = store1.block_on(async {
                store1
                    .db
                    .query("UPSERT type::record('vectors', 'doc1') SET embed = $embed")
                    .bind(("embed", v2560))
                    .await
            });
            assert!(q_res.is_ok(), "insert 2560 dim vector failed");

            let kv_res = store1.block_on(async {
                store1
                    .db
                    .query(
                        "UPSERT type::record('kv', 'persistent') SET k = 'persistent', v = 'value'",
                    )
                    .await
            });
            assert!(kv_res.is_ok(), "insert persistent kv row failed");

            // 验证 meta 表已记录 2560
            let meta_dim: Option<i64> = store1.block_on(async {
                let mut resp = store1
                    .db
                    .query("SELECT dim FROM type::record('meta', 'vector_index')")
                    .await
                    .ok()?;
                let rows: Vec<super::MetaRow> = resp.take(0).ok()?;
                rows.into_iter().next().map(|r| r.dim)
            });
            assert_eq!(meta_dim, Some(2560));

            let super::SurrealStore { db, rt, .. } = store1;
            drop(db);
            super::block_on(&rt, async {
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            });
            drop(rt);
        }

        std::thread::sleep(std::time::Duration::from_millis(200));

        // 2. 模拟升级：以 512 维重新打开同一路径
        {
            let store2 = super::SurrealStore::new("surrealkv", path, 512)
                .expect("reopen store with 512 dim");

            // 验证旧向量已清空（HNSW 索引重建，旧维度不兼容数据被清除）
            let vec_count: Option<i64> = store2.block_on(async {
                let mut resp = store2
                    .db
                    .query("SELECT count() AS count FROM vectors GROUP ALL")
                    .await
                    .ok()?;
                let rows: Vec<super::CountRow> = resp.take(0).ok()?;
                rows.into_iter().next().map(|r| r.count)
            });
            assert_eq!(
                vec_count.unwrap_or(0),
                0,
                "vectors table must be cleared upon dimension migration"
            );

            // 验证 meta 表已更新为 512
            let meta_dim: Option<i64> = store2.block_on(async {
                let mut resp = store2
                    .db
                    .query("SELECT dim FROM type::record('meta', 'vector_index')")
                    .await
                    .ok()?;
                let rows: Vec<super::MetaRow> = resp.take(0).ok()?;
                rows.into_iter().next().map(|r| r.dim)
            });
            assert_eq!(
                meta_dim,
                Some(512),
                "meta:vector_index must be updated to 512"
            );

            let persisted_value: Option<String> = store2.block_on(async {
                let mut response = store2
                    .db
                    .query("SELECT v FROM type::record('kv', 'persistent')")
                    .await
                    .ok()?;
                let rows: Vec<super::VRow> = response.take(0).ok()?;
                rows.into_iter().next().map(|row| row.v)
            });
            assert_eq!(persisted_value.as_deref(), Some("value"));

            // 验证可以正常写入 512 维新向量
            let v512 = vec![0.2f32; 512];
            let q_res = store2.block_on(async {
                store2
                    .db
                    .query("UPSERT type::record('vectors', 'doc2') SET embed = $embed")
                    .bind(("embed", v512))
                    .await
            });
            assert!(q_res.is_ok(), "insert 512 dim vector must succeed");
        }

        let _ = std::fs::remove_dir_all(&temp_dir);
    }
}
