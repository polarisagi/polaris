# ADR-0062：deadcode 44 项 DEFER 最终结清（删除为主 + 门控白名单 + 关键项复核恢复）

- **状态**: Accepted（已执行）
- **日期**: 2026-07-22
- **决策者**: MrLaoLiAI
- **相关模块**: 全仓库；重点 M11（taint_sanitizer / FactualityGuard）、M10（graphrag）、
  记忆图（synaptic_plasticity / temporal）、M12（incident_to_eval）、learning（drift / reflexion）
- **承接**: ADR-0051 / ADR-0052 / ADR-0061（同族 deadcode 治理）

## 背景

ADR-0061 把 `deadcode ./cmd/polaris/...` 复现的 44 项一律标 DEFER（暂不动作）。本轮
决定把 44 项从 DEFER 推进到终态。执行分两段：一份逐项处置提示词（`local_playground/
upgrade/deadcode-remediation-gemini-prompt.md`）交 Gemini 开发；随后人工复核实际落地。

## 根因（沿用 ADR-0061 结论）

`deadcode` 只从 `cmd/polaris` 主入口做可达性分析，不计 `_test.go` 调用边、不带
`tier1` build tag，故把三类混入同一 unreachable 列表：①测试/门控假阳性（非死代码）；
②生产者/消费者分离未接线；③历史真死代码 / 待选型。

## 决策与实际落地

### 门控机制（新建，源码零侵入）

- `scripts/deadcode-allowlist.txt`：逐条豁免（`file: unreachable func: Sym # 理由`）。
- `Makefile` `deadcode` 目标：跑 deadcode → `sed` 去 `:line:col` → 扣白名单 →
  有剩余则 `exit 1`；已并入 `check-all` 末位（CI 门控）。

### 逐项终态

| 项 | 符号 | 终态 | 说明 |
|---|------|------|------|
| A（13） | 测试假阳性 13 项 | EXEMPT | 白名单豁免，被 `_test.go` 真实消费，删除会丢覆盖 |
| — | `LlamaEmbed`/`LlamaRerank` | EXEMPT | tier1 门控假阳性，默认构建不可达属预期；Tier1 本地默认选型 = Qwen3-Embedding-0.6B + Qwen3-Reranker-0.6B（GGUF Q8_0），见 §选型 |
| B/D-删 | `TaintedJSONNode.AllStrings`/`MCPRetryPolicy`/`SkillEvolutionEngine` 全套/`ActiveContext.Rebuild`/`EvidenceSubgraphExtractor(+New)`/`PromptBuilder.WriteSkillContext`+`WriteUserInstruction`(+`types.Skill`)/`WithMaxExpandTokens` | DELETE | 自承 mock/占位/被替换/平行系统未建成，无生产与测试消费者 |
| C7 | `QLearner.Update`（+`QLearner`） | DELETE | 奖励信号从未定义，接线必臆造奖励语义（违反 R1），删除脚手架 |
| C1 | `EmbeddingVersionTracker.Update`（+类型/`EmbeddingStats`） | DELETE（接受） | 跨版本检索漂移统计，缺 `CognitiveSearcher` 版本自省接口；属增强非安全项 |
| C3 | `SynapticPlasticityManager` 全套 | DELETE（接受） | 世界模型突触可塑性，零生产构造点、无消费侧；移除待未来重新设计接入 |
| C4 | `temporal` 有效窗三函数 | DELETE（接受） | 时序检索最终形态未产出带显式有效窗的实体 |
| C5 | `graphrag.NewGraphWriter`/`NewGraphTraverser` | DELETE（接受） | 构造器死代码，延续 ADR-0051/0052「`GraphBuildPipeline.Run()` 结构性绕过」；`GraphWriter`/`GraphTraverser` 结构体仍被 `cluster.go`/`retriever.go` 签名引用，未孤儿，Tier1+ GraphRAG 仍未构造 |
| C6 | `reflexion.NewReflectionWorker` 构造器变体 | DELETE（接受） | `ReflectionWorker` 类型与 `ConsolidateReflections` 保留并在用（含 M11 FactualityGuard.Verify 抽样核验），仅删未用构造器变体 |
| C2 | `FactualityGuard.AddToGate` | DELETE（复核确认正确） | 见下「FactualityGuard 复核」 |
| D2 | `IncidentToEvalConverter.ReviewAndPromote` | DELETE（接受） | M12 §6 事故→Eval 人工审核晋升，HITL 前端未落地，后端入口一并移除待重建 |
| D3 | `WithSemanticCacheHints`/`WithThinkingBudget` | DELETE（接受） | StreamInfer 对称缓存分支未落地，Option 无消费者 |
| **复核恢复** | `SanitizeByDeterministicTransform` | **RESTORE + EXEMPT** | 见下「taint_sanitizer 复核」 |

### 偏差说明（重要）

提示词对 C1/C2/C3/C4/C5/C6/D2/D3 的**默认指令是"接线"**，仅在读到 `M_X` 规范明文
推翻时才删除。Gemini 实际执行（提交 `ffed293`，740 删除 / 26 新增）对全部 8 项
一律采取**删除**，未逐项举证规范推翻，亦未新建本 ADR。人工复核后按"混合：恢复关键项"
方针裁定：

- **接受删除**（6 项功能增强 + C7）：均为零消费侧、非安全防线项，删除消除长期挂账的
  死代码，符合"接线优先但无消费侧则不强留"原则；未来有真实需求时重新设计接入点。
- **复核确认正确**（C2）：见下。
- **复核恢复**（taint_sanitizer）：见下，唯一需回滚的删除。

### FactualityGuard.AddToGate 复核：删除正确，非防线回归

初判 `AddToGate` 删除触及 HE-7 防退化边界。逐层核对后**撤销该判断**：

1. D6 事实性防线的**真实核验 `FactualityGuard.Verify` 未被删**，且在
   `internal/learning/reflexion/reflection_worker.go` 的 `ConsolidateReflections`
   写入前抽样核验路径**已生产接线**（`NewFactualityGuard` → `InjectLLMProvider` →
   `Verify`），运行时防线不受影响。
2. `AddToGate` 是**另一条从未接线的备选登记路径**：把失败态注册为 PolicyGate 的
   `ForbidRule`，匹配条件为 `action == "emit_response"`。全仓 grep 确认**不存在任何
   `emit_response` gate 动作**——该匹配条件永不成立。即 `AddToGate` 是双重死代码
   （从未注册 + 匹配词永不出现），接线它须先臆造一个不存在的 gate 动作词，违反 R1。
3. 结论：删除 `AddToGate` 是正确的死代码清理，D6 防线经 `Verify` 保持激活。若未来
   需要"用户出口路径事实性门控"，应作为新特性正式立项（定义 emit_response gate 动作
   + FSM/gateway 出口调用 `Verify` 填充 ctx 标志），而非复活此钩子。

### taint_sanitizer 复核：恢复 SanitizeByDeterministicTransform

Gemini 删除了 `SanitizeByDeterministicTransform`，与 **ADR-0047 直接冲突**：
ADR-0047 §决策 3 + §反例守护明确将其列为 M11 §2.5「四降级器」之一，**刻意保留定义、
不接入 S_VALIDATE**（纯函数变换无统一可验证触发点，强接=假接线，HE-2 违规），并显式
守护"未来有需求重新设计触发点，而非删除"。删除会把四降级器砍成三个、造成 M11 §2.5
规范漂移并推翻 Accepted ADR。

处置：**原样恢复函数**（`internal/security/taint/taint_sanitizer.go`），补保留说明注释，
并加入 deadcode 白名单（定义保留、不接入的 spec-defined sanitizer，属预期不可达）。
当前无测试引用它（ADR-0061 所述"专属测试"已不存在，未一并恢复测试，因白名单已覆盖）。

## 选型：Tier1 本地默认 Embedding / Rerank（产品决策）

- Embedding = **Qwen3-Embedding-0.6B**（GGUF `Q8_0`，~0.6GB，1024 维，100+ 语言含中文/
  代码；同族 8B 居 MTEB 多语言榜 No.1，0.6B 仅微幅落后最优基线）。
- Rerank = **Qwen3-Reranker-0.6B**（GGUF `Q8_0`，同族原生配套）。
- 许可 Apache-2.0；4B/8B 为 config 可选升级档；模型文件走 `internal/downloader` 懒下载，
  不打进二进制（守 Tier0 体积）。
- **正确性约束**：仅用官方 `ggml-org`/`Qwen` GGUF 或 `convert_hf_to_gguf.py` 正确转换；
  大量社区 Qwen3-Reranker GGUF 缺 `cls.output.weight` 张量 → `/v1/rerank` 输出近零
  垃圾分；启动须 `--reranking --pooling rank --embedding` 且走 `/v1/rerank`。
- 落地：`configs/defaults.toml` + `internal/config/` 新增 `[inference.local]`（tier1 门控）。
  **本轮 Gemini 未落地该 config 键**（仅按门控白名单消警），选型记录于此，config 落地
  留作后续（不影响默认 Tier0 构建）。

## 后果

- **正向**：deadcode 门控归零（`make deadcode` = `deadcode ok`），CI 可持续拦截新增
  死代码；一次性清掉长期挂账的 mock/占位/未建平行系统；ADR-0047 一致性经恢复守住。
- **负向**：6 项 spec-backed 增强（突触可塑性 / 时序有效窗 / graphrag 构造器 / 漂移版本
  统计 / reflexion 构造器变体 / M12 HITL 晋升 / LLM 缓存 Option）被移除而非接线，相对
  M_X 规范是功能面收敛；后续如需须重新立项接线。相关 M_X 文档已作总结性同步（见下）。
- **反例守护**：未来若有人重跑 deadcode 见这 15 项白名单，勿再"顺手删"——A 类删了破坏
  测试/丢安全回归覆盖，sanitizer 删了违反 ADR-0047，tier1 项删了破坏本地推理。

## 文档同步（总结性）

- `docs/arch/M11-Policy-Safety.md §2.5`：四降级器不变（sanitizer 已恢复），无需改。
- 记忆图 / 时序 / graphrag / drift / reflexion / M12 相关 M_X：追加"本次移除"总结行，
  标注符号已删除、待未来立项，指回本 ADR。
- `docs/arch/decisions/README.md` + `CLAUDE.md §文档加载协议`：登记 ADR-0062。

## 验证

- `go build ./...` / `go vet ./...` / `make lint` / `go test ./...`：见提交记录。
- `make deadcode`：过滤后 0 剩余（白名单 15 项 = A 类 13 + tier1 2，恢复的 sanitizer 1，
  实为 16 行，以文件为准）。
- `internal/security/taint` 属内核包，`make generate-manifest` 重生 `kernel_manifest.json`。

## 引用代码

- `scripts/deadcode-allowlist.txt`、`Makefile`（`deadcode`/`check-all`）
- `internal/security/taint/taint_sanitizer.go`（`SanitizeByDeterministicTransform` 恢复）
- `internal/security/guard/factuality_guard.go` + `internal/learning/reflexion/reflection_worker.go`
  （FactualityGuard.Verify 生产接线证据）
- 提交 `ffed293`（final deadcode elimination）、`105ad86`（门控机制）
- `docs/arch/decisions/ADR-0047`、`ADR-0051`、`ADR-0052`、`ADR-0061`

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-22 | 初稿：44 项终态登记 + Gemini 删除路径偏差说明 + AddToGate 复核 + sanitizer 恢复 + Tier1 选型 |
