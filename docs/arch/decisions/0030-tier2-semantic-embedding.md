# ADR 0030: Tier 2 Semantic Embedding Upgrade

## Status
Accepted (2026-06-25)

## Context
Previously, the Polaris system relied on Tier 1 (token overlap via keyword matching) for evaluating the relevance of Ambient Skills and Extension Catalog searches. This created a semantic gap where conceptually similar phrases but different keywords would fail to match. To improve relevance and search quality without forcing local heavy compute dependencies, we needed a semantic embedding approach (Tier 2).

## Decision
1. **Tier 2 Semantic Embedding Integration**: Introduce `OpenAICompatibleEmbeddingAdapter` (to support DeepSeek/OpenAI embeddings in addition to Ollama) and use semantic vectors to measure the relevance of skills and extension searches.
2. **Rust FFI for Vector Operations**: Implement `VecCosineF32` using Rust SIMD optimizations, bridged to Go via `purego`（零 CGO，ADR-0011）through `internal/ffi/vec_ops.go`, dramatically accelerating cosine similarity checks on vector datasets.
3. **Graceful Degradation**: Continue to support Tier 1 as a fallback mechanism. If the Tier 2 embedding provider is unavailable or vectors cannot be retrieved, the system seamlessly falls back to Tier 1 token matching.
4. **Asynchronous Computation**: Extension Catalog vectorization occurs asynchronously via `EmbeddingIndexer` hooked into the sync cycle, preventing delays to the marketplace synchronization processes.

## Consequences
- **正向**: 自然语言查询对 Ambient Skill 及 Extension Catalog 的匹配精度显著提升；用户无需本地 GPU 即可接入云端嵌入 API；Rust SIMD purego 桥零 CGO 开销。
- **负向**: 冷启动前 Extension Catalog 无向量（首次 marketplace sync 后异步填充）；云端 API 不可用时静默降级 Tier 1。
- **反例守护**: 禁止将 `vec_cosine_f32` 改用 CGO 调用（违反 ADR-0011）；禁止在 Tier 2 路径引入 Ollama 硬依赖（需保持 provider-agnostic）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| Ollama-only embedding | 引入本地推理硬依赖，Tier 0 VPS 无 GPU 不可运行 |
| CGO 桥接 Rust vec_cosine | 违反 ADR-0011（purego 零 CGO 方针），增加构建复杂度 |
| 进程外 Python embedding sidecar | 进程间通信延迟不可接受，增加部署复杂度 |

## 引用代码

- `rust/substrate/src/lib.rs`（`vec_cosine_f32` SIMD 实现，ABI minor=2）
- `internal/ffi/vec_ops.go`（Go purego 桥，带纯 Go fallback）
- `internal/ffi/dylib.go`（`ExpectedABIMinor=2`）
- `internal/llm/adapter/embedding.go`（`OpenAICompatibleEmbeddingAdapter`）
- `internal/gateway/server/chat/sse.go`（`buildAmbientSkillsSection` Tier 2 路径）
- `internal/gateway/server/plugin/embedding_indexer.go`（Extension Catalog 向量预计算）
- `cmd/polaris/boot_substrate.go`（embedding 初始化路由）
- `configs/defaults.toml §[embedding]`
- `docs/arch/M13-bis-Extension-Registry.md §8.3`（Ambient Skill 注入设计）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Gemini 实现 + 审查修正（boot_substrate.go autoConf 隔离、purego 描述纠错） |
