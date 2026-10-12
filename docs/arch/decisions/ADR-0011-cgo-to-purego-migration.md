# ADR-0011: purego（零 CGO）作为 Go→Rust FFI 桥接方式

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0005/0030/0034/0063 ｜ 模块：M2/M11 `internal/security` / `internal/store` / `internal/ffi` / `rust/substrate/`

## 决策一：purego 桥接 + Tree-sitter 受限例外

`cedar_ffi.go`（Cedar）与 `surreal_store.go`（SurrealDB-Core）经 purego 桥接 Rust dylib，零 CGO。
- ABI 版本协议：`substrate_abi_version()`，major 不匹配 panic，minor 不匹配 warn+continue（当前 minor=4）
- 字节流约定：Go→C 用 ptr+len（`*uint8`+`uintptr`），空 slice 经 `bytePtrOrNil()` 返回 nil+0；C→Go 立即拷贝并调 `*_free_*`；新增符号不用 null-terminated（存量 `*const c_char` 入口见 `internal/protocol/ffi-abi.md`）
- **例外（原 ADR-0034）**：`internal/knowledge/` 代码切分（离线/后台知识库索引）允许 CGO（`go-tree-sitter`）；`chunker_cgo.go`/`chunker_nocgo.go` build tag 双实现，`CGO_ENABLED=0` 降级字符串匹配，parse 失败回退 `fallbackChunk`。在线请求路径（agent/gateway 等）不得援引。

## 决策二：Tier-2 语义嵌入 SIMD 桥接（原 ADR-0030）

`OpenAICompatibleEmbeddingAdapter`（DeepSeek/OpenAI/Ollama）对 Ambient Skill 与 Extension Catalog 做向量检索（Tier-2），Tier-1 关键词匹配保留为降级路径。向量运算用 Rust SIMD `VecCosineF32`，经 purego 桥接。Catalog 向量化由 `EmbeddingIndexer` 挂在 marketplace sync 周期异步执行。

## 决策三：llama_infer 控制面/计算面分离（原 ADR-0063）

保持"单槽位单模型推理串行"（`STATE: Mutex` 序列化 `generate` 与 `load`/`unload`/`evict`），仅拆出两条无需独占锁的能力：
1. **协作式取消**：`ABORT_FLAG: AtomicBool`，`generate()` 每步检查，命中以 `finish_reason="abort"` 退出；`unload_model()` 取锁前先置位。
2. **status 无锁镜像**：`STATUS: RwLock<Option<StatusSnapshot>>`，写点仅在 `STATE` 锁内的加载成功/卸载两处。

`evict_kv_cache()` 仍需独占锁（生成中途清 KV 是正确性要求）。不引入按 token 流式取消或多模型并行槽位。

## 决策四：native_sandbox V1 导出保留（2026-08-12）

Go 侧 2026-07-02 起统一走 `native_sandbox_exec_v2`，V1 `native_sandbox_exec` 生产零绑定。裁定**保留 Rust 侧 V1 导出**，登记 `tools/baselines/deadcode-allowlist.txt`，不删。
理由：V1 是整条平行实现链（`native_sandbox_exec` → `dispatch_sandbox` → 5 个平台 `exec_*`，跨 `dispatch.rs`/`seatbelt.rs`/`bwrap.rs`/`fallback.rs`）并带集成测试 `test_ffi_exec_echo`；摘除回归面大于收益。
边界：仅豁免 V1 这一个符号；其余 Rust C-FFI 导出均有真实 Go 绑定，不得援引本条批量登记白名单（2026-08-12 批量登记的真因是 `tools/ffi_symbol_check.go` 提取正则失配，已修并加"绑定数为 0 即失败"自检）。

## 反例守护

- 为简化代码改回 CGO — 违反零 CGO 交叉编译/单二进制目标，新增 Rust 互操作点一律 purego
- 将 Tree-sitter 例外扩大到在线请求路径
- Tier-2 路径硬依赖 Ollama — VPS 无 GPU 场景需 provider-agnostic

## 重新评估触发条件

- Tree-sitter CGO 被发现用于在线请求路径 → 立即整改，不追认
- 交叉编译/单二进制分发目标被放弃（如改容器分发）→ 才重议整体桥接
- V1 与 V2 同一 `SandboxContext` 结果不一致；`dispatch_sandbox` 降级规则与 V2 漂移；Tier-0 dylib 体积成为约束 → 重议删除 V1

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 初版（决策一） |
| 2026-06-25~07-22 | 决策二（ADR-0030）、决策三（ADR-0063）、Tree-sitter 例外（ADR-0034） |
| 2026-07-28 | 合并 ADR-0005/0030/0063 |
| 2026-08-12 | 决策四：V1 导出保留 |

## 引用代码

`internal/security/policy/cedar_ffi.go`、`internal/store/surreal_store.go`、`internal/knowledge/chunker_cgo.go`、`internal/knowledge/chunker_nocgo.go`、`rust/substrate/src/lib.rs`、`internal/ffi/vec_ops.go`、`internal/llm/adapter/embedding.go`、`rust/substrate/src/llama_infer/mod.rs`、`internal/tool/sandbox/rust_native_sandbox.go`
