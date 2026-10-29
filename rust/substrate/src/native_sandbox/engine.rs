// native_sandbox/engine.rs — 执行引擎与工具辅助函数
//
// 包含：
//   run_with_timeout  — 带并发管道排空的超时执行
//   which_tool        — PATH 可执行查找
//   shell_quote_value — 单引号 shell 转义
//   windows_path_to_wsl — Windows 路径 → WSL2 挂载路径
//   ns_write_cstr / ns_read_cstr — FFI C 字符串辅助

use std::env;
use std::ffi::{CStr, CString};
use std::os::raw::c_char;
use std::path::Path;
use std::process::Command;
use std::time::Duration;

use super::env::path_separator;
use super::types::NativeSandboxResponse;

// ─── 执行引擎 ─────────────────────────────────────────────────────────────────

/// 带超时的命令执行，合并 stdout+stderr。
///
/// 必须用独立线程并发排空管道：若子进程输出 > OS 管道缓冲区（Linux ~64KB），
/// 子进程会阻塞在 write(2)，try_wait 永远返回 None，最终超时 kill 导致输出丢失。
/// 线程持续消费管道，子进程不会阻塞，try_wait 在进程退出后正常返回。
/// max_memory_mb > 0 时，在 unix 平台的 pre_exec 中通过 setrlimit(RLIMIT_AS) 真实施加
/// 地址空间上限——对 `bash -c` 与直接 `exec_path` 两种执行形态统一生效，取代仅对 shell
/// 命令有效的 `ulimit -v` 前缀（后者在 exec_path 直执行路径下完全失效，却仍上报
/// memory_limited=true，属虚假声明）。memory_limited 由本函数据实计算：仅当在 unix 上
/// 实际下达 rlimit 时才为 true（HE-1 诚实上报）。
pub(super) fn run_with_timeout(
    mut cmd: Command,
    timeout_ms: u64,
    method: &str,
    max_memory_mb: u64,
) -> Result<NativeSandboxResponse, String> {
    use std::io::Read;
    use std::sync::{Arc, Mutex};
    use std::thread;

    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    // unix 上是否真正施加了内存限制（用于诚实上报 memory_limited）。
    let memory_limited = cfg!(unix) && max_memory_mb > 0;

    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        // 溢出保护：MB → 字节；异常大的值饱和到 u64::MAX 而非回绕成小值。
        let mem_bytes = max_memory_mb.saturating_mul(1024 * 1024);
        unsafe {
            cmd.pre_exec(move || {
                libc::setpgid(0, 0);
                if mem_bytes > 0 {
                    let lim = libc::rlimit {
                        rlim_cur: mem_bytes as libc::rlim_t,
                        rlim_max: mem_bytes as libc::rlim_t,
                    };
                    // 失败不致命：部分宿主（如 macOS）对 RLIMIT_AS 强制较弱，与原
                    // `ulimit -v` 语义一致，尽力而为、不阻断执行。
                    libc::setrlimit(libc::RLIMIT_AS, &lim);
                }
                Ok(())
            });
        }
    }

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
                    #[cfg(unix)]
                    unsafe {
                        libc::killpg(child.id() as libc::pid_t, libc::SIGKILL);
                    }
                    #[cfg(not(unix))]
                    let _ = child.kill();

                    // kill 后管道 EOF，线程自然结束
                    let _ = t_out.join();
                    let _ = t_err.join();
                    // 超时前已排空的 stdout/stderr 不丢弃，随错误一并回传（前缀保留
                    // "timeout" 以便 dispatch 层仍映射为 NS_ERR_TIMEOUT）。
                    let partial = {
                        let o = stdout_buf.lock().unwrap_or_else(|e| e.into_inner());
                        let e = stderr_buf.lock().unwrap_or_else(|e| e.into_inner());
                        let mut s = String::from_utf8_lossy(&o).into_owned();
                        let es = String::from_utf8_lossy(&e);
                        if !es.is_empty() {
                            if !s.is_empty() {
                                s.push('\n');
                            }
                            s.push_str(&es);
                        }
                        s
                    };
                    const MAX_PARTIAL: usize = 8192;
                    let mut end = MAX_PARTIAL.min(partial.len());
                    while end > 0 && !partial.is_char_boundary(end) {
                        end -= 1;
                    }
                    if partial.is_empty() {
                        return Err(format!("timeout after {}ms", timeout_ms));
                    }
                    let suffix = if end < partial.len() {
                        format!("…[truncated {} bytes]", partial.len() - end)
                    } else {
                        String::new()
                    };
                    return Err(format!(
                        "timeout after {}ms; partial output: {}{}",
                        timeout_ms,
                        &partial[..end],
                        suffix
                    ));
                }
                thread::sleep(Duration::from_millis(50));
            }
            Err(e) => {
                #[cfg(unix)]
                unsafe {
                    libc::killpg(child.id() as libc::pid_t, libc::SIGKILL);
                }
                #[cfg(not(unix))]
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
        // 默认 false；只有 dispatch 层确认走了 seatbelt/bwrap 真实隔离路径才会覆盖为 true。
        net_isolated: false,
    })
}

// ─── 工具查找 ─────────────────────────────────────────────────────────────────

/// 判定 candidate 是否为可执行普通文件。
/// Unix：必须是文件且置了任一执行位（0o111）；非 Unix：是文件即可。
fn is_executable_file(candidate: &Path) -> bool {
    if !candidate.is_file() {
        return false;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::metadata(candidate)
            .map(|m| m.permissions().mode() & 0o111 != 0)
            .unwrap_or(false)
    }
    #[cfg(not(unix))]
    {
        true
    }
}

/// 在 PATH 中查找可执行文件，返回绝对路径。
pub(super) fn which_tool(name: &str) -> Option<String> {
    let path_var = env::var("PATH").unwrap_or_default();
    let sep = path_separator();
    for dir in path_var.split(sep) {
        let candidate = Path::new(dir).join(name);
        if is_executable_file(&candidate) {
            return Some(candidate.to_string_lossy().to_string());
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

    // 也检查常见固定路径（PATH 可能不完整）；同样要求执行位，避免返回不可执行文件
    // 导致后续 spawn 必然失败（S4：与 PATH 查找口径一致）。
    let fixed_locations: &[&str] = &["/usr/bin", "/usr/local/bin", "/opt/homebrew/bin", "/bin"];
    for dir in fixed_locations {
        let candidate = Path::new(dir).join(name);
        if is_executable_file(&candidate) {
            return Some(candidate.to_string_lossy().to_string());
        }
    }

    None
}

/// shell 值引用（单引号包裹，内部单引号 escape）
pub(super) fn shell_quote_value(s: &str) -> String {
    // 使用单引号：最安全的 shell 值引用方式
    // 内部的 ' 替换为 '\''
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// Windows 路径转 WSL2 挂载路径（C:\foo → /mnt/c/foo）
#[cfg(target_os = "windows")]
pub(super) fn windows_path_to_wsl(path: &str) -> String {
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
pub(super) fn windows_path_to_wsl(_path: &str) -> String {
    String::new()
}

// ─── FFI 出参辅助 ─────────────────────────────────────────────────────────────

pub(super) fn ns_write_cstr(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    let s = msg.replace('\0', "?");
    if let Ok(cs) = CString::new(s) {
        unsafe { *out = cs.into_raw() };
    }
}

pub(super) unsafe fn ns_read_cstr<'a>(ptr: *const c_char) -> Result<&'a str, ()> {
    unsafe {
        if ptr.is_null() {
            return Ok("");
        }
        CStr::from_ptr(ptr).to_str().map_err(|_| ())
    }
}

// ─── 单元测试 ─────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_shell_quote_value() {
        assert_eq!(shell_quote_value("hello"), "'hello'");
        assert_eq!(shell_quote_value("it's"), "'it'\\''s'");
        assert_eq!(shell_quote_value("/path/to/bin"), "'/path/to/bin'");
    }

    #[cfg(target_os = "windows")]
    #[test]
    fn test_windows_path_to_wsl() {
        assert_eq!(windows_path_to_wsl("C:\\Users\\test"), "/mnt/c/Users/test");
        assert_eq!(windows_path_to_wsl("D:\\workspace"), "/mnt/d/workspace");
    }
}
