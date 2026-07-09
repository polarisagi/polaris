// native_sandbox/seatbelt.rs — macOS Apple Seatbelt 沙箱（V1 + V2）
//
// V1: build_seatbelt_profile / sbpl_escape / exec_seatbelt
// V2: build_seatbelt_profile_v2 / exec_seatbelt_v2 / build_wrap_argv_seatbelt
//
// 设计依据: ADR-0008-sandbox-three-tier-fallback.md §macOS

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
; 写数据仅放行"非普通文件"：继承的 stdout/stderr 管道、tty、/dev/null 等 fd 级写入
; （deny default 下缺此规则任何命令都无法输出，SIGABRT exit 134）。普通文件的写入
; 一律由下方 subpath 白名单显式授予，杜绝全局 (allow file-write-data) 覆写宿主任意现有
; 文件（~/.zshrc、~/.ssh/authorized_keys 等）的持久化逃逸面（HE-2 可验证边界）。
(allow file-write-data (require-not (vnode-type REGULAR-FILE)))
(allow file-ioctl)
; 读取全局放行：本沙箱威胁模型为写隔离 + 网络隔离 + 凭据剥离，非读机密性；且按 subpath
; 枚举读路径在 macOS 15+（Tahoe/26）因 dyld 共享缓存迁移到 Cryptexes/Preboot 卷而无法
; 覆盖，导致解释器/编译器加载即 SIGABRT。全局只读与 Linux bwrap 侧只读绑定系统树 + HOME
; 工具目录的姿态一致，出站数据已由 (deny network*) 兜底。
(allow file-read*)
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

    run_with_timeout(cmd, timeout_ms, "seatbelt", req.max_memory_mb.unwrap_or(0))
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
    // 语义与 build_seatbelt_profile（V1）一致：写数据仅放行非普通文件（管道/tty），
    // 普通文件写入走 subpath 白名单；读取全局放行（macOS 15+ dyld 缓存迁移使 subpath
    // 枚举失效，且本沙箱不以读机密性为边界）。详见 V1 注释。
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
(allow file-write-data (require-not (vnode-type REGULAR-FILE)))
(allow file-ioctl)
(allow file-read*)
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
    // script_path 读权限已被上方全局 file-read* 覆盖，无需单独 literal 授权。
    let _ = script_path;

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

    run_with_timeout(cmd, timeout_ms, "seatbelt", ctx.max_memory_mb.unwrap_or(0))
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

    // S1 回归：写权限必须收敛到非普通文件，禁止再出现无过滤的全局 file-write-data，
    // 否则沙箱内可覆写宿主任意现有文件（持久化逃逸）。
    #[test]
    #[cfg(target_os = "macos")]
    fn test_seatbelt_write_scoped_not_global() {
        for profile in [
            build_seatbelt_profile(&[], "/tmp", true, "/usr/bin"),
            build_seatbelt_profile_v2(&[], "/tmp", "deny", &[], None),
        ] {
            assert!(
                profile.contains("(allow file-write-data (require-not (vnode-type REGULAR-FILE)))"),
                "write-data must be scoped to non-regular files"
            );
            assert!(
                !profile.contains("(allow file-write-data)\n"),
                "must not contain unscoped global file-write-data"
            );
            assert!(
                profile.contains("(allow file-read*)"),
                "read is globally allowed (dyld cache on Cryptexes volume)"
            );
        }
    }
}
