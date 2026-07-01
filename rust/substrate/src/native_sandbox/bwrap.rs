// native_sandbox/bwrap.rs — Linux bubblewrap 沙箱（V1 + V2）
//
// V1: exec_bwrap
// V2: build_bwrap_args_v2 / exec_bwrap_v2 / build_wrap_argv_bwrap
//
// 设计依据: ADR-0008-sandbox-three-tier-fallback.md §Linux

// 所有 import 仅在 #[cfg(target_os = "linux")] 代码块内使用；
// 非 Linux 平台编译时 rustc 报 unused——用 allow 压制，禁止随意删 import。
#![allow(unused_imports)]

use std::path::Path;
use std::process::Command;

use super::engine::{run_with_timeout, which_tool};
use super::env::{build_env_v2, build_safe_env, build_sandbox_path, build_ulimit_prefix};
use super::types::{
    NativeSandboxRequest, NativeSandboxResponse, SandboxContextV2, WrapArgvResponseV2,
};

// ─── Linux bubblewrap V1 ─────────────────────────────────────────────────────

#[cfg(target_os = "linux")]
pub(super) fn exec_bwrap(
    req: &NativeSandboxRequest,
) -> Result<NativeSandboxResponse, String> {
    let bwrap_path = req
        .bwrap_path
        .as_deref()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .or_else(|| which_tool("bwrap"))
        .ok_or_else(|| {
            "bwrap not found; install bubblewrap: sudo apt-get install bubblewrap".to_string()
        })?;

    let workdir = req.workdir.as_deref().unwrap_or("/tmp");
    let allowed: Vec<String> = req.allowed_paths.clone().unwrap_or_default();
    let network_block = req.network_block.unwrap_or(true);
    let timeout_ms = req.timeout_ms.unwrap_or(30_000);

    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);

    let mut args: Vec<String> = Vec::new();

    // 命名空间隔离
    args.extend([
        "--unshare-pid".into(),
        "--unshare-uts".into(),
        "--unshare-ipc".into(),
    ]);

    // 网络隔离（对齐默认全禁）
    if network_block {
        args.push("--unshare-net".into());
    }

    // 系统目录只读绑定（保证 bash/python/node 等可访问）
    let ro_system: &[&str] = &[
        "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/etc", "/opt", "/nix",
    ];
    for dir in ro_system {
        if Path::new(dir).exists() {
            args.extend(["--ro-bind-try".into(), dir.to_string(), dir.to_string()]);
        }
    }

    // 用户工具目录只读绑定（cargo/pyenv/nvm/等）
    if let Ok(home) = std::env::var("HOME") {
        let tool_dirs = vec![
            format!("{}/.cargo", home),
            format!("{}/.rustup", home),
            format!("{}/.pyenv", home),
            format!("{}/.nvm", home),
            format!("{}/.local", home),
            format!("{}/go", home),
            format!("{}/.go", home),
            format!("{}/.deno", home),
            format!("{}/.bun", home),
            format!("{}/.asdf", home),
            format!("{}/.rye", home),
            format!("{}/.local/share/mise", home),
            format!("{}/.rbenv", home),
        ];
        for d in &tool_dirs {
            if Path::new(d).exists() {
                args.extend(["--ro-bind-try".into(), d.clone(), d.clone()]);
            }
        }
    }

    // 系统语言运行时特殊路径（conda/snap/flatpak）
    for rt_dir in [
        "/opt/conda",
        "/opt/miniconda3",
        "/opt/anaconda3",
        "/snap",
        "/var/lib/flatpak",
    ] {
        if Path::new(rt_dir).exists() {
            args.extend(["--ro-bind-try".into(), rt_dir.into(), rt_dir.into()]);
        }
    }

    // 必要虚拟文件系统
    args.extend([
        "--proc".into(),
        "/proc".into(),
        "--dev".into(),
        "/dev".into(),
        "--tmpfs".into(),
        "/tmp".into(),
        "--tmpfs".into(),
        "/run".into(),
    ]);

    // Workspace 路径可读写绑定
    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
    for p in &allowed {
        if !seen.contains(p) && Path::new(p).exists() {
            args.extend(["--bind-try".into(), p.clone(), p.clone()]);
            seen.insert(p.clone());
        }
    }
    if !workdir.is_empty() && !seen.contains(workdir) && Path::new(workdir).exists() {
        args.extend([
            "--bind-try".into(),
            workdir.to_string(),
            workdir.to_string(),
        ]);
    }
    if !workdir.is_empty() {
        args.extend(["--chdir".into(), workdir.to_string()]);
    }

    // 环境变量注入（bwrap 默认清空所有 env，必须显式传入）
    for (k, v) in &env_vars {
        args.extend(["--setenv".into(), k.clone(), v.clone()]);
    }

    // 执行命令
    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let full_cmd = format!("{}{}", ulimit_prefix, req.command);
    args.extend(["--".into(), "bash".into(), "-c".into(), full_cmd]);

    let mut cmd = Command::new(&bwrap_path);
    cmd.args(&args);

    run_with_timeout(cmd, timeout_ms, "bwrap", req.max_memory_mb.unwrap_or(0) > 0)
}

// ─── Linux bubblewrap V2 ─────────────────────────────────────────────────────

#[cfg(target_os = "linux")]
pub(super) fn build_bwrap_args_v2(
    ctx: &SandboxContextV2,
    env_vars: &[(String, String)],
    exec_target: &str,
    use_bash_c: bool,
    exec_argv: &[String],
) -> Vec<String> {
    let workdir = ctx.workdir.as_deref().unwrap_or("/tmp");
    let allowed: Vec<String> = ctx.allowed_paths.clone().unwrap_or_default();
    let network_policy = ctx.network_policy.as_deref().unwrap_or("deny");
    let bind_host_tmp = ctx.bind_host_tmp.unwrap_or(false);

    let mut args: Vec<String> = Vec::new();
    args.extend([
        "--unshare-pid".into(),
        "--unshare-uts".into(),
        "--unshare-ipc".into(),
    ]);

    // 网络策略：deny/domain_whitelist 均隔离（Linux bwrap 不支持 DNS 过滤，
    // domain_whitelist 在本层降级为 deny+log，真正白名单需宿主 iptables）
    let net_deny = network_policy != "allow";
    if net_deny {
        args.push("--unshare-net".into());
    }

    for dir in &[
        "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/etc", "/opt", "/nix",
    ] {
        if Path::new(dir).exists() {
            args.extend(["--ro-bind-try".into(), dir.to_string(), dir.to_string()]);
        }
    }

    if let Ok(home) = std::env::var("HOME") {
        for d in &[
            format!("{}/.cargo", home),
            format!("{}/.rustup", home),
            format!("{}/.pyenv", home),
            format!("{}/.nvm", home),
            format!("{}/.local", home),
            format!("{}/go", home),
            format!("{}/.go", home),
            format!("{}/.deno", home),
            format!("{}/.bun", home),
            format!("{}/.asdf", home),
            format!("{}/.rye", home),
            format!("{}/.local/share/mise", home),
            format!("{}/.rbenv", home),
        ] {
            if Path::new(d).exists() {
                args.extend(["--ro-bind-try".into(), d.clone(), d.clone()]);
            }
        }
    }

    for rt in &[
        "/opt/conda",
        "/opt/miniconda3",
        "/opt/anaconda3",
        "/snap",
        "/var/lib/flatpak",
    ] {
        if Path::new(rt).exists() {
            args.extend(["--ro-bind-try".into(), rt.to_string(), rt.to_string()]);
        }
    }

    args.extend([
        "--proc".into(),
        "/proc".into(),
        "--dev".into(),
        "/dev".into(),
    ]);

    if bind_host_tmp {
        // CodeAct：脚本写在 host /tmp，bind 而非 tmpfs，确保沙箱内可见
        if Path::new("/tmp").exists() {
            args.extend(["--bind".into(), "/tmp".into(), "/tmp".into()]);
        }
    } else {
        args.extend(["--tmpfs".into(), "/tmp".into()]);
        // bind_host_tmp=false 但有 script_path：单文件 bind
        if let Some(sp) = &ctx.script_path {
            if Path::new(sp).exists() {
                args.extend(["--ro-bind-try".into(), sp.clone(), sp.clone()]);
            }
        }
    }
    args.extend(["--tmpfs".into(), "/run".into()]);

    let mut seen = std::collections::HashSet::<String>::new();
    for p in &allowed {
        if !seen.contains(p) && Path::new(p).exists() {
            args.extend(["--bind-try".into(), p.clone(), p.clone()]);
            seen.insert(p.clone());
        }
    }
    if !workdir.is_empty() && !seen.contains(workdir) && Path::new(workdir).exists() {
        args.extend([
            "--bind-try".into(),
            workdir.to_string(),
            workdir.to_string(),
        ]);
    }
    if !workdir.is_empty() {
        args.extend(["--chdir".into(), workdir.to_string()]);
    }

    // bwrap 默认清空 env，必须 --setenv 显式注入
    for (k, v) in env_vars {
        args.extend(["--setenv".into(), k.clone(), v.clone()]);
    }

    args.push("--".into());
    if use_bash_c {
        args.extend(["bash".into(), "-c".into(), exec_target.to_string()]);
    } else {
        args.push(exec_target.to_string());
        args.extend_from_slice(exec_argv);
    }
    args
}

#[cfg(target_os = "linux")]
pub(super) fn exec_bwrap_v2(ctx: &SandboxContextV2) -> Result<NativeSandboxResponse, String> {
    let bwrap_path = ctx
        .bwrap_path
        .as_deref()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .or_else(|| which_tool("bwrap"))
        .ok_or_else(|| "bwrap not found; install: sudo apt-get install bubblewrap".to_string())?;

    let caller_type = ctx.caller_type.as_deref().unwrap_or("builtin");
    let timeout_ms = ctx.timeout_ms.unwrap_or(30_000);
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let ulimit_prefix = build_ulimit_prefix(ctx.max_memory_mb);

    let (exec_target, use_bash_c, exec_argv): (String, bool, Vec<String>) =
        if let Some(ep) = &ctx.exec_path {
            (ep.clone(), false, ctx.exec_args.clone().unwrap_or_default())
        } else {
            let cmd = ctx.command.as_deref().unwrap_or("true");
            (format!("{}{}", ulimit_prefix, cmd), true, vec![])
        };

    let args = build_bwrap_args_v2(ctx, &env_vars, &exec_target, use_bash_c, &exec_argv);
    let mut cmd = Command::new(&bwrap_path);
    cmd.args(&args);
    run_with_timeout(cmd, timeout_ms, "bwrap", ctx.max_memory_mb.unwrap_or(0) > 0)
}

#[cfg(target_os = "linux")]
pub(super) fn build_wrap_argv_bwrap(
    ctx: &SandboxContextV2,
) -> Result<WrapArgvResponseV2, String> {
    let bwrap_path = ctx
        .bwrap_path
        .as_deref()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .or_else(|| which_tool("bwrap"))
        .ok_or_else(|| "bwrap not found; install: sudo apt-get install bubblewrap".to_string())?;

    let caller_type = ctx.caller_type.as_deref().unwrap_or("mcp");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );

    let (exec_target, use_bash_c, exec_argv): (String, bool, Vec<String>) =
        if let Some(ep) = &ctx.exec_path {
            (ep.clone(), false, ctx.exec_args.clone().unwrap_or_default())
        } else {
            (
                ctx.command.clone().unwrap_or_else(|| "true".to_string()),
                true,
                vec![],
            )
        };

    let args = build_bwrap_args_v2(ctx, &env_vars, &exec_target, use_bash_c, &exec_argv);
    let net_isolated = ctx.network_policy.as_deref().unwrap_or("deny") != "allow";

    Ok(WrapArgvResponseV2 {
        executable: bwrap_path,
        argv: args,
        env: vec![], // bwrap 通过 --setenv 在 argv 内注入，env_in_argv=true
        env_in_argv: true,
        sandbox_method: "bwrap".to_string(),
        net_isolated,
    })
}
