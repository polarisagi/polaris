# ADR-0062: 死代码治理 — 判定方法论 + `make deadcode` 门控

> 状态：Accepted（Tier1 embedding/rerank 选型条已被 [ADR-0109](./ADR-0109-local-inference-off-by-default-inprocess-embedder.md) 取代） ｜ 首次：2026-07-22 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0050/0051/0052/0053/0061 ｜ 模块：全仓库

## 决策：持续性 `make deadcode` CI 门控

`tools/baselines/deadcode-allowlist.txt` 逐条豁免 + `Makefile deadcode` 目标并入 `check-all`，取代"每隔几日人工重跑 `deadcode ./cmd/polaris/...` 再写一份 ADR"的模式（原五轮审计）。新发现直接进白名单或修复并注明理由，不再逐轮建 ADR。

## 判定方法论

`deadcode` 仅从 `cmd/polaris` 做可达性分析，不计 `_test.go` 边、不带 `tier1` build tag，命中混杂三类：
- **WIRE**：真实实现已在、有专属测试验证真实行为（非仅符号存在），只缺一处可定位调用点/适配器 → 接入
- **DELETE**：自承 mock/占位，或全仓零生产+零测试消费，或已被另一实现结构性替代 → 删除
- **EXEMPT**（白名单+理由）：spec 要求的安全原语但无可验证的统一触发点——强接即假接线，比不接更危险；测试专属/门控假阳性也属此类

核查"是否有设计依据"时检索范围须含 `docs/arch/M*.md` 本体，不能只查 `decisions/`（教训：`TaintBoundarySerializer` 初判有误）。

## 关键先例（不可推翻）

- `taint_sanitizer.SanitizeByDeterministicTransform` 必须保留不接入：[ADR-0007](./ADR-0007-taint-level-five-tier.md) 列为四降级器之一，刻意不接 S_VALIDATE；曾误删一次，已恢复入白名单——自动化清理命中此符号一律跳过
- `FactualityGuard.AddToGate` 删除正确，不构成 HE-7 回归：真实核验路径 `FactualityGuard.Verify` 独立接线于 `reflection_worker.go`
- `downloader.Get`/`NewSingleCredentialPool`/`planner.DefaultSpawner` 等"生产零调用但专属测试覆盖真实行为"的符号不得删除
- ~~Tier1 本地 Embedding/Rerank 选型 Qwen3-0.6B GGUF~~：已被 ADR-0109 取代（默认进程内 ONNX，llama-server 显式启用）；唯一保留的经验：社区转换 Reranker GGUF 常缺 `cls.output.weight` 致输出近零，仅用官方 GGUF 或正确转换脚本

## 反例守护

重跑 deadcode 见白名单条目不得"顺手删除"：测试假阳性删除丢安全回归覆盖；spec-backed sanitizer 删除违反 ADR-0007；tier1 门控项删除破坏本地推理路径。

## 重新评估触发条件

三类判定标准与先例长期适用，不因单次审计变化重议；仅当先例被证明不适用于新符号（如 `SanitizeByDeterministicTransform` 出现可验证的统一触发点）才重评是否接入 S_VALIDATE。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-07-14~07-22 | 五轮人工审计（0050/0051/0052/0053/0061） |
| 2026-07-22 | 建立 `make deadcode` 门控 |
| 2026-07-28 | 合并五份 ADR |
| 2026-10-02 | Tier1 选型条被 ADR-0109 取代；白名单迁至 `tools/baselines/` |

## 引用代码

`tools/baselines/deadcode-allowlist.txt`、`Makefile`（`deadcode`/`check-all`）、`internal/security/taint/taint_sanitizer.go`
