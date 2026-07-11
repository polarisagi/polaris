use std::ffi::CString;
use std::os::raw::{c_char, c_int};
use std::panic;
use std::path::Path;
use std::sync::{Arc, OnceLock};
use std::thread;
use std::time::Duration;
use wasmtime::*;
use wasmtime_wasi::{DirPerms, FilePerms, WasiCtxBuilder};

// ─── Wall-clock 超时（epoch interruption） ─────────────────────────────────────
//
// 背景（Batch11 GR-7.1）：`store.set_fuel` 的 fuel 机制只对 WASM 指令执行计量，
// host 侧网络 IO 等待期间不执行 WASM 指令、不消耗 fuel。若 network_allowed=1 的
// 模块发起一个永远不返回的网络请求，宿主线程会无界阻塞。
//
// epoch interruption 在 WASM 生成代码的函数入口/循环回边处检查独立于 fuel 的
// 墙钟 deadline，可靠覆盖"CPU-bound 死循环绕过 fuel"场景（已有 Rust 测试覆盖，
// 见文件末尾 test_epoch_interruption_stops_infinite_loop）。
//
// 但 epoch 检查点只在 WASM 生成代码边界触发，无法打断已经陷入阻塞态 host 系统调用
// （如 network_allowed=1 时挂起的 TCP connect/read）的执行线程——这是 wasmtime
// 官方文档明确的已知限制，而不是本实现的疏漏。这类场景的最终兜底防线在 Go 侧
// 调用方（internal/tool/sandbox/rust_wasmtime_sandbox.go），通过
// context.WithTimeout + 独立 goroutine + select 保证宿主 goroutine 不会无界
// 阻塞，代价是极端情况下可能牺牲一次 OS 线程（该线程随 Rust 侧调用最终返回/
// 进程退出而回收，不会无限累积）。
const EPOCH_TICK_MS: u64 = 50;

// Wasm 执行墙钟超时默认值：与 internal/sandbox/sandbox_impl.go SandboxSpec.CPUQuotaMs
// 的既有仓库约定"0 = 默认 5000ms"保持一致（native_os_sandbox.go/sandbox_container.go
// 均遵循该默认值），timeout_ms<=0 时启用。Go 侧调用方应显式传入
// spec.CPUQuotaMs（见 internal/tool/sandbox/wasmtime_sandbox.go），此值仅作
// 兜底，防止遗漏传参的调用方退化为无界等待。
const DEFAULT_TIMEOUT_MS: u64 = 5_000;

// ─── FFI 错误码 ────────────────────────────────────────────────────────────────
const WASMTIME_OK: c_int = 0;
const WASMTIME_ERR_INTERNAL: c_int = -1;
const WASMTIME_ERR_COMPILE: c_int = -2;
const WASMTIME_ERR_EXECUTE: c_int = -3;
const WASMTIME_ERR_UTF8: c_int = -4;

pub struct EngineState {
    pub engine: Engine,
}

struct SandboxState {
    wasi: wasmtime_wasi::p1::WasiP1Ctx,
    max_pages: usize,
}

impl wasmtime::ResourceLimiter for SandboxState {
    fn memory_growing(
        &mut self,
        _current: usize,
        desired: usize,
        _maximum: Option<usize>,
    ) -> Result<bool> {
        // desired is in bytes. max_pages is in 64KiB pages.
        Ok(desired <= self.max_pages * 65536)
    }

    fn table_growing(
        &mut self,
        _current: usize,
        _desired: usize,
        _maximum: Option<usize>,
    ) -> Result<bool> {
        Ok(true)
    }
}

impl EngineState {
    pub fn new() -> Result<Self, anyhow::Error> {
        let mut config = Config::new();
        config.wasm_component_model(true); // 开启 Component Model
        config.consume_fuel(true); // 开启燃料计费，用于大模型代码防死循环
        config.epoch_interruption(true); // 墙钟超时，见上方 EPOCH_TICK_MS 注释

        let engine = Engine::new(&config)?;

        // 全局 epoch ticker：每 EPOCH_TICK_MS 对 Engine 递增一次 epoch 计数，
        // 各 Store 通过 set_epoch_deadline(N) 独立设定"N 个 tick 后触发中断"的
        // 相对 deadline。用 Engine::weak 而非强引用持有，避免 ticker 线程
        // 人为延长 Engine 生命周期（wasmtime 官方文档对 increment_epoch 的
        // 推荐用法）；Engine 被回收后 upgrade() 返回 None，线程自行退出。
        let weak = engine.weak();
        thread::spawn(move || loop {
            thread::sleep(Duration::from_millis(EPOCH_TICK_MS));
            match weak.upgrade() {
                Some(e) => e.increment_epoch(),
                None => break,
            }
        });

        Ok(Self { engine })
    }
}

// 全局 Engine 单例，避免重复创建（开销较大）
static GLOBAL_ENGINE: OnceLock<Arc<EngineState>> = OnceLock::new();

#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_pool_init(_n: c_int) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        static INIT: std::sync::Mutex<bool> = std::sync::Mutex::new(false);
        let mut initialized = INIT.lock().unwrap();
        if !*initialized {
            if GLOBAL_ENGINE.get().is_none() {
                match EngineState::new() {
                    Ok(state) => {
                        let _ = GLOBAL_ENGINE.set(Arc::new(state));
                        *initialized = true;
                    }
                    Err(_) => panic!("EngineState::new failed"),
                }
            } else {
                *initialized = true;
            }
        }
        WASMTIME_OK
    });

    match result {
        Ok(code) => code,
        Err(_) => WASMTIME_ERR_INTERNAL,
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_init(out_err: *mut *mut c_char) -> c_int {
    let result = panic::catch_unwind(|| -> c_int {
        let mut err_code = WASMTIME_OK;
        static INIT: std::sync::Mutex<bool> = std::sync::Mutex::new(false);
        let mut initialized = INIT.lock().unwrap();

        if !*initialized {
            if GLOBAL_ENGINE.get().is_none() {
                match EngineState::new() {
                    Ok(state) => {
                        let _ = GLOBAL_ENGINE.set(Arc::new(state));
                        write_err(out_err, "");
                        *initialized = true;
                    }
                    Err(e) => {
                        write_err(out_err, &format!("Engine init failed: {}", e));
                        err_code = WASMTIME_ERR_INTERNAL;
                    }
                }
            } else {
                write_err(out_err, "");
                *initialized = true;
            }
        } else if GLOBAL_ENGINE.get().is_none() {
            write_err(out_err, "Engine not initialized");
            err_code = WASMTIME_ERR_INTERNAL;
        } else {
            write_err(out_err, "");
        }
        err_code
    });

    match result {
        Ok(code) => code,
        Err(_) => {
            write_err(out_err, "Panic in wasmtime_init");
            WASMTIME_ERR_INTERNAL
        }
    }
}

/// 释放由 wasmtime 侧分配的字符串
#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_free_string(ptr: *mut c_char) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        if !ptr.is_null() {
            unsafe { drop(CString::from_raw(ptr)) };
        }
    }));
}

/// 释放由 wasmtime 侧分配的字节切片
#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_free_bytes(ptr: *mut u8, len: usize) {
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        if !ptr.is_null() {
            unsafe { drop(Vec::from_raw_parts(ptr, len, len)) };
        }
    }));
}

/// 写入 out 指针处的错误字符串（调用方须用 wasmtime_free_string 释放）。
fn write_err(out: *mut *mut c_char, msg: &str) {
    if out.is_null() {
        return;
    }
    match CString::new(msg) {
        Ok(cs) => unsafe { *out = cs.into_raw() },
        Err(_) => {
            let safe = msg.replace('\0', "?");
            if let Ok(cs) = CString::new(safe) {
                unsafe { *out = cs.into_raw() }
            }
        }
    }
}

// MVP: Provide a ping function to verify FFI linkage
#[unsafe(no_mangle)]
pub extern "C" fn wasmtime_ping() -> c_int {
    42
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn wasmtime_execute(
    wasm_bytes: *const u8,
    wasm_len: usize,
    input_json: *const c_char,
    workspace_dir: *const c_char,
    max_pages: c_int,
    max_fuel: u64,
    network_allowed: c_int,
    max_output_bytes: c_int,
    timeout_ms: u64,
    out_json: *mut *mut u8,
    out_json_len: *mut usize,
    out_err: *mut *mut c_char,
) -> c_int {
    unsafe {
        let result = panic::catch_unwind(|| -> c_int {
            // 先取出 Arc clone
            let engine_arc = match GLOBAL_ENGINE.get() {
                Some(s) => Arc::clone(s),
                None => {
                    write_err(out_err, "Engine not initialized; call wasmtime_init first");
                    return WASMTIME_ERR_INTERNAL;
                }
            };

            let input_str = if input_json.is_null() {
                "{}"
            } else {
                match std::ffi::CStr::from_ptr(input_json).to_str() {
                    Ok(s) => s,
                    Err(_) => {
                        write_err(out_err, "Invalid UTF-8 in input_json");
                        return WASMTIME_ERR_UTF8;
                    }
                }
            };

            let bytes = std::slice::from_raw_parts(wasm_bytes, wasm_len);

            // 编译验证（在锁外执行，Engine 本身线程安全）
            let module = match Module::from_binary(&engine_arc.engine, bytes) {
                Ok(m) => m,
                Err(e) => {
                    write_err(out_err, &format!("Compile error: {}", e));
                    return WASMTIME_ERR_COMPILE;
                }
            };

            let mut linker: Linker<SandboxState> = Linker::new(&engine_arc.engine);
            if let Err(e) = wasmtime_wasi::p1::add_to_linker_sync(&mut linker, |cx| &mut cx.wasi) {
                write_err(out_err, &format!("Failed to add wasi to linker: {}", e));
                return WASMTIME_ERR_INTERNAL;
            }

            let stdin = wasmtime_wasi::p2::pipe::MemoryInputPipe::new(bytes::Bytes::from(
                input_str.to_owned(),
            ));

            let max_out = if max_output_bytes > 0 {
                max_output_bytes as usize
            } else {
                10 * 1024 * 1024
            };
            let stdout = wasmtime_wasi::p2::pipe::MemoryOutputPipe::new(max_out);

            let mut builder = WasiCtxBuilder::new();
            builder.stdin(stdin.clone()).stdout(stdout.clone());

            if network_allowed == 1 {
                builder.inherit_network();
                builder.allow_ip_name_lookup(true);
            }

            // 如果传入了工作目录，则挂载为 /workspace
            if !workspace_dir.is_null()
                && let Ok(host_path_str) = std::ffi::CStr::from_ptr(workspace_dir).to_str()
                && !host_path_str.is_empty()
            {
                let host_path = Path::new(host_path_str);
                if let Err(e) = builder.preopened_dir(
                    host_path,
                    "/workspace",
                    DirPerms::all(),
                    FilePerms::all(),
                ) {
                    write_err(out_err, &format!("Failed to preopen directory: {}", e));
                    return WASMTIME_ERR_INTERNAL;
                }
            }

            let wasi_ctx = builder.build_p1();

            // 最大内存页数（默认 256 页 = 16MB）
            let limit_pages = if max_pages > 0 {
                max_pages as usize
            } else {
                256
            };

            let state = SandboxState {
                wasi: wasi_ctx,
                max_pages: limit_pages,
            };
            let mut store = Store::new(&engine_arc.engine, state);
            store.limiter(|s| s as &mut dyn wasmtime::ResourceLimiter);

            // 燃料设定
            if let Err(e) = store.set_fuel(if max_fuel > 0 { max_fuel } else { 100_000_000 }) {
                write_err(out_err, &format!("Failed to set fuel: {}", e));
                return WASMTIME_ERR_INTERNAL;
            }

            // 墙钟超时设定（epoch interruption，独立于 fuel，见文件头注释）
            let effective_timeout_ms = if timeout_ms > 0 {
                timeout_ms
            } else {
                DEFAULT_TIMEOUT_MS
            };
            let deadline_ticks = effective_timeout_ms.div_ceil(EPOCH_TICK_MS).max(1);
            store.set_epoch_deadline(deadline_ticks);

            let instance = match linker.instantiate(&mut store, &module) {
                Ok(i) => i,
                Err(e) => {
                    write_err(out_err, &format!("Instantiate error: {}", e));
                    return WASMTIME_ERR_EXECUTE;
                }
            };

            let start = match instance.get_typed_func::<(), ()>(&mut store, "_start") {
                Ok(f) => f,
                Err(_) => {
                    write_err(out_err, "Module does not export '_start' function");
                    return WASMTIME_ERR_EXECUTE;
                }
            };

            let exec_result = start.call(&mut store, ());

            if let Err(e) = exec_result {
                // 在整条错误因果链中查找 I32Exit。WASI proc_exit 经 p1 适配器抛出时，
                // I32Exit 可能位于 std::error::Error 的 source 链中，而 anyhow 顶层
                // downcast_ref 只查 anyhow 上下文层、不遍历 source 链 → 会漏判，把正常
                // exit(0) 误当作执行错误。改用 e.chain() 遍历全链以稳健识别退出码。
                let exit_code = e
                    .chain()
                    .find_map(|cause| cause.downcast_ref::<wasmtime_wasi::I32Exit>().map(|x| x.0));
                match exit_code {
                    // exit(0) 为正常退出，视为成功
                    Some(0) => {}
                    Some(code) => {
                        write_err(out_err, &format!("Execution error: exit status {}", code));
                        return WASMTIME_ERR_EXECUTE;
                    }
                    None => {
                        // 单独识别 epoch interruption 触发的 wall-clock 超时 trap，
                        // 便于 Go 侧日志/排障区分"执行超时"与其它执行错误
                        // （Batch11 GR-7.1）。
                        let is_timeout = e.chain().any(|cause| {
                            cause
                                .downcast_ref::<wasmtime::Trap>()
                                .is_some_and(|t| *t == wasmtime::Trap::Interrupt)
                        });
                        if is_timeout {
                            write_err(
                                out_err,
                                &format!(
                                    "Execution error: wall-clock timeout after {}ms (epoch interruption)",
                                    effective_timeout_ms
                                ),
                            );
                        } else {
                            write_err(out_err, &format!("Execution error: {}", e));
                        }
                        return WASMTIME_ERR_EXECUTE;
                    }
                }
            }

            let output_bytes = stdout.contents();
            // into_boxed_slice() 保证底层分配 capacity == len，与 wasmtime_free_bytes 的
            // Vec::from_raw_parts(ptr, len, len) 严格匹配。原 shrink_to_fit() 仅尽力收缩、
            // 不保证 capacity == len，按 len 重建 Vec 释放会因容量不符触发 UB。
            let mut boxed = output_bytes.to_vec().into_boxed_slice();
            let len = boxed.len();
            let ptr = boxed.as_mut_ptr();
            std::mem::forget(boxed);
            if !out_json.is_null() {
                *out_json = ptr;
            }
            if !out_json_len.is_null() {
                *out_json_len = len;
            }
            WASMTIME_OK
        });

        match result {
            Ok(code) => code,
            Err(_) => {
                write_err(out_err, "Panic in wasmtime_execute");
                WASMTIME_ERR_INTERNAL
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::CString;

    // 构造一个 _start 导出、函数体为无限循环、不依赖任何 WASI 导入的极简 WASM
    // 模块，内联从 WAT 文本编译（wat crate，仅 dev-dependency），避免依赖外部
    // wasm32-wasip1 工具链或预置 .wasm 测试定件。
    fn infinite_loop_wasm() -> Vec<u8> {
        let wat = r#"
            (module
              (func $start (loop $l (br $l)))
              (export "_start" (func $start))
              (memory (export "memory") 1)
            )
        "#;
        wat::parse_str(wat).expect("failed to compile test WAT module")
    }

    /// 验证 epoch interruption 能在预期时间内打断 CPU-bound 死循环（不涉及
    /// host 网络调用的场景），而不是无限挂起——这是 wall-clock 超时能可靠覆盖
    /// 的场景。network_allowed=1 时挂起的阻塞网络 syscall 场景不在本测试覆盖
    /// 范围内（epoch 检查点无法打断已阻塞的 host 系统调用，见文件头注释），
    /// 该场景的最终兜底防线在 Go 侧调用方的 context.WithTimeout 包装。
    #[test]
    fn test_epoch_interruption_stops_infinite_loop() {
        // 确保 Engine 已初始化（幂等，可能已被同进程其它测试触发过初始化）。
        let mut init_err: *mut c_char = std::ptr::null_mut();
        unsafe {
            wasmtime_init(&mut init_err as *mut *mut c_char);
            if !init_err.is_null() {
                wasmtime_free_string(init_err);
            }
        }

        let wasm = infinite_loop_wasm();
        let input = CString::new("{}").unwrap();

        let mut out_json: *mut u8 = std::ptr::null_mut();
        let mut out_json_len: usize = 0;
        let mut out_err: *mut c_char = std::ptr::null_mut();

        let started = std::time::Instant::now();
        let rc = unsafe {
            wasmtime_execute(
                wasm.as_ptr(),
                wasm.len(),
                input.as_ptr(),
                std::ptr::null(),
                16,             // max_pages
                u64::MAX / 2,   // max_fuel：刻意给到近乎无限，确保先触发的是
                                // epoch 墙钟而非 fuel 耗尽，测试的才是本次要
                                // 验证的机制
                0,              // network_allowed
                1024,           // max_output_bytes
                200,            // timeout_ms：200ms
                &mut out_json,
                &mut out_json_len,
                &mut out_err,
            )
        };
        let elapsed = started.elapsed();

        let err_msg = unsafe {
            if out_err.is_null() {
                String::new()
            } else {
                let s = std::ffi::CStr::from_ptr(out_err)
                    .to_string_lossy()
                    .into_owned();
                wasmtime_free_string(out_err);
                s
            }
        };
        if !out_json.is_null() {
            unsafe { wasmtime_free_bytes(out_json, out_json_len) };
        }

        assert_eq!(
            rc, WASMTIME_ERR_EXECUTE,
            "expected execute error (wall-clock timeout), got rc={} err={}",
            rc, err_msg
        );
        assert!(
            err_msg.contains("timeout") || err_msg.contains("epoch"),
            "expected wall-clock timeout error message, got: {}",
            err_msg
        );
        // 应在远小于"无限循环"的时间内返回；给 5x timeout_ms 的宽松上限覆盖
        // CI 环境调度抖动，同时足以证明不是无界挂起。
        assert!(
            elapsed < Duration::from_millis(1000),
            "epoch interruption did not fire within expected wall-clock bound: {:?}",
            elapsed
        );
    }
}
