# ADR-0063：llama_infer 控制面/计算面分离（协作式取消 + status 无锁镜像）

- **状态**: Accepted（已执行）
- **日期**: 2026-07-22
- **决策者**: MrLaoLiAI
- **相关模块**: `rust/substrate/src/llama_infer/mod.rs`（Tier-1 本地推理）
- **承接**: 审计发现 GD-11.1（`local_playground/reports/gemini-review-design.md`）；工单 WO-10（`local_playground/upgrade/gemini-remediation.md`）

## 背景

`generate()` 在整个 token 生成长循环（可长达数十秒）内独占全局互斥锁 `STATE: Mutex<Option<ModelHolder>>`。后果：推理期间同进程其它调用——`status()` 监控、`unload_model()` 卸载、`evict_kv_cache()` 回收——全部因争用 `STATE` 而无限期阻塞，宿主侧 Cancellation Context 失去实际资源释放/打断作用（GD-11.1）。

## 决策

不改"单槽位、单模型、推理串行"这一既有取舍（`STATE` 仍序列化 `generate` 与 `load`/`unload`/`evict` 的计算与生命周期操作；llama.cpp 底层 C API 本就要求同一 context 非并发调用）。仅拆出两条不需要独占计算锁的能力：

### 1. 协作式取消（ABORT_FLAG）

`static ABORT_FLAG: AtomicBool`。`generate()` 的 token 循环每步检查，命中即以 `finish_reason="abort"` 退出并释放 `STATE`；`unload_model()` 在取锁前先置位 flag，使正在运行的 `generate()` 尽快让出锁，卸载不再无限等待。`load_model()`/`generate()` 入口复位 flag。

### 2. status 无锁只读镜像（STATUS）

`static STATUS: RwLock<Option<StatusSnapshot>>`，缓存 `status()` 所需的加载后不变字段（`path`/`n_ctx`/`n_ctx_train`/`n_embd`/`n_gpu_layers`）。`status()` 只读此镜像，与持有 `STATE` 的推理无锁竞争，推理期监控不再阻塞。

**一致性保证**：镜像写点仅两处，且均在 `STATE` 锁保护下发生——加载成功后写入、卸载（含热切换的旧模型驱逐）时清空。故 `STATUS` 不会与 `STATE` 漂移。`RwLock` 中毒时 `status()` 退化为 `not-loaded`、`set_status_snapshot` 静默跳过，监控镜像故障不阻断主流程。

## 边界（本 ADR 明确不做）

- `evict_kv_cache()` 变更模型内部状态，**仍需** `STATE` 独占锁，推理期调用仍会等待——这是正确性要求（不能在生成中途清 KV），非缺陷。
- 不引入按 token 流式取消的宿主回调、不引入多模型并行槽位——超出 Tier-1 单机单模型定位，另行立项。
- `StatusResponse` 未新增 `generating` 字段，避免触动 Go 侧 FFI 解码契约；如需"是否推理中"信号再行扩展。

## 影响

- 新增全局 `ABORT_FLAG`/`STATUS` 均为 Rust 侧 `AtomicBool`/`RwLock`（非 Go 的包级可变变量，不受 ADR-0001 约束）。
- `dispatch.rs` FFI 导出签名不变；Go 侧无改动。
- `cargo check` 通过；模块既有单测（`test_status_when_not_loaded` 等）语义不变。
