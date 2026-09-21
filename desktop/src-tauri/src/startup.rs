//! 启动判定状态机（ADR-0096 决策七）。
//!
//! ```text
//! 找 polaris 二进制
//!   ├─ 没找到 ──────────────► 故障页「核心未安装」
//!   └─ 找到 → service status --json
//!        ├─ problem 非空 ───► 故障页「运行时状态异常」，**不得**重复拉起
//!        ├─ running=false ──► 【宿主模式】spawn + 指数退避轮询至就绪
//!        │                     （含 stale：kill -9 残留的 run/ 文件，照常拉起）
//!        └─ running=true ───► 已有进程持锁，**不得**拉起，轮询至就绪：
//!              端口未发布            → 仍在装配，继续等
//!              ① GET /healthz          存活性（免鉴权，附带版本比对）
//!              ② GET /v1/config + 令牌  凭证有效性
//!              ├─ ①② 均通过 → 【附着模式】
//!              └─ ① 通 ② 401 → 故障页「凭证不匹配」
//! ```
//!
//! running 由 Go 侧按**单实例锁**判定而非按 run/ 文件判定——文件在 kill -9 后会残留，
//! 锁由内核随进程退出释放。2026-09-21 实跑发现：按文件判活时，崩溃后点"重新连接"
//! 会走到"运行时状态异常"并要求用户手动删 run/，与 ADR-0096 决策七不符。
//!
//! 两段式探测不可省为只探 /healthz：该端点在服务端的免鉴权白名单里，探通它只证明
//! "有进程在听"，不证明"本外壳能调 API"。省掉第二段，凭证不匹配时外壳会进入附着
//! 模式，随后每个业务请求 401，用户看到的是"界面全白但服务是好的"。

use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::thread::sleep;
use std::time::{Duration, Instant};

use crate::discovery::{self, RuntimeStatus};
use crate::probe::{self, ProbeResult};

/// 外壳与守护进程的关系。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// 守护进程由本外壳拉起：退出时可以停它。
    Host,
    /// 连上了别人拉起的守护进程：只关窗口，不碰它的生命周期。
    Attach,
}

/// 启动判定的结局。
pub enum Outcome {
    Ready {
        mode: Mode,
        base_url: String,
        // Box：RuntimeStatus 含多个 String，不装箱时 Ready 比 Failed 大一个数量级，
        // 每个 Outcome 都按最大变体分配栈空间。
        status: Box<RuntimeStatus>,
        child: Option<Child>,
        bin: PathBuf,
    },
    /// 走故障页。code 决定页面文案，detail 是给用户看的补充信息。
    Failed { code: FailCode, detail: String },
}

#[derive(Debug, Clone, Copy)]
pub enum FailCode {
    /// 核心未安装：外壳被单独拷到了没装守护进程的机器上
    CoreMissing,
    /// 拉起了但没能在超时内就绪
    StartTimeout,
    /// 服务活着，但本外壳的凭证调不通
    CredentialMismatch,
    /// run/ 状态文件存在但读不出来（权限过宽 / 内容损坏）
    RuntimeBroken,
    /// 外壳与守护进程版本不兼容（二者分开安装、各自更新，见 ADR-0096 决策四）
    VersionMismatch,
}

impl FailCode {
    pub fn as_str(self) -> &'static str {
        match self {
            FailCode::CoreMissing => "core-missing",
            FailCode::StartTimeout => "start-timeout",
            FailCode::CredentialMismatch => "credential-mismatch",
            FailCode::RuntimeBroken => "runtime-broken",
            FailCode::VersionMismatch => "version-mismatch",
        }
    }
}

/// 就绪轮询的总时限。超过就报错并给出"打开日志"入口，而不是无限转圈——
/// 一个永远在转的加载动画和一个已经死掉的进程，在用户那里长得一模一样。
const READY_TIMEOUT: Duration = Duration::from_secs(60);

pub fn run() -> Outcome {
    let Some(bin) = discovery::find_binary() else {
        return Outcome::Failed {
            code: FailCode::CoreMissing,
            detail: String::new(),
        };
    };

    let status = match discovery::query(&bin) {
        Ok(s) => s,
        Err(e) => {
            // 二进制在，但问不出状态：多半是版本过旧（没有 --json 子命令）或文件损坏。
            return Outcome::Failed {
                code: FailCode::CoreMissing,
                detail: e,
            };
        }
    };

    if !status.problem.is_empty() {
        return Outcome::Failed {
            code: FailCode::RuntimeBroken,
            detail: status.problem.clone(),
        };
    }

    // running 由 Go 侧按**单实例锁**判定，不按 run/ 文件存在判定：kill -9 之后文件
    // 残留、锁已释放，此时 running=false、stale=true，这里照常走宿主模式拉起即可
    // （守护进程启动时会原子覆盖残留文件）。
    if status.running {
        // 已有进程持锁：可能已就绪，也可能正在装配（持锁在前、发布端口在后）。
        // 两种情况都不得拉起——拉起的那个必被单实例锁拒绝。
        return wait_ready(bin, None, Mode::Attach);
    }
    host(bin)
}

/// 宿主模式：拉起守护进程并等待就绪。
fn host(bin: PathBuf) -> Outcome {
    let child = match Command::new(&bin)
        .arg("serve")
        // 日志由守护进程自己写进 logs/，这里丢弃管道即可；继承 stdio 会让
        // 外壳退出时的管道关闭影响到子进程的写入。
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
    {
        Ok(c) => c,
        Err(e) => {
            return Outcome::Failed {
                code: FailCode::CoreMissing,
                detail: format!("拉起 {} 失败: {e}", bin.display()),
            }
        }
    };
    wait_ready(bin, Some(child), Mode::Host)
}

/// 轮询直到守护进程就绪（两段式探测通过），或确定失败。宿主与附着共用。
///
/// 指数退避：守护进程要完成整套装配（DB、模型、沙箱）才会监听端口，
/// 固定短间隔的密集轮询只是白耗 CPU。
fn wait_ready(bin: PathBuf, mut child: Option<Child>, mode: Mode) -> Outcome {
    let deadline = Instant::now() + READY_TIMEOUT;
    let mut wait = Duration::from_millis(200);
    let mut first = true;

    while Instant::now() < deadline {
        if !first {
            sleep(wait);
            wait = (wait * 2).min(Duration::from_secs(3));
        }
        first = false;

        // 宿主模式下子进程提前退出：不必等满超时，直接报出退出码。最常见的原因
        // 是另一个实例恰好在这之间抢到了锁，或配置有误导致装配失败。
        if let Some(c) = child.as_mut() {
            if let Ok(Some(code)) = c.try_wait() {
                return Outcome::Failed {
                    code: FailCode::StartTimeout,
                    detail: format!("守护进程启动后立即退出（{code}），请打开日志查看原因"),
                };
            }
        }

        let Ok(s) = discovery::query(&bin) else {
            continue;
        };
        if !s.problem.is_empty() {
            return Outcome::Failed {
                code: FailCode::RuntimeBroken,
                detail: s.problem,
            };
        }
        // 持锁但尚未发布端口：仍在装配，继续等。
        if !s.running || s.port == 0 {
            continue;
        }
        match check_health(s.port) {
            Health::Ok => {}
            Health::VersionMismatch(daemon) => return version_mismatch(&daemon),
            Health::Down => continue,
        }

        // ② 凭证探测。只探 /healthz 是不够的——它免鉴权，见文件头。
        let token = (!s.token.is_empty()).then_some(s.token.as_str());
        match probe::get(s.port, "/v1/config", token) {
            ProbeResult::Ok => {
                return Outcome::Ready {
                    mode,
                    base_url: base_url(&s),
                    status: Box::new(s),
                    child,
                    bin,
                }
            }
            ProbeResult::Unauthorized => {
                return Outcome::Failed {
                    code: FailCode::CredentialMismatch,
                    detail: "守护进程可能由其他账号启动，或启动时设置了 POLARIS_API_KEY".into(),
                }
            }
            _ => continue,
        }
    }

    Outcome::Failed {
        code: FailCode::StartTimeout,
        detail: format!("守护进程在 {} 秒内未就绪", READY_TIMEOUT.as_secs()),
    }
}

fn base_url(s: &RuntimeStatus) -> String {
    if s.base_url.is_empty() {
        format!("http://127.0.0.1:{}", s.port)
    } else {
        s.base_url.clone()
    }
}

/// /healthz 探测结果（含版本比对）。
enum Health {
    Ok,
    Down,
    VersionMismatch(String),
}

/// 探 /healthz 并比对版本。
///
/// 外壳与守护进程分开安装、各自更新（ADR-0096 决策四），版本不一致是可预期
/// 状态而非异常。不比对的代价：新外壳连上旧守护进程后，某些界面功能"莫名其妙
/// 不工作"，两端各自看起来都正常。
fn check_health(port: u16) -> Health {
    let Some((code, body)) = probe::get_body(port, "/healthz", None) else {
        return Health::Down;
    };
    if !(200..300).contains(&code) {
        return Health::Down;
    }
    let daemon = serde_json::from_str::<serde_json::Value>(&body)
        .ok()
        .and_then(|v| v.get("version").and_then(|x| x.as_str()).map(String::from))
        .unwrap_or_default();
    if compatible(env!("CARGO_PKG_VERSION"), &daemon) {
        Health::Ok
    } else {
        Health::VersionMismatch(daemon)
    }
}

fn version_mismatch(daemon: &str) -> Outcome {
    Outcome::Failed {
        code: FailCode::VersionMismatch,
        detail: format!(
            "外壳版本 {}，守护进程版本 {}",
            env!("CARGO_PKG_VERSION"),
            daemon
        ),
    }
}

/// 版本兼容判定。
///
/// - 任一方不是合法 semver（开发构建为 "dev"）→ 视为兼容：开发期不该被自己拦住。
/// - 1.x 及以上：主版本相同即兼容。
/// - 0.x：semver 约定次版本即破坏性变更，故主次版本都须相同。
pub fn compatible(shell: &str, daemon: &str) -> bool {
    let (Some(a), Some(b)) = (parse_semver(shell), parse_semver(daemon)) else {
        return true;
    };
    if a.0 == 0 || b.0 == 0 {
        return a.0 == b.0 && a.1 == b.1;
    }
    a.0 == b.0
}

fn parse_semver(v: &str) -> Option<(u64, u64, u64)> {
    let v = v.trim().trim_start_matches('v');
    let core = v.split(['-', '+']).next()?;
    let mut it = core.split('.');
    let major = it.next()?.parse().ok()?;
    let minor = it.next()?.parse().ok()?;
    let patch = it.next().unwrap_or("0").parse().ok()?;
    Some((major, minor, patch))
}

#[cfg(test)]
mod tests {
    use super::compatible;

    #[test]
    fn 版本兼容判定() {
        // 开发构建不拦
        assert!(compatible("0.1.0", "dev"));
        assert!(compatible("dev", "v1.2.3"));
        // 1.x：主版本相同即兼容，tag 带 v 前缀也能比
        assert!(compatible("1.4.0", "v1.2.9"));
        assert!(!compatible("2.0.0", "v1.9.9"));
        // 0.x：次版本即破坏性
        assert!(compatible("0.3.1", "v0.3.7"));
        assert!(!compatible("0.3.1", "v0.4.0"));
        // 预发布后缀不影响判定
        assert!(compatible("1.0.0-rc.1", "v1.0.0"));
    }
}
