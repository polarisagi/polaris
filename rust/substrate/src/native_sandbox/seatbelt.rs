// native_sandbox/seatbelt.rs — macOS Apple Seatbelt 沙箱（V1 + V2）
//
// V1: build_seatbelt_profile / sbpl_escape / exec_seatbelt
// V2: build_seatbelt_profile_v2 / exec_seatbelt_v2 / build_wrap_argv_seatbelt
//
// 设计依据: ADR-0008-sandbox-three-tier-fallback.md §macOS

use std::env;
use std::path::Path;
use std::process::Command;

use super::engine::{run_with_timeout, shell_quote_value, which_tool};
use super::env::{build_env_v2, build_safe_env, build_sandbox_path, build_ulimit_prefix};
use super::types::{
    NativeSandboxRequest, NativeSandboxResponse, SandboxContextV2, WrapArgvResponseV2,
};

// ─── macOS Seatbelt V1 ────────────────────────────────────────────────────────

/// 构建 SBPL（Apple Sandbox Profile Language）策略。
/// 默认拒绝，白名单开放：系统只读 + AllowedPaths 读写 + /tmp 读写。
#[cfg(target_os = "macos")]
pub(super) fn build_seatbelt_profile(
    allowed_paths: &[String],
    workdir: &str,
    network_block: bool,
    sandbox_path: &str,
) -> String {
    let mut profile = String::from(
        r#"(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target self))
(allow sysctl-read)
(allow ipc-posix*)
(allow mach-lookup)
(allow mach-register)
; 继承的 fd（stdout/stderr 管道）写入——fd 级操作不受 subpath 约束。
; 没有这两行，deny default 下任何 sandboxed 命令都无法输出（SIGABRT exit 134）。
(allow file-write-data)
(allow file-ioctl)
; 系统目录只读（编译器/解释器/标准库/Homebrew）
(allow file-read*
  (subpath "/usr")
  (subpath "/bin")
  (subpath "/sbin")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/private/etc")
  (subpath "/private/var/db")
  (subpath "/opt")
  (subpath "/nix")
  (subpath "/Applications")
)
(allow file-read-metadata)
; /tmp 可读写（编译器缓存/临时文件）
(allow file* (subpath "/tmp"))
(allow file* (subpath "/private/tmp"))
(allow file* (subpath "/var/folders"))
(allow file* (subpath "/private/var/folders"))
"#,
    );

    // Workspace 路径可读写
    for p in allowed_paths {
        profile.push_str(&format!("(allow file* (subpath {}))\n", sbpl_escape(p)));
    }
    if !workdir.is_empty() && !allowed_paths.contains(&workdir.to_string()) {
        profile.push_str(&format!(
            "(allow file* (subpath {}))\n",
            sbpl_escape(workdir)
        ));
    }

    // 用户 HOME 工具目录只读（cargo/pyenv/etc.）
    if let Ok(home) = env::var("HOME") {
        let tool_dirs = [
            format!("{}/.cargo", home),
            format!("{}/.pyenv", home),
            format!("{}/.nvm", home),
            format!("{}/.local", home),
            format!("{}/go", home),
            format!("{}/.deno", home),
            format!("{}/.bun", home),
            format!("{}/.asdf", home),
            format!("{}/.rye", home),
            format!("{}/.local/share/mise", home),
        ];
        for d in &tool_dirs {
            if Path::new(d).exists() {
                profile.push_str(&format!(
                    "(allow file-read* (subpath {}))\n",
                    sbpl_escape(d)
                ));
            }
        }
    }

    // 网络策略
    if network_block {
        profile.push_str(
            "; 禁止所有出站网络（对齐 Claude Code / Codex CLI 默认行为）\n(deny network*)\n",
        );
    } else {
        profile.push_str("; 允许所有出站网络\n(allow network*)\n");
    }

    let _ = sandbox_path; // V1 PATH 由 env_prefix 注入，不内嵌于 profile
    profile
}

/// SBPL 路径字符串转义（仅处理双引号和反斜杠）
#[cfg(target_os = "macos")]
pub(super) fn sbpl_escape(path: &str) -> String {
    let escaped = path.replace('\\', "\\\\").replace('"', "\\\"");
    format!("\"{}\"", escaped)
}

#[cfg(target_os = "macos")]
pub(super) fn exec_seatbelt(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    let sandbox_exec = which_tool("sandbox-exec")
        .ok_or_else(|| "sandbox-exec not found; macOS version too old?".to_string())?;

    let workdir = req.workdir.as_deref().unwrap_or("/tmp");
    let allowed: Vec<String> = req.allowed_paths.clone().unwrap_or_default();
    let network_block = req.network_block.unwrap_or(true);
    let timeout_ms = req.timeout_ms.unwrap_or(30_000);

    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);

    let profile = build_seatbelt_profile(&allowed, workdir, network_block, &sandbox_path);

    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let env_prefix: String = env_vars
        .iter()
        .map(|(k, v)| format!("export {}={};", k, shell_quote_value(v)))
        .collect::<Vec<_>>()
        .join(" ");

    let full_command = format!("{}{}{}", ulimit_prefix, env_prefix, req.command);

    let mut cmd = Command::new(&sandbox_exec);
    cmd.args(["-p", &profile, "bash", "-c", &full_command]);
    cmd.current_dir(workdir);
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }

    run_with_timeout(
        cmd,
        timeout_ms,
        "seatbelt",
        req.max_memory_mb.unwrap_or(0) > 0,
    )
}

// ─── macOS Seatbelt V2 ────────────────────────────────────────────────────────

#[cfg(target_os = "macos")]
pub(super) fn build_seatbelt_profile_v2(
    allowed_paths: &[String],
    workdir: &str,
    network_policy: &str,
    network_domains: &[String],
    script_path: Option<&str>,
) -> String {
    let mut p = String::from(
        r#"(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target self))
(allow sysctl-read)
(allow ipc-posix*)
(allow mach-lookup)
(allow mach-register)
; 继承的 fd（stdout/stderr 管道）写入——fd 级操作不受 subpath 约束。
(allow file-write-data)
(allow file-ioctl)
(allow file-read*
  (subpath "/usr") (subpath "/bin") (subpath "/sbin")
  (subpath "/System") (subpath "/Library")
  (subpath "/private/etc") (subpath "/private/var/db")
  (subpath "/opt") (subpath "/nix") (subpath "/Applications")
)
(allow file-read-metadata)
(allow file* (subpath "/tmp"))
(allow file* (subpath "/private/tmp"))
(allow file* (subpath "/var/folders"))
(allow file* (subpath "/private/var/folders"))
"#,
    );

    for path in allowed_paths {
        p.push_str(&format!("(allow file* (subpath {}))\n", sbpl_escape(path)));
    }
    if !workdir.is_empty() && !allowed_paths.contains(&workdir.to_string()) {
        p.push_str(&format!(
            "(allow file* (subpath {}))\n",
            sbpl_escape(workdir)
        ));
    }
    if let Some(sp) = script_path {
        p.push_str(&format!(
            "(allow file-read* (literal {}))\n",
            sbpl_escape(sp)
        ));
    }

    if let Ok(home) = env::var("HOME") {
        for d in &[
            format!("{}/.cargo", home),
            format!("{}/.pyenv", home),
            format!("{}/.nvm", home),
            format!("{}/.local", home),
            format!("{}/go", home),
            format!("{}/.deno", home),
            format!("{}/.bun", home),
            format!("{}/.asdf", home),
            format!("{}/.rye", home),
            format!("{}/.local/share/mise", home),
        ] {
            if Path::new(d).exists() {
                p.push_str(&format!(
                    "(allow file-read* (subpath {}))\n",
                    sbpl_escape(d)
                ));
            }
        }
    }

    match network_policy {
        "allow" => {
            p.push_str("(allow network*)\n");
        }
        "domain_whitelist" if !network_domains.is_empty() => {
            // SBPL 仅支持 IP 级过滤；域名白名单以 allow + DNS 规则近似实现
            // 真正的 DNS 级过滤需在宿主防火墙层实现（pf rules）
            p.push_str("; domain_whitelist（SBPL 近似：允许 DNS + 白名单端口）\n");
            p.push_str("(allow network-outbound (remote port 53))\n");
            p.push_str("(allow network-inbound (local port 53))\n");
            for domain in network_domains {
                p.push_str(&format!(
                    "(allow network-outbound (remote host {:?} port 443))\n(allow network-outbound (remote host {:?} port 80))\n",
                    domain, domain
                ));
            }
        }
        _ => {
            p.push_str("(deny network*)\n");
        }
    }
    p
}

#[cfg(target_os = "macos")]
pub(super) fn exec_seatbelt_v2(ctx: &SandboxContextV2) -> Result<NativeSandboxResponse, String> {
    let sandbox_exec =
        which_tool("sandbox-exec").ok_or_else(|| "sandbox-exec not found".to_string())?;
    let workdir = ctx.workdir.as_deref().unwrap_or("/tmp");
    let allowed: Vec<String> = ctx.allowed_paths.clone().unwrap_or_default();
    let network_policy = ctx.network_policy.as_deref().unwrap_or("deny");
    let network_domains = ctx.network_domains.clone().unwrap_or_default();
    let timeout_ms = ctx.timeout_ms.unwrap_or(30_000);
    let caller_type = ctx.caller_type.as_deref().unwrap_or("builtin");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let profile = build_seatbelt_profile_v2(
        &allowed,
        workdir,
        network_policy,
        &network_domains,
        ctx.script_path.as_deref(),
    );
    let ulimit_prefix = build_ulimit_prefix(ctx.max_memory_mb);

    let mut cmd = Command::new(&sandbox_exec);
    cmd.env_clear();
    for (k, v) in &env_vars {
        cmd.env(k, v);
    }
    cmd.current_dir(workdir);

    if let Some(exec_path) = &ctx.exec_path {
        let mut args = vec!["-p".to_string(), profile, exec_path.clone()];
        args.extend(ctx.exec_args.clone().unwrap_or_default());
        cmd.args(&args);
    } else {
        let command = ctx.command.as_deref().unwrap_or("true");
        let full_cmd = format!("{}{}", ulimit_prefix, command);
        cmd.args(["-p", &profile, "bash", "-c", &full_cmd]);
    }

    run_with_timeout(
        cmd,
        timeout_ms,
        "seatbelt",
        ctx.max_memory_mb.unwrap_or(0) > 0,
    )
}

#[cfg(target_os = "macos")]
pub(super) fn build_wrap_argv_seatbelt(
    ctx: &SandboxContextV2,
) -> Result<WrapArgvResponseV2, String> {
    let sandbox_exec =
        which_tool("sandbox-exec").ok_or_else(|| "sandbox-exec not found".to_string())?;
    let workdir = ctx.workdir.as_deref().unwrap_or("/tmp");
    let allowed: Vec<String> = ctx.allowed_paths.clone().unwrap_or_default();
    let network_policy = ctx.network_policy.as_deref().unwrap_or("deny");
    let network_domains = ctx.network_domains.clone().unwrap_or_default();
    let caller_type = ctx.caller_type.as_deref().unwrap_or("mcp");
    let sandbox_path = build_sandbox_path();
    let env_vars = build_env_v2(
        caller_type,
        ctx.env_preset.as_deref(),
        ctx.env_extra.as_deref().unwrap_or(&[]),
        &sandbox_path,
    );
    let profile = build_seatbelt_profile_v2(
        &allowed,
        workdir,
        network_policy,
        &network_domains,
        ctx.script_path.as_deref(),
    );

    let mut argv = vec!["-p".to_string(), profile];
    if let Some(exec_path) = &ctx.exec_path {
        argv.push(exec_path.clone());
        argv.extend(ctx.exec_args.clone().unwrap_or_default());
    } else {
        let command = ctx.command.as_deref().unwrap_or("true");
        argv.extend(["bash".into(), "-c".into(), command.to_string()]);
    }

    let env_list: Vec<String> = env_vars
        .iter()
        .map(|(k, v)| format!("{}={}", k, v))
        .collect();
    Ok(WrapArgvResponseV2 {
        executable: sandbox_exec,
        argv,
        env: env_list,
        env_in_argv: false, // seatbelt 通过 cmd.Env() 注入
        sandbox_method: "seatbelt".to_string(),
        net_isolated: matches!(network_policy, "deny"),
    })
}

// ─── 单元测试 ─────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    #[cfg(target_os = "macos")]
    use super::*;

    #[test]
    #[cfg(target_os = "macos")]
    fn test_seatbelt_profile_contains_network_deny() {
        let profile = build_seatbelt_profile(&[], "/tmp", true, "/usr/bin");
        assert!(
            profile.contains("deny network*"),
            "should deny network when block=true"
        );
    }

    #[test]
    #[cfg(target_os = "macos")]
    fn test_seatbelt_profile_network_allow() {
        let profile = build_seatbelt_profile(&[], "/tmp", false, "/usr/bin");
        assert!(
            profile.contains("allow network*"),
            "should allow network when block=false"
        );
    }
}
