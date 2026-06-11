use std::path::Path;
use wasmtime::*;
use wasmtime_wasi::p1::{add_to_linker_sync, WasiP1Ctx};
use wasmtime_wasi::{WasiCtxBuilder, DirPerms, FilePerms};

struct MyState {
    wasi: WasiP1Ctx,
    max_pages: usize,
}

impl ResourceLimiter for MyState {
    fn memory_growing(&mut self, current: usize, desired: usize, _maximum: Option<usize>) -> Result<bool> {
        Ok(desired <= self.max_pages)
    }
    fn table_growing(&mut self, current: usize, desired: usize, _maximum: Option<usize>) -> Result<bool> {
        Ok(true)
    }
}

pub fn check() -> Result<()> {
    let mut config = Config::new();
    let engine = Engine::new(&config)?;
    let mut linker: Linker<MyState> = Linker::new(&engine);
    add_to_linker_sync(&mut linker, |s| &mut s.wasi)?;
    
    let mut builder = WasiCtxBuilder::new();
    builder.preopened_dir(Path::new("/tmp"), "/workspace", DirPerms::all(), FilePerms::all())?;
    
    let wasi = builder.build_p1();
    
    let state = MyState {
        wasi,
        max_pages: 256,
    };
    
    let mut store = Store::new(&engine, state);
    store.limiter(|s| s as &mut dyn ResourceLimiter);
    Ok(())
}
