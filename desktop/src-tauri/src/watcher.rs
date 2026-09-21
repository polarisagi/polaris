//! 后台巡检：守护进程存活监测 + HITL 审批提醒。
//!
//! 为什么外壳要自己轮询，而不是让 Web UI 去提醒：窗口关掉（收进托盘）之后
//! Web UI 就不在跑了，而"Agent 卡在一个等人批准的操作上"恰恰最常发生在用户
//! 没盯着界面的时候。审批超时会让任务失败——系统通知是桌面版相对网页版最实际
//! 的价值（ADR-0096）。
//!
//! 轮询走的是普通 HTTP + 本地令牌，经过网关的鉴权、限流与审计，**不是**旁路 IPC
//! （决策一）。

use std::collections::HashSet;
use std::time::Duration;

use tauri::{AppHandle, Manager};

use crate::probe::{self, ProbeResult};
use crate::startup::Mode;
use crate::{notify, tray, Conn, ShellState};

/// 存活探测间隔。
const HEALTH_EVERY: Duration = Duration::from_secs(5);
/// 审批轮询是存活探测的倍数（15 秒）：审批有分钟级的截止时间，
/// 没必要和存活探测一样频繁地打网关。
const APPROVAL_EVERY_N_TICKS: u32 = 3;

pub fn spawn(app: AppHandle) {
    std::thread::spawn(move || {
        let mut tick: u32 = 0;
        loop {
            std::thread::sleep(HEALTH_EVERY);
            tick = tick.wrapping_add(1);
            check_health(&app);
            if tick.is_multiple_of(APPROVAL_EVERY_N_TICKS) {
                check_approvals(&app);
            }
        }
    });
}

fn check_health(app: &AppHandle) {
    let (conn, port, mode, child_exited) = {
        let state = app.state::<ShellState>();
        let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
        // 宿主模式下子进程已退出：比端口探测更早、更确定的信号。
        let exited = match g.child.as_mut() {
            Some(c) => matches!(c.try_wait(), Ok(Some(_))),
            None => false,
        };
        (g.conn, g.port, g.mode, exited)
    };

    if conn != Conn::Connected && conn != Conn::Lost {
        return; // 启动中或故障页：交给 connect 的判定，不在这里抢
    }

    let alive = !child_exited && probe::get(port, "/healthz", None) == ProbeResult::Ok;

    let next = if alive { Conn::Connected } else { Conn::Lost };
    if next == conn {
        return;
    }
    {
        let state = app.state::<ShellState>();
        let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
        g.conn = next;
        if child_exited {
            g.child = None;
        }
    }
    tray::refresh(app);
    eprintln!("[polaris-desktop] 连接状态 {conn:?} -> {next:?}");

    match next {
        Conn::Lost => {
            let hint = if mode == Some(Mode::Host) {
                "守护进程已退出。可从托盘「重新连接 / 启动守护进程」恢复，或打开日志查看原因。"
            } else {
                "连接的守护进程已无响应。可从托盘「重新连接」，或检查它的运行状态。"
            };
            notify(app, "Polaris 守护进程无响应", hint);
        }
        Conn::Connected => notify(app, "Polaris 已恢复", "守护进程重新可用。"),
        _ => {}
    }
}

fn check_approvals(app: &AppHandle) {
    let (conn, port, token) = {
        let state = app.state::<ShellState>();
        let g = state.0.lock().unwrap_or_else(|e| e.into_inner());
        (g.conn, g.port, g.token.clone())
    };
    if conn != Conn::Connected || token.is_empty() {
        return;
    }

    let Some((code, body)) = probe::get_body(port, "/v1/approvals/pending", Some(&token)) else {
        return;
    };
    // 501 表示 HITL 未启用：合法状态，静默跳过。
    if code != 200 {
        return;
    }
    let Ok(v) = serde_json::from_str::<serde_json::Value>(&body) else {
        return;
    };
    let Some(items) = v.get("pending").and_then(|p| p.as_array()) else {
        return;
    };

    let current: Vec<(String, String)> = items
        .iter()
        .filter_map(|it| {
            let id = it.get("id")?.as_str()?.to_string();
            let text = it
                .get("prompt_text")
                .and_then(|t| t.as_str())
                .unwrap_or("")
                .to_string();
            Some((id, text))
        })
        .collect();

    let fresh: Vec<(String, String)> = {
        let state = app.state::<ShellState>();
        let mut g = state.0.lock().unwrap_or_else(|e| e.into_inner());
        let fresh: Vec<_> = current
            .iter()
            .filter(|(id, _)| !g.seen_approvals.contains(id))
            .cloned()
            .collect();
        // 只保留当前仍待审批的 ID：已处理的条目若不清出去，集合会随运行时长无限增长。
        let live: HashSet<String> = current.iter().map(|(id, _)| id.clone()).collect();
        g.seen_approvals.retain(|id| live.contains(id));
        g.seen_approvals
            .extend(fresh.iter().map(|(id, _)| id.clone()));
        fresh
    };

    if !fresh.is_empty() {
        eprintln!("[polaris-desktop] 新审批 {} 项", fresh.len());
    }
    match fresh.len() {
        0 => {}
        1 => notify(app, "Polaris 需要你的批准", &truncate(&fresh[0].1, 120)),
        n => notify(
            app,
            "Polaris 需要你的批准",
            &format!("有 {n} 项操作等待审批。打开窗口处理。"),
        ),
    }
}

/// 按字符截断（不按字节，避免切断中文）。
fn truncate(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let mut out: String = s.chars().take(max).collect();
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::truncate;

    #[test]
    fn 截断不切断多字节字符() {
        assert_eq!(truncate("审批请求", 2), "审批…");
        assert_eq!(truncate("短", 10), "短");
    }
}
