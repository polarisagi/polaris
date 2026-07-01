// native_sandbox/env.rs — 环境变量与 PATH 构建
//
// 包含 V1（build_sandbox_path / build_safe_env / build_ulimit_prefix）
// 和 V2（build_env_v2）的环境构建逻辑。
// 凭据过滤依赖 types::is_credential_key。

use std::env;
use std::path::Path;

// SandboxContextV2 在 build_env_v2 函数签名中使用；
// 非 Linux/Windows 平台下调用方被 cfg 跳过导致误报 unused——用 allow 压制。
#[allow(unused_imports)]
use super::types::{SandboxContextV2, is_credential_key};

// ─── PATH 自动构建 ─────────────────────────────────────────────────────────────

/// 构建沙箱进程的 PATH 环境变量。
///
/// 策略：继承宿主 PATH（避免丢失用户安装的工具），再追加平台常见工具目录（去重）。
/// 优先级：宿主 PATH > 平台基础目录 > 用户工具目录
pub(super) fn build_sandbox_path() -> String {
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
pub(super) fn path_separator() -> char {
    if cfg!(windows) { ';' } else { ':' }
}

/// 无重复追加路径
pub(super) fn add_unique(list: &mut Vec<String>, item: String) {
    if !list.contains(&item) {
        list.push(item);
    }
}

/// 过滤危险环境变量（注入攻击向量），保留安全变量。
/// 策略：白名单保留 + 显式剔除高危变量。
pub(super) fn build_safe_env(extra: &[String], sandbox_path: &str) -> Vec<(String, String)> {
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
pub(super) fn build_ulimit_prefix(max_memory_mb: Option<u64>) -> String {
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

// ─── 环境构建 V2 ──────────────────────────────────────────────────────────────

pub(super) fn build_env_v2(
    caller_type: &str,
    preset: Option<&str>,
    extra: &[String],
    sandbox_path: &str,
) -> Vec<(String, String)> {
    let effective_preset = preset.unwrap_or(match caller_type {
        "builtin" | "codeact" | "skill" | "hook" => "runtime",
        _ => "minimal",
    });

    let mut result: Vec<(String, String)> = vec![
        ("PATH".to_string(), sandbox_path.to_string()),
        (
            "HOME".to_string(),
            env::var("HOME").unwrap_or_else(|_| "/tmp".to_string()),
        ),
        ("TMPDIR".to_string(), "/tmp".to_string()),
        ("TEMP".to_string(), "/tmp".to_string()),
    ];

    let passthrough: &[&str] = match effective_preset {
        "minimal" => &["LANG", "LC_ALL", "TZ"],
        "runtime" => &[
            "LANG",
            "LC_ALL",
            "LC_CTYPE",
            "LC_MESSAGES",
            "TZ",
            "TERM",
            "PYTHONPATH",
            "PYTHONDONTWRITEBYTECODE",
            "VIRTUAL_ENV",
            "NODE_PATH",
            "NODE_ENV",
            "GOPATH",
            "GOROOT",
            "GOMODCACHE",
            "GOCACHE",
            "CARGO_HOME",
            "RUSTUP_HOME",
            "JAVA_HOME",
            "CLASSPATH",
            "MAKEFLAGS",
        ],
        _ => &[
            // passthrough_safe
            "LANG",
            "LC_ALL",
            "LC_CTYPE",
            "LC_MESSAGES",
            "TZ",
            "TERM",
            "COLORTERM",
            "USER",
            "LOGNAME",
            "PYTHONPATH",
            "PYTHONDONTWRITEBYTECODE",
            "VIRTUAL_ENV",
            "NODE_PATH",
            "NODE_ENV",
            "GOPATH",
            "GOROOT",
            "GOMODCACHE",
            "GOCACHE",
            "CARGO_HOME",
            "RUSTUP_HOME",
            "JAVA_HOME",
            "CLASSPATH",
            "MAKEFLAGS",
            "CMAKE_PREFIX_PATH",
            "http_proxy",
            "https_proxy",
            "no_proxy",
            "HTTP_PROXY",
            "HTTPS_PROXY",
            "NO_PROXY",
        ],
    };

    for key in passthrough {
        if let Ok(val) = env::var(key)
            && !is_credential_key(key)
        {
            result.push((key.to_string(), val));
        }
    }

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

    for kv in extra {
        if let Some((k, v)) = kv.split_once('=') {
            let ku = k.to_uppercase();
            if danger_list.iter().any(|d| *d == ku) {
                continue;
            }
            if is_credential_key(k) {
                continue;
            }
            if let Some(pos) = result.iter().position(|(ek, _)| ek == k) {
                result[pos].1 = v.to_string();
            } else {
                result.push((k.to_string(), v.to_string()));
            }
        }
    }
    result
}

// ─── 单元测试 ─────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_build_sandbox_path_non_empty() {
        let path = build_sandbox_path();
        assert!(!path.is_empty(), "PATH should not be empty");
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
        assert!(
            !env.iter().any(|(k, _)| k == "LD_PRELOAD"),
            "LD_PRELOAD should be filtered"
        );
        assert!(
            env.iter().any(|(k, v)| k == "MYVAR" && v == "hello"),
            "MYVAR should be preserved"
        );
        assert!(env.iter().any(|(k, _)| k == "PATH"), "PATH must be present");
    }
}
