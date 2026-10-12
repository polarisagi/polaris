# ADR-0109: Embedding 与本地推理：进程内 ONNX 嵌入器默认、本地推理显式启用、调度有界

> 状态：Accepted（`backend="llama_server"` 未接线，见决策二后果） ｜ 首次：2026-09-25（原 0099）/ 2026-10-02（原 0109） ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0099
> 取代：ADR-0062 中「Tier1 默认 embedding/rerank = Qwen3-0.6B GGUF + llama.cpp」选型；`probe` Embedding 内存阶梯自动升档（Local→HQ→Ultra→Max）

## 决策一：默认不启动任何本地推理子进程（原 ADR-0109 决策）

**向量化默认用进程内 ONNX 小模型；llama-server/Ollama 降为「用户显式配置才启用」。** 触发事实：2026-10-02 Intel i9/16GB 实测 `ollama serve → llama-server --embedding`（qwen3-embedding:4b）877% CPU；根因为门控只看空闲内存不看算力、失败无限重试（EmbedBatch 超时 → 5min reindexer/插件回填反复重来）、优先级与注释相反（配了远程仍拉起本地 Ollama）。文本生成（会话/情景记忆、摘要、GraphRAG 抽取）一律走路由到的对话模型，本地推理只承担向量化。

### D1 选择顺序（`cmd/polaris/boot_embedding.go` chooseEmbedding 为唯一实现）
1. `[embedding].base_url` 非空 → 远程 OpenAI 兼容 API，最高优先，绝不拉起本地 Ollama。
2. `backend = "ollama" | "llama_server"` 且 `model` 非空 → 显式本地服务（D4）。
3. `backend = "auto" | "onnx"` 且 ONNX 可用 → 进程内 ONNX 嵌入器（D2）。
4. 不可用（平台无库 / macOS < 15.5 / 基准判不支持）→ 不挂载嵌入器，检索降级 FTS。

`[embedding].backend = "auto"|"onnx"|"ollama"|"llama_server"|"none"`，默认 `auto`（= 1→3→4）；`none` 显式关闭向量化。

### D2 进程内嵌入器（默认，与语音解耦）
- 不复用 `audiorun` 的资产/slot/版本钉；自带独立钉版本的 `libonnxruntime`（purego 调 ORT C API，ADR-0011，无 cgo、无子进程），独立下载与 sha256。代价：磁盘多一份 ORT（约 20–35MB）。
- 模型分档（模型文件按需下载到 `models_dir`，不内嵌二进制）：

| 档位 | 模型 | 规模/维度 | 选用条件 |
|---|---|---|---|
| 平衡（默认） | EmbeddingGemma-300M ONNX int8，Matryoshka 截断 512 维 + 重新 L2 归一化 | 308M，常驻 < 200MB；Gemma SentencePiece | 基准单条 p95 ≤ 80ms |
| 轻量 | bge-small-zh-v1.5 int8 | 24M / 512 维；BERT WordPiece | 平衡档不达标且 p95 ≤ 30ms，或总内存 < 1.5GB / 可用 < 600MB |
| 远程 | `[embedding].base_url` | — | 质量最高，本地零 CPU |

- **两档统一 512 维（硬约束）**：切档不改 HNSW `DIMENSION`，只按 `embed_model_version` 重嵌；不同模型向量即使同维也不可混检（D5）。
- 运行形态不作判据（不识别桌面/VPS）；只看内存下限 + 基准。两档均不达标 → FTS。
- 档位结论持久化到 `preferences` 键 `embed.tier_bench`（`embedonnx.PrefKeyTierBench`），重启复用；仅模型资产变更、用户手动重评估或 `retry_on_start` 时重测。基准沿用「空闲窗口 + Contended 不落定论」判定（`internal/llm/embedonnx/bench.go` DecideTier）。
- 手动覆盖 `[embedding].onnx_model = "auto"|"embeddinggemma"|"bge-small-zh"`。
- 生命周期：懒加载（首次检索/写入记忆）、活跃期常驻、**空闲 10 分钟卸载**（`DefaultIdleUnloadTimeout`）。线程上限（硬约束）：前台 `intra_op = min(2, 逻辑核/4)`，后台回填 1 线程低优先级，`inter_op = 1`。

### D3 硬件探测只决定「能否/建议」，不决定「自动启动」
`probe.AcceleratorInfo`（`internal/observability/probe/accelerator.go`）：darwin/arm64 统一内存 ≥ 16GB 可加速；darwin/amd64 一律否；linux/windows NVIDIA 显存 ≥ 6GB 可加速。Embedding 档位不再按空闲内存升档（删除 `FeatureHQEmbedding/Ultra/Max`），`FeatureLocalEmbedding` = 「ONNX 嵌入器可用」。可加速机器仅由前端设置页**提示**可升级到 Qwen3-Embedding，用户确认写入 D1 第 2 条配置。

### D4 llama-server / Ollama 仅显式配置
- 不再默认 `EnsureOllama`；仅 `[embedding].backend` 或 `[providers]` 显式指向本地服务时才安装/启动（`cmd/polaris/boot_embedding.go` setupOllamaEmbedding）。
- 显式启用护栏：`-t` 线程上限（默认物理核/2）、子进程 nice +10、`-np 1`（`internal/llm/ollamamgr/manager.go`）；退出或连续失败走 D5 熔断。外部已装 Ollama 经 `base_url` 指向，不管理其生命周期。
- **现状（以代码为准）**：`validateEmbeddingConfig` 对 `backend="llama_server"` 显式拒绝（未接线，不静默降级）；仅 `ollama` 后端可用。

### D5 后台向量任务护栏（与后端无关）
- online reindexer 与插件向量回填：连续失败 3 次 → 指数退避 5m→30m→2h→停止，直至后端变更或手动触发（`retrieval.EmbedBackoff`，`cmd/polaris/boot_memory.go`）；状态暴露到 `/v1/system/status`。
- 后台批走 Low 通道（决策二），单批超时 = 交互通道 3 倍。
- 模型 ID + 维度随向量落库；后端/模型变化时低速重建（每轮限量），旧模型向量重建完成前不参与混检，期间 FTS 兜底。
- HNSW 维度迁移：启动时比对 `meta:vector_index` 已落库维度（无元数据则抽样推断）与期望维度，不一致则删旧向量与索引按新维度重建并触发重嵌（`rust/substrate/src/surreal_store/mod.rs`）。

### D6 本地对话模型：删除自动注册
删除 `TierLocalModel` 驱动的 `ollama-local` 自动注册（现无非测试注册点）；本地对话模型仅在 `[providers]` 显式添加（带明确 role）。`llama-local`（FFI）同理。QLoRA/PRM/Steering 等依赖本地 Ollama 的适配器门控条件由「硬件 Tier」改为「本地推理后端已显式启用」（`localBackendEnabled`）。`probe.TierLocalModel` 仍存在并供 `auto_config.go` 填充 `LocalModelID`，不触发注册。

**后果**：默认无常驻本地推理子进程；默认向量质量低于 qwen3-embedding:4b（UI 提示配置远程 embedding）；旧 2560 维向量低速重建期间 FTS 为主；macOS < 15.5 无本地嵌入器。

## 决策二：Embedding 调度——双通道独立、下游调用有界、Embedder 携带 ctx（原 ADR-0099 决策）

触发事实：2026-09-22 起 `SyncBatcherAdapter: embed failed ... context deadline exceeded` 累计 580+ 次；后端健康（ollama 4b 单条 0.09s、100 条 13.8s）。根因：High/Low 混装一批 + 单 goroutine 串行 flush 造成队头阻塞；`flushBatch` 用长生命周期 ctx 调 embedFn，后端挂起冻结整条队列；`search.Embedder.Embed(text)` 无 ctx，调用方截止传不下去（ADR-0098 决策五只能在边界放弃等待）。

- **双通道独立**：High、Low 各一个 flush 循环与各自在途调用，High 永不在 Polaris 内排在 Low 批之后（`internal/store/search/embedding_batcher.go`）。
- **`embed.low_max_batch_size = 8`**：后端串行处理，8 条约 1s，使交互嵌入最坏排队落在 Agent 召回 3s 预算内（初稿 16 ≈ 1.9s，部署后仍超预算，2026-09-25 收紧）。
- **`embed.call_timeout_s = 30`**：每次 embedFn 调用派生独立超时，后端挂起只让该批失败。
- **`search.Embedder.Embed(ctx, text)`**：调用方 ctx 贯穿到 HTTP 请求，适配器仍叠加 30s 上限；无上游截止的调用点显式传 `context.Background()` 并注释原因。
- **调用方放弃 ≠ 嵌入失败**：调用方截止先到记 Debug，仅下游超时/出错记 WARN。
- 原「Low 等待 >100ms 升 High」「20% 槽位防饥饿」在双通道下删除。

**后果**：交互嵌入延迟回到后端单次耗时量级；Agent 召回可真正取消；回填吞吐下降（每批 8 条，后台可接受）。阈值 SSoT：`internal/config/thresholds.go`、`configs/threshold-examples/m1_router.toml`。

## 反例守护
- 拒绝任何「按空闲内存自动启动/升档本地 llama-server/Ollama」（原 0109）。
- 拒绝在纯 CPU 机器（含 Intel Mac）把 ≥0.6B 嵌入模型设为默认（原 0109）。
- 拒绝后台向量任务无退避的固定周期重试；拒绝按硬件 Tier 自动注册本地对话 Provider（原 0109）。
- 拒绝「High/Low 合并一批提升吞吐」；「Low 批放大到 100」须同时给出后端并行能力证据（交互等待 ≤2s）（原 0099）。

## 已驳方案
| 方案 | 驳回理由 | 来源 |
|---|---|---|
| 保留 Ollama 仅默认降到 qwen3-embedding:0.6b | 仍两个常驻子进程 + 自动安装 | 0109 |
| 有 GPU 且内存够时自动开 llama-server | 门控只能估算；短文本上 4b 收益小，代价 2.5GB 下载与维度切换重建 | 0109 |
| 进程内 llama.cpp FFI 跑 0.6B GGUF | 需 tier1 构建；CPU 成本约为平衡档数倍 | 0109 |
| 复用语音 ORT 资产与 slot | 版本钉与语音耦合，升级牵连检索 | 0109 |
| Model2Vec 静态向量 | 中文语义召回偏弱；仅作 ONNX 不支持时的候选降级，暂不默认 | 0109 |
| 直接删除向量化只留 FTS | 语义召回明显退化 | 0109 |
| 仅调大适配器 30s 超时 | 掩盖队头阻塞 | 0099 |
| 回填绕开批处理器同步直连后端 | 失去去重与背压，洪峰压垮后端 | 0099 |
| High 抢占在途 Low 批（取消重发） | 浪费已完成计算，ollama 取消语义不可靠 | 0099 |

## 重新评估触发条件
- 两候选模型在本仓真实样本 Recall@10 均低于 qwen3-embedding:4b 的 80%；平衡档主力 CPU 回填持续占用 > 1 核超 10 分钟；主力硬件变为 Apple Silicon ≥16GB 且本地 0.6B 嵌入占用 < 1 核。
- 后端支持并行嵌入（ollama `OLLAMA_NUM_PARALLEL>1` 且 High 在 Low 并发下 p95 < 1s）→ 可放大 `low_max_batch_size`；回填积压持续 > 1h → 重估 Low 批上限。

## 沿革
| 日期 | 变更 | 原编号/理由 |
|---|---|---|
| 2026-09-25 | 双通道 + ctx 初稿；Low 批 16 → 8，调用方放弃改记 Debug | 原 0099 |
| 2026-10-02 | 本地推理默认关闭，ONNX 嵌入器 P0–P4 落地（512 维、HNSW 平滑迁移、硬件探测提示） | 原 0109 |
| 2026-10-02 | 取代 ADR-0062 的 Qwen3-0.6B GGUF 默认选型 | 原 0109 |
| 2026-10-12 | 0099 并入；核实代码：`llama_server` 后端被配置校验拒绝，其余 D1–D6 已接线 | 文档合并 |

## 引用代码
- `cmd/polaris/boot_embedding.go`、`cmd/polaris/boot_memory.go`
- `internal/llm/embedonnx/`、`internal/llm/embedassets/`、`internal/llm/ollamamgr/manager.go`、`internal/llm/dynamic_embedder.go`
- `internal/observability/probe/accelerator.go`、`internal/observability/probe/feature_gate_degradation.go`
- `internal/store/search/embedding_batcher.go`、`internal/store/search/batcher_adapter.go`、`internal/store/search/semantic_cache.go`
- `internal/config/config.go`（validateEmbeddingConfig）、`rust/substrate/src/surreal_store/mod.rs`
- `docs/arch/M01-Inference-Runtime.md`
