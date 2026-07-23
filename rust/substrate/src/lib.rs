// Polaris — Rust substrate crate
// 包含 Cedar 策略引擎 FFI 接口与 SIMD 向量运算路径。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
//
// FFI 设计约束:
//   - 所有跨边界内存必须显式 free（cedar_free_bytes）
//   - 字符串参数：ptr+len 风格（*const u8 + usize），无 NUL-terminated 要求；Rust 侧 slice::from_raw_parts 构建 &[u8]
//   - panic 不可越过 FFI 边界 —— 所有函数捕获 panic 转为错误码
//   - thread-safety: PolicyStore 通过 Arc<RwLock<>> 保护并发读写

#![allow(clippy::missing_safety_doc)]

use std::os::raw::c_int;
use std::panic;
use std::sync::{Arc, OnceLock, RwLock};

use cedar_policy::{Authorizer, Context, Decision, Entities, EntityUid, PolicySet, Request};

// ─── 全局 PolicyStore ──────────────────────────────────────────────────────────

/// 全局 PolicySet，保护并发读写。
/// OnceLock 确保初始化只发生一次（对应 Go 侧 `sync.Once` 初始化语义）。
static POLICY_STORE: OnceLock<Arc<RwLock<PolicySet>>> = OnceLock::new();

fn policy_store() -> Arc<RwLock<PolicySet>> {
    POLICY_STORE
        .get_or_init(|| Arc::new(RwLock::new(PolicySet::new())))
        .clone()
}

// ─── 有界锁获取（GR-7.2） ────────────────────────────────────────────────────────
//
// cedar_evaluate 原先各自内联实现"try_read + 自旋 1ms + timeout_ms deadline"，
// 而 cedar_load_policies/cedar_policy_count 直接调用标准库 RwLock::write()/
// read()——一旦发生锁竞争（例如另一线程长时间持有写锁），这两个 FFI 调用会
// 无限期阻塞对应的 Go 宿主 goroutine。此处把 cedar_evaluate 验证过的模式抽成
// 共享辅助函数，三者统一锁获取策略。timeout_ms == 0 约定为"不设超时"
// （与既有 cedar_evaluate/Go 侧 cedar_ffi.go 调用惯例一致）。

/// acquire_read_with_timeout/acquire_write_with_timeout 的失败原因。
enum LockAcquireError {
    /// 在 timeout_ms 内未能取到锁。
    Timeout,
    /// 锁已中毒（持有者 panic）。
    Poisoned,
}

fn acquire_read_with_timeout<T>(
    lock: &RwLock<T>,
    timeout_ms: u64,
) -> Result<std::sync::RwLockReadGuard<'_, T>, LockAcquireError> {
    if timeout_ms == 0 {
        return lock.read().map_err(|_| LockAcquireError::Poisoned);
    }
    let deadline = std::time::Instant::now() + std::time::Duration::from_millis(timeout_ms);
    loop {
        match lock.try_read() {
            Ok(g) => return Ok(g),
            Err(std::sync::TryLockError::WouldBlock) => {
                if std::time::Instant::now() >= deadline {
                    return Err(LockAcquireError::Timeout);
                }
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            Err(std::sync::TryLockError::Poisoned(_)) => return Err(LockAcquireError::Poisoned),
        }
    }
}

fn acquire_write_with_timeout<T>(
    lock: &RwLock<T>,
    timeout_ms: u64,
) -> Result<std::sync::RwLockWriteGuard<'_, T>, LockAcquireError> {
    if timeout_ms == 0 {
        return lock.write().map_err(|_| LockAcquireError::Poisoned);
    }
    let deadline = std::time::Instant::now() + std::time::Duration::from_millis(timeout_ms);
    loop {
        match lock.try_write() {
            Ok(g) => return Ok(g),
            Err(std::sync::TryLockError::WouldBlock) => {
                if std::time::Instant::now() >= deadline {
                    return Err(LockAcquireError::Timeout);
                }
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            Err(std::sync::TryLockError::Poisoned(_)) => return Err(LockAcquireError::Poisoned),
        }
    }
}

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────

/// 评估结果
const CEDAR_ALLOW: c_int = 0;
const CEDAR_DENY: c_int = 1;
const CEDAR_ERR_PARSE: c_int = -1; // 策略解析失败
const CEDAR_ERR_CONTEXT: c_int = -2; // Context/Entities 构造失败
const CEDAR_ERR_INTERNAL: c_int = -3; // panic 或锁中毒
const CEDAR_ERR_UTF8: c_int = -4; // 非法 UTF-8 输入
const CEDAR_ERR_TIMEOUT: c_int = -5; // 评估超时

// ─── ABI 版本协议 ──────────────────────────────────────────────────────────────
// 设计依据: docs/arch/decisions/ADR-0011-cgo-to-purego-migration.md
// Go 侧加载 dylib 后立即调用 substrate_abi_version() 验证 major 匹配；
// major 不匹配 → panic（防 ABI silent drift）。
// 升 major: 破坏性变更（删/改导出函数签名）；升 minor: 加法变更（新增导出函数）。

/// ABI 主版本号：破坏性变更时递增。
// 2026-07-04 由 1→2：cedar_evaluate 新增 timeout_ms: u64 参数，属于
// docs/internal/protocol/ffi-abi.md §"升 major：删导出符号、改函数签名、
// 改错误码语义" 明确列出的"改函数签名"情形——purego 按位置绑定参数，
// 旧版 Go 调用方以 8 参数调用新版 9 参数的 dylib 会造成参数错位/未定义行为，
// 必须靠 major 不匹配 panic 的 fail-fast 机制拦截，不能只标记为 minor。
//
// 由 2→3（Batch11 GR-7.1/GR-7.2 修复）：
//   - wasmtime_execute 新增 timeout_ms: u64 参数（epoch interruption 墙钟超时）
//   - cedar_load_policies 新增 timeout_ms: u64 参数
//   - cedar_policy_count 新增 timeout_ms: u64 参数（原为零参数）
// 三者均属于"改函数签名"，同一批修复合并为一次 major 递增。
const SUBSTRATE_ABI_MAJOR: u16 = 3;

/// ABI 次版本号：加法变更时递增。
/// 1: 新增 surreal_set_worker_threads / surreal_vec_delete / surreal_fts_delete /
///    surreal_graph_delete_edges；surreal_stats 扩展四路计数字段；
///    HNSW 替换 MTREE；docs 表移除 doc_id 字段（type::thing + record::id()）。
/// 2: 新增 tier1 feature 门控下的 llama_infer_* 本地推理 FFI（load/unload/generate/
///    embed/rerank/evict_kv_cache/status/free_string，P3-1）；非 tier1 构建下这些
///    符号不导出，Go 侧通过 purego RegisterLibFunc + recover 优雅降级探测。
const SUBSTRATE_ABI_MINOR: u16 = 3;

/// 返回当前 ABI 版本（高 16 位 major | 低 16 位 minor）。
/// Go 侧用 `(version >> 16) & 0xFFFF` 提取 major。
#[unsafe(no_mangle)]
pub extern "C" fn substrate_abi_version() -> u32 {
    ((SUBSTRATE_ABI_MAJOR as u32) << 16) | (SUBSTRATE_ABI_MINOR as u32)
}

// ─── cedar_load_policies ───────────────────────────────────────────────────────

/// 从 Cedar 策略文本（ptr+len 风格 UTF-8 字符串（无 NUL 结尾要求））加载/替换全局 PolicySet。
/// 返回 0 表示成功，负数表示错误（错误详情通过 out_err 返回）。
/// out_err 由 Rust 分配，调用方须调用 cedar_free_bytes 释放。
/// timeout_ms == 0 表示不设超时（阻塞等待写锁，仅建议在能确认无竞争的场景使用）。
///
/// # Safety
/// policies_ptr 必须是有效的 ptr+len 风格 UTF-8 字符串（无 NUL 结尾要求），caller 负责生命周期。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cedar_load_policies(
    policies_ptr: *const u8,
    policies_len: usize,
    timeout_ms: u64,
    out_err_ptr: *mut *const u8,
    out_err_len: *mut usize,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            // 解析输入字符串
            let text = match slice_to_str(policies_ptr, policies_len) {
                Ok(s) => s,
                Err(_) => {
                    write_bytes(out_err_ptr, out_err_len, "invalid UTF-8 in policy text");
                    return CEDAR_ERR_UTF8;
                }
            };

            // 解析 PolicySet
            let new_set = match text.parse::<PolicySet>() {
                Ok(ps) => ps,
                Err(e) => {
                    write_bytes(
                        out_err_ptr,
                        out_err_len,
                        &format!("policy parse error: {e}"),
                    );
                    return CEDAR_ERR_PARSE;
                }
            };

            // 写入全局 PolicyStore（GR-7.2：与 cedar_evaluate 共享同一套有界锁获取策略）
            let store = policy_store();
            let mut guard = match acquire_write_with_timeout(&store, timeout_ms) {
                Ok(g) => g,
                Err(LockAcquireError::Timeout) => {
                    write_bytes(out_err_ptr, out_err_len, "timeout");
                    return CEDAR_ERR_TIMEOUT;
                }
                Err(LockAcquireError::Poisoned) => {
                    write_bytes(out_err_ptr, out_err_len, "lock poisoned");
                    return CEDAR_ERR_INTERNAL;
                }
            };
            *guard = new_set;
            write_bytes(out_err_ptr, out_err_len, "");
            0
        });

        match result {
            Ok(code) => code,
            Err(_) => {
                write_bytes(out_err_ptr, out_err_len, "panic in cedar_load_policies");
                CEDAR_ERR_INTERNAL
            }
        }
    }
}

// ─── cedar_evaluate ────────────────────────────────────────────────────────────

/// 评估单次策略请求。
/// 参数均为 ptr+len 风格 UTF-8 字符串（无 NUL 结尾要求）:
///   principal: Cedar EntityUID 格式，例如 `Agent::"agent-42"`
///   action:    Cedar EntityUID 格式，例如 `Action::"infer"`
///   resource:  Cedar EntityUID 格式，例如 `Resource::"llm_api"`
///   context_json: JSON 对象，例如 `{"trust_level": 3, "capability_token_valid": true}`
///
/// 返回 0(ALLOW) / 1(DENY) / 负数(错误)。
/// out_reason 由 Rust 分配，调用方须调用 cedar_free_bytes 释放。
///
/// # Safety
/// 所有 *const c_char 参数须为有效 ptr+len 风格 UTF-8 字符串（无 NUL 结尾要求）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cedar_evaluate(
    principal_ptr: *const u8,
    principal_len: usize,
    action_ptr: *const u8,
    action_len: usize,
    resource_ptr: *const u8,
    resource_len: usize,
    context_ptr: *const u8,
    context_len: usize,
    timeout_ms: u64,
    out_reason_ptr: *mut *const u8,
    out_reason_len: *mut usize,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            // 解析四个输入参数
            let p_str = match slice_to_str(principal_ptr, principal_len) {
                Ok(s) => s,
                Err(_) => {
                    write_bytes(out_reason_ptr, out_reason_len, "invalid UTF-8: principal");
                    return CEDAR_ERR_UTF8;
                }
            };
            let a_str = match slice_to_str(action_ptr, action_len) {
                Ok(s) => s,
                Err(_) => {
                    write_bytes(out_reason_ptr, out_reason_len, "invalid UTF-8: action");
                    return CEDAR_ERR_UTF8;
                }
            };
            let r_str = match slice_to_str(resource_ptr, resource_len) {
                Ok(s) => s,
                Err(_) => {
                    write_bytes(out_reason_ptr, out_reason_len, "invalid UTF-8: resource");
                    return CEDAR_ERR_UTF8;
                }
            };
            let ctx_str = match slice_to_str(context_ptr, context_len) {
                Ok(s) => s,
                Err(_) => {
                    write_bytes(
                        out_reason_ptr,
                        out_reason_len,
                        "invalid UTF-8: context_json",
                    );
                    return CEDAR_ERR_UTF8;
                }
            };

            // 构造 EntityUID
            let p_uid = match p_str.parse::<EntityUid>() {
                Ok(u) => u,
                Err(e) => {
                    write_bytes(
                        out_reason_ptr,
                        out_reason_len,
                        &format!("principal parse: {e}"),
                    );
                    return CEDAR_ERR_CONTEXT;
                }
            };
            let a_uid = match a_str.parse::<EntityUid>() {
                Ok(u) => u,
                Err(e) => {
                    write_bytes(
                        out_reason_ptr,
                        out_reason_len,
                        &format!("action parse: {e}"),
                    );
                    return CEDAR_ERR_CONTEXT;
                }
            };
            let r_uid = match r_str.parse::<EntityUid>() {
                Ok(u) => u,
                Err(e) => {
                    write_bytes(
                        out_reason_ptr,
                        out_reason_len,
                        &format!("resource parse: {e}"),
                    );
                    return CEDAR_ERR_CONTEXT;
                }
            };

            // 构造 Context（从 JSON）
            let ctx = match Context::from_json_str(ctx_str, None) {
                Ok(c) => c,
                Err(e) => {
                    write_bytes(
                        out_reason_ptr,
                        out_reason_len,
                        &format!("context parse: {e}"),
                    );
                    return CEDAR_ERR_CONTEXT;
                }
            };

            let request = Request::new(p_uid, a_uid, r_uid, ctx, None)
                .expect("Request::new should not fail with validated EntityUIDs");

            // Cedar 评估是纯 CPU、无 IO、无循环的有界计算，不需要独立线程 + mpsc 做超时——
            // 原实现每次调用 spawn 一个线程，且超时返回后被 detach 的线程仍持读锁直到评估完
            // 成，高频热路径下既有线程创建开销又存在锁泄漏风险（HE-5：FSM 热路径不宜每次
            // spawn）。改为同步评估：唯一可能阻塞的是与 cedar_load_policies 写锁竞争时的取锁，
            // 故仅对"取读锁"施加 timeout 预算，评估本身在当前线程直接完成。
            //
            // GR-7.2：与 cedar_load_policies/cedar_policy_count 共享同一套有界锁获取
            // 策略（acquire_read_with_timeout），timeout_ms == 0 约定为"不设超时"
            // （与 Go 侧 cedar_ffi.go 调用惯例一致）。
            let store = policy_store();
            let guard = match acquire_read_with_timeout(&store, timeout_ms) {
                Ok(g) => g,
                Err(LockAcquireError::Timeout) => {
                    write_bytes(out_reason_ptr, out_reason_len, "timeout");
                    return CEDAR_ERR_TIMEOUT;
                }
                Err(LockAcquireError::Poisoned) => {
                    write_bytes(out_reason_ptr, out_reason_len, "lock poisoned");
                    return CEDAR_ERR_INTERNAL;
                }
            };

            let authorizer = Authorizer::new();
            // 空 Entities —— 实体属性通过 context_json 传递（MVP 简化）
            let entities = Entities::empty();
            let response = authorizer.is_authorized(&request, &guard, &entities);
            let (code, reason) = match response.decision() {
                Decision::Allow => (CEDAR_ALLOW, "allow"),
                Decision::Deny => (CEDAR_DENY, "deny"),
            };
            write_bytes(out_reason_ptr, out_reason_len, reason);
            code
        });

        match result {
            Ok(code) => code,
            Err(_) => {
                write_bytes(out_reason_ptr, out_reason_len, "panic in cedar_evaluate");
                CEDAR_ERR_INTERNAL
            }
        }
    }
}

// ─── cedar_policy_count ────────────────────────────────────────────────────────

/// 返回当前 PolicySet 中的策略数量。
/// 用于健康检查和热更新验证。
/// timeout_ms == 0 表示不设超时（阻塞等待读锁）。返回负数表示错误：
/// CEDAR_ERR_TIMEOUT(-5) 为取锁超时，CEDAR_ERR_INTERNAL(-3) 为锁中毒/panic。
/// GR-7.2：与 cedar_evaluate/cedar_load_policies 共享同一套有界锁获取策略。
#[unsafe(no_mangle)]
pub extern "C" fn cedar_policy_count(timeout_ms: u64) -> c_int {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let store = policy_store();
        let guard = match acquire_read_with_timeout(&store, timeout_ms) {
            Ok(g) => g,
            Err(LockAcquireError::Timeout) => return CEDAR_ERR_TIMEOUT,
            Err(LockAcquireError::Poisoned) => return CEDAR_ERR_INTERNAL,
        };
        guard.policies().count() as c_int
    }))
    .unwrap_or(CEDAR_ERR_INTERNAL)
}

// ─── cedar_free_bytes ──────────────────────────────────────────────────────────

// SAFETY: Caller must ensure ptr is valid for len bytes...
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cedar_free_bytes(ptr: *mut u8, len: usize) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        if !ptr.is_null() {
            unsafe {
                drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len)));
            }
        }
    }));
}

// ─── 内部工具函数 ──────────────────────────────────────────────────────────────

// write_bytes: 将 &str 写入 (out_ptr, out_len) — ADR-0011 ptr+len 约定
pub(crate) unsafe fn write_bytes(out_ptr: *mut *const u8, out_len: *mut usize, msg: &str) {
    if out_ptr.is_null() || out_len.is_null() {
        return;
    }
    let boxed: Box<[u8]> = msg.as_bytes().to_vec().into_boxed_slice();
    let len = boxed.len();
    let ptr = Box::into_raw(boxed) as *const u8;
    unsafe {
        *out_ptr = ptr;
        *out_len = len;
    }
}

// slice_to_str: 将 (ptr, len) 解引用为 &str，不扫描 NUL
pub(crate) unsafe fn slice_to_str<'a>(
    ptr: *const u8,
    len: usize,
) -> Result<&'a str, std::str::Utf8Error> {
    if ptr.is_null() || len == 0 {
        return Ok("");
    }
    unsafe { std::str::from_utf8(std::slice::from_raw_parts(ptr, len)) }
}

// ─── Rust 单元测试 ─────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    // Cedar 测试共享全局 POLICY_STORE；并行运行会导致竞态，用此锁序列化。
    static CEDAR_TEST_LOCK: Mutex<()> = Mutex::new(());

    #[test]
    fn test_load_and_evaluate_allow() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 先强制重置为空 PolicySet，避免并行测试的全局状态污染
        let empty_ps = b"// empty\n";
        let mut reset_err_ptr: *const u8 = std::ptr::null();
        let mut reset_err_len: usize = 0;
        unsafe {
            cedar_load_policies(
                empty_ps.as_ptr(),
                empty_ps.len(),
                100,
                &mut reset_err_ptr,
                &mut reset_err_len,
            )
        };
        unsafe { cedar_free_bytes(reset_err_ptr as *mut u8, reset_err_len) };

        // deny-by-default: 无 permit 策略时全部拒绝
        let p = b"Agent::\"agent-1\"";
        let a = b"Action::\"infer\"";
        let r = b"Resource::\"llm_api\"";
        let ctx = b"{\"trust_level\": 3}";
        let mut out_ptr: *const u8 = std::ptr::null();
        let mut out_len: usize = 0;

        let result = unsafe {
            cedar_evaluate(
                p.as_ptr(),
                p.len(),
                a.as_ptr(),
                a.len(),
                r.as_ptr(),
                r.len(),
                ctx.as_ptr(),
                ctx.len(),
                100,
                &mut out_ptr,
                &mut out_len,
            )
        };
        unsafe { cedar_free_bytes(out_ptr as *mut u8, out_len) };
        assert_eq!(result, CEDAR_DENY, "empty policy set should deny");

        // 加载 permit 策略
        let policies = b"
            permit(
                principal,
                action == Action::\"infer\",
                resource
            ) when {
                context.trust_level >= 1
            };
        ";
        let mut err_ptr: *const u8 = std::ptr::null();
        let mut err_len: usize = 0;
        let load_result = unsafe {
            cedar_load_policies(
                policies.as_ptr(),
                policies.len(),
                100,
                &mut err_ptr,
                &mut err_len,
            )
        };
        let err_str = if err_ptr.is_null() {
            String::new()
        } else {
            let s = unsafe { slice_to_str(err_ptr, err_len) }
                .unwrap_or("")
                .to_string();
            unsafe { cedar_free_bytes(err_ptr as *mut u8, err_len) };
            s
        };
        assert_eq!(load_result, 0, "load failed: {err_str}");

        // 再次评估 — 应 ALLOW
        let mut out2_ptr: *const u8 = std::ptr::null();
        let mut out2_len: usize = 0;
        let result2 = unsafe {
            cedar_evaluate(
                p.as_ptr(),
                p.len(),
                a.as_ptr(),
                a.len(),
                r.as_ptr(),
                r.len(),
                ctx.as_ptr(),
                ctx.len(),
                100, // timeout_ms
                &mut out2_ptr,
                &mut out2_len,
            )
        };
        unsafe { cedar_free_bytes(out2_ptr as *mut u8, out2_len) };
        assert_eq!(
            result2, CEDAR_ALLOW,
            "should allow after loading permit policy"
        );
    }

    #[test]
    fn test_forbid_overrides_permit() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 同时存在 permit 和 forbid，forbid 应胜出
        let policies = b"
            permit(principal, action, resource);
            forbid(
                principal,
                action == Action::\"delete_data\",
                resource
            ) when {
                context.approval_status != \"approved\"
            };
        ";
        let mut err_ptr: *const u8 = std::ptr::null();
        let mut err_len: usize = 0;
        let load_result = unsafe {
            cedar_load_policies(
                policies.as_ptr(),
                policies.len(),
                100,
                &mut err_ptr,
                &mut err_len,
            )
        };
        unsafe { cedar_free_bytes(err_ptr as *mut u8, err_len) };
        assert_eq!(load_result, 0);

        let p = b"Agent::\"agent-1\"";
        let a = b"Action::\"delete_data\"";
        let r = b"Resource::\"prod_db\"";
        let ctx = b"{\"approval_status\": \"pending\"}";
        let mut out_ptr: *const u8 = std::ptr::null();
        let mut out_len: usize = 0;
        let result = unsafe {
            cedar_evaluate(
                p.as_ptr(),
                p.len(),
                a.as_ptr(),
                a.len(),
                r.as_ptr(),
                r.len(),
                ctx.as_ptr(),
                ctx.len(),
                100,
                &mut out_ptr,
                &mut out_len,
            )
        };
        unsafe { cedar_free_bytes(out_ptr as *mut u8, out_len) };
        assert_eq!(result, CEDAR_DENY, "forbid should override permit");
    }

    #[test]
    fn test_policy_count() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 重置为空
        let empty = b"";
        let mut err_ptr: *const u8 = std::ptr::null();
        let mut err_len: usize = 0;
        let _ = unsafe {
            cedar_load_policies(empty.as_ptr(), empty.len(), 100, &mut err_ptr, &mut err_len)
        };
        unsafe { cedar_free_bytes(err_ptr as *mut u8, err_len) };

        assert_eq!(cedar_policy_count(100), 0);
    }

    #[test]
    fn test_free_null_is_safe() {
        // cedar_free_bytes(null) 不 panic
        unsafe { cedar_free_bytes(std::ptr::null_mut(), 0) };
    }

    #[test]
    fn test_invalid_utf8_returns_error() {
        let bad = b"\xff\xfe";
        let mut out_ptr: *const u8 = std::ptr::null();
        let mut out_len: usize = 0;
        let a = b"Action::\"infer\"";
        let r = b"Resource::\"x\"";
        let ctx = b"{}";
        let result = unsafe {
            cedar_evaluate(
                bad.as_ptr(),
                bad.len(),
                a.as_ptr(),
                a.len(),
                r.as_ptr(),
                r.len(),
                ctx.as_ptr(),
                ctx.len(),
                100,
                &mut out_ptr,
                &mut out_len,
            )
        };
        unsafe { cedar_free_bytes(out_ptr as *mut u8, out_len) };
        assert_eq!(result, CEDAR_ERR_UTF8);
    }

    #[test]
    fn test_cedar_evaluate_no_timeout() {
        let p = b"Agent::\"agent-1\"";
        let a = b"Action::\"infer\"";
        let r = b"Resource::\"llm_api\"";
        let ctx = b"{\"trust_level\": 3}";
        let mut out_ptr: *const u8 = std::ptr::null();
        let mut out_len: usize = 0;

        let result = unsafe {
            cedar_evaluate(
                p.as_ptr(),
                p.len(),
                a.as_ptr(),
                a.len(),
                r.as_ptr(),
                r.len(),
                ctx.as_ptr(),
                ctx.len(),
                0, // 0 ms timeout means no timeout
                &mut out_ptr,
                &mut out_len,
            )
        };
        unsafe { cedar_free_bytes(out_ptr as *mut u8, out_len) };
        assert!(
            result >= 0,
            "0ms timeout means no timeout, should return allow (0) or deny (1)"
        );
    }

    // GR-7.2 验收：模拟锁竞争场景，验证 acquire_write_with_timeout 在 timeout_ms
    // 内取不到锁时返回 Timeout 错误而不是死等。直接测试 lock.rs 共享辅助函数、
    // 使用独立的本地 RwLock（不涉及全局 POLICY_STORE），避免与其它并行 cedar
    // 测试共享状态而 flaky。
    #[test]
    fn test_acquire_write_with_timeout_returns_timeout_on_contention() {
        let lock: RwLock<i32> = RwLock::new(0);
        // 持有读锁不释放，模拟"另一线程长时间持有锁"的竞争场景
        let _held = lock.read().unwrap();

        let start = std::time::Instant::now();
        let result = acquire_write_with_timeout(&lock, 100);
        let elapsed = start.elapsed();

        assert!(
            matches!(result, Err(LockAcquireError::Timeout)),
            "expected Timeout error under contention"
        );
        // 应在略高于 100ms 的合理范围内返回，而不是无限期阻塞。
        assert!(
            elapsed < std::time::Duration::from_millis(500),
            "acquire_write_with_timeout did not time out promptly: {:?}",
            elapsed
        );
    }

    // 对称验证 acquire_read_with_timeout：写锁持有期间，读锁获取也应超时返回。
    #[test]
    fn test_acquire_read_with_timeout_returns_timeout_on_contention() {
        let lock: RwLock<i32> = RwLock::new(0);
        let _held = lock.write().unwrap();

        let start = std::time::Instant::now();
        let result = acquire_read_with_timeout(&lock, 100);
        let elapsed = start.elapsed();

        assert!(
            matches!(result, Err(LockAcquireError::Timeout)),
            "expected Timeout error under contention"
        );
        assert!(
            elapsed < std::time::Duration::from_millis(500),
            "acquire_read_with_timeout did not time out promptly: {:?}",
            elapsed
        );
    }
}

#[test]
fn test_vec_cosine_identical() {
    let a = [1.0_f32, 0.0, 0.0];
    let r = unsafe { vec_cosine_f32(a.as_ptr(), 3, a.as_ptr(), 3) };
    assert!((r - 1.0).abs() < 1e-6, "same vector → cosine=1");
}

#[test]
fn test_vec_cosine_orthogonal() {
    let a = [1.0_f32, 0.0];
    let b = [0.0_f32, 1.0];
    let r = unsafe { vec_cosine_f32(a.as_ptr(), 2, b.as_ptr(), 2) };
    assert!(r.abs() < 1e-6, "orthogonal → cosine≈0");
}

#[test]
fn test_vec_cosine_empty_returns_zero() {
    let r = unsafe { vec_cosine_f32(std::ptr::null(), 0, std::ptr::null(), 0) };
    assert_eq!(r, 0.0);
}

// ═══════════════════════════════════════════════════════════════════════════════
// [Storage-SurrealDB-Core] 认知检索轴 FFI
// 架构文档: docs/arch/M02-Storage-Fabric.md §10，ADR-0010
//
// 功能: KV + HNSW 向量 + 有向图遍历 + BM25 全文检索
// ═══════════════════════════════════════════════════════════════════════════════

// ─── vec_cosine_f32 ─────────────────────────────────────────────────────────
// 计算两个 f32 向量的余弦相似度（Rust 编译器在 opt-level=3 下自动 SIMD 向量化）。
// 参数：ptr + 元素个数（非字节数）；空向量或长度不等返回 0.0。
// panic 被 catch_unwind 捕获，不越过 FFI 边界。
// SAFETY: Caller must ensure ptr is valid for len bytes...
#[unsafe(no_mangle)]
pub unsafe extern "C" fn vec_cosine_f32(
    a_ptr: *const f32,
    a_len: usize,
    b_ptr: *const f32,
    b_len: usize,
) -> f32 {
    std::panic::catch_unwind(|| {
        if a_ptr.is_null() || b_ptr.is_null() || a_len == 0 || a_len != b_len {
            return 0.0_f32;
        }
        // Safety: 调用方保证 ptr 有效且生命周期覆盖此调用
        let a = unsafe { std::slice::from_raw_parts(a_ptr, a_len) };
        let b = unsafe { std::slice::from_raw_parts(b_ptr, b_len) };

        let mut dot = 0.0_f32;
        let mut na = 0.0_f32;
        let mut nb = 0.0_f32;
        // 此循环在 -C opt-level=3 + target-cpu=native 下被 LLVM 自动向量化（AVX2/NEON）
        for i in 0..a_len {
            dot += a[i] * b[i];
            na += a[i] * a[i];
            nb += b[i] * b[i];
        }
        let denom = na.sqrt() * nb.sqrt();
        if denom == 0.0 {
            return 0.0;
        }
        let r = dot / denom;
        // 输入含 NaN/Inf 时结果可能非有限——清洗为 0.0，不让 NaN 越过 FFI 边界污染
        // 调用方排序/阈值判断（与 encode_scored 的 sanitize 口径一致）。
        if r.is_finite() { r } else { 0.0 }
    })
    .unwrap_or(0.0_f32)
}

// ═══════════════════════════════════════════════════════════════════════════════

mod surreal_store;

pub mod wasmtime_engine;

// 平台原生进程沙箱：macOS Seatbelt / Linux bwrap / Windows WSL2
// FFI: native_sandbox_exec / native_sandbox_probe_tools / native_sandbox_free_string
mod native_sandbox;

// 本地推理（P3-1，tier1 feature 门控，见 Cargo.toml [features]）。
// FFI: llama_infer_load / unload / generate / embed / rerank / evict_kv_cache /
//      status / free_string —— 仅 --features tier1 构建时导出符号。
#[cfg(feature = "tier1")]
mod llama_infer;
