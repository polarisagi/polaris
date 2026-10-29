//! 托盘：状态行、窗口显隐、重新连接、打开日志、退出。
//!
//! 退出分两项（ADR-0096 决策七）：
//! - 「退出界面」——**默认动作**，守护进程继续运行。定时任务、聊天通道、HITL 审批是
//!   产品的核心能力，"关界面即停服务"会让桌面版比网页版能力更弱。
//! - 「完全退出」——仅在宿主模式（守护进程由本外壳拉起）下出现，连带停止它。
//!   附着模式下不提供此项：用户可能是先用 CLI 起的服务，外壳没有资格停它。
//!
//! 把"会不会影响后台任务"写进菜单项文字本身，而不是藏在文档里：用户在点之前
//! 就该知道后果。
//!
//! 2026-09-21 追记：ADR 初稿写的是偏好项 `quit_stops_daemon`（默认 false），实施时
//! 改为两个显式菜单项——效果等价（默认不停），但无需在外壳里另建一个配置存储，
//! 且用户每次退出时都能看到两种后果并自行选择。

use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Manager};

use crate::startup::Mode;
use crate::{Conn, ShellState};

const TRAY_ID: &str = "polaris-tray";

pub fn install(app: &AppHandle) -> tauri::Result<()> {
    let menu = build_menu(app)?;
    TrayIconBuilder::with_id(TRAY_ID)
        .tooltip("Polaris")
        .menu(&menu)
        .icon(app.default_window_icon().cloned().unwrap_or_else(|| {
            // 没有内置图标时用 1x1 占位：托盘缺图标不该让整个外壳起不来。
            tauri::image::Image::new_owned(vec![0, 0, 0, 0], 1, 1)
        }))
        .on_menu_event(|app, event| match event.id.as_ref() {
            "show" => show_window(app),
            "reconnect" => {
                let handle = app.clone();
                std::thread::spawn(move || {
                    stop_owned_child(&handle);
                    crate::connect(&handle);
                });
            }
            "logs" => open_log_dir(app),
            "quit_ui" => app.exit(0),
            "quit_all" => {
                stop_owned_child(app);
                app.exit(0);
            }
            _ => {}
        })
        .build(app)?;
    Ok(())
}

/// 状态变化后重建菜单与提示文字。
pub fn refresh(app: &AppHandle) {
    let Some(tray) = app.tray_by_id(TRAY_ID) else {
        return;
    };
    if let Ok(menu) = build_menu(app) {
        let _ = tray.set_menu(Some(menu));
    }
    let _ = tray.set_tooltip(Some(status_line(app)));
}

fn build_menu(app: &AppHandle) -> tauri::Result<Menu<tauri::Wry>> {
    let (conn, mode) = snapshot(app);

    let status = MenuItem::with_id(app, "status", status_line(app), false, None::<&str>)?;
    let sep1 = PredefinedMenuItem::separator(app)?;
    let show = MenuItem::with_id(app, "show", "显示窗口", true, None::<&str>)?;
    let reconnect_label = match (conn, mode) {
        (Conn::Lost, _) | (Conn::Failed, _) => "重新连接 / 启动守护进程",
        (_, Some(Mode::Host)) => "重启守护进程",
        _ => "重新连接",
    };
    let reconnect = MenuItem::with_id(
        app,
        "reconnect",
        reconnect_label,
        conn != Conn::Starting,
        None::<&str>,
    )?;
    let logs = MenuItem::with_id(app, "logs", "打开日志目录", true, None::<&str>)?;
    let sep2 = PredefinedMenuItem::separator(app)?;
    let quit_ui = MenuItem::with_id(
        app,
        "quit_ui",
        "退出界面（守护进程继续运行）",
        true,
        None::<&str>,
    )?;

    if mode == Some(Mode::Host) {
        let quit_all = MenuItem::with_id(
            app,
            "quit_all",
            "完全退出（同时停止守护进程）",
            true,
            None::<&str>,
        )?;
        Menu::with_items(
            app,
            &[
                &status, &sep1, &show, &reconnect, &logs, &sep2, &quit_ui, &quit_all,
            ],
        )
    } else {
        Menu::with_items(
            app,
            &[&status, &sep1, &show, &reconnect, &logs, &sep2, &quit_ui],
        )
    }
}

fn snapshot(app: &AppHandle) -> (Conn, Option<Mode>) {
    let state = app.state::<ShellState>();
    let g = state.0.lock().unwrap_or_else(|e| e.into_inner());
    (g.conn, g.mode)
}

fn status_line(app: &AppHandle) -> String {
    let (conn, mode) = snapshot(app);
    match (conn, mode) {
        (Conn::Starting, _) => "● 正在连接…".into(),
        (Conn::Connected, Some(Mode::Host)) => "● 运行中（由本程序启动）".into(),
        (Conn::Connected, _) => "● 运行中（已连接到现有服务）".into(),
        (Conn::Lost, _) => "● 守护进程无响应".into(),
        (Conn::Failed, _) => "● 未连接".into(),
    }
}

fn show_window(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.set_focus();
    }
}

/// 用系统文件管理器打开日志目录。路径取自守护进程自己报告的 log_dir，
/// 不在外壳里另拼一份（同 discovery 的理由）。
fn open_log_dir(app: &AppHandle) {
    let state = app.state::<ShellState>();
    let dir = state
        .0
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .log_dir
        .clone();
    if dir.is_empty() {
        return;
    }
    let cmd = if cfg!(target_os = "macos") {
        "open"
    } else if cfg!(target_os = "windows") {
        "explorer"
    } else {
        "xdg-open"
    };
    let _ = std::process::Command::new(cmd).arg(dir).spawn();
}

/// 停止本外壳拉起的守护进程；附着模式下 child 为 None，什么都不做。
pub fn stop_owned_child(app: &AppHandle) {
    let state = app.state::<ShellState>();
    let child = state
        .0
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .child
        .take();
    if let Some(mut c) = child {
        graceful_stop(&mut c);
    }
}

#[cfg(unix)]
fn graceful_stop(child: &mut std::process::Child) {
    use std::time::{Duration, Instant};

    // SIGTERM 触发守护进程的四阶优雅关停：排空在途请求、落盘单写者、校验审计链。
    // std 的 Child::kill 发的是 SIGKILL，会让最后一段窗口里的写入丢失、审计链留下断点。
    extern "C" {
        fn kill(pid: i32, sig: i32) -> i32;
    }
    // SAFETY: kill(2) 只读取两个整数参数；pid 来自本进程自己 spawn 的子进程句柄。
    unsafe {
        kill(child.id() as i32, 15);
    }
    let deadline = Instant::now() + Duration::from_secs(30);
    while Instant::now() < deadline {
        match child.try_wait() {
            Ok(Some(_)) => return,
            Ok(None) => std::thread::sleep(Duration::from_millis(200)),
            Err(_) => break,
        }
    }
    // 30 秒与守护进程自身的关停超时一致；超过仍未退出才强杀。
    let _ = child.kill();
}

#[cfg(not(unix))]
fn graceful_stop(child: &mut std::process::Child) {
    // Windows 没有可用于非控制台子进程的"请优雅退出"信号；直接终止。
    let _ = child.kill();
}
