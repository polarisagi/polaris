# ADR-0062: 死代码治理 — 判定方法论 + `make deadcode` 门控（合并 ADR-0050/0051/0052/0053/0061）

- **状态**: Accepted（已执行）| **日期**: 2026-07-22（历次复核 2026-07-14~07-22 合并，2026-07-28）
- **模块**: 全仓库

## 决策

**建立持续性 `make deadcode` CI 门控**（`scripts/deadcode-allowlist.txt` 逐条豁免 + `Makefile deadcode` 目标并入 `check-all`），取代此前每隔几日人工重跑 `deadcode ./cmd/polaris/...` 并写一份 ADR 记录本轮结论的重复模式（原 ADR-0050/0051/0052/0053/0061 共五轮审计均属此模式，此后不再逐轮建 ADR，新发现直接进白名单或修复并注明理由）。

## 判定方法论（贯穿全部历次审计，予以固化为规范）

`deadcode` 静态分析仅从 `cmd/polaris` 主入口做可达性分析，不计 `_test.go` 调用边、不带 `tier1` build tag，因此命中列表混杂三类：①测试专属/门控假阳性（非死代码，保留+白名单）；②生产者/消费者分离、真实实现只缺一处接线（WIRE，找到即接入）；③自承 mock/占位或已被替代的真死代码（DELETE）。

三类判定标准：
- **WIRE**：真实实现已存在、有专门测试验证真实行为（非仅验证符号存在），只缺一处可定位的调用点/适配器。
- **DELETE**：自承 mock/占位标记，或全仓库零生产+零测试消费，或已被另一套实现结构性替代。
- **保留但不接入（EXEMPT，需白名单+理由）**：spec 明文要求的安全原语但缺乏可验证的统一触发点——强行接入即"看起来在跑但没有真实触发条件"的假接线，比不接入更危险。

核查一个符号"是否有设计依据"时，检索范围必须包含 `docs/arch/M*.md` 规范文档本体，不能只查 `decisions/` 目录——后者是已决议方案索引，前者才是权威功能需求源（教训来自 `TaintBoundarySerializer` 初判有误案例）。

## 关键个案先例（后续排查同类问题时优先援引，不可推翻）

- **`taint_sanitizer.SanitizeByDeterministicTransform` 必须保留不接入**：[ADR-0007](./ADR-0007-taint-level-five-tier.md)（含原 ADR-0047 决策）明文列为四降级器之一，刻意不接入 S_VALIDATE（无统一可验证触发点）。历史上曾被误删一次，已恢复并列入白名单——任何自动化清理脚本若再次命中此符号，直接跳过。
- **`FactualityGuard.AddToGate` 删除是正确的**，不构成 HE-7 防退化边界回归——真实核验路径 `FactualityGuard.Verify` 独立生产接线于 `reflection_worker.go`，`AddToGate` 是另一条从未注册且匹配条件永不成立的备选路径。
- **`downloader.Get`/`NewSingleCredentialPool`/`planner.DefaultSpawner` 等一类"生产零调用但有专属测试覆盖真实行为"的符号不得删除**——判断标准是测试是否验证真实业务行为而非仅符号存在。
- **Tier1 本地 Embedding/Rerank 选型**：Qwen3-Embedding-0.6B + Qwen3-Reranker-0.6B（GGUF Q8_0，Apache-2.0）。社区转换的 Reranker GGUF 常缺 `cls.output.weight` 张量导致输出近零垃圾分，仅用官方 GGUF 或正确转换脚本。config 落地为独立后续任务。

## 反例守护

未来重跑 deadcode 见白名单条目，不得"顺手删除"——A 类测试假阳性删除会丢安全回归覆盖；spec-backed sanitizer 删除违反 [ADR-0007](./ADR-0007-taint-level-five-tier.md)；tier1 门控项删除会破坏本地推理路径。

## 引用代码

`scripts/deadcode-allowlist.txt`、`Makefile`（`deadcode`/`check-all`）、`internal/security/taint/taint_sanitizer.go`

> 2026-08-09 追记：重新评估触发条件——本 ADR 的三类判定标准（WIRE/DELETE/EXEMPT）
> 与关键个案先例是长期适用的方法论，不因单次审计结果变化而重议；唯一的重议
> 触发点是先例本身被证明不适用于新发现的符号（如 `SanitizeByDeterministicTransform`
> 若未来真的出现可验证的统一触发点，才重新评估是否接入 S_VALIDATE，而非维持
> 永久 EXEMPT）。白名单收窄需求见 `local_playground/reports/plan-side-findings.md`
> 及各轮审计报告，本条不重复列出。
