// Polaris 桌面外壳。
//
// 职责边界（ADR-0096 决策一，**不可扩张**）：本外壳只做子进程管理、窗口、托盘、
// 系统通知、深链与自更新。**禁止任何业务语义的 Tauri command**——客户端与内核之间
// 只有 HTTP/SSE 一条路，旁路一条 IPC 就等于旁路了网关的鉴权、限流、审计与埋点。
// 下面 invoke_handler 的清单即白名单，新增须先改 ADR。
//
// 窗口加载的是守护进程地址（http://127.0.0.1:<port>），不是打包进外壳的前端资源：
// 同源之下相对路径 fetch、EventSource、Cookie 鉴权全部原样工作，前端一行不用改
// （决策三）。dist/ 里只有启动页与故障页两张静态页，不含任何业务 UI。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod discovery;
mod probe;
mod startup;
mod tray;
mod watcher;

use std::collections::HashSet;
use std::path::PathBuf;
use std::process::Child;
use std::sync::Mutex;

use tauri::{AppHandle, Manager, WindowEvent};

use startup::{Mode, Outcome};

/// 连接状态。托盘状态行与菜单项据此渲染。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Conn {
    #[default]
    Starting,
    Connected,
    /// 已连接过但当前探测不通（守护进程崩溃或被停止）
    Lost,
    /// 启动判定走到了故障页
    Failed,
}

/// 外壳运行期状态。启动判定在后台线程完成，故全部字段在 setup 之后才逐步填入。
#[derive(Default)]
pub struct Inner {
    pub conn: Conn,
    pub mode: Option<Mode>,
    pub bin: Option<PathBuf>,
    pub base_url: String,
    pub port: u16,
    pub token: String,
    pub log_dir: String,
    /// 本外壳拉起的守护进程。附着模式下为 None——**不碰别人的进程**。
    pub child: Option<Child>,
    pub close_hint_shown: bool,
    /// 已通知过的审批 ID：同一条审批只提醒一次。
    pub seen_approvals: HashSet<String>,
    /// 正在重连时为 true，避免托盘被连点触发并发的启动判定。
    pub connecting: bool,
}

#[derive(Default)]
pub struct ShellState(pub Mutex<Inner>);

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        // 业务通道白名单：当前为空。见文件头的职责边界。
        .invoke_handler(tauri::generate_handler![])
        .setup(|app| {
            app.manage(ShellState::default());
            tray::install(app.handle())?;
            // 启动判定放到后台线程：宿主模式要等守护进程完成整套装配（最长 60 秒），
            // 在 setup 里同步等待会卡住事件循环，启动页根本画不出来——用户看到的是
            // 一个无响应的空窗口。
            let handle = app.handle().clone();
            std::thread::spawn(move || connect(&handle));
            watcher::spawn(app.handle().clone());
            Ok(())
        })
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                // 关窗收进托盘：定时任务、聊天通道、HITL 审批必须在窗口关闭后继续跑
                // （决策七）。真正退出走托盘菜单。
                api.prevent_close();
                let _ = window.hide();
                notify_close_once(window.app_handle());
            }
        })
        .run(tauri::generate_context!())
        .expect("桌面外壳启动失败");
}

/// 执行一次完整的启动判定并据此导航窗口。首次启动与托盘「重新连接」共用这一条路径——
/// 重连时守护进程可能已经换了端口（配置 port = 0 时每次启动都由内核重新分配），
/// 只重试旧地址会永远连不上。
pub fn connect(app: &AppHandle) {
    {
        let state = app.state::<ShellState>();
        let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
        if g.connecting {
            return;
        }
        g.connecting = true;
        g.conn = Conn::Starting;
    }
    tray::refresh(app);
    navigate_local(app, "boot.html");

    let outcome = startup::run();

    let state = app.state::<ShellState>();
    let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
    g.connecting = false;
    match outcome {
        Outcome::Ready {
            mode,
            base_url,
            status,
            child,
            bin,
        } => {
            eprintln!("[polaris-desktop] 就绪 mode={mode:?} url={base_url}");
            g.conn = Conn::Connected;
            g.mode = Some(mode);
            g.bin = Some(bin);
            g.port = status.port;
            g.token = status.token.clone();
            g.log_dir = status.log_dir.clone();
            if child.is_some() {
                g.child = child;
            }
            drop(g);
            if let (Some(w), Ok(url)) = (app.get_webview_window("main"), base_url.parse()) {
                let _ = w.navigate(url);
            }
        }
        Outcome::Failed { code, detail } => {
            eprintln!(
                "[polaris-desktop] 故障 code={} detail={detail}",
                code.as_str()
            );
            g.conn = Conn::Failed;
            drop(g);
            // 故障页是纯静态页，不调用任何 /v1 端点（决策三的唯一例外）。
            navigate_local(
                app,
                &format!(
                    "error.html?code={}&detail={}",
                    code.as_str(),
                    urlencode(&detail)
                ),
            );
        }
    }
    tray::refresh(app);
}

/// 导航到外壳内置的静态页。
fn navigate_local(app: &AppHandle, page: &str) {
    let Some(w) = app.get_webview_window("main") else {
        return;
    };
    // 各平台内置资源的协议不同：macOS/Linux 是 tauri://localhost，Windows 是
    // http://tauri.localhost。写死其一会让另一平台的故障页打不开。
    let base = if cfg!(windows) {
        "http://tauri.localhost"
    } else {
        "tauri://localhost"
    };
    if let Ok(u) = format!("{base}/{page}").parse() {
        let _ = w.navigate(u);
    }
}

/// 首次关窗时提示一次，避免用户以为已经退出。
fn notify_close_once(app: &AppHandle) {
    let state = app.state::<ShellState>();
    let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
    if g.close_hint_shown {
        return;
    }
    g.close_hint_shown = true;
    drop(g);
    notify(
        app,
        "Polaris 仍在后台运行",
        "定时任务与聊天通道继续工作。从托盘图标可重新打开窗口或退出。",
    );
}

/// 发一条系统通知。失败静默：通知权限被用户关掉是合法状态，不该影响外壳。
pub fn notify(app: &AppHandle, title: &str, body: &str) {
    use tauri_plugin_notification::NotificationExt;
    let _ = app.notification().builder().title(title).body(body).show();
}

/// 极小 URL 编码：故障页 detail 里可能有路径、引号与中文。
fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.as_bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(*b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}
