//! 服务发现：定位 polaris 二进制，并向它询问运行时状态。
//!
//! 外壳**不自己拼 run/ 路径**。路径 SSoT 是 Go 侧的 config.DataLayout
//! （ADR-0096 决策六），Rust 再实现一遍必然漂移其一，而漂移的表现是"外壳连不上
//! 一个正在运行的服务"，两边各自看起来还都对。故这里只做两件事：找到二进制，
//! 然后跑 `polaris service status --json`，其余全部由它回答——顺带也就自动
//! 支持了 config.toml 覆盖 data_dir 的情况。

use std::path::{Path, PathBuf};
use std::process::Command;

use serde::Deserialize;

/// `polaris service status --json` 的输出（与 cmd/polaris/cli_service.go 的
/// serviceRuntimeJSON 一一对应）。
// 字段与 Go 侧结构一一对应，即便外壳当前只用到其中几个：删掉"暂时没用的"字段，
// 等于让契约漂移在这一侧变得不可见——下次 Go 侧改了名字，这里反序列化照样成功，
// 只是那个字段永远是零值。
#[allow(dead_code)]
#[derive(Debug, Clone, Deserialize, Default)]
pub struct RuntimeStatus {
    #[serde(default)]
    pub registered: bool,
    #[serde(default)]
    pub running: bool,
    #[serde(default)]
    pub pid: i64,
    #[serde(default)]
    pub port: u16,
    #[serde(default)]
    pub base_url: String,
    #[serde(default)]
    pub token: String,
    #[serde(default)]
    pub data_dir: String,
    #[serde(default)]
    pub bin_path: String,
    #[serde(default)]
    pub log_dir: String,
    /// run/ 下有残留文件但无人持锁（上次异常退出）。按未运行处理即可。
    #[serde(default)]
    pub stale: bool,
    /// 「有状态文件但读不出来」的原因（权限过宽 / 内容损坏）。
    /// 与 running=false 是两回事：那是没在跑，这是在跑但拿不到凭证。
    #[serde(default)]
    pub problem: String,
}

/// 查找 polaris 二进制。顺序固定，先显式后约定。
///
/// 这是外壳里**唯一**硬编码的路径知识，且只是默认安装位置——拿到二进制之后，
/// 数据目录、run/ 路径、日志目录一律问它。
pub fn find_binary() -> Option<PathBuf> {
    // ① 显式指定：开发与非常规安装位置
    if let Ok(p) = std::env::var("POLARIS_BIN") {
        let p = PathBuf::from(p);
        if is_executable(&p) {
            return Some(p);
        }
    }

    // ② 约定安装位置：install.sh 装到 <home>/.polarisagi/polaris/bin/
    if let Some(home) = home_dir() {
        let p = home
            .join(".polarisagi")
            .join("polaris")
            .join("bin")
            .join(exe_name());
        if is_executable(&p) {
            return Some(p);
        }
    }

    // ③ 外壳可执行文件旁边：开发期把二进制拷过来即可跑
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let p = dir.join(exe_name());
            if is_executable(&p) {
                return Some(p);
            }
        }
    }

    // ④ PATH：Homebrew / 手工安装
    which_in_path(&exe_name())
}

/// 向二进制询问运行时状态。
pub fn query(bin: &Path) -> Result<RuntimeStatus, String> {
    let out = Command::new(bin)
        .args(["service", "status", "--json"])
        .output()
        .map_err(|e| format!("执行 {} 失败: {e}", bin.display()))?;

    if !out.status.success() {
        return Err(format!(
            "{} service status --json 退出码非零: {}",
            bin.display(),
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    serde_json::from_slice::<RuntimeStatus>(&out.stdout)
        .map_err(|e| format!("解析运行时状态失败: {e}"))
}

fn exe_name() -> String {
    if cfg!(windows) {
        "polaris.exe".into()
    } else {
        "polaris".into()
    }
}

pub fn home_dir() -> Option<PathBuf> {
    let key = if cfg!(windows) { "USERPROFILE" } else { "HOME" };
    std::env::var_os(key).map(PathBuf::from)
}

/// 存在且可执行。Windows 上只判存在——可执行性由扩展名决定，权限位无意义。
fn is_executable(p: &Path) -> bool {
    if !p.is_file() {
        return false;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        p.metadata()
            .map(|m| m.permissions().mode() & 0o111 != 0)
            .unwrap_or(false)
    }
    #[cfg(not(unix))]
    {
        true
    }
}

fn which_in_path(name: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|dir| dir.join(name))
        .find(|p| is_executable(p))
}
