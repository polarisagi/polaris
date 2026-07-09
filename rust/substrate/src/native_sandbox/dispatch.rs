// native_sandbox/dispatch.rs — 平台分发 + 工具探测 + 公开 FFI 函数
//
// V1: dispatch_sandbox / probe_tools / native_sandbox_exec / native_sandbox_probe_tools / native_sandbox_free_string
// V2: dispatch_exec_v2 / dispatch_wrap_argv / native_sandbox_exec_v2 / native_sandbox_wrap_argv

use std::ffi::CString;
use std::os::raw::{c_char, c_int};
use std::panic;

use super::engine::{ns_read_cstr, ns_write_cstr, which_tool};
use super::env::build_sandbox_path;
use super::types::{
    NS_ERR_INTERNAL, NS_ERR_TIMEOUT, NS_ERR_UTF8, NS_OK, NativeSandboxRequest,
    NativeSandboxResponse, SandboxContextV2, ToolProbeResult, WrapArgvResponseV2,
};

// 平台分发辅助：各平台实现通过 super 访问
#[cfg(target_os = "linux")]
use super::bwrap::{build_wrap_argv_bwrap, exec_bwrap, exec_bwrap_v2};
#[cfg(target_os = "windows")]
use super::fallback::{build_wrap_argv_wsl2, exec_wsl2, exec_wsl2_v2};
#[cfg(target_os = "macos")]
use super::seatbelt::{build_wrap_argv_seatbelt, exec_seatbelt, exec_seatbelt_v2};

use super::fallback::{build_wrap_argv_bare, exec_bare, exec_bare_v2};

#[cfg(target_os = "linux")]
use super::fallback::exec_namespace_fallback;
#[cfg(target_os = "linux")]
use super::fallback::exec_namespace_fallback_v2;

// ─── 平台分发 V1 ──────────────────────────────────────────────────────────────

/// 按当前平台选择沙箱实现，失败时自动降级。
///
/// fail-closed 规则（HE-Rule 2：物理断裂 > 概率过滤）：
/// 调用方以 network_block=true 声明"需要网络隔离"时，若平台原生隔离工具
/// （seatbelt/bwrap/wsl2）不可用，一律拒绝执行——不静默降级到 bare/namespace，
/// 因为那些降级路径不提供任何网络隔离，会让"声明了网络隔离"的调用方在
/// 毫无感知的情况下裸奔。network_block=false 时降级行为不变（兼容 Tier-0）。
fn dispatch_sandbox(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    let network_block = req.network_block.unwrap_or(false);

    #[cfg(target_os = "macos")]
    {
        match exec_seatbelt(req) {
            Ok(mut r) => {
                r.net_isolated = network_block;
                return Ok(r);
            }
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: sandbox-exec(seatbelt) unavailable ({}); refusing to run with network_block=true \
                         because the bare fallback provides no network isolation. Install Xcode Command Line Tools, \
                         or explicitly pass network_block=false to allow degraded (unisolated) execution.",
                        e
                    ));
                }
                eprintln!(
                    "[native_sandbox] seatbelt failed ({}), falling back to bare",
                    e
                );
            }
        }
        return exec_bare(req);
    }

    #[cfg(target_os = "linux")]
    {
        match exec_bwrap(req) {
            Ok(mut r) => {
                r.net_isolated = network_block;
                return Ok(r);
            }
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: bwrap unavailable ({}); refusing to run with network_block=true \
                         because the namespace fallback provides no network isolation. Install bubblewrap \
                         (apt-get install bubblewrap), or explicitly pass network_block=false to allow \
                         degraded (unisolated) execution.",
                        e
                    ));
                }
                eprintln!(
                    "[native_sandbox] bwrap failed ({}), falling back to namespace",
                    e
                );
            }
        }
        return exec_namespace_fallback(req);
    }

    #[cfg(target_os = "windows")]
    {
        match exec_wsl2(req) {
            // WSL2 的 --net unshare 是否真正生效取决于宿主 WSL2 内核配置，未经验证，
            // 诚实上报 net_isolated=false（与 build_wrap_argv_wsl2 保持一致）。
            Ok(r) => return Ok(r),
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: WSL2 unavailable ({}); refusing to run with network_block=true \
                         because the bare fallback provides no network isolation. Install WSL2 \
                         (https://aka.ms/wsl2), or explicitly pass network_block=false to allow degraded \
                         (unisolated) execution.",
                        e
                    ));
                }
                eprintln!("[native_sandbox] wsl2 failed ({}), falling back to bare", e);
            }
        }
        return exec_bare(req);
    }

    // 其他平台（FreeBSD 等）：无任何原生隔离工具，network_block=true 时直接拒绝。
    #[allow(unreachable_code)]
    {
        if network_block {
            return Err(
                "sandbox degraded: no native sandbox tool available on this platform; refusing to run \
                 with network_block=true. Explicitly pass network_block=false to allow degraded \
                 (unisolated) execution.".to_string(),
            );
        }
        exec_bare(req)
    }
}

// ─── 工具探测 ─────────────────────────────────────────────────────────────────

/// 探测当前系统上可用的沙箱方法和语言运行时。
/// 供 Go 侧在启动时或 sys_probe 工具中调用，生成诊断报告。
fn probe_tools() -> ToolProbeResult {
    let platform = std::env::consts::OS.to_string();
    let sandbox_path = build_sandbox_path();

    let (sandbox_method, bwrap_path, seatbelt_available, wsl2_available) = {
        #[cfg(target_os = "macos")]
        {
            let sa = which_tool("sandbox-exec").is_some();
            (
                if sa { "seatbelt" } else { "bare" }.to_string(),
                None,
                sa,
                false,
            )
        }
        #[cfg(target_os = "linux")]
        {
            let bp = which_tool("bwrap");
            (
                if bp.is_some() { "bwrap" } else { "namespace" }.to_string(),
                bp,
                false,
                false,
            )
        }
        #[cfg(target_os = "windows")]
        {
            let w = which_tool("wsl.exe").is_some();
            (if w { "wsl2" } else { "bare" }.to_string(), None, false, w)
        }
        #[cfg(not(any(target_os = "macos", target_os = "linux", target_os = "windows")))]
        {
            ("bare".to_string(), None, false, false)
        }
    };

    // 探测常见语言运行时
    let runtimes = [
        "python3", "python", "node", "npm", "npx", "deno", "bun", "go", "cargo", "rustc", "java",
        "javac", "ruby", "gem", "perl", "php", "julia", "tsc", "pnpm", "yarn",
    ];
    let found_runtimes: Vec<String> = runtimes
        .iter()
        .filter_map(|&rt| which_tool(rt).map(|path| format!("{}={}", rt, path)))
        .collect();

    ToolProbeResult {
        platform,
        sandbox_method,
        resolved_path: sandbox_path,
        bwrap_path,
        seatbelt_available,
        wsl2_available,
        found_runtimes,
        wasi_network_supported: true,
    }
}

// ─── V2 平台分发 ─────────────────────────────────────────────────────────────

/// V2 同 dispatch_sandbox 的 fail-closed 规则：network_policy != "allow"
/// （即 "deny" 或 "domain_whitelist"）且原生隔离工具不可用时拒绝执行。
/// domain_whitelist 在无原生工具时同样必须拒绝——它本身就只是 deny+DNS 近似
/// 实现（见 bwrap.rs/seatbelt.rs 注释），没有 seatbelt/bwrap 承载时等于零隔离。
fn dispatch_exec_v2(ctx: &SandboxContextV2) -> Result<NativeSandboxResponse, String> {
    let network_block = ctx.network_policy.as_deref().unwrap_or("deny") != "allow";
    // net_isolated 上报口径与 build_wrap_argv_* 对齐：只有纯 "deny" 才算真正隔离；
    // "domain_whitelist" 本身只是 deny+DNS 近似实现（见 bwrap.rs/seatbelt.rs 注释），
    // 不管有没有原生工具承载都不应声明为 isolated=true。
    let net_isolated_claim = ctx.network_policy.as_deref() == Some("deny");

    #[cfg(target_os = "macos")]
    {
        match exec_seatbelt_v2(ctx) {
            Ok(mut r) => {
                // run_with_timeout 默认写 false；此处按 network_policy 回填真实隔离状态。
                r.net_isolated = net_isolated_claim;
                return Ok(r);
            }
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: seatbelt unavailable ({}); refusing network_policy={:?} \
                         because bare fallback has no network isolation",
                        e, ctx.network_policy
                    ));
                }
                eprintln!("[native_sandbox_v2] seatbelt failed ({}), fallback bare", e);
            }
        }
        return exec_bare_v2(ctx);
    }

    #[cfg(target_os = "linux")]
    {
        match exec_bwrap_v2(ctx) {
            Ok(mut r) => {
                r.net_isolated = net_isolated_claim;
                return Ok(r);
            }
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: bwrap unavailable ({}); refusing network_policy={:?} \
                         because namespace fallback has no network isolation",
                        e, ctx.network_policy
                    ));
                }
                eprintln!(
                    "[native_sandbox_v2] bwrap failed ({}), fallback namespace",
                    e
                );
            }
        }
        return exec_namespace_fallback_v2(ctx);
    }

    #[cfg(target_os = "windows")]
    {
        match exec_wsl2_v2(ctx) {
            Ok(r) => return Ok(r),
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: WSL2 unavailable ({}); refusing network_policy={:?} \
                         because bare fallback has no network isolation",
                        e, ctx.network_policy
                    ));
                }
                eprintln!("[native_sandbox_v2] wsl2 failed ({}), fallback bare", e);
            }
        }
        return exec_bare_v2(ctx);
    }

    #[allow(unreachable_code)]
    {
        if network_block {
            return Err(format!(
                "sandbox degraded: no native sandbox tool available on this platform; refusing network_policy={:?}",
                ctx.network_policy
            ));
        }
        exec_bare_v2(ctx)
    }
}

fn dispatch_wrap_argv(ctx: &SandboxContextV2) -> Result<WrapArgvResponseV2, String> {
    let network_block = ctx.network_policy.as_deref().unwrap_or("deny") != "allow";

    #[cfg(target_os = "macos")]
    {
        match build_wrap_argv_seatbelt(ctx) {
            Ok(r) => return Ok(r),
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: seatbelt unavailable ({}); refusing to build unisolated argv \
                         for network_policy={:?}",
                        e, ctx.network_policy
                    ));
                }
                eprintln!(
                    "[native_sandbox_v2] wrap_argv seatbelt failed ({}), fallback bare",
                    e
                );
            }
        }
        return build_wrap_argv_bare(ctx);
    }

    #[cfg(target_os = "linux")]
    {
        match build_wrap_argv_bwrap(ctx) {
            Ok(r) => return Ok(r),
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: bwrap unavailable ({}); refusing to build unisolated argv \
                         for network_policy={:?}",
                        e, ctx.network_policy
                    ));
                }
                eprintln!(
                    "[native_sandbox_v2] wrap_argv bwrap failed ({}), fallback bare",
                    e
                );
            }
        }
        return build_wrap_argv_bare(ctx);
    }

    #[cfg(target_os = "windows")]
    {
        match build_wrap_argv_wsl2(ctx) {
            Ok(r) => return Ok(r),
            Err(e) => {
                if network_block {
                    return Err(format!(
                        "sandbox degraded: WSL2 unavailable ({}); refusing to build unisolated argv \
                         for network_policy={:?}",
                        e, ctx.network_policy
                    ));
                }
                eprintln!(
                    "[native_sandbox_v2] wrap_argv wsl2 failed ({}), fallback bare",
                    e
                );
            }
        }
        return build_wrap_argv_bare(ctx);
    }

    #[allow(unreachable_code)]
    {
        if network_block {
            return Err(format!(
                "sandbox degraded: no native sandbox tool available on this platform; refusing to build \
                 unisolated argv for network_policy={:?}",
                ctx.network_policy
            ));
        }
        build_wrap_argv_bare(ctx)
    }
}

// ─── 公开 FFI 函数 V1 ─────────────────────────────────────────────────────────

/// native_sandbox_exec — 在平台沙箱中执行命令。
///
/// # Safety
/// input_json, out_json, out_err 必须是有效指针（out_* 可为 null）。
/// 返回值：NS_OK(0) 表示进程成功启动并退出（exit_code 见 out_json）；
///         负数表示沙箱自身出错（非命令执行失败）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn native_sandbox_exec(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match ns_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    ns_write_cstr(out_err, "invalid UTF-8 in input_json");
                    return NS_ERR_UTF8;
                }
            };

            let req: NativeSandboxRequest = match serde_json::from_str(json_str) {
                Ok(r) => r,
                Err(e) => {
                    ns_write_cstr(out_err, &format!("JSON parse error: {}", e));
                    return NS_ERR_INTERNAL;
                }
            };

            match dispatch_sandbox(&req) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(json) => {
                        ns_write_cstr(out_json, &json);
                        NS_OK
                    }
                    Err(e) => {
                        ns_write_cstr(out_err, &format!("JSON serialize error: {}", e));
                        NS_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    if e.contains("timeout") {
                        ns_write_cstr(out_err, &e);
                        return NS_ERR_TIMEOUT;
                    }
                    ns_write_cstr(out_err, &e);
                    NS_ERR_INTERNAL
                }
            }
        });

        match result {
            Ok(code) => code,
            Err(_) => {
                ns_write_cstr(out_err, "panic in native_sandbox_exec");
                NS_ERR_INTERNAL
            }
        }
    }
}

/// native_sandbox_probe_tools — 探测沙箱方法和语言运行时，返回 JSON。
///
/// # Safety
/// out_json, out_err 必须是有效指针或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn native_sandbox_probe_tools(
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        let probe = probe_tools();
        match serde_json::to_string(&probe) {
            Ok(json) => {
                ns_write_cstr(out_json, &json);
                NS_OK
            }
            Err(e) => {
                ns_write_cstr(out_err, &format!("serialize error: {}", e));
                NS_ERR_INTERNAL
            }
        }
    });

    match result {
        Ok(code) => code,
        Err(_) => {
            ns_write_cstr(out_err, "panic in native_sandbox_probe_tools");
            NS_ERR_INTERNAL
        }
    }
}

/// native_sandbox_free_string — 释放由 native_sandbox_* 分配的 C 字符串。
///
/// # Safety
/// ptr 须为 native_sandbox_* 分配的指针，或 null。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn native_sandbox_free_string(ptr: *mut c_char) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
        if !ptr.is_null() {
            drop(CString::from_raw(ptr));
        }
    }));
}

// ─── 公开 FFI 函数 V2 ─────────────────────────────────────────────────────────

/// native_sandbox_exec_v2 — V2 统一沙箱执行（run-to-completion）。
/// 用于 Bash/CodeAct/Skill/Hook/Builtin 等短生命周期命令。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn native_sandbox_exec_v2(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match ns_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    ns_write_cstr(out_err, "invalid UTF-8");
                    return NS_ERR_UTF8;
                }
            };
            let ctx: SandboxContextV2 = match serde_json::from_str(json_str) {
                Ok(c) => c,
                Err(e) => {
                    ns_write_cstr(out_err, &format!("JSON parse: {}", e));
                    return NS_ERR_INTERNAL;
                }
            };
            match dispatch_exec_v2(&ctx) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(json) => {
                        ns_write_cstr(out_json, &json);
                        NS_OK
                    }
                    Err(e) => {
                        ns_write_cstr(out_err, &format!("serialize: {}", e));
                        NS_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    if e.contains("timeout") {
                        ns_write_cstr(out_err, &e);
                        return NS_ERR_TIMEOUT;
                    }
                    ns_write_cstr(out_err, &e);
                    NS_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                ns_write_cstr(out_err, "panic in native_sandbox_exec_v2");
                NS_ERR_INTERNAL
            }
        }
    }
}

/// native_sandbox_wrap_argv — V2 仅返回封装后 argv，不启动进程。
/// 用于 MCP stdio 长进程：Go 侧用返回 argv 创建 exec.Cmd 并持有 stdin/stdout 管道。
///
/// # Safety
/// input_json, out_json, out_err 须为有效指针（out_* 可为 null）。
#[unsafe(no_mangle)]
pub unsafe extern "C" fn native_sandbox_wrap_argv(
    input_json: *const c_char,
    out_json: *mut *mut c_char,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            let json_str = match ns_read_cstr(input_json) {
                Ok(s) => s,
                Err(_) => {
                    ns_write_cstr(out_err, "invalid UTF-8");
                    return NS_ERR_UTF8;
                }
            };
            let ctx: SandboxContextV2 = match serde_json::from_str(json_str) {
                Ok(c) => c,
                Err(e) => {
                    ns_write_cstr(out_err, &format!("JSON parse: {}", e));
                    return NS_ERR_INTERNAL;
                }
            };
            match dispatch_wrap_argv(&ctx) {
                Ok(resp) => match serde_json::to_string(&resp) {
                    Ok(json) => {
                        ns_write_cstr(out_json, &json);
                        NS_OK
                    }
                    Err(e) => {
                        ns_write_cstr(out_err, &format!("serialize: {}", e));
                        NS_ERR_INTERNAL
                    }
                },
                Err(e) => {
                    ns_write_cstr(out_err, &e);
                    NS_ERR_INTERNAL
                }
            }
        });
        match result {
            Ok(code) => code,
            Err(_) => {
                ns_write_cstr(out_err, "panic in native_sandbox_wrap_argv");
                NS_ERR_INTERNAL
            }
        }
    }
}

// ─── 单元测试 ──────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use std::ffi::{CStr, CString};

    use super::*;

    #[test]
    fn test_probe_tools_serializes() {
        let probe = probe_tools();
        let json = serde_json::to_string(&probe).unwrap();
        assert!(!json.is_empty());
        assert!(json.contains("platform"));
    }

    #[test]
    fn test_ffi_exec_echo() {
        let input = serde_json::json!({
            "command": "echo hello_sandbox",
            "workdir": "/tmp",
            "network_block": true,
            "timeout_ms": 5000
        });
        let input_cstr = CString::new(input.to_string()).unwrap();
        let mut out_json: *mut c_char = std::ptr::null_mut();
        let mut out_err: *mut c_char = std::ptr::null_mut();

        let code = unsafe { native_sandbox_exec(input_cstr.as_ptr(), &mut out_json, &mut out_err) };

        if !out_err.is_null() {
            let err_msg = unsafe { CStr::from_ptr(out_err).to_str().unwrap_or("") };
            if !err_msg.is_empty() {
                eprintln!("sandbox error: {}", err_msg);
            }
            unsafe { native_sandbox_free_string(out_err) };
        }

        if !out_json.is_null() {
            let json_str = unsafe { CStr::from_ptr(out_json).to_str().unwrap_or("") };
            assert!(
                json_str.contains("hello_sandbox") || json_str.contains("exit_code"),
                "output should contain hello_sandbox or exit_code, got: {}",
                json_str
            );
            unsafe { native_sandbox_free_string(out_json) };
        }

        assert_eq!(code, NS_OK, "exec should return NS_OK");
    }

    #[test]
    fn test_ffi_probe_tools() {
        let mut out_json: *mut c_char = std::ptr::null_mut();
        let mut out_err: *mut c_char = std::ptr::null_mut();

        let code = unsafe { native_sandbox_probe_tools(&mut out_json, &mut out_err) };
        assert_eq!(code, NS_OK);

        if !out_json.is_null() {
            let s = unsafe { CStr::from_ptr(out_json).to_str().unwrap_or("") };
            assert!(s.contains("platform"));
            unsafe { native_sandbox_free_string(out_json) };
        }
        if !out_err.is_null() {
            unsafe { native_sandbox_free_string(out_err) };
        }
    }

    #[test]
    fn test_free_null_safe() {
        // 不 panic
        unsafe { native_sandbox_free_string(std::ptr::null_mut()) };
    }
}
