// surreal_store/graph.rs — 有向图 FFI：surreal_graph_relate / delete_edges / spreading_activation / traverse

use std::ffi::CStr;
use std::os::raw::{c_char, c_int};
use std::panic;

use super::{
    edge_record_key, encode_ids, encode_scored, get_store, write_cstr, ToIdRow, ToIdWeightRow,
    VecRow, SURREAL_ERR_LOCK, SURREAL_ERR_PANIC, SURREAL_ERR_QUERY, SURREAL_ERR_UTF8, SURREAL_OK,
};

// ─── surreal_graph_relate ─────────────────────────────────────────────────────

/// 创建或更新有向加权边。使用确定性 edge_key 作为 record ID，
/// UPSERT 保证 (from, type, to) 唯一，避免蔓延激活因重复边能量被放大。
/// 原 INSERT INTO edges：每次调用创建新记录，同一条边重复插入，图结构错误。
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
        // type::record('edges', $ek) 确定性 ID = edges:(from\x1fet\x1fto)
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
/// 返回 JSON CString（须 surreal_free_string 释放）:
/// [{"id":"<node>","score":<energy>},...]
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

                // 查询出错时返回 SURREAL_ERR_QUERY，不以空邻居伪装"无出边"
                let neighbors: Vec<ToIdWeightRow> = match guard.rt.block_on(async {
                    let mut resp = guard
                        .db
                        .query(&sql)
                        .bind(("curr", curr_node.clone()))
                        .await?;
                    resp.take(0)
                }) {
                    Ok(n) => n,
                    Err(e) => {
                        eprintln!("[surreal_graph_spreading_activation] query error: {e}");
                        return SURREAL_ERR_QUERY;
                    }
                };

                for edge in neighbors {
                    let transferred_energy = curr_energy * edge.weight * energy_decay;
                    if transferred_energy >= dormancy_threshold {
                        *next_frontier.entry(edge.to_id.clone()).or_insert(0.0) +=
                            transferred_energy;
                        *node_energy.entry(edge.to_id.clone()).or_insert(0.0) +=
                            transferred_energy;
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

/// BFS 图遍历，返回 JSON CString（须 surreal_free_string 释放）。
/// edge_type 为空串表示匹配所有边类型。不包含起点自身。
/// 结果排序保证确定性（原 HashSet 顺序不确定，影响测试可复现性）。
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
                "SELECT to_id FROM edges WHERE from_id IN $frontier AND edge_type = $et"
                    .to_string()
            };
            // 查询出错时返回 SURREAL_ERR_QUERY，不以空结果伪装"无可达节点"
            let next: Vec<String> = match guard.rt.block_on(async {
                let mut resp = guard
                    .db
                    .query(&sql)
                    .bind(("frontier", frontier.clone()))
                    .bind(("et", et.clone()))
                    .await?;
                let rows: Vec<ToIdRow> = resp.take(0)?;
                Ok::<Vec<String>, surrealdb::Error>(rows.into_iter().map(|r| r.to_id).collect())
            }) {
                Ok(n) => n,
                Err(e) => {
                    eprintln!("[surreal_graph_traverse] query error: {e}");
                    return SURREAL_ERR_QUERY;
                }
            };

            frontier = next
                .into_iter()
                .filter(|id| visited.insert(id.clone()))
                .collect();
        }

        // 排序保证结果确定性
        let mut result_ids: Vec<String> =
            visited.into_iter().filter(|id| id != &start).collect();
        result_ids.sort();
        write_cstr(out_json, &encode_ids(&result_ids));
        SURREAL_OK
    });
    result.unwrap_or(SURREAL_ERR_PANIC)
}
