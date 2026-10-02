# ADR-0109: 本地推理默认关闭——向量化改用独立的进程内 ONNX 嵌入器，llama-server/Ollama 仅显式配置启用

- **状态**: Proposed
- **日期**: 2026-10-02
- **决策者**: 用户（需求）/ Opus（设计）/ Sonnet（实现）
- **相关模块**: M1 Inference / `cmd/polaris/boot_substrate.go` / `cmd/polaris/boot_memory.go` / `internal/observability/probe/` / `internal/llm/` / `internal/memory/retrieval/` / `internal/ffi/`
- **取代**: ADR-0062 中"Tier1 默认 embedding/rerank = Qwen3-0.6B GGUF + llama.cpp"的选型部分；`probe` 的 Embedding 内存阶梯自动升档（Local→HQ→Ultra→Max）

## 上下文

2026-10-02 实测（Intel i9-9880H / 16GB / macOS 15.8，无可用 GPU 加速）：`polaris serve → ollama serve → llama-server --embedding` 加载 qwen3-embedding:4b，**877% CPU、28% 内存**。根因三条叠加：

1. **门控只看空闲内存不看算力**：≥6GB 空闲即升到 4b，纯 CPU 机器上是最差组合。
2. **失败无限重试**：4b 太慢 → `EmbedBatch` 超时/runner EOF（当日 19 次）→ 5 分钟 online reindexer + 插件向量回填反复重来，CPU 持续占满而向量从未写入。ADR-0108 R3"TTS 基准被向量回填/runner 预热污染"是同一根因的另一症状。
3. **优先级与配置注释相反**：代码顺序为"显式本地模型 → 自动检测 → 远程 base_url"，即用户配了远程 API 仍会拉起本地 Ollama；config.toml 却写"用户配置优先"。

另：Tier1 时自动注册 `ollama-local` 对话 Provider（`TierLocalModel` → `Qwen3-8B-Q4_K_M`），该模型从未下载且不是合法 Ollama 标签；其 role 为空 → `costTier=0`，与 DeepSeek default 同档按 healthScore 竞争，后台调用（记忆/摘要等）可能被路由到这个空壳 Provider。

说明：会话记忆、情景记忆、知识库摘要、GraphRAG 抽取等**文本生成**一律走路由到的对话模型（当前为远程 DeepSeek），不依赖本地模型；本地 llama-server 只承担**向量化**（记忆检索、知识库 RAG、插件/技能匹配、surprise 漂移）。

## 决策

**默认不启动任何本地推理子进程。向量化默认用进程内 ONNX 小模型；llama-server/Ollama 降为"用户显式配置才启用"的加速选项。**

### D1 Embedding 选择顺序（唯一真相源，替换 boot_substrate 现有分支）

1. `[embedding].base_url` 非空 → 远程 OpenAI 兼容 API。**最高优先，绝不再拉起本地 Ollama。**
2. `[embedding].backend = "ollama" | "llama_server"` 且 `model` 非空 → 启动对应本地服务（显式开启，见 D4）。
3. 否则 `backend = "onnx"`（默认）→ 进程内 ONNX 嵌入器（D2）。
4. ONNX 不可用（平台无库 / macOS < 15.5 / 基准判不支持）→ 不挂载嵌入器，检索降级 FTS（既有 Tier1 路径）。

新增 `[embedding].backend = "auto"|"onnx"|"ollama"|"llama_server"|"none"`，默认 `auto`（= 1→3→4）。`none` 显式关闭向量化。

### D2 独立的进程内嵌入器（默认后端，与语音解耦）

- **与语音完全解耦**：不复用 `audiorun` 的资产、slot、版本钉。嵌入器自带并独立钉版本的 `libonnxruntime`（经 purego 调 ORT C API，ADR-0011 路线；不新增子进程、不引入 cgo），独立下载与 sha256 校验、独立生命周期。代价：磁盘多一份 ORT（约 20–35MB），换取语音升级/降级不牵连检索。
- **模型分档（质量与资源平衡，P0 spike 定稿）**：

| 档位 | 模型 | 规模 / 维度 | 定位 |
|------|------|------------|------|
| 平衡档（默认） | `EmbeddingGemma-300M` ONNX int8，Matryoshka 截断到 **512 维** | 308M（其中约 2/3 为词表，计算量接近 100M 级 BERT）；量化后常驻 < 200MB | 100+ 语言含中文、2K 上下文；<500M 量级多语言榜首 |
| 轻量档（弱 CPU 自动降档） | `bge-small-zh-v1.5` int8 | 24M / 512 维，~24MB | 中文短文本，CPU 最省 |
| 远程档 | `[embedding].base_url` | — | 质量最高、本地零 CPU |

  P0 spike 必须用本仓记忆 + 知识库真实样本比较平衡档、轻量档与当前 qwen3-embedding:4b 的 Recall@10，并在 Intel i9 上实测单条延迟与回填吞吐；许可证（Gemma 使用条款）一并核实，不满足则平衡档改为 `bge-base-zh-v1.5`（102M / 768 维）。
- **两档统一 512 维（硬约束）**：平衡档 Matryoshka 截断到 512 并重新 L2 归一化，轻量档原生 512。档位切换不改变 SurrealDB HNSW `DIMENSION`，只需按 `embed_model_version` 重嵌，不需要重建索引结构。不同模型的向量即使同维也不可混检（D5）。
- **运行形态不作判据**：不识别"桌面 / VPS"。低配 VPS 与弱 CPU 笔记本走同一条规则：先过内存下限（总内存 < 1.5GB 或可用 < 600MB → 直接轻量档），再由启用基准决定。只要基准达标，VPS 上也优先用 EmbeddingGemma。
- **档位决定持久化**：基准结论写入 `preferences`（键 `embed.tier_bench`，结构参照 `audio.tts_bench`），重启直接复用，不在每次启动重测，避免档位来回切换导致反复重嵌。仅在模型资产变更、用户手动"重新评估"或结论标记 `retry_on_start` 时重测。
- **手动覆盖**：`[embedding].onnx_model = "auto"|"embeddinggemma"|"bge-small-zh"`，默认 `auto`。
- **分词器**：平衡档为 Gemma SentencePiece（纯 Go 实现可用，如 `go-sentencepiece`）；轻量档为 BERT WordPiece（纯 Go 自实现）。
- **生命周期（与语音同一理念，各自独立实现）**：懒加载——首次检索或写入记忆时加载（预期 < 1s）；活跃期常驻（空闲时 CPU 为 0，只占内存）；**空闲 10 分钟卸载**。CPU 只在实际编码时消耗。
- **资源上限（硬约束）**：前台查询 `intra_op = min(2, 逻辑核/4)`；后台回填 1 线程 + 低优先级；`inter_op = 1`。
- **启用基准与自动降档**：首次启用跑吞吐基准（64 条中文短文本），沿用 ADR-0108 D2 的"空闲窗口 + Contended 不落定论"判定：平衡档单条 p95 ≤ 80ms → 用平衡档；否则试轻量档，p95 ≤ 30ms → 用轻量档；仍不满足 → 降级 FTS。
- 模型文件按需下载到 `models_dir`，不内嵌二进制（保持 Tier0 体积约束）。

### D3 硬件探测扩面（只决定"能否/建议"，不决定"自动启动"）

`probe` 新增 `AcceleratorInfo`：

| 平台 | 探测内容 | 判定为"可加速" |
|------|---------|--------------|
| darwin/arm64 | Apple Silicon 型号、统一内存 | 统一内存 ≥ 16GB |
| darwin/amd64 | `system_profiler SPDisplaysDataType` | **一律否**（Intel Mac 的 AMD/Intel GPU 不作为 llama.cpp 加速目标） |
| linux / windows | `nvidia-smi` 显存、驱动；无则视为无 | NVIDIA 显存 ≥ 6GB |

另记录物理核数、CPU 代际、近 1 分钟负载。Embedding 档位**不再按空闲内存自动升档**：删除 `FeatureHQEmbedding/Ultra/Max` 的自动启用语义，`FeatureLocalEmbedding` 改为表示"ONNX 嵌入器可用"。

"可加速"机器上，前端设置页**提示**可升级到 Qwen3-Embedding（llama-server），用户确认后写入 D1 第 2 条配置——**不自动启动**。

### D4 llama-server / Ollama：仅显式配置

- 不再默认 `EnsureOllama` 自动安装/下载 ollama-dist；仅当 `[embedding].backend` 或 `[providers]` 显式指向本地服务时才安装、启动。
- 显式启用时强制护栏：`-t` 线程上限（默认 物理核/2）、子进程 `nice +10`、`-np 1`；进程退出或连续失败走 D5 熔断。
- 用户已装的外部 Ollama 可通过 `base_url` 指向，polaris 不管理其生命周期。

### D5 后台向量任务护栏（与后端无关，全部生效）

- online reindexer 与插件向量回填：连续失败 3 次 → 指数退避 5m→30m→2h→停止，直至后端变更或手动触发；状态暴露到 `/v1/system/status`。
- 后台批一律走 Low 通道（ADR-0099），单批超时 = 交互通道的 3 倍而非无上限重试。
- 嵌入模型 ID + 维度随向量落库；后端/模型变化时由 reindexer 低速重建（每轮限量），旧模型向量在重建完成前不参与混检，期间 FTS 兜底。
- **HNSW 维度迁移（修复既有缺陷）**：`rust/substrate/src/surreal_store/mod.rs` 以 `DEFINE INDEX IF NOT EXISTS … DIMENSION {vec_dim}` 建索引，维度一旦变化旧索引不会更新，新向量写入静默失败。启动时必须比对已落库维度与期望维度，不一致则删除旧向量与索引后按新维度重建，并触发重嵌。

### D6 本地对话模型：删除自动注册

- 删除 `TierLocalModel` 驱动的 `ollama-local` 自动注册。本地对话模型只在 `[providers]` 中由用户显式添加时注册（与远程 Provider 同一路径，带明确 role）。
- `llama-local`（FFI）同理：仅显式配置模型路径时注册。
- QLoRA / PRM / Steering 等依赖本地 Ollama 的适配器：门控条件从"硬件 Tier"改为"本地推理后端已显式启用"。

## 后果

- **正向**:
  - 默认安装不再有任何常驻本地推理子进程；向量化 CPU 占用从"多核占满"降到"按需 ≤2 线程、空闲卸载"。
  - 远程 embedding 配置真正优先；本地对话空壳 Provider 不再抢占后台路由。
  - 消除 ADR-0108 R3 的争用源之一。
- **负向**:
  - 默认向量质量低于 qwen3-embedding:4b（平衡档约低一档，轻量档更低）；知识库长文档召回会下降，需在 UI 提示"配置远程 embedding 可获得更好检索"。
  - 已有 2560 维向量需重建（D5 低速重建期间检索以 FTS 为主）。
  - 新增纯 Go 分词器（SentencePiece + WordPiece）与 ORT purego 绑定的维护面；ORT 与语音各持一份。
  - macOS < 15.5（ORT minos）无本地嵌入器，只能远程或 FTS。
- **反例守护**:
  - 拒绝任何"按空闲内存自动启动/升档本地 llama-server/Ollama"的提议。
  - 拒绝在纯 CPU 机器（含 Intel Mac）上把 ≥0.6B 的嵌入模型设为默认。
  - 拒绝后台向量任务无退避的固定周期重试。
  - 拒绝按硬件 Tier 自动注册本地对话 Provider。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留 Ollama，只把默认降到 qwen3-embedding:0.6b | 仍是两个常驻子进程 + 自动安装；0.6B 在 Intel CPU 上回填仍会长时间占核 |
| 有 GPU 且内存够时自动开启 llama-server | 门控只能估算，用户机器同时跑其他负载；短文本匹配上 4b 相对小模型收益小，代价是 2.5GB 下载与维度切换重建 |
| 进程内 llama.cpp FFI 跑 0.6B GGUF | 需 tier1 构建；CPU 成本约为平衡档数倍 |
| 复用语音的 ORT 资产与 slot | 版本钉（sherpa 1.13.2）与语音耦合，语音升级会牵连检索；用户要求两者独立 |
| Model2Vec 静态向量（无神经网络推理） | CPU 近零但中文语义召回偏弱，作为 D2 不支持时的候选降级保留，暂不默认 |
| 直接删除向量化，只留 FTS | 记忆/知识库语义召回明显退化 |

## 实施拆分（交 Sonnet，合入 main）

1. **P0 spike**：独立 ORT purego 最小绑定 + 两种分词器；EmbeddingGemma-300M(512 维) / bge-small-zh / qwen3-embedding:4b 召回对比；Intel i9 实测延迟与回填吞吐；核实许可证。产出定稿回填 D2。
2. **P1 止血**（可先于 P0 合入）：D1 选择顺序 + `backend` 配置键；D4 取消默认 EnsureOllama；D5 熔断退避；D6 删除自动注册。完成后默认路径 = 远程或 FTS。
3. **P2**：`internal/llm/embedonnx`（或同级包）实现 `search.Embedder`，接入 D1 第 3 条；独立的懒加载/空闲卸载生命周期；启用基准与自动降档。
4. **P3**：D3 `AcceleratorInfo` 探测 + 删除 Embedding 内存阶梯 + 前端"可升级"提示。
5. **P4**：维度迁移（模型 ID/维度落库、低速重建、旧维度隔离）；config.toml 注释与 `configs/defaults.toml` 同步。

每步验收：`go test ./...` + `make check-all` 自跑，不采信自述；P1 后实测 `ps` 中不再出现 ollama/llama-server。

## 引用代码

- `cmd/polaris/boot_substrate.go`（embedding 选择分支、`ollama-local` 注册）
- `cmd/polaris/boot_memory.go`（online reindexer 5m ticker）
- `internal/observability/probe/feature_gate.go`、`feature_gate_degradation.go`（Embedding 阶梯、`TierLocalModel`）
- `internal/llm/provider_registry.go`（`costTier` 空 role = 0 档）
- `internal/llm/audiorun/`（仅作生命周期与基准门控的参考实现，不复用）
- `internal/ffi/dylib.go`（purego 加载）

## 重新评估触发条件

- P0 spike 显示两候选模型在本仓真实样本上 Recall@10 均低于 qwen3-embedding:4b 的 80%。
- 平衡档在主力 CPU 机器上回填期间持续占用 > 1 核超过 10 分钟。
- 用户群主力硬件变为 Apple Silicon ≥16GB 且实测本地 0.6B 嵌入后台 CPU 占用 < 1 核。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-02 | 初稿（Proposed） |
