// Polaris — Rust substrate crate
// 包含 Cedar 策略引擎 FFI 接口与 SIMD 向量运算路径。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
//
// FFI 设计约束:
//   - 所有跨边界内存必须显式 free（cedar_free_string）
//   - 字符串参数：Go 侧保证 NUL-terminated UTF-8，Rust 侧不信任
//   - panic 不可越过 FFI 边界 —— 所有函数捕获 panic 转为错误码
//   - thread-safety: PolicyStore 通过 Arc<RwLock<>> 保护并发读写

#![allow(clippy::missing_safety_doc)]

use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
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

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────

/// 评估结果
const CEDAR_ALLOW: c_int = 0;
const CEDAR_DENY: c_int = 1;
const CEDAR_ERR_PARSE: c_int = -1; // 策略解析失败
const CEDAR_ERR_CONTEXT: c_int = -2; // Context/Entities 构造失败
const CEDAR_ERR_INTERNAL: c_int = -3; // panic 或锁中毒
const CEDAR_ERR_UTF8: c_int = -4; // 非法 UTF-8 输入

// ─── ABI 版本协议 ──────────────────────────────────────────────────────────────
// 设计依据: docs/arch/decisions/ADR-0011-cgo-to-purego-migration.md
// Go 侧加载 dylib 后立即调用 substrate_abi_version() 验证 major 匹配；
// major 不匹配 → panic（防 ABI silent drift）。
// 升 major: 破坏性变更（删/改导出函数签名）；升 minor: 加法变更（新增导出函数）。

/// ABI 主版本号：破坏性变更时递增。
const SUBSTRATE_ABI_MAJOR: u16 = 1;

/// ABI 次版本号：加法变更时递增。
const SUBSTRATE_ABI_MINOR: u16 = 0;

/// 返回当前 ABI 版本（高 16 位 major | 低 16 位 minor）。
/// Go 侧用 `(version >> 16) & 0xFFFF` 提取 major。
#[no_mangle]
pub extern "C" fn substrate_abi_version() -> u32 {
    ((SUBSTRATE_ABI_MAJOR as u32) << 16) | (SUBSTRATE_ABI_MINOR as u32)
}

// ─── cedar_load_policies ───────────────────────────────────────────────────────

/// 从 Cedar 策略文本（NUL-terminated UTF-8）加载/替换全局 PolicySet。
/// 返回 0 表示成功，负数表示错误（错误详情通过 out_err 返回）。
/// out_err 由 Rust 分配，调用方须调用 cedar_free_string 释放。
///
/// # Safety
/// policies_text 必须是有效的 NUL-terminated C 字符串，caller 负责生命周期。
#[no_mangle]
pub unsafe extern "C" fn cedar_load_policies(
    policies_text: *const c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        // 解析输入字符串
        let text = match unsafe_cstr_to_str(policies_text) {
            Ok(s) => s,
            Err(_) => {
                write_err(out_err, "invalid UTF-8 in policy text");
                return CEDAR_ERR_UTF8;
            }
        };

        // 解析 PolicySet
        let new_set = match text.parse::<PolicySet>() {
            Ok(ps) => ps,
            Err(e) => {
                write_err(out_err, &format!("policy parse error: {e}"));
                return CEDAR_ERR_PARSE;
            }
        };

        // 写入全局 PolicyStore
        let store = policy_store();
        // 显式绑定 write guard，防止临时值在 match 结束前 drop（E0597）
        let mut guard = match store.write() {
            Ok(g) => g,
            Err(e) => {
                write_err(out_err, &format!("lock poisoned: {e}"));
                return CEDAR_ERR_INTERNAL;
            }
        };
        *guard = new_set;
        write_err(out_err, "");
        0
    });

    match result {
        Ok(code) => code,
        Err(_) => {
            write_err(out_err, "panic in cedar_load_policies");
            CEDAR_ERR_INTERNAL
        }
    }
}

// ─── cedar_evaluate ────────────────────────────────────────────────────────────

/// 评估单次策略请求。
/// 参数均为 NUL-terminated UTF-8 C 字符串:
///   principal: Cedar EntityUID 格式，例如 `Agent::"agent-42"`
///   action:    Cedar EntityUID 格式，例如 `Action::"infer"`
///   resource:  Cedar EntityUID 格式，例如 `Resource::"llm_api"`
///   context_json: JSON 对象，例如 `{"trust_level": 3, "capability_token_valid": true}`
///
/// 返回 0(ALLOW) / 1(DENY) / 负数(错误)。
/// out_reason 由 Rust 分配，调用方须调用 cedar_free_string 释放。
///
/// # Safety
/// 所有 *const c_char 参数须为有效 NUL-terminated C 字符串。
#[no_mangle]
pub unsafe extern "C" fn cedar_evaluate(
    principal: *const c_char,
    action: *const c_char,
    resource: *const c_char,
    context_json: *const c_char,
    out_reason: *mut *mut c_char,
) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        // 解析四个输入参数
        let p_str = match unsafe_cstr_to_str(principal) {
            Ok(s) => s,
            Err(_) => {
                write_err(out_reason, "invalid UTF-8: principal");
                return CEDAR_ERR_UTF8;
            }
        };
        let a_str = match unsafe_cstr_to_str(action) {
            Ok(s) => s,
            Err(_) => {
                write_err(out_reason, "invalid UTF-8: action");
                return CEDAR_ERR_UTF8;
            }
        };
        let r_str = match unsafe_cstr_to_str(resource) {
            Ok(s) => s,
            Err(_) => {
                write_err(out_reason, "invalid UTF-8: resource");
                return CEDAR_ERR_UTF8;
            }
        };
        let ctx_str = match unsafe_cstr_to_str(context_json) {
            Ok(s) => s,
            Err(_) => {
                write_err(out_reason, "invalid UTF-8: context_json");
                return CEDAR_ERR_UTF8;
            }
        };

        // 构造 EntityUID
        let p_uid = match p_str.parse::<EntityUid>() {
            Ok(u) => u,
            Err(e) => {
                write_err(out_reason, &format!("principal parse: {e}"));
                return CEDAR_ERR_CONTEXT;
            }
        };
        let a_uid = match a_str.parse::<EntityUid>() {
            Ok(u) => u,
            Err(e) => {
                write_err(out_reason, &format!("action parse: {e}"));
                return CEDAR_ERR_CONTEXT;
            }
        };
        let r_uid = match r_str.parse::<EntityUid>() {
            Ok(u) => u,
            Err(e) => {
                write_err(out_reason, &format!("resource parse: {e}"));
                return CEDAR_ERR_CONTEXT;
            }
        };

        // 构造 Context（从 JSON）
        let ctx = match Context::from_json_str(ctx_str, None) {
            Ok(c) => c,
            Err(e) => {
                write_err(out_reason, &format!("context parse: {e}"));
                return CEDAR_ERR_CONTEXT;
            }
        };

        let store = policy_store();
        // 显式绑定 guard，防止 store 在 guard 使用前 drop（E0597）
        let guard = match store.read() {
            Ok(g) => g,
            Err(e) => {
                write_err(out_reason, &format!("lock poisoned: {e}"));
                return CEDAR_ERR_INTERNAL;
            }
        };
        let policy_set: &PolicySet = &guard;

        let request = Request::new(p_uid, a_uid, r_uid, ctx, None)
            .expect("Request::new should not fail with validated EntityUIDs");

        let authorizer = Authorizer::new();
        // 空 Entities —— 实体属性通过 context_json 传递（MVP 简化）
        let entities = Entities::empty();
        let response = authorizer.is_authorized(&request, policy_set, &entities);

        let (code, reason) = match response.decision() {
            Decision::Allow => (CEDAR_ALLOW, "allow"),
            Decision::Deny => (CEDAR_DENY, "deny"),
        };
        write_err(out_reason, reason);
        code
    });

    match result {
        Ok(code) => code,
        Err(_) => {
            write_err(out_reason, "panic in cedar_evaluate");
            CEDAR_ERR_INTERNAL
        }
    }
}

// ─── cedar_policy_count ────────────────────────────────────────────────────────

/// 返回当前 PolicySet 中的策略数量。
/// 用于健康检查和热更新验证。
#[no_mangle]
pub extern "C" fn cedar_policy_count() -> c_int {
    let store = policy_store();
    let guard = match store.read() {
        Ok(g) => g,
        Err(_) => return -1,
    };
    // 显式绑定避免临时 guard 在 match 结束时 drop（E0597）
    let count = guard.policies().count() as c_int;
    count
}

// ─── cedar_free_string ─────────────────────────────────────────────────────────

/// 释放由 cedar_* 函数分配的 C 字符串。
/// 必须与 cedar_load_policies/cedar_evaluate 的 out_err/out_reason 配对调用。
///
/// # Safety
/// ptr 须为 cedar_* 函数分配的指针，或 null。
#[no_mangle]
pub unsafe extern "C" fn cedar_free_string(ptr: *mut c_char) {
    if !ptr.is_null() {
        // 重新构造 CString 以正确释放
        unsafe { drop(CString::from_raw(ptr)) };
    }
}

// ─── 内部工具函数 ──────────────────────────────────────────────────────────────

/// 将 C 字符串指针安全转为 &str（非拷贝，lifetime 绑定到原始指针生命周期）。
unsafe fn unsafe_cstr_to_str<'a>(ptr: *const c_char) -> Result<&'a str, std::str::Utf8Error> {
    if ptr.is_null() {
        return Ok("");
    }
    unsafe { CStr::from_ptr(ptr) }.to_str()
}

/// 写入 out 指针处的错误字符串（调用方须用 cedar_free_string 释放）。
fn write_err(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    match CString::new(msg) {
        Ok(cs) => unsafe { *out = cs.into_raw() },
        Err(_) => {
            // msg 含 NUL 字节，写入截断版本
            let safe = msg.replace('\0', "?");
            if let Ok(cs) = CString::new(safe) {
                unsafe { *out = cs.into_raw() }
            }
        }
    }
}

// ─── Rust 单元测试 ─────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::CString;
    use std::sync::Mutex;

    // Cedar 测试共享全局 POLICY_STORE；并行运行会导致竞态，用此锁序列化。
    static CEDAR_TEST_LOCK: Mutex<()> = Mutex::new(());

    fn cstr(s: &str) -> CString {
        CString::new(s).unwrap()
    }

    #[test]
    fn test_load_and_evaluate_allow() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 先强制重置为空 PolicySet，避免并行测试的全局状态污染
        let empty_ps = cstr("// empty\n");
        let mut reset_err: *mut c_char = std::ptr::null_mut();
        unsafe { cedar_load_policies(empty_ps.as_ptr(), &mut reset_err) };
        unsafe { cedar_free_string(reset_err) };

        // deny-by-default: 无 permit 策略时全部拒绝
        let p = cstr("Agent::\"agent-1\"");
        let a = cstr("Action::\"infer\"");
        let r = cstr("Resource::\"llm_api\"");
        let ctx = cstr("{\"trust_level\": 3}");
        let mut out: *mut c_char = std::ptr::null_mut();

        let result =
            unsafe { cedar_evaluate(p.as_ptr(), a.as_ptr(), r.as_ptr(), ctx.as_ptr(), &mut out) };
        unsafe { cedar_free_string(out) };
        assert_eq!(result, CEDAR_DENY, "empty policy set should deny");

        // 加载 permit 策略
        let policies = cstr(
            r#"
            permit(
                principal,
                action == Action::"infer",
                resource
            ) when {
                context.trust_level >= 1
            };
        "#,
        );
        let mut err: *mut c_char = std::ptr::null_mut();
        let load_result = unsafe { cedar_load_policies(policies.as_ptr(), &mut err) };
        let err_str = if err.is_null() {
            String::new()
        } else {
            let s = unsafe { CStr::from_ptr(err) }
                .to_str()
                .unwrap_or("")
                .to_string();
            unsafe { cedar_free_string(err) };
            s
        };
        assert_eq!(load_result, 0, "load failed: {err_str}");

        // 再次评估 — 应 ALLOW
        let mut out2: *mut c_char = std::ptr::null_mut();
        let result2 =
            unsafe { cedar_evaluate(p.as_ptr(), a.as_ptr(), r.as_ptr(), ctx.as_ptr(), &mut out2) };
        unsafe { cedar_free_string(out2) };
        assert_eq!(
            result2, CEDAR_ALLOW,
            "should allow after loading permit policy"
        );
    }

    #[test]
    fn test_forbid_overrides_permit() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 同时存在 permit 和 forbid，forbid 应胜出
        let policies = cstr(
            r#"
            permit(principal, action, resource);
            forbid(
                principal,
                action == Action::"delete_data",
                resource
            ) when {
                context.approval_status != "approved"
            };
        "#,
        );
        let mut err: *mut c_char = std::ptr::null_mut();
        let load_result = unsafe { cedar_load_policies(policies.as_ptr(), &mut err) };
        unsafe { cedar_free_string(err) };
        assert_eq!(load_result, 0);

        let p = cstr("Agent::\"agent-1\"");
        let a = cstr("Action::\"delete_data\"");
        let r = cstr("Resource::\"prod_db\"");
        let ctx = cstr("{\"approval_status\": \"pending\"}");
        let mut out: *mut c_char = std::ptr::null_mut();
        let result =
            unsafe { cedar_evaluate(p.as_ptr(), a.as_ptr(), r.as_ptr(), ctx.as_ptr(), &mut out) };
        unsafe { cedar_free_string(out) };
        assert_eq!(result, CEDAR_DENY, "forbid should override permit");
    }

    #[test]
    fn test_policy_count() {
        let _guard = CEDAR_TEST_LOCK.lock().unwrap();
        // 重置为空
        let empty = cstr("");
        let mut err: *mut c_char = std::ptr::null_mut();
        let _ = unsafe { cedar_load_policies(empty.as_ptr(), &mut err) };
        unsafe { cedar_free_string(err) };

        assert_eq!(cedar_policy_count(), 0);
    }

    #[test]
    fn test_free_null_is_safe() {
        // cedar_free_string(null) 不 panic
        unsafe { cedar_free_string(std::ptr::null_mut()) };
    }

    #[test]
    fn test_invalid_utf8_returns_error() {
        let bad: *const c_char = b"\xff\xfe\0".as_ptr() as *const c_char;
        let mut out: *mut c_char = std::ptr::null_mut();
        let a = cstr("Action::\"infer\"");
        let r = cstr("Resource::\"x\"");
        let ctx = cstr("{}");
        let result = unsafe { cedar_evaluate(bad, a.as_ptr(), r.as_ptr(), ctx.as_ptr(), &mut out) };
        unsafe { cedar_free_string(out) };
        assert_eq!(result, CEDAR_ERR_UTF8);
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// [Storage-SurrealDB-Core] 认知检索轴 FFI
// 架构文档: docs/arch/M02-Storage-Fabric.md §10，ADR-0010
//
// 功能: KV + HNSW 向量 + 有向图遍历 + BM25 全文检索
// 后端: surreal-mem（默认，kv-mem）/ surreal-rocksdb（显式，≥16GB）
// ═══════════════════════════════════════════════════════════════════════════════

mod surreal_store;

pub mod wasmtime_engine;

// 平台原生进程沙箱：macOS Seatbelt / Linux bwrap / Windows WSL2
// FFI: native_sandbox_exec / native_sandbox_probe_tools / native_sandbox_free_string
mod native_sandbox;
