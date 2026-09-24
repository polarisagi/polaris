# ADR-0099: Embedding 调度——交互通道独立、下游调用有界、Embedder 接口携带 ctx

- **状态**: Accepted
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M1（internal/llm）/ M2（internal/store/search）/ M5 / M10 / M4

## 上下文

2026-09-22 起日志持续出现 `SyncBatcherAdapter: embed failed ... context deadline exceeded`（累计 580+ 次），交互式检索、Agent 召回每次卡满 30s。实测（2026-09-25）：本机 ollama `qwen3-embedding:4b` 单条 0.09s、20 条 2.3s、**100 条 13.8s**；后端本身健康。根因在 `EmbeddingBatcher`：

1. **队头阻塞**：唯一的定时器 goroutine 串行 flush，High/Low 混装一批（High ≤80 + Low 补齐至 100）。后台回填（扩展目录、重索引）持续灌满 Low，交互请求须等在途整批（~14s）再与之同批（~14s），合计逼近 30s。
2. **下游调用无界**：`flushBatch` 用批处理器的长生命周期 ctx 调 embedFn，后端挂起即冻结整条队列。
3. **`search.Embedder` 接口无 ctx**：`Embed(text) []float32`，适配器内部固定 `Background+30s`，调用方截止时间传不下去——Agent 召回只能在边界"放弃等待"（ADR-0098 决策五），无法真正取消。

## 决策

**High 与 Low 各自独立 flush 循环；Low 单批上限收紧以约束其在后端的占用时长；每次下游调用有界；Embedder 接口携带 ctx。**

- **双通道独立**：High、Low 各一个 flush 循环与各自在途调用，High 永不在 Polaris 内排在 Low 批次之后。High 单批上限 `maxBatchSize`（交互请求量小，实际远小于上限）。
- **Low 单批上限 `low_max_batch_size=8`**：后端（ollama）通常串行处理，High 仍可能在后端排到一个在途 Low 批之后。实测单条约 0.12s（20 条 2.34s），8 条约 1s，使交互嵌入最坏排队稳定落在 Agent 召回的 3s 预算内（初稿取 16 ≈ 1.9s，部署后仍有调用方预算到期，2026-09-25 复核收紧）。回填吞吐下降可接受（后台工作）。
- **调用方放弃 ≠ 嵌入失败**：ctx 贯穿后，调用方截止先到按 Debug 记录（预期降级），仅下游超时/出错记 WARN，避免把正常降级报成故障。
- **下游调用超时 `call_timeout_s=30`**：每次 embedFn 调用派生独立超时，后端挂起只让该批失败，不冻结队列。
- **`search.Embedder.Embed(ctx, text)`**：调用方 ctx 贯穿到 HTTP 请求；适配器仍叠加 30s 上限防调用方无截止。无 ctx 的调用点显式传 `context.Background()` 并在注释说明为何无上游截止。
- 原"Low 等待 >100ms 升 High""20% 槽位防饥饿"在双通道下不再需要（两通道互不挤占），随之删除。

## 后果

- **正向**: 交互嵌入延迟回到后端单次耗时量级（亚秒）；Agent 召回可被真正取消；后端挂起不再冻结全部检索。
- **负向**: 后台回填吞吐降低（每批 16 条）；接口签名变更波及 5 个实现、15 处调用。
- **反例守护**: 提议"High/Low 合并一批以提升吞吐"——即本 ADR 所修队头阻塞；提议"Low 批次放大到 100"须同时给出后端并行能力证据（交互等待 ≤2s）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 仅调大适配器 30s 超时 | 掩盖队头阻塞，交互延迟不变 |
| 回填改为同步直连后端、绕开批处理器 | 失去去重与背压，回填洪峰直接压垮后端 |
| High 请求抢占在途 Low 批（取消重发） | 浪费已完成的后端计算，且 ollama 取消语义不可靠 |

## 引用代码

- `internal/store/search/embedding_batcher.go`（双通道）
- `internal/store/search/batcher_adapter.go`、`semantic_cache.go`（Embedder 接口）
- `internal/llm/dynamic_embedder.go`、`internal/llm/adapter/embedding.go`（实现）
- `docs/arch/M01-Inference-Runtime.md §6.1`

## 重新评估触发条件

- 后端支持并行嵌入（如 ollama `OLLAMA_NUM_PARALLEL>1` 且实测 High 在 Low 批并发下 p95 < 1s）→ 可放大 `low_max_batch_size`。
- 回填积压持续 > 1h（`polaris_storage_*` 回填指标）→ 重估 Low 批上限。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-25 | 初稿 |
| 2026-09-25 | 复核：Low 批上限 16 → 8（部署实测调用方 3s 预算仍被在途 Low 批吃掉）；调用方放弃改记 Debug |
