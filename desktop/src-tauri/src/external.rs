// 外链处理：窗口内的 target=_blank / window.open 一律交系统浏览器。
//
// 为什么在外壳做：WKWebView/WebView2 收到新窗口请求时若无人接管就静默丢弃，
// 用户看到的是"点了没反应"。不引 opener 插件——它要给远程源（守护进程地址）
// 开 IPC 权限，违反 ADR-0096 决策一"窗口不拿任何 Tauri API"。这里只走
// on_new_window 回调 + 系统 open 命令，不新增任何 invoke command。

use std::process::Command;

use tauri::Url;

/// 仅放行 http/https：file:、javascript:、自定义协议交给系统打开等于开了本地执行口子。
pub fn is_openable(url: &Url) -> bool {
    matches!(url.scheme(), "http" | "https")
}

/// 用系统默认浏览器打开。失败静默（无默认浏览器是合法状态，不应影响外壳）。
pub fn open(url: &Url) {
    if !is_openable(url) {
        return;
    }
    let u = url.as_str();
    #[cfg(target_os = "macos")]
    let r = Command::new("open").arg(u).spawn();
    // rundll32 不经 cmd 解析，URL 里的 & 不会被截断。
    #[cfg(target_os = "windows")]
    let r = Command::new("rundll32")
        .args(["url.dll,FileProtocolHandler", u])
        .spawn();
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    let r = Command::new("xdg-open").arg(u).spawn();
    if let Err(e) = r {
        eprintln!("[polaris-desktop] 打开外链失败 url={u} err={e}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_http_schemes_openable() {
        for (u, want) in [
            ("https://github.com/a/b", true),
            ("http://example.com", true),
            ("file:///etc/passwd", false),
            ("javascript:alert(1)", false),
            ("tauri://localhost/x", false),
        ] {
            assert_eq!(is_openable(&u.parse().unwrap()), want, "{u}");
        }
    }
}
