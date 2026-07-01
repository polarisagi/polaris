// native_sandbox/fallback.rs — 降级沙箱实现（V1 + V2）
//
// namespace-only 降级（Linux）、WSL2（Windows）、bare exec（跨平台）
// V1: exec_namespace_fallback / exec_wsl2 / exec_bare
// V2: exec_namespace_fallback_v2 / exec_wsl2_v2 / build_wrap_argv_wsl2
//     exec_bare_v2 / build_wrap_argv_bare

// 部分 import 仅在特定平台 #[cfg] 块内使用；非目标平台编译时触发 unused——用 allow 压制。
#![allow(unused_imports)]

use std::process::Command;

use super::engine::{run_with_timeout, shell_quote_value, which_tool, windows_path_to_wsl};
use super::env::{build_env_v2, build_safe_env, build_sandbox_path, build_ulimit_prefix};
use super::types::{
    NativeSandboxRequest, NativeSandboxResponse, SandboxContextV2, WrapArgvResponseV2,
};

// ─── Linux namespace-only 降级 V1 ─────────────────────────────────────────────

/// bwrap 不可用时的最小隔离：只注入干净 env + 工作目录。
/// 无文件系统隔离，仅环境变量清洁。
#[cfg(target_os = "linux")]
pub(super) fn exec_namespace_fallback(
    req: &NativeSandboxRequest,
) -> Result<NativeSandboxResponse, String> {
    let workdir = req.workdir.as_deref().unwrap_or("/tmp");
    let timeout_ms = req.timeout_ms.unwrap_or(30_000);
    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);

    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let full_bare_cmd = format!("{}{}", ulimit_prefix, req.command);

    let mut cmd = Command::new("bash");
    cmd.args(["-c", &full_bare_cmd]);
    cmd.current_dir(workdir);
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }

    // Linux: 注入 PID+UTS namespace 作为最后防线（需要 unshare 命令）
    if let Some(unshare) = which_tool("unshare") {
        let env_prefix: String = env_vars
            .iter()
            .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
            .collect::<Vec<_>>()
            .join(" ");
        let full_cmd = format!("{}{}{}", ulimit_prefix, env_prefix, req.command);
        let mut ns_cmd = Command::new(&unshare);
        ns_cmd.args(["--pid", "--fork", "bash", "-c", &full_cmd]);
        ns_cmd.current_dir(workdir);
        ns_cmd.env_clear();
        for (k, v) in &env_vars {
            ns_cmd.env(k, v);
        }
        return run_with_timeout(
            ns_cmd,
            timeout_ms,
            "namespace",
            req.max_memory_mb.unwrap_or(0) > 0,
        );
    }

    run_with_timeout(cmd, timeout_ms, "bare", req.max_memory_mb.unwrap_or(0) > 0)
}

// ─── Windows WSL2 V1 ──────────────────────────────────────────────────────────

#[cfg(target_os = "windows")]
pub(super) fn exec_wsl2(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    let wsl = which_tool("wsl.exe")
        .ok_or_else(|| "wsl.exe not found; install WSL2: https://aka.ms/wsl2".to_string())?;

    let timeout_ms = req.timeout_ms.unwrap_or(30_000);
    let network_block = req.network_block.unwrap_or(true);
    if network_block {
        eprintln!(
            "[native_sandbox] WARNING: using unshare --net in WSL2 for network blocking, requires WSL2 Linux env to support it"
        );
    }

    let mut args: Vec<String> = Vec::new();

    let workdir_unix = if let Some(ref wd) = req.workdir {
        windows_path_to_wsl(wd)
    } else {
        String::new()
    };
    if !workdir_unix.is_empty() {
        args.extend(["--cd".into(), workdir_unix.clone()]);
    }

    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);
    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let env_prefix: String = env_vars
        .iter()
        .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
        .collect::<Vec<_>>()
        .join(" ");
    let full_command = format!("{}{}{}", ulimit_prefix, env_prefix, req.command);

    if network_block {
        args.extend([
            "-e".into(),
            "unshare".into(),
            "--net".into(),
            "bash".into(),
            "-c".into(),
            full_command,
        ]);
    } else {
        args.extend(["-e".into(), "bash".into(), "-c".into(), full_command]);
    }

    let mut cmd = Command::new(&wsl);
    cmd.args(&args);

    run_with_timeout(cmd, timeout_ms, "wsl2", req.max_memory_mb.unwrap_or(0) > 0)
}

// ─── 跨平台 bare 降级 V1 ─────────────────────────────────────────────────────

/// 无平台沙箱时的最小安全实现：清洁 env + workdir。
pub(super) fn exec_bare(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    let workdir = req.workdir.as_deref().unwrap_or("/tmp");
    let timeout_ms = req.timeout_ms.unwrap_or(30_000);
    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);

    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let shell = if cfg!(windows) { "cmd.exe" } else { "bash" };
    let shell_flag = if cfg!(windows) { "/C" } else { "-c" };

    let full_cmd = if cfg!(windows) {
        req.command.clone()
    } else {
        format!("{}{}", ulimit_prefix, req.command)
    };

    let mut cmd = Command::new(shell);
    cmd.args([shell_flag, &full_cmd]);
    if !workdir.is_empty() {
        cmd.current_dir(workdir);
    }
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }

    let memory_limited = !cfg!(windows) && req.max_memory_mb.unwrap_or(0) > 0;
    run_with_timeout(cmd, timeout_ms, "bare", memory_limited)
}

// ─── Linux namespace 降级 V2 ─────────────────────────────────────────────────

#[cfg(target_os = "linux")]
pub(super) fn exec_namespace_fallback_v2(
    ctx: &SandboxContextV2,
) -> Result<NativeSandboxResponse, String> {
    let workdir = ctx.workdir.as_deref().unwrap_or("/tmp");
    let timeout_ms = ctx.timeout_ms.unwrap_or(30_000);
    let caller_type = ctx.caller_type.as_deref().unwrap_or("builtin");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let ulimit_prefix = build_ulimit_prefix(ctx.max_memory_mb);
    let command = ctx.command.as_deref().unwrap_or("true");

    if let Some(unshare) = which_tool("unshare") {
        let env_prefix: String = env_vars
            .iter()
            .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
            .collect::<Vec<_>>()
            .join(" ");
        let full_cmd = format!("{}{}{}", ulimit_prefix, env_prefix, command);
        let mut cmd = Command::new(&unshare);
        cmd.args(["--pid", "--fork", "bash", "-c", &full_cmd]);
        cmd.current_dir(workdir);
        cmd.env_clear();
        for (k, v) in &env_vars {
            cmd.env(k, v);
        }
        return run_with_timeout(
            cmd,
            timeout_ms,
            "namespace",
            ctx.max_memory_mb.unwrap_or(0) > 0,
        );
    }

    let full_cmd = format!("{}{}", ulimit_prefix, command);
    let mut cmd = Command::new("bash");
    cmd.args(["-c", &full_cmd]);
    cmd.current_dir(workdir);
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }
    run_with_timeout(cmd, timeout_ms, "bare", ctx.max_memory_mb.unwrap_or(0) > 0)
}

// ─── Windows WSL2 V2 ─────────────────────────────────────────────────────────

#[cfg(target_os = "windows")]
pub(super) fn exec_wsl2_v2(ctx: &SandboxContextV2) -> Result<NativeSandboxResponse, String> {
    let wsl = which_tool("wsl.exe").ok_or_else(|| "wsl.exe not found".to_string())?;
    let timeout_ms = ctx.timeout_ms.unwrap_or(30_000);
    let network_policy = ctx.network_policy.as_deref().unwrap_or("deny");
    let network_block = network_policy != "allow";
    let caller_type = ctx.caller_type.as_deref().unwrap_or("builtin");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let workdir_unix = ctx
        .workdir
        .as_deref()
        .map(windows_path_to_wsl)
        .unwrap_or_default();
    let ulimit_prefix = build_ulimit_prefix(ctx.max_memory_mb);
    let env_prefix: String = env_vars
        .iter()
        .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
        .collect::<Vec<_>>()
        .join(" ");
    let inner_cmd = if let Some(ep) = &ctx.exec_path {
        let args_str = ctx
            .exec_args
            .as_deref()
            .unwrap_or(&[])
            .iter()
            .map(|s| shell_quote_value(s))
            .collect::<Vec<_>>()
            .join(" ");
        format!("{} {}", shell_quote_value(ep), args_str)
    } else {
        ctx.command.clone().unwrap_or_else(|| "true".to_string())
    };
    let full_cmd = format!("{}{}{}", ulimit_prefix, env_prefix, inner_cmd);
    let mut args: Vec<String> = Vec::new();
    if !workdir_unix.is_empty() {
        args.extend(["--cd".into(), workdir_unix]);
    }
    if network_block {
        args.extend([
            "-e".into(),
            "unshare".into(),
            "--net".into(),
            "bash".into(),
            "-c".into(),
            full_cmd,
        ]);
    } else {
        args.extend(["-e".into(), "bash".into(), "-c".into(), full_cmd]);
    }
    let mut cmd = Command::new(&wsl);
    cmd.args(&args);
    run_with_timeout(cmd, timeout_ms, "wsl2", ctx.max_memory_mb.unwrap_or(0) > 0)
}

#[cfg(target_os = "windows")]
pub(super) fn build_wrap_argv_wsl2(
    ctx: &SandboxContextV2,
) -> Result<WrapArgvResponseV2, String> {
    let wsl = which_tool("wsl.exe").ok_or_else(|| "wsl.exe not found".to_string())?;
    let network_policy = ctx.network_policy.as_deref().unwrap_or("deny");
    let network_block = network_policy != "allow";
    let caller_type = ctx.caller_type.as_deref().unwrap_or("mcp");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let workdir_unix = ctx
        .workdir
        .as_deref()
        .map(windows_path_to_wsl)
        .unwrap_or_default();
    let env_prefix: String = env_vars
        .iter()
        .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
        .collect::<Vec<_>>()
        .join(" ");
    let inner_cmd = if let Some(ep) = &ctx.exec_path {
        let args_str = ctx
            .exec_args
            .as_deref()
            .unwrap_or(&[])
            .iter()
            .map(|s| shell_quote_value(s))
            .collect::<Vec<_>>()
            .join(" ");
        format!("{} {}", shell_quote_value(ep), args_str)
    } else {
        ctx.command.clone().unwrap_or_else(|| "true".to_string())
    };
    let full_cmd = format!("{}{}", env_prefix, inner_cmd);
    let mut argv: Vec<String> = Vec::new();
    if !workdir_unix.is_empty() {
        argv.extend(["--cd".into(), workdir_unix]);
    }
    if network_block {
        argv.extend([
            "-e".into(),
            "unshare".into(),
            "--net".into(),
            "bash".into(),
            "-c".into(),
            full_cmd,
        ]);
    } else {
        argv.extend(["-e".into(), "bash".into(), "-c".into(), full_cmd]);
    }
    Ok(WrapArgvResponseV2 {
        executable: wsl,
        argv,
        env: vec![],
        env_in_argv: false,
        sandbox_method: "wsl2".to_string(),
        net_isolated: network_block,
    })
}

// ─── 跨平台降级：bare V2 ─────────────────────────────────────────────────────

pub(super) fn exec_bare_v2(ctx: &SandboxContextV2) -> Result<NativeSandboxResponse, String> {
    let workdir = ctx.workdir.as_deref().unwrap_or("/tmp");
    let timeout_ms = ctx.timeout_ms.unwrap_or(30_000);
    let caller_type = ctx.caller_type.as_deref().unwrap_or("builtin");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let ulimit_prefix = build_ulimit_prefix(ctx.max_memory_mb);
    let memory_limited = ctx.max_memory_mb.unwrap_or(0) > 0;

    let mut cmd = if let Some(ep) = &ctx.exec_path {
        let mut c = Command::new(ep);
        c.args(ctx.exec_args.as_deref().unwrap_or(&[]));
        c
    } else {
        let command = ctx.command.as_deref().unwrap_or("true");
        let full_cmd = format!("{}{}", ulimit_prefix, command);
        if cfg!(windows) {
            let mut c = Command::new("cmd.exe");
            c.args(["/C", &full_cmd]);
            c
        } else {
            let mut c = Command::new("bash");
            c.args(["-c", &full_cmd]);
            c
        }
    };

    if !workdir.is_empty() {
        cmd.current_dir(workdir);
    }
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }
    run_with_timeout(cmd, timeout_ms, "bare", memory_limited)
}

pub(super) fn build_wrap_argv_bare(
    ctx: &SandboxContextV2,
) -> Result<WrapArgvResponseV2, String> {
    let caller_type = ctx.caller_type.as_deref().unwrap_or("mcp");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let env_list: Vec<String> = env_vars
        .iter()
        .map(|(k, v)| format!("{}={}", k, v))
        .collect();
    let (executable, argv) = if let Some(ep) = &ctx.exec_path {
        (ep.clone(), ctx.exec_args.clone().unwrap_or_default())
    } else {
        let command = ctx.command.as_deref().unwrap_or("true");
        if cfg!(windows) {
            (
                "cmd.exe".to_string(),
                vec!["/C".to_string(), command.to_string()],
            )
        } else {
            (
                "bash".to_string(),
                vec!["-c".to_string(), command.to_string()],
            )
        }
    };
    Ok(WrapArgvResponseV2 {
        executable,
        argv,
        env: env_list,
        env_in_argv: false,
        sandbox_method: "bare".to_string(),
        net_isolated: false,
    })
}
