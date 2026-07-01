// native_sandbox/types.rs — 公共数据结构与凭据黑名单
//
// 包含 V1（NativeSandboxRequest/Response/ToolProbeResult）
// 和 V2（SandboxContextV2/WrapArgvResponseV2）所有 FFI 数据结构。
// 同模块所有 fn 通过 `use super::types::*` 引入。

use std::os::raw::c_int;

use serde::{Deserialize, Serialize};

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
pub(super) const NS_OK: c_int = 0;
pub(super) const NS_ERR_INTERNAL: c_int = -1;
pub(super) const NS_ERR_UTF8: c_int = -4;
pub(super) const NS_ERR_TIMEOUT: c_int = -5;

// ─── V1 数据结构 ──────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub(super) struct NativeSandboxRequest {
    pub(super) command: String,
    pub(super) workdir: Option<String>,
    pub(super) allowed_paths: Option<Vec<String>>,
    pub(super) network_block: Option<bool>,
    pub(super) env_extra: Option<Vec<String>>,
    pub(super) timeout_ms: Option<u64>,
    // 仅 Linux exec_bwrap 使用，其他平台编译时视为 dead_code
    #[allow(dead_code)]
    pub(super) bwrap_path: Option<String>,
    pub(super) max_memory_mb: Option<u64>,
}

#[derive(Serialize)]
pub(super) struct NativeSandboxResponse {
    pub(super) output: String,
    pub(super) exit_code: i32,
    pub(super) sandbox_method: String,
    pub(super) memory_limited: bool,
}

/// 工具探测结果（供 Go 侧展示给 LLM 或用于调试）
#[derive(Serialize)]
pub(super) struct ToolProbeResult {
    pub(super) platform: String,
    pub(super) sandbox_method: String,
    /// 探测到的 PATH 字符串
    pub(super) resolved_path: String,
    /// bwrap 路径（Linux 有效）
    pub(super) bwrap_path: Option<String>,
    /// sandbox-exec 是否可用（macOS 有效）
    pub(super) seatbelt_available: bool,
    /// WSL2 是否可用（Windows 有效）
    pub(super) wsl2_available: bool,
    /// 探测到的语言运行时目录（存在的）
    pub(super) found_runtimes: Vec<String>,
    /// Wasmtime 运行时是否支持网络
    pub(super) wasi_network_supported: bool,
}

// ─── 凭据黑名单 ───────────────────────────────────────────────────────────────

/// 变量名后缀黑名单：命中则永远不进沙箱（无论 caller 如何声明）
pub(super) const CREDENTIAL_STRIP_SUFFIXES: &[&str] = &[
    "_API_KEY",
    "_SECRET",
    "_TOKEN",
    "_PASSWORD",
    "_CREDENTIALS",
    "_AUTH",
    "_PRIVATE_KEY",
    "_ACCESS_KEY",
    "_SIGNING_KEY",
];

/// 精确匹配黑名单
pub(super) const CREDENTIAL_STRIP_EXACT: &[&str] = &[
    "ANTHROPIC_API_KEY",
    "OPENAI_API_KEY",
    "DEEPSEEK_API_KEY",
    "GOOGLE_API_KEY",
    "GEMINI_API_KEY",
    "AZURE_OPENAI_API_KEY",
    "COHERE_API_KEY",
    "MISTRAL_API_KEY",
    "AWS_SECRET_ACCESS_KEY",
    "AWS_ACCESS_KEY_ID",
    "AWS_SESSION_TOKEN",
    "GITHUB_TOKEN",
    "GITLAB_TOKEN",
    "HUGGING_FACE_HUB_TOKEN",
    "DATABASE_URL",
    "REDIS_URL",
    "MONGODB_URI",
    "POSTGRES_URL",
    "POLARIS_MASTER_KEY",
    "POLARIS_JWT_SECRET",
];

pub(super) fn is_credential_key(key: &str) -> bool {
    let upper = key.to_uppercase();
    if CREDENTIAL_STRIP_EXACT.contains(&upper.as_str()) {
        return true;
    }
    CREDENTIAL_STRIP_SUFFIXES.iter().any(|s| upper.ends_with(s))
}

// ─── V2 数据结构 ──────────────────────────────────────────────────────────────

/// V2 统一沙箱请求（JSON 反序列化）
#[derive(Deserialize)]
pub(super) struct SandboxContextV2 {
    /// 调用方："builtin"|"mcp"|"codeact"|"skill"|"hook"|"plugin"
    pub(super) caller_type: Option<String>,
    /// Shell 命令（bash -c 包裹，与 exec_path 二选一）
    pub(super) command: Option<String>,
    /// 直接执行路径（MCP/wrap_argv 模式，不走 bash -c）
    pub(super) exec_path: Option<String>,
    /// 直接执行参数（与 exec_path 配合）
    pub(super) exec_args: Option<Vec<String>>,
    /// 工作目录（默认 /tmp）
    pub(super) workdir: Option<String>,
    /// 可读写路径白名单
    pub(super) allowed_paths: Option<Vec<String>>,
    /// 环境预设："minimal"|"runtime"|"passthrough_safe"
    pub(super) env_preset: Option<String>,
    /// 额外注入 KEY=VALUE（凭据过滤后追加）
    pub(super) env_extra: Option<Vec<String>>,
    /// 网络策略："deny"|"domain_whitelist"|"allow"
    pub(super) network_policy: Option<String>,
    /// domain_whitelist 时允许的域名列表（macOS Seatbelt 原生支持）
    pub(super) network_domains: Option<Vec<String>>,
    /// true = bwrap 用 --bind /tmp /tmp（CodeAct 脚本在 host /tmp）
    /// Linux bwrap 专用，非 Linux 平台不读取。
    #[allow(dead_code)]
    pub(super) bind_host_tmp: Option<bool>,
    /// 单文件显式绑定（如 CodeAct 临时脚本）
    pub(super) script_path: Option<String>,
    /// 超时毫秒（默认 30000）
    pub(super) timeout_ms: Option<u64>,
    /// Linux bwrap 路径覆盖；非 Linux 平台不读取。
    #[allow(dead_code)]
    pub(super) bwrap_path: Option<String>,
    /// 内存限制 MB（0 = 不限）
    pub(super) max_memory_mb: Option<u64>,
}

/// native_sandbox_wrap_argv 响应：Go 侧用此构建 exec.Cmd
#[derive(Serialize)]
pub(super) struct WrapArgvResponseV2 {
    pub(super) executable: String,
    pub(super) argv: Vec<String>,
    /// KEY=VALUE 列表（env_in_argv=true 时为空）
    pub(super) env: Vec<String>,
    /// true = env 已通过 --setenv 嵌入 argv（bwrap），Go 侧无需再 cmd.Env()
    pub(super) env_in_argv: bool,
    pub(super) sandbox_method: String,
    pub(super) net_isolated: bool,
}
