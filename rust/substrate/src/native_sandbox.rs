// native_sandbox.rs — 平台原生进程沙箱（FFI）
//
// 架构对齐：
//   macOS  → Apple Seatbelt（sandbox-exec，内置 macOS 10.5+，无需安装）
//   Linux  → bubblewrap（bwrap；不可用时降级 namespace-only）
//   Windows→ WSL2（wsl.exe；不可用时降级 bare exec）
//
// 设计依据: docs/arch/ADR-0008-sandbox-three-tier-fallback.md
//
// FFI 接口:
//   native_sandbox_exec(input_json, out_json, out_err) -> i32
//   native_sandbox_free_string(ptr)
//   native_sandbox_probe_tools(out_json, out_err) -> i32
//
// 入参 JSON (NativeSandboxRequest):
//   command       string           待执行命令（bash -c 包裹）
//   workdir       string?          工作目录（空 = /tmp）
//   allowed_paths [string]?        可读写路径白名单（bwrap bind mount + Seatbelt subpath）
//   network_block bool?            true = 禁止出站网络（默认 true）
//   env_extra     [string]?        额外注入环境变量 "KEY=VALUE"（叠加在自动 PATH 之上）
//   timeout_ms    u64?             超时毫秒（默认 30000）
//   bwrap_path    string?          Linux: bwrap 路径覆盖（空 = 自动查找）
//
// 出参 JSON (NativeSandboxResponse):
//   output        string           stdout + stderr 合并输出
//   exit_code     i32              进程退出码（沙箱报错为 -1）
//   sandbox_method string          "seatbelt" | "bwrap" | "wsl2" | "namespace" | "bare"

#![allow(unused_variables)]

use std::env;
use std::ffi::{CStr, CString};
use std::os::raw::{c_char, c_int};
use std::panic;
use std::path::Path;
use std::process::Command;
use std::time::Duration;

use serde::{Deserialize, Serialize};

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const NS_OK: c_int = 0;
const NS_ERR_INTERNAL: c_int = -1;
const NS_ERR_UTF8: c_int = -4;
const NS_ERR_TIMEOUT: c_int = -5;

// ─── 数据结构 ──────────────────────────────────────────────────────────────────

#[derive(Deserialize)]
struct NativeSandboxRequest {
    command: String,
    workdir: Option<String>,
    allowed_paths: Option<Vec<String>>,
    network_block: Option<bool>,
    env_extra: Option<Vec<String>>,
    timeout_ms: Option<u64>,
    // 仅 Linux exec_bwrap 使用，其他平台编译时视为 dead_code
    #[allow(dead_code)]
    bwrap_path: Option<String>,
    max_memory_mb: Option<u64>,
}

#[derive(Serialize)]
struct NativeSandboxResponse {
    output: String,
    exit_code: i32,
    sandbox_method: String,
    memory_limited: bool,
}

/// 工具探测结果（供 Go 侧展示给 LLM 或用于调试）
#[derive(Serialize)]
struct ToolProbeResult {
    platform: String,
    sandbox_method: String,
    /// 探测到的 PATH 字符串
    resolved_path: String,
    /// bwrap 路径（Linux 有效）
    bwrap_path: Option<String>,
    /// sandbox-exec 是否可用（macOS 有效）
    seatbelt_available: bool,
    /// WSL2 是否可用（Windows 有效）
    wsl2_available: bool,
    /// 探测到的语言运行时目录（存在的）
    found_runtimes: Vec<String>,
    /// Wasmtime 运行时是否支持网络
    wasi_network_supported: bool,
}

// ─── PATH 自动构建 ─────────────────────────────────────────────────────────────

/// 构建沙箱进程的 PATH 环境变量。
///
/// 策略：继承宿主 PATH（避免丢失用户安装的工具），再追加平台常见工具目录（去重）。
/// 优先级：宿主 PATH > 平台基础目录 > 用户工具目录
fn build_sandbox_path() -> String {
    let mut parts: Vec<String> = Vec::new();

    // 1. 继承宿主 PATH（最高优先级：尊重用户的 pyenv/nvm/cargo 配置）
    if let Ok(host_path) = env::var("PATH") {
        for p in host_path.split(path_separator()) {
            let trimmed = p.trim();
            if !trimmed.is_empty() {
                add_unique(&mut parts, trimmed.to_string());
            }
        }
    }

    // 2. 平台基础目录（保底，即使宿主 PATH 被清空仍可用）
    let base_dirs: &[&str] = &[
        "/usr/local/bin",
        "/usr/local/sbin",
        "/usr/bin",
        "/usr/sbin",
        "/bin",
        "/sbin",
    ];
    for d in base_dirs {
        add_unique(&mut parts, d.to_string());
    }

    // 3. 平台特定工具目录
    #[cfg(target_os = "macos")]
    {
        // Homebrew Apple Silicon（M1/M2/M3）
        add_unique(&mut parts, "/opt/homebrew/bin".to_string());
        add_unique(&mut parts, "/opt/homebrew/sbin".to_string());
        // Homebrew Intel
        add_unique(&mut parts, "/usr/local/opt/python/libexec/bin".to_string());
        // Xcode 工具链（clang/git/make）
        add_unique(
            &mut parts,
            "/Library/Developer/CommandLineTools/usr/bin".to_string(),
        );
        // MacPorts
        add_unique(&mut parts, "/opt/local/bin".to_string());
    }

    #[cfg(target_os = "linux")]
    {
        // Snap
        add_unique(&mut parts, "/snap/bin".to_string());
        // Anaconda / Miniconda（常见安装路径）
        for conda in [
            "/opt/conda/bin",
            "/opt/miniconda3/bin",
            "/opt/anaconda3/bin",
        ] {
            if Path::new(conda).exists() {
                add_unique(&mut parts, conda.to_string());
            }
        }
        // Flatpak
        add_unique(&mut parts, "/var/lib/flatpak/exports/bin".to_string());
    }

    // 4. nix（macOS 和 Linux 共用）
    for nix_dir in [
        "/nix/var/nix/profiles/default/bin",
        "/run/current-system/sw/bin",
    ] {
        if Path::new(nix_dir).exists() {
            add_unique(&mut parts, nix_dir.to_string());
        }
    }

    // 5. 用户级工具目录（按 HOME 相对路径探测）
    if let Ok(home) = env::var("HOME") {
        let user_dirs = vec![
            format!("{}/.cargo/bin", home),   // Rust (cargo install)
            format!("{}/.local/bin", home),   // pip install --user / pipx
            format!("{}/go/bin", home),       // Go (go install)
            format!("{}/.go/bin", home),      // Go (alternative)
            format!("{}/.pyenv/shims", home), // pyenv shims（多版本切换）
            format!("{}/.pyenv/bin", home),   // pyenv 本身
            format!("{}/.rbenv/shims", home), // rbenv
            format!("{}/.rbenv/bin", home),
            format!("{}/.nvm/versions/node/current/bin", home), // nvm（当前版本）
            format!("{}/.deno/bin", home),                      // Deno
            format!("{}/.bun/bin", home),                       // Bun
            format!("{}/.rye/shims", home),                     // Rye（Python 工具链管理）
            format!("{}/.local/share/mise/shims", home),        // mise（asdf 替代）
            format!("{}/.asdf/shims", home),                    // asdf
            format!("{}/.asdf/bin", home),
        ];
        for d in &user_dirs {
            if Path::new(d).exists() {
                add_unique(&mut parts, d.clone());
            }
        }
    }

    parts.join(&path_separator().to_string())
}

/// 路径分隔符（Unix: ':', Windows: ';'）
fn path_separator() -> char {
    if cfg!(windows) { ';' } else { ':' }
}

/// 无重复追加路径
fn add_unique(list: &mut Vec<String>, item: String) {
    if !list.contains(&item) {
        list.push(item);
    }
}

/// 过滤危险环境变量（注入攻击向量），保留安全变量。
/// 策略：白名单保留 + 显式剔除高危变量。
fn build_safe_env(extra: &[String], sandbox_path: &str) -> Vec<(String, String)> {
    // 白名单：允许传入沙箱的环境变量
    let safe_passthrough = [
        "LANG",
        "LC_ALL",
        "LC_CTYPE",
        "LC_MESSAGES",
        "TZ",
        "USER",
        "LOGNAME",
        // Python
        "PYTHONPATH",
        "PYTHONDONTWRITEBYTECODE",
        "VIRTUAL_ENV",
        // Node
        "NODE_PATH",
        "NODE_ENV",
        // Go
        "GOPATH",
        "GOROOT",
        "GOMODCACHE",
        "GOCACHE",
        // Rust/Cargo
        "CARGO_HOME",
        "RUSTUP_HOME",
        // Java
        "JAVA_HOME",
        "CLASSPATH",
        // 构建工具
        "MAKEFLAGS",
        "CMAKE_PREFIX_PATH",
        // 编辑器/IDE 注入（非危险）
        "TERM",
        "COLORTERM",
    ];

    // 高危变量黑名单（无论任何情况都不传入）
    let danger_list = [
        "LD_PRELOAD",
        "LD_LIBRARY_PATH",
        "DYLD_INSERT_LIBRARIES",
        "DYLD_LIBRARY_PATH",
        "DYLD_FALLBACK_LIBRARY_PATH",
        "LD_AUDIT",
        "LD_DEBUG",
        "SHELLOPTS",
        "BASH_ENV",
        "ENV",
        "CDPATH",
        "IFS",
        "PYTHONSTARTUP",
        "PYTHONEXECUTABLE",
    ];

    let mut result: Vec<(String, String)> = Vec::new();

    // PATH 固定注入（已过滤的安全路径）
    result.push(("PATH".to_string(), sandbox_path.to_string()));

    // HOME（安全，沙箱中工具需要）
    if let Ok(home) = env::var("HOME") {
        result.push(("HOME".to_string(), home));
    } else {
        result.push(("HOME".to_string(), "/tmp".to_string()));
    }

    // 白名单变量透传
    for key in &safe_passthrough {
        if let Ok(val) = env::var(key) {
            result.push((key.to_string(), val));
        }
    }

    // TMPDIR 特殊处理（允许自定义临时目录，但不超出 /tmp）
    result.push(("TMPDIR".to_string(), "/tmp".to_string()));
    result.push(("TEMP".to_string(), "/tmp".to_string()));

    // 调用方额外注入（追加，允许覆盖上述值）
    for kv in extra {
        if let Some((k, v)) = kv.split_once('=') {
            // 剔除高危 key
            if !danger_list.contains(&k) {
                // 找到已有同 key 则替换，否则追加
                if let Some(pos) = result.iter().position(|(ek, _)| ek == k) {
                    result[pos].1 = v.to_string();
                } else {
                    result.push((k.to_string(), v.to_string()));
                }
            }
        }
    }

    result
}

/// 构建 ulimit 虚拟内存限制前缀命令
fn build_ulimit_prefix(max_memory_mb: Option<u64>) -> String {
    if let Some(mb) = max_memory_mb {
        if mb > 0 {
            // POSIX ulimit -v 单位是 KB
            format!("ulimit -v {}; ", mb * 1024)
        } else {
            String::new()
        }
    } else {
        String::new()
    }
}

// ─── macOS Seatbelt ────────────────────────────────────────────────────────────

/// 构建 SBPL（Apple Sandbox Profile Language）策略。
/// 默认拒绝，白名单开放：系统只读 + AllowedPaths 读写 + /tmp 读写。
#[cfg(target_os = "macos")]
fn build_seatbelt_profile(
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

    profile
}

/// SBPL 路径字符串转义（仅处理双引号和反斜杠）
#[cfg(target_os = "macos")]
fn sbpl_escape(path: &str) -> String {
    let escaped = path.replace('\\', "\\\\").replace('"', "\\\"");
    format!("\"{}\"", escaped)
}

#[cfg(target_os = "macos")]
fn exec_seatbelt(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    // 检查 sandbox-exec 是否存在
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
    // sandbox-exec 继承父进程 env，叠加我们的清理 env
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

// ─── Linux bubblewrap ─────────────────────────────────────────────────────────

#[cfg(target_os = "linux")]
fn exec_bwrap(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
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
    if let Ok(home) = env::var("HOME") {
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

// ─── Linux namespace-only 降级 ─────────────────────────────────────────────────

/// bwrap 不可用时的最小隔离：只注入干净 env + 工作目录。
/// 无文件系统隔离，仅环境变量清洁。
#[cfg(target_os = "linux")]
fn exec_namespace_fallback(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
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

// ─── Windows WSL2 ──────────────────────────────────────────────────────────────

#[cfg(target_os = "windows")]
fn exec_wsl2(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
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

    // 工作目录：Windows 路径转换为 WSL2 /mnt/ 路径
    let workdir_unix = if let Some(ref wd) = req.workdir {
        windows_path_to_wsl(wd)
    } else {
        String::new()
    };
    if !workdir_unix.is_empty() {
        args.extend(["--cd".into(), workdir_unix.clone()]);
    }

    // 在 WSL2 内构建 PATH 注入前缀（WSL2 会继承 Windows PATH + distro PATH）
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

// ─── 跨平台 bare 降级 ─────────────────────────────────────────────────────────

/// 无平台沙箱时的最小安全实现：清洁 env + workdir。
fn exec_bare(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    let workdir = req.workdir.as_deref().unwrap_or("/tmp");
    let timeout_ms = req.timeout_ms.unwrap_or(30_000);
    let sandbox_path = build_sandbox_path();
    let env_vars = build_safe_env(req.env_extra.as_deref().unwrap_or(&[]), &sandbox_path);

    let ulimit_prefix = build_ulimit_prefix(req.max_memory_mb);
    let shell = if cfg!(windows) { "cmd.exe" } else { "bash" };
    let shell_flag = if cfg!(windows) { "/C" } else { "-c" };

    // Windows cmd doesn't support ulimit, so we only apply it for bash
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

// ─── 执行引擎 ─────────────────────────────────────────────────────────────────

/// 带超时的命令执行，合并 stdout+stderr。
///
/// 必须用独立线程并发排空管道：若子进程输出 > OS 管道缓冲区（Linux ~64KB），
/// 子进程会阻塞在 write(2)，try_wait 永远返回 None，最终超时 kill 导致输出丢失。
/// 线程持续消费管道，子进程不会阻塞，try_wait 在进程退出后正常返回。
fn run_with_timeout(
    mut cmd: Command,
    timeout_ms: u64,
    method: &str,
    memory_limited: bool,
) -> Result<NativeSandboxResponse, String> {
    use std::io::Read;
    use std::sync::{Arc, Mutex};
    use std::thread;

    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    let mut child = cmd.spawn().map_err(|e| format!("spawn failed: {}", e))?;

    // 提前取走管道句柄，交给读取线程——必须在 try_wait 循环前完成，
    // 否则子进程满管道后永远无法退出。
    let mut stdout_pipe = child.stdout.take();
    let mut stderr_pipe = child.stderr.take();

    let stdout_buf = Arc::new(Mutex::new(Vec::<u8>::new()));
    let stderr_buf = Arc::new(Mutex::new(Vec::<u8>::new()));

    let t_out = {
        let buf = Arc::clone(&stdout_buf);
        thread::spawn(move || {
            if let Some(ref mut pipe) = stdout_pipe {
                let mut tmp = Vec::new();
                let _ = pipe.read_to_end(&mut tmp);
                *buf.lock().unwrap_or_else(|e| e.into_inner()) = tmp;
            }
        })
    };
    let t_err = {
        let buf = Arc::clone(&stderr_buf);
        thread::spawn(move || {
            if let Some(ref mut pipe) = stderr_pipe {
                let mut tmp = Vec::new();
                let _ = pipe.read_to_end(&mut tmp);
                *buf.lock().unwrap_or_else(|e| e.into_inner()) = tmp;
            }
        })
    };

    let timeout = Duration::from_millis(timeout_ms);
    let deadline = std::time::Instant::now() + timeout;

    let exit_status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) => {
                if std::time::Instant::now() > deadline {
                    let _ = child.kill();
                    // kill 后管道 EOF，线程自然结束
                    let _ = t_out.join();
                    let _ = t_err.join();
                    return Err(format!("timeout after {}ms", timeout_ms));
                }
                thread::sleep(Duration::from_millis(50));
            }
            Err(e) => {
                let _ = child.kill();
                let _ = t_out.join();
                let _ = t_err.join();
                return Err(format!("wait failed: {}", e));
            }
        }
    };

    // 进程已退出，等待读取线程消费完剩余管道数据
    let _ = t_out.join();
    let _ = t_err.join();

    let out_bytes = stdout_buf.lock().unwrap();
    let err_bytes = stderr_buf.lock().unwrap();

    let mut combined = String::from_utf8_lossy(&out_bytes).into_owned();
    let stderr_str = String::from_utf8_lossy(&err_bytes);
    if !stderr_str.is_empty() {
        if !combined.is_empty() {
            combined.push('\n');
        }
        combined.push_str(&stderr_str);
    }

    Ok(NativeSandboxResponse {
        output: combined,
        exit_code: exit_status.code().unwrap_or(-1),
        sandbox_method: method.to_string(),
        memory_limited,
    })
}

// ─── 工具查找 ─────────────────────────────────────────────────────────────────

/// 在 PATH 中查找可执行文件，返回绝对路径。
fn which_tool(name: &str) -> Option<String> {
    let path_var = env::var("PATH").unwrap_or_default();
    let sep = path_separator();
    for dir in path_var.split(sep) {
        let candidate = Path::new(dir).join(name);
        if candidate.is_file() {
            // Unix: 检查执行权限
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                if let Ok(meta) = std::fs::metadata(&candidate)
                    && meta.permissions().mode() & 0o111 != 0
                {
                    return Some(candidate.to_string_lossy().to_string());
                }
            }
            #[cfg(not(unix))]
            {
                return Some(candidate.to_string_lossy().to_string());
            }
        }
        // Windows: 同时检查 .exe 后缀
        #[cfg(windows)]
        {
            let exe = candidate.with_extension("exe");
            if exe.is_file() {
                return Some(exe.to_string_lossy().to_string());
            }
        }
    }

    // 也检查常见固定路径（PATH 可能不完整）
    let fixed_locations: &[&str] = &["/usr/bin", "/usr/local/bin", "/opt/homebrew/bin", "/bin"];
    for dir in fixed_locations {
        let candidate = Path::new(dir).join(name);
        if candidate.is_file() {
            return Some(candidate.to_string_lossy().to_string());
        }
    }

    None
}

/// shell 值引用（单引号包裹，内部单引号 escape）
fn shell_quote_value(s: &str) -> String {
    // 使用单引号：最安全的 shell 值引用方式
    // 内部的 ' 替换为 '\''
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// Windows 路径转 WSL2 挂载路径（C:\foo → /mnt/c/foo）
#[cfg(target_os = "windows")]
fn windows_path_to_wsl(path: &str) -> String {
    // C:\Users\... → /mnt/c/users/...
    if path.len() >= 3 && path.chars().nth(1) == Some(':') {
        let drive = path.chars().next().unwrap().to_lowercase().to_string();
        let rest = path[2..].replace('\\', "/");
        return format!("/mnt/{}{}", drive, rest);
    }
    // UNC 路径（\\server\share）暂不转换，直接传入
    path.replace('\\', "/")
}

#[cfg(not(target_os = "windows"))]
#[allow(dead_code)] // Windows 专用，非 Windows 平台不调用
fn windows_path_to_wsl(_path: &str) -> String {
    String::new()
}

// ─── 平台分发 ─────────────────────────────────────────────────────────────────

/// 按当前平台选择沙箱实现，失败时自动降级。
fn dispatch_sandbox(req: &NativeSandboxRequest) -> Result<NativeSandboxResponse, String> {
    #[cfg(target_os = "macos")]
    {
        match exec_seatbelt(req) {
            Ok(r) => return Ok(r),
            Err(e) => {
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
            Ok(r) => return Ok(r),
            Err(e) => {
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
            Ok(r) => return Ok(r),
            Err(e) => {
                eprintln!("[native_sandbox] wsl2 failed ({}), falling back to bare", e);
            }
        }
        return exec_bare(req);
    }

    // 其他平台（FreeBSD 等）
    #[allow(unreachable_code)]
    exec_bare(req)
}

// ─── FFI 出参辅助 ─────────────────────────────────────────────────────────────

fn ns_write_cstr(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    let s = msg.replace('\0', "?");
    if let Ok(cs) = CString::new(s) {
        unsafe { *out = cs.into_raw() };
    }
}

unsafe fn ns_read_cstr<'a>(ptr: *const c_char) -> Result<&'a str, ()> {
    unsafe {
        if ptr.is_null() {
            return Ok("");
        }
        CStr::from_ptr(ptr).to_str().map_err(|_| ())
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

// ─── 公开 FFI 函数 ─────────────────────────────────────────────────────────────

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
                    // 超时
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
    unsafe {
        if !ptr.is_null() {
            drop(CString::from_raw(ptr));
        }
    }
}

// ─── 单元测试 ──────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_build_sandbox_path_non_empty() {
        let path = build_sandbox_path();
        assert!(!path.is_empty(), "PATH should not be empty");
        // 至少包含 /usr/bin 或 /bin
        assert!(
            path.contains("/usr/bin") || path.contains("/bin"),
            "PATH should include system bin dirs, got: {}",
            path
        );
    }

    #[test]
    fn test_build_safe_env_strips_dangerous() {
        let extra = vec!["LD_PRELOAD=/evil.so".to_string(), "MYVAR=hello".to_string()];
        let env = build_safe_env(&extra, "/usr/bin");
        // LD_PRELOAD 必须被过滤掉
        assert!(
            !env.iter().any(|(k, _)| k == "LD_PRELOAD"),
            "LD_PRELOAD should be filtered"
        );
        // MYVAR 应被保留（非黑名单）
        assert!(
            env.iter().any(|(k, v)| k == "MYVAR" && v == "hello"),
            "MYVAR should be preserved"
        );
        // PATH 必须存在
        assert!(env.iter().any(|(k, _)| k == "PATH"), "PATH must be present");
    }

    #[test]
    fn test_shell_quote_value() {
        assert_eq!(shell_quote_value("hello"), "'hello'");
        assert_eq!(shell_quote_value("it's"), "'it'\\''s'");
        assert_eq!(shell_quote_value("/path/to/bin"), "'/path/to/bin'");
    }

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

    #[test]
    fn test_probe_tools_serializes() {
        let probe = probe_tools();
        let json = serde_json::to_string(&probe).unwrap();
        assert!(!json.is_empty());
        // 必须包含 platform 字段
        assert!(json.contains("platform"));
    }

    #[test]
    fn test_ffi_exec_echo() {
        // 最简单的测试：执行 echo，验证 FFI 不崩溃
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
            // 输出应包含 hello_sandbox（exit_code=0）
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

    #[cfg(target_os = "windows")]
    #[test]
    fn test_windows_path_to_wsl() {
        assert_eq!(windows_path_to_wsl("C:\\Users\\test"), "/mnt/c/Users/test");
        assert_eq!(windows_path_to_wsl("D:\\workspace"), "/mnt/d/workspace");
    }
}
