# ADR-0011: purego（零 CGO）作为 Go→Rust FFI 桥接方式（含原 ADR-0005/0030/0063，含 Tree-sitter 例外）

- **状态**: Accepted（已执行）| **日期**: 2026-05-16（扩展 2026-06-25~07-22，合并 2026-07-28）| **模块**: M2/M11 `internal/security` / `internal/store` / `internal/ffi` / `rust/substrate/`

## 决策一：purego 桥接（原决策）

`cedar_ffi.go`（Cedar 策略引擎）+ `surreal_store.go`（SurrealDB-Core，21 个 FFI 函数）经 purego 桥接 Rust dylib，零 CGO，引入 ABI 版本协议（`substrate_abi_version()`：major 不匹配 panic，minor 不匹配 warn+continue）。字节流生命周期约定：Go→C 用 ptr+len（`*uint8`+`uintptr`）传递 `[]byte`，空 slice 经 `bytePtrOrNil()` 返回 nil+0；C→Go 立即拷贝立即调 `*_free_*`；不使用 null-terminated 约定。

**受限例外（原 ADR-0034）**：`internal/knowledge/` 的代码切分（chunking）路径允许依赖 CGO（`go-tree-sitter`）——不在请求热路径（仅离线/后台知识库索引），物理隔离（`chunker_cgo.go`/`chunker_nocgo.go` build tag 双实现，`CGO_ENABLED=0` 自动降级至字符串匹配 fallback），失败安全（parse 失败回退 `fallbackChunk`）。仅限该离线索引路径，在线请求路径（agent/gateway 等）不得援引此例外。

## 决策二：Tier-2 语义嵌入 SIMD 桥接（原 ADR-0030）

引入 `OpenAICompatibleEmbeddingAdapter`（支持 DeepSeek/OpenAI/Ollama）对 Ambient Skill 与 Extension Catalog 做语义向量相似度检索（Tier-2），替代此前仅有的 Tier-1 关键词/Token 重叠匹配；Tier-1 保留作为降级路径。向量运算用 Rust SIMD `VecCosineF32`，经 purego 桥接（零 CGO，遵从决策一）。Extension Catalog 向量化经 `EmbeddingIndexer` 挂载 marketplace sync 周期异步执行，不阻塞。

## 决策三：llama_infer 控制面/计算面分离（原 ADR-0063）

不改"单槽位单模型推理串行"取舍（`STATE: Mutex` 仍序列化 `generate` 与 `load`/`unload`/`evict`）。仅拆出两条不需独占计算锁的能力：

1. **协作式取消**：`static ABORT_FLAG: AtomicBool`，`generate()` token 循环每步检查，命中即以 `finish_reason="abort"` 退出释放锁；`unload_model()` 取锁前先置位，避免无限期等待。
2. **status 无锁只读镜像**：`static STATUS: RwLock<Option<StatusSnapshot>>`，缓存加载后不变字段，`status()` 只读镜像，与推理无锁竞争。写点仅在 `STATE` 锁保护下的加载成功/卸载两处，不会漂移。

`evict_kv_cache()` 仍需独占锁（不能在生成中途清 KV，是正确性要求非缺陷）。不引入按 token 流式取消或多模型并行槽位——超出 Tier-1 单机单模型定位。

## 反例守护

拒绝为简化代码改回 CGO——违反零 CGO 交叉编译/单二进制分发目标，任何新增 Rust 互操作点一律走 purego。拒绝将 Tree-sitter CGO 例外扩大到在线请求路径。拒绝 Tier-2 路径硬依赖 Ollama——VPS 无 GPU 场景需保持 provider-agnostic。

## 引用代码

`internal/security/policy/cedar_ffi.go`、`internal/store/surreal_store.go`、`internal/knowledge/{chunker_cgo,chunker_nocgo}.go`、`rust/substrate/src/lib.rs`（`vec_cosine_f32`，ABI minor=2）、`internal/ffi/vec_ops.go`、`internal/llm/adapter/embedding.go`、`rust/substrate/src/llama_infer/mod.rs`

> 2026-08-09 追记：重新评估触发条件——① Tree-sitter CGO 例外若被发现已悄悄用于
> 在线请求路径（违反"仅限离线索引路径"边界），须立即整改而非追认扩大范围；
> ② 若交叉编译/单二进制分发目标本身被放弃（如改为容器分发），purego 零 CGO
> 纪律的前提才失效，重议整体桥接方式。
