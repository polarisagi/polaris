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
pub(super) fn run_with_timeout(
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

    #[cfg(unix)]
    unsafe {
        use std::os::unix::process::CommandExt;
        cmd.pre_exec(|| {
            libc::setpgid(0, 0);
            Ok(())
        });
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
                    return Err(format!("timeout after {}ms", timeout_ms));
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
    })
}

// ─── 工具查找 ─────────────────────────────────────────────────────────────────

/// 在 PATH 中查找可执行文件，返回绝对路径。
pub(super) fn which_tool(name: &str) -> Option<String> {
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
