# ADR-0033: 记忆子系统范围限制与扩展综合决策

- **状态**: Accepted
- **日期**: 2026-07-02
- **决策者**: 架构组
- **相关模块**: M05（Memory 记忆子系统）/ M09（Self-Improvement 自进化引擎）/ `internal/memory/`

## 上下文

在 M05/M09 记忆与自进化子系统全量重构过程中，评估了多项潜在扩展能力。
本 ADR 作为 M05 记忆子系统的综合决策档案，收录三类决策：

1. **不做（Won't Do）**：明确排除以防止过度工程化和范围蔓延的能力项
2. **新增核心记忆工作区**：`ZoneCoreMemory` + `core_memory_edit` 工具（原 ADR-0036）
3. **时序记忆检索与信念修正**：`AsOf` 时间戳过滤 + `ExclusiveWriter` Jaccard 信念修正（原 ADR-0035）

---

## 决策一：不做列表（Won't Do）

### 1. SurrealKV O(1) 技能签名检索（源自 M05 §5）

**提案**：双轨技能检索——在 SQLite 之外引入 SurrealKV 快速 O(1) 查找路径。

**排除理由**：M6 架构已具备基于 SQLite 的 Registry + Selector，在当前规模下性能充足。引入双轨 SurrealKV 路径会带来状态同步复杂性，而在预期技能规模下无明确的即时性能收益。

**反例守护**：未来如有人提议"为加速技能查找引入 SurrealKV 双轨"——引用本 ADR 拒绝，现有 SQLite 路径达到瓶颈且压测证明后方可另立 ADR 重议。

### 2. L3/L4 进化多签审批机制

**提案**：为高层级自主自进化工作流（L3/L4 Evolution）引入多方密码学签名审批机制。

**排除理由**：当前基于启发式的自主改进与沙箱 fail-closed 机制已足够。引入多签要求会大幅降低自进化速率。单用户场景下多签无实际意义，已由"全量回归 + 影子部署报告 + 强制冷却期"的单人审批机制替代（详见 ADR-0029 §K 影子执行器）。

**反例守护**：未来如有人提议"L3/L4 自进化启用多签"——引用本 ADR 拒绝，多签适用于多租户生产场景，不适用于单用户本地 Agent 定位。

---

## 决策二：核心工作记忆区（ZoneCoreMemory）

> **来源**：原 ADR-0036（Core Working Memory Zone），2026-07-09 综合并入本 ADR。

### 背景

Polaris Agent 缺少一个可显式编辑、跨 session 持久的"核心记忆"区，用于维护长期上下文（如人物设定、长期任务状态、用户偏好）。此前 `PromptBuilder` 仅有三个 Zone（`ZoneImmutable` / `ZoneMutableSkill` / `ZoneTaintedData`），缺少可由 Agent 主动写入的持久状态槽位。

需要一个类似"LLM as OS"（GD-8-002）的机制，让 Agent 显式管理跨会话状态。

### 决策

**引入 `ZoneCoreMemory` 和 `core_memory_edit` 工具。**

**1. 结构化块 vs 单一自由文本**

- **已驳回方案**：单一自由文本块。
- **驳回理由**：单一文本块容易被 LLM 意外覆盖或丢失上下文。
- **决策**：核心记忆以键值块集合表示（如 `persona` / `task_state` / `user_prefs`），LLM 按块粒度编辑。

**2. 操作语义（set/append/delete）vs JSON Patch**

- **已驳回方案**：使用 JSON Patch 对记忆结构做精确修改。
- **驳回理由**：JSON Patch 语法对 LLM 生成而言较脆弱，容易格式错误。
- **决策**：`core_memory_edit` 工具采用简单的 `set` / `append` / `delete` 语义，最大化健壮性。

**3. 持久化（State-in-DB）vs 纯内存**

- **已驳回方案**：核心记忆仅保存在进程内存（如 ContextWindow）。
- **驳回理由**：核心状态应跨 Agent 重启持久化，支持跨 session 访问。
- **决策**：块持久化到独立的 `core_memory_blocks` SQL 表（`034_core_memory.sql`）。

**4. 硬上限**

- 单块大小上限：2KB；总核心记忆上限：8KB。
- 配置在 `state.yaml` 和 `thresholds.go` 中定义，防止 context window 爆炸。

**5. 污点追踪**

- 核心记忆写入时，保留执行上下文的污点级别，防止跨 session 的 Prompt Injection 漏洞。

### 后果

- **正向**：Agent prompt 新增 `<core_memory>` 标签区；Agent 可维护显式持久状态，提升长期任务连贯性。
- **负向**：`core_memory_edit` 工具必须通过标准 PolicyGate 检查，增加执行路径。
- **反例守护**：未来如有人提议"核心记忆只存内存不落库"——本 ADR 拒绝（违反 HE-6 State-in-DB 不变量）；如有人提议"跳过 PolicyGate 直接写核心记忆"——本 ADR 拒绝（违反 HE-7 防退化边界）。

---

## 决策三：时序记忆检索与 Jaccard 信念修正

> **来源**：原 ADR-0035（Temporal Memory Retrieval and Jaccard Belief Revision），2026-07-09 综合并入本 ADR。

### 背景

记忆检索时，有时需要查询某一特定时间点的事实快照（"as of"查询）。语义记忆通过 `valid_from` / `valid_until` 追踪事实生命周期，但 `HybridRetriever` 原本不支持时间过滤参数。

此外，在从情节事件提取新实体（记忆巩固阶段）时，某些实体（特别是 `user_preference`）可能在语义上与旧事实冲突但名称不完全相同，需要基于 Jaccard 相似度的自动信念修正机制。

### 决策

**1. AsOf 时序检索**

- 为 `memory_search` 引入 `as_of` 时间戳参数，通过 `RetrievalConfig` 向下传递到 `HybridRetriever`。
- 任何命中的认知结果在解析为语义实体时，检查 `valid_from` / `valid_until`：若实体在 `as_of` 时间点无效，则跳过该命中。
- 过滤在命中解析时、评分前执行，对 BM25、向量检索、图检索三路均透明适用。

**2. ExclusiveWriter（信念修正闭包）**

- 将巩固管线中的 Jaccard 冲突解析逻辑抽取为可复用的 `ExclusiveWriter` 组件。
- `ExclusiveWriter` 对 `user_preference` 实体执行 Jaccard 相似度比较（阈值 > 0.6），与现有活跃实体匹配时，自动调用 `MarkEntitySuperseded` 将旧事实标记为已被取代，再插入新事实。

**反例守护**：
- 未来如有人提议"直接覆写历史记忆条目而非标记 superseded"——本 ADR 拒绝，时序查询的前提是历史记录不可变。
- 未来如有人提议"user_preference 直接 UPSERT 不经 Jaccard 判断"——本 ADR 拒绝，相似但不同名的偏好冲突是最常见的记忆退化场景。

### 后果

- **正向**：获得时序查询能力而不变更历史记录；巩固逻辑解耦，更易测试；`ExclusiveWriter` 在 upsert 时自动处理信念修正。
- **负向**：`HybridRetriever` 命中解析路径略微复杂化（需额外查实体有效性）。
- **合规**：变更严格遵守单写者规则（inv_M2_01），`ExclusiveWriter` 通过 MutationBus 安全接线。

---

## 被驳回的方案汇总

| 方案 | 驳回理由 |
|------|---------|
| SurrealKV 双轨技能检索 | 现有 SQLite 路径充足；引入额外状态同步复杂度 |
| L3/L4 多签审批 | 单用户场景无意义；降低自进化速率；已由单人审批+影子部署替代 |
| 核心记忆单一自由文本块 | LLM 易意外覆盖；无块粒度保护 |
| JSON Patch 操作语义 | LLM 生成 JSON Patch 容易出错；语义过于复杂 |
| 核心记忆纯内存 | 违反 HE-6；跨 session 不持久 |
| 直接覆写历史记忆 | 破坏时序查询语义；历史记录必须不可变 |
| user_preference 直接 UPSERT 无冲突检测 | 语义冲突的旧偏好无法被正确淘汰 |

---

## 引用代码

- `internal/memory/store/semantic_mem.go`（`valid_from` / `valid_until` 语义实体字段）
- `internal/memory/retrieval/hybrid_retriever.go`（`RetrievalConfig.AsOf` + AsOf 过滤）
- `internal/memory/consolidation/exclusive_writer.go`（`ExclusiveWriter`，Jaccard 信念修正闭包）
- `internal/memory/consolidation/`（记忆巩固管线）
- `internal/protocol/schema/034_core_memory.sql`（`core_memory_blocks` 表 DDL SSoT）
- `internal/tool/builtin/core_memory_edit/`（`core_memory_edit` 工具实现）
- `internal/agent/context/`（`PromptBuilder.ZoneCoreMemory` 注入）
- `internal/observability/probe/feature_gate.go`（`FeatureActivationSteer`，Tier-1+ 门控）
- `internal/llm/adapter/steering.go`（`SteeringAdapter.SteerActivations`，Activation Steering 实现）
- `docs/arch/M05-Memory-System.md §3.1 §5 §6`

---

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-02 | 初稿，Accepted（范围限制决策：SurrealKV 双轨 + L3/L4 多签 Won't Do）|
| 2026-07-03 | 更新 Activation Steering 及 L3/L4 多签审批状态（Activation Steering 已实现，移出 Won't Do）|
| 2026-07-09 | 综合扩展：将原 ADR-0035（时序记忆检索 + Jaccard 信念修正）和原 ADR-0036（核心工作记忆区 ZoneCoreMemory）内容并入本 ADR；全文翻译为中文；补完整标准结构；Won't Do 第 2 项（L3/L4 多签）说明与 ADR-0038 关联 |
| 2026-07-22 | 时序检索的显式"有效窗"辅助（`memory/graph/temporal.go` 的 `SetValidWindow`/`IsValidAt`/`FormatValidWindow`）在 ADR-0062 中删除——最终时序检索未产出带显式有效窗的实体，衰减/时间戳已覆盖需求；世界模型突触可塑性（`SynapticPlasticityManager` 全套）同批删除（零生产构造点），均待未来立项重建（见 ADR-0062） |
