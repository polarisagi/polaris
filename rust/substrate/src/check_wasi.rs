// check_wasi.rs — WASI 环境自检辅助（内部测试用）
//
// 验证 wasmtime + WASI P1 运行时可正确初始化，供 CI 健康检查调用。
// 注意：此模块不导出公开 FFI 函数，不被 lib.rs 引用，仅作编译期验证。
// 如需运行时自检，请使用 wasmtime_ping() FFI 函数。

#![allow(dead_code)]

use std::path::Path;
use wasmtime::*;
use wasmtime_wasi::p1::{add_to_linker_sync, WasiP1Ctx};
use wasmtime_wasi::{DirPerms, FilePerms, WasiCtxBuilder};

struct CheckState {
    wasi: WasiP1Ctx,
    max_pages: usize,
}

impl ResourceLimiter for CheckState {
    fn memory_growing(
        &mut self,
        _current: usize,
        desired: usize,
        _maximum: Option<usize>,
    ) -> Result<bool> {
        Ok(desired <= self.max_pages)
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

/// check 验证 wasmtime + WASI 可正常初始化（不执行任何 Wasm 模块）。
/// 返回 Ok(()) 表示运行时可用；Err 表示环境异常。
pub fn check() -> Result<()> {
    let config = Config::new();
    let engine = Engine::new(&config)?;
    let mut linker: Linker<CheckState> = Linker::new(&engine);
    add_to_linker_sync(&mut linker, |s| &mut s.wasi)?;

    let mut builder = WasiCtxBuilder::new();
    builder.preopened_dir(
        Path::new("/tmp"),
        "/workspace",
        DirPerms::all(),
        FilePerms::all(),
    )?;

    let wasi = builder.build_p1();
    let state = CheckState {
        wasi,
        max_pages: 256,
    };

    let mut store = Store::new(&engine, state);
    store.limiter(|s| s as &mut dyn ResourceLimiter);

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_wasi_init_ok() {
        assert!(check().is_ok(), "wasmtime+WASI should initialize without error");
    }
}
