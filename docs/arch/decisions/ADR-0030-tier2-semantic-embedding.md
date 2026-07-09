# ADR-0030: Tier-2 语义嵌入升级（OpenAI 兼容 Embedding + Rust SIMD）

- **状态**: Accepted
- **日期**: 2026-06-25
- **决策者**: 架构组
- **相关模块**: M13 Gateway（Ambient Skill 注入）/ `internal/llm/adapter/` / `internal/ffi/` / `rust/substrate/`
- **实现详情**: [M13-bis §8.3](../M13-bis-Extension-Registry.md) | [ADR-0011](./ADR-0011-cgo-to-purego-migration.md)（purego 零 CGO 方针）

## 上下文

此前 Polaris 依赖 Tier-1（关键词匹配 + Token 重叠）评估 Ambient Skill 和 Extension Catalog 搜索的相关性，导致语义语义相近但关键词不同的短语无法匹配（语义鸿沟）。

需要一种语义嵌入方案（Tier-2），在不强制引入本地重度计算依赖的前提下提升相关性和搜索质量。

## 决策

**引入 Tier-2 语义嵌入，同时保留 Tier-1 作为降级路径。**

**1. Tier-2 语义嵌入集成**

引入 `OpenAICompatibleEmbeddingAdapter`，支持 DeepSeek / OpenAI / Ollama 等多种 Provider 的嵌入向量，对技能检索和 Extension 搜索进行语义向量相似度度量。

**2. Rust FFI 向量运算（零 CGO）**

使用 Rust SIMD 优化实现 `VecCosineF32`，通过 `internal/ffi/vec_ops.go` 以 `purego` 桥（零 CGO，遵从 ADR-0011）桥接至 Go，大幅加速向量数据集上的余弦相似度计算。

**3. 优雅降级（Graceful Degradation）**

持续支持 Tier-1 作为 fallback 路径。若 Tier-2 Embedding Provider 不可用或无法获取向量，系统无缝回退到 Tier-1 Token 匹配，不中断用户请求。

**4. 异步向量化（Async Computation）**

Extension Catalog 向量化通过 `EmbeddingIndexer` 挂载到 marketplace sync 周期中异步执行，不阻塞市场同步进程。

## 后果

- **正向**: 自然语言查询对 Ambient Skill 及 Extension Catalog 的匹配精度显著提升；用户无需本地 GPU 即可接入云端嵌入 API；Rust SIMD purego 桥零 CGO 开销
- **负向**: 冷启动前 Extension Catalog 无向量（首次 marketplace sync 后异步填充）；云端 API 不可用时静默降级至 Tier-1
- **反例守护**:
  - 未来如有人提议"将 `vec_cosine_f32` 改用 CGO 调用"——本 ADR + ADR-0011 联合拒绝（违反 purego 零 CGO 方针）
  - 未来如有人提议"在 Tier-2 路径引入 Ollama 硬依赖"——本 ADR 拒绝（需保持 provider-agnostic，VPS 无 GPU 场景不可强依赖本地推理）

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 仅支持 Ollama Embedding | 引入本地推理硬依赖；Tier-0 VPS 无 GPU 不可运行 |
| CGO 桥接 Rust `vec_cosine_f32` | 违反 ADR-0011（purego 零 CGO 方针），增加构建复杂度 |
| 进程外 Python Embedding Sidecar | 进程间通信延迟不可接受；增加部署复杂度；违反单二进制约束 |

## 引用代码

- `rust/substrate/src/lib.rs`（`vec_cosine_f32` SIMD 实现，ABI minor=2）
- `internal/ffi/vec_ops.go`（Go purego 桥，含纯 Go fallback）
- `internal/ffi/dylib.go`（`ExpectedABIMinor=2`）
- `internal/llm/adapter/embedding.go`（`OpenAICompatibleEmbeddingAdapter`）
- `internal/gateway/server/chat/sse.go`（`buildAmbientSkillsSection` Tier-2 路径）
- `internal/gateway/server/plugin/embedding_indexer.go`（Extension Catalog 向量预计算）
- `cmd/polaris/boot_substrate.go`（embedding 初始化路由）
- `configs/defaults.toml §[embedding]`
- `docs/arch/M13-bis-Extension-Registry.md §8.3`（Ambient Skill 注入设计）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-25 | 初稿，Accepted（Gemini 实现 + 审查修正：boot_substrate.go autoConf 隔离、purego 描述纠错）|
| 2026-07-09 | 全文翻译为中文；补完整标准 header 字段 |
