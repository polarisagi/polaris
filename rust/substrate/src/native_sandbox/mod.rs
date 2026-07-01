// native_sandbox — 平台原生进程沙箱（FFI）
//
// 架构对齐：
//   macOS  → Apple Seatbelt（sandbox-exec，内置 macOS 10.5+，无需安装）
//   Linux  → bubblewrap（bwrap；不可用时降级 namespace-only）
//   Windows→ WSL2（wsl.exe；不可用时降级 bare exec）
//
// 设计依据: docs/arch/ADR-0008-sandbox-three-tier-fallback.md
//
// FFI 接口（V1）:
//   native_sandbox_exec(input_json, out_json, out_err) -> i32
//   native_sandbox_probe_tools(out_json, out_err) -> i32
//   native_sandbox_free_string(ptr)
//
// FFI 接口（V2）:
//   native_sandbox_exec_v2(input_json, out_json, out_err) -> i32
//   native_sandbox_wrap_argv(input_json, out_json, out_err) -> i32
//
// 模块结构：
//   types    — V1/V2 数据结构 + 凭据黑名单
//   env      — PATH 构建 + 环境变量过滤（V1/V2）
//   engine   — run_with_timeout + which_tool + FFI C 字符串辅助
//   seatbelt — macOS Seatbelt 执行（V1/V2）
//   bwrap    — Linux bubblewrap 执行（V1/V2）
//   fallback — namespace/WSL2/bare 降级（V1/V2）
//   dispatch — 平台分发 + 工具探测 + 公开 FFI 函数

#![allow(unused_variables)]

mod types;
mod env;
mod engine;
mod seatbelt;
mod bwrap;
mod fallback;
mod dispatch;
